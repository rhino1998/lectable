package store

import (
	"cmp"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Cached is a Store that serves its hot reads from memory, invalidated by
// the inner Store's own write notifications (OnChange) rather than by
// hand-listing which writes touch what: each cached read declares the
// tables it reads, and any committed write to one of those tables (or an
// unclassifiable one, AllTables) drops every entry depending on it before
// the write method returns. Everything not overridden below - every write,
// and reads not worth caching - passes straight through to the inner
// Store via embedding.
//
// Why: job dispatch, lookahead and the live topics re-read the
// same book/chapter/character/preset rows constantly, far more often than
// those rows change.
//
// Values handed out are copies (see the clone* helpers), so a caller
// mutating what it got back can't corrupt the cache.
type Cached struct {
	Store

	mu sync.Mutex
	// gens counts invalidations per table (allGen for AllTables). A read
	// snapshots the sum over its tables before loading and only stores
	// its result if the sum is unchanged afterward - otherwise a write
	// landed mid-read and the result may already be stale.
	gens    map[string]uint64
	allGen  uint64
	entries map[string]any
	byTable map[string]map[string]struct{} // table -> keys depending on it
	// narration is ChapterNarration's own cache, per book - see there.
	narration map[string]*bookNarration
	// narrationAll counts writes to narrationTables that named no book
	// (BookScope), narrationBook those that did, per book.
	narrationAll  uint64
	narrationBook map[string]uint64

	hits, misses atomic.Uint64
}

// maxCacheEntries bounds memory: past it the whole cache is dropped and
// refills from scratch. Generous - a long book's chapters, paragraphs and
// characters are a few thousand entries.
const maxCacheEntries = 50000

// NewCached wraps inner with a read cache and subscribes it to inner's
// write notifications.
func NewCached(inner Store) *Cached {
	c := &Cached{
		Store:   inner,
		gens:    map[string]uint64{},
		entries: map[string]any{},
		byTable: map[string]map[string]struct{}{},
	}
	inner.OnChange(c.invalidate)
	return c
}

func (c *Cached) invalidate(tables []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range tables {
		if t == AllTables {
			c.allGen++
			c.entries = map[string]any{}
			c.byTable = map[string]map[string]struct{}{}
			c.narration = nil
			return
		}
	}
	scope, narration := "", false
	for _, t := range tables {
		if IsBookScope(t) {
			scope = t
			continue
		}
		narration = narration || slices.Contains(narrationTables, t)
		c.gens[t]++
		for key := range c.byTable[t] {
			delete(c.entries, key)
		}
		delete(c.byTable, t)
	}
	switch {
	case !narration:
	case scope == "":
		c.narrationAll++
	default:
		if c.narrationBook == nil {
			c.narrationBook = map[string]uint64{}
		}
		c.narrationBook[scope]++
	}
}

func (c *Cached) genLocked(tables []string) uint64 {
	sum := c.allGen
	for _, t := range tables {
		sum += c.gens[t]
	}
	return sum
}

// Stats reports cache hits and misses since startup.
func (c *Cached) Stats() (hits, misses uint64) {
	return c.hits.Load(), c.misses.Load()
}

// LogStatsEvery logs the hit rate every interval until stop closes.
func (c *Cached) LogStatsEvery(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	var lastHits, lastMisses uint64
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			hits, misses := c.Stats()
			dh, dm := hits-lastHits, misses-lastMisses
			lastHits, lastMisses = hits, misses
			if dh+dm == 0 {
				continue
			}
			c.mu.Lock()
			n := len(c.entries)
			c.mu.Unlock()
			log.Printf("store: cache %d hits / %d misses (%.0f%%) in last %s, %d entries", dh, dm, 100*float64(dh)/float64(dh+dm), interval, n)
		}
	}
}

