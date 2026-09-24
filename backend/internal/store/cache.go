package store

import (
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
// Why: the store has a single DuckDB connection, so every read waits for
// every other one. Job dispatch, lookahead and the live topics re-read the
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
			return
		}
	}
	for _, t := range tables {
		c.gens[t]++
		for key := range c.byTable[t] {
			delete(c.entries, key)
		}
		delete(c.byTable, t)
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
	p.DescribesCharacters = slices.Clone(p.DescribesCharacters)
	p.Pronunciation = slices.Clone(p.Pronunciation)
	p.Emphasis = slices.Clone(p.Emphasis)
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