// cachedRead is the read-through step every cached method shares: key
// identifies the call (method + args), tables are what it reads, clone
// copies a value so neither the cache nor a caller sees the other's
// mutations.
func cachedRead[T any](c *Cached, key string, tables []string, clone func(T) T, load func() (T, error)) (T, error) {
	c.mu.Lock()
	if v, ok := c.entries[key]; ok {
		c.mu.Unlock()
		c.hits.Add(1)
		return clone(v.(T)), nil
	}
	gen := c.genLocked(tables)
	c.mu.Unlock()
	c.misses.Add(1)

	v, err := load()
	if err != nil {
		return v, err
	}

	c.mu.Lock()
	if c.genLocked(tables) == gen {
		if len(c.entries) >= maxCacheEntries {
			c.entries = map[string]any{}
			c.byTable = map[string]map[string]struct{}{}
		}
		c.entries[key] = clone(v)
		for _, t := range tables {
			keys := c.byTable[t]
			if keys == nil {
				keys = map[string]struct{}{}
				c.byTable[t] = keys
			}
			keys[key] = struct{}{}
		}
	}
	c.mu.Unlock()
	return v, nil
}

func cacheKey(method string, args ...string) string {
	return method + "\x00" + strings.Join(args, "\x00")
}

func same[T any](v T) T { return v }

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

func cloneParagraph(p Paragraph) Paragraph {
	p.Pronunciation = slices.Clone(p.Pronunciation)
	p.Emphasis = slices.Clone(p.Emphasis)
	p.Table.Cells = slices.Clone(p.Table.Cells)
	return p
}

func cloneParagraphPtr(p *Paragraph) *Paragraph {
	if p == nil {
		return nil
	}
	c := cloneParagraph(*p)
	return &c
}

func cloneParagraphs(ps []Paragraph) []Paragraph {
	if ps == nil {
		return nil
	}
	out := make([]Paragraph, len(ps))
	for i, p := range ps {
		out[i] = cloneParagraph(p)
	}
	return out
}

func cloneCharacterPtr(ch *Character) *Character {
	if ch == nil {
		return nil
	}
	c := *ch
	c.Aliases = slices.Clone(c.Aliases)
	return &c
}

func cloneCharacters(cs []Character) []Character {
	if cs == nil {
		return nil
	}
	out := make([]Character, len(cs))
	for i, ch := range cs {
		out[i] = ch
		out[i].Aliases = slices.Clone(ch.Aliases)
	}
	return out
}

func cloneMap[K comparable, V any](m map[K]V) map[K]V {
	if m == nil {
		return nil
	}
	out := make(map[K]V, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

var (
	booksTables          = []string{"books", PositionTable}
	chaptersTables       = []string{"chapters"}
	paragraphsTables     = []string{"paragraphs"}
	charactersTables     = []string{"characters"}
	characterVoiceTables = []string{"character_voices"}
	voicePresetTables    = []string{"voice_presets"}
	defaultVoiceTables   = []string{"default_voice"}
)

// ListLexicon is read once per generation task (jobs.Manager.lexiconFor),
// far more often than a listener edits the lexicon.
func (c *Cached) ListLexicon(scope string) ([]LexiconEntry, error) {
	return cachedRead(c, cacheKey("ListLexicon", scope), []string{"pronunciation_lexicon"}, slices.Clone[[]LexiconEntry], func() ([]LexiconEntry, error) {
		return c.Store.ListLexicon(scope)
	})
}

func (c *Cached) GetBook(id string) (*Book, error) {
	return cachedRead(c, cacheKey("GetBook", id), booksTables, clonePtr[Book], func() (*Book, error) {
		return c.Store.GetBook(id)
	})
}

func (c *Cached) GetChapterByID(id string) (*Chapter, error) {
	return cachedRead(c, cacheKey("GetChapterByID", id), chaptersTables, clonePtr[Chapter], func() (*Chapter, error) {
		return c.Store.GetChapterByID(id)
	})
}

func (c *Cached) GetChapterByIdx(bookID string, idx int) (*Chapter, error) {
	return cachedRead(c, cacheKey("GetChapterByIdx", bookID, strconv.Itoa(idx)), chaptersTables, clonePtr[Chapter], func() (*Chapter, error) {
		return c.Store.GetChapterByIdx(bookID, idx)
	})
}

func (c *Cached) GetParagraph(id string) (*Paragraph, error) {
	return cachedRead(c, cacheKey("GetParagraph", id), paragraphsTables, cloneParagraphPtr, func() (*Paragraph, error) {
		return c.Store.GetParagraph(id)
	})
}

func (c *Cached) ListParagraphsRaw(chapterID string) ([]Paragraph, error) {
	return cachedRead(c, cacheKey("ListParagraphsRaw", chapterID), paragraphsTables, cloneParagraphs, func() ([]Paragraph, error) {
		return c.Store.ListParagraphsRaw(chapterID)
	})
}

func (c *Cached) GetCharacter(id string) (*Character, error) {
	return cachedRead(c, cacheKey("GetCharacter", id), charactersTables, cloneCharacterPtr, func() (*Character, error) {
		return c.Store.GetCharacter(id)
	})
}

func (c *Cached) GetCharacterByName(scope, name string) (*Character, error) {
	return cachedRead(c, cacheKey("GetCharacterByName", scope, name), charactersTables, cloneCharacterPtr, func() (*Character, error) {
		return c.Store.GetCharacterByName(scope, name)
	})
}

func (c *Cached) ListCharacters(scope string) ([]Character, error) {
	return cachedRead(c, cacheKey("ListCharacters", scope), charactersTables, cloneCharacters, func() ([]Character, error) {
		return c.Store.ListCharacters(scope)
	})
}

func (c *Cached) CharacterVoiceForModel(characterID, cloneModel string) (string, error) {
	return cachedRead(c, cacheKey("CharacterVoiceForModel", characterID, cloneModel), characterVoiceTables, same[string], func() (string, error) {
		return c.Store.CharacterVoiceForModel(characterID, cloneModel)
	})
}

func (c *Cached) CharacterVoicesForModel(characterIDs []string, cloneModel string) (map[string]string, error) {
	key := cacheKey("CharacterVoicesForModel", append([]string{cloneModel}, characterIDs...)...)
	return cachedRead(c, key, characterVoiceTables, cloneMap[string, string], func() (map[string]string, error) {
		return c.Store.CharacterVoicesForModel(characterIDs, cloneModel)
	})
}

func (c *Cached) GetVoicePreset(id string) (*VoicePreset, error) {
	return cachedRead(c, cacheKey("GetVoicePreset", id), voicePresetTables, clonePtr[VoicePreset], func() (*VoicePreset, error) {
		return c.Store.GetVoicePreset(id)
	})
}

func (c *Cached) ListVoicePresets() ([]VoicePreset, error) {
	return cachedRead(c, cacheKey("ListVoicePresets"), voicePresetTables, slices.Clone[[]VoicePreset], func() ([]VoicePreset, error) {
		return c.Store.ListVoicePresets()
	})
}

func (c *Cached) GetDefaultVoice() (DefaultVoice, error) {
	return cachedRead(c, cacheKey("GetDefaultVoice"), defaultVoiceTables, same[DefaultVoice], func() (DefaultVoice, error) {
		return c.Store.GetDefaultVoice()
	})
}

var _ Store = (*Cached)(nil)

// bookNarration is one book's cached ChapterNarration result: the chapters
// as read for one voice assignment (key), and the fingerprint each was
// read at or after.
type bookNarration struct {
	key      string
	chapters []ChapterNarration
	prints   map[string]string
	// gen counts the writes that could have changed this book's
	// fingerprints (narrationGenLocked), from before prints was read: while
	// it still stands, nothing a fingerprint covers has been written.
	gen uint64
}

// narrationTables are the tables a write must name to change a chapter's
// fingerprint (ChapterFingerprints): its row, its paragraphs (chapter_text
// is kept by their triggers) and its paragraph_audio rows.
var narrationTables = []string{"chapters", "paragraphs", "chapter_text", "paragraph_audio"}

// narrationGenLocked counts the writes to narrationTables that could have
// reached bookID: every one that named no book, and those naming this one.
func (c *Cached) narrationGenLocked(bookID string) uint64 {
	return c.allGen + c.narrationAll + c.narrationBook[BookScope(bookID)]
}

// ChapterNarration answers a whole-book call (no chapterIDs) from the last
// result for the same book and voices, re-reading only the chapters whose
// fingerprint changed since (ChapterFingerprints: the chapter's row, its
// paragraphs' text and speakers, its paragraph_audio rows). Why: the
// library listing is rebuilt on every paragraph that finishes generating,
// and a write to one book - an imported chapter, a pass finishing - must
// not cost a re-read of every other book. With no write since the last
// call that could have changed this book's fingerprints (narrationTables,
// less the writes naming another book - BookScope), they aren't read at
// all: on a library of 108 books they were 170ms of a 200ms
// listing, most of whose rebuilds follow a saved position or a job.
func (c *Cached) ChapterNarration(bookID, voiceID string, overrides []SpeakerVoice, chapterIDs []string) ([]ChapterNarration, error) {
	if len(chapterIDs) > 0 {
		return c.Store.ChapterNarration(bookID, voiceID, overrides, chapterIDs)
	}
	var key strings.Builder
	key.WriteString(voiceID)
	for _, ov := range overrides {
		fmt.Fprintf(&key, "\x00%s\x00%d\x00%s", ov.Speaker, ov.SpeakerIndex, ov.VoiceID)
	}

	c.mu.Lock()
	gen := c.allGen
	tablesGen := c.narrationGenLocked(bookID)
	prev := c.narration[bookID]
	c.mu.Unlock()
	if prev != nil && prev.key == key.String() && prev.gen == tablesGen {
		c.hits.Add(1)
		return slices.Clone(prev.chapters), nil
	}

	// Fingerprints are read before the chapters, so a write landing in
	// between leaves a chapter newer than its fingerprint - re-read next
	// time, never served stale.
	prints, err := c.Store.ChapterFingerprints(bookID)
	if err != nil {
		return nil, err
	}
	var chapters []ChapterNarration
	// A chapter added or removed re-reads the book: prev has no row to
	// keep for it.
	sameChapters := prev != nil && prev.key == key.String() && len(prints) == len(prev.chapters)
	var changed []string
	if sameChapters {
		for _, ch := range prev.chapters {
			fp, ok := prints[ch.ID]
			if !ok {
				sameChapters = false
				break
			}
			if fp != prev.prints[ch.ID] {
				changed = append(changed, ch.ID)
			}
		}
	}
	switch {
	case !sameChapters:
		c.misses.Add(1)
		if chapters, err = c.Store.ChapterNarration(bookID, voiceID, overrides, nil); err != nil {
			return nil, err
		}
	case len(changed) == 0:
		c.hits.Add(1)
		chapters = slices.Clone(prev.chapters)
	default:
		c.misses.Add(1)
		fresh, err := c.Store.ChapterNarration(bookID, voiceID, overrides, changed)
		if err != nil {
			return nil, err
		}
		byID := make(map[string]ChapterNarration, len(fresh))
		for _, ch := range fresh {
			byID[ch.ID] = ch
		}
		chapters = slices.Clone(prev.chapters)
		for i, ch := range chapters {
			if f, ok := byID[ch.ID]; ok {
				chapters[i] = f
			}
		}
		// A re-read chapter may have moved.
		slices.SortStableFunc(chapters, func(a, b ChapterNarration) int { return cmp.Compare(a.Idx, b.Idx) })
	}

	c.mu.Lock()
	if c.allGen == gen {
		if c.narration == nil {
			c.narration = map[string]*bookNarration{}
		}
		c.narration[bookID] = &bookNarration{key: key.String(), chapters: slices.Clone(chapters), prints: prints, gen: tablesGen}
	}
	c.mu.Unlock()
	return chapters, nil
}
