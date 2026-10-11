package semantic

import (
	"container/heap"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// Embedder turns texts into unit-length vectors, one each, in order -
// internal/ttsworker.Manager.Embed. query asks for the CPU copy of the
// model (a search phrase); a chapter's paragraphs go to the GPU one.
type Embedder interface {
	Embed(ctx context.Context, texts []string, query bool) ([][]float32, error)
}

// Library is what the service reads of the library (store.Store).
type Library interface {
	ChapterTextRevs(bookID string) (map[string]int64, error)
	AllChapterTextRevs() (map[string]map[string]int64, error)
	ChapterIdxByID(bookID string) (map[string]int, error)
	ListParagraphsRaw(chapterID string) ([]store.Paragraph, error)
	GetParagraph(id string) (*store.Paragraph, error)
}

const (
	// minChars is the shortest paragraph worth a vector. Shorter ones
	// ("Loth." / "Like what?") say nothing to search for and crowd out
	// real answers: a fifth of the library's paragraphs.
	minChars = 20
	// maxChars is how much of a paragraph is embedded; the worker reads
	// 512 tokens of it at most anyway.
	maxChars = 2400
	// embedBatch is how many paragraphs one worker call carries.
	embedBatch = 64
	// cacheBytes bounds the vectors held in memory for scanning (a series
	// of 150,000 paragraphs is ~115 MB).
	cacheBytes = 1 << 30
)

// Service is the semantic index and the search over it.
type Service struct {
	lib Library
	emb Embedder
	idx *index

	mu     sync.Mutex
	cache  map[string]*bookVectors
	recent []string // cached book ids, least recently used first

	// embedded is EmbeddedBooks' last answer, good until embeddedAt +
	// embeddedTTL or the index's next write.
	embedded   map[string]bool
	embeddedAt time.Time
}

// embeddedTTL is how long EmbeddedBooks trusts its last look at the
// library: the index's own writes drop the answer at once, a reworded or
// added chapter is noticed this much later.
const embeddedTTL = 5 * time.Second

// Open opens the index in dataDir (embeddings.sqlite). modelPath is the
// embedding model in use: an index built with another is started over.
func Open(dataDir, modelPath string, lib Library, emb Embedder) (*Service, error) {
	idx, err := openIndex(filepath.Join(dataDir, "embeddings.sqlite"), filepath.Base(modelPath))
	if err != nil {
		return nil, err
	}
	return &Service{lib: lib, emb: emb, idx: idx, cache: map[string]*bookVectors{}}, nil
}

// Close closes the index.
func (s *Service) Close() error { return s.idx.close() }

// embeddable reports whether p gets a vector.
func embeddable(p store.Paragraph) bool {
	return !p.Unvoiced() && utf8.RuneCountInString(strings.TrimSpace(p.Text)) >= minChars
}

// documentText is what is embedded for a paragraph.
func documentText(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > maxChars {
		text = text[:maxChars]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return documentPrefix + text
}

// StaleChapters returns the ids of bookID's chapters whose paragraphs
// changed (or were never read) since the index last went over them, and
// forgets chapters the book no longer has.
func (s *Service) StaleChapters(bookID string) (map[string]bool, error) {
	revs, err := s.lib.ChapterTextRevs(bookID)
	if err != nil {
		return nil, err
	}
	indexed, err := s.idx.chapterRevs(bookID)
	if err != nil {
		return nil, err
	}
	var gone []string
	for id := range indexed {
		if _, ok := revs[id]; !ok {
			gone = append(gone, id)
		}
	}
	if len(gone) > 0 {
		if err := s.idx.dropChapters(gone); err != nil {
			return nil, err
		}
		s.forget(bookID)
	}
	stale := map[string]bool{}
	for id, rev := range revs {
		if had, ok := indexed[id]; !ok || had != rev {
			stale[id] = true
		}
	}
	return stale, nil
}

// EmbedChapter brings the index up to date with one chapter: embeds the
// paragraphs that are new or reworded, drops the ones that are gone, and
// answers how many it embedded. The work behind a jobs.KindEmbedding task
// - it embeds on the GPU, so it belongs in the job queue's LLM pool.
func (s *Service) EmbedChapter(ctx context.Context, bookID, chapterID string) (int, error) {
	// The rev is read before the paragraphs: a change landing in between
	// leaves the chapter stale, to be read again.
	revs, err := s.lib.ChapterTextRevs(bookID)
	if err != nil {
		return 0, err
	}
	rev, ok := revs[chapterID]
	if !ok {
		return 0, s.idx.dropChapters([]string{chapterID})
	}
	paragraphs, err := s.lib.ListParagraphsRaw(chapterID)
	if err != nil {
		return 0, err
	}
	had, err := s.idx.hashes(chapterID)
	if err != nil {
		return 0, err
	}

	var todo []row
	var texts []string
	keep := map[string]bool{}
	for _, p := range paragraphs {
		if !embeddable(p) {
			continue
		}
		keep[p.ID] = true
		hash := textHash(p.Text)
		if h, ok := had[p.ID]; ok && h == hash {
			continue
		}
		todo = append(todo, row{paragraphID: p.ID, hash: hash})
		texts = append(texts, documentText(p.Text))
	}
	var remove []string
	for id := range had {
		if !keep[id] {
			remove = append(remove, id)
		}
	}
	for start := 0; start < len(texts); start += embedBatch {
		end := min(start+embedBatch, len(texts))
		vecs, err := s.emb.Embed(ctx, texts[start:end], false)
		if err != nil {
			return 0, fmt.Errorf("embed: %w", err)
		}
		for i, v := range vecs {
			todo[start+i].vec = quantize(v)
		}
	}
	if err := s.idx.saveChapter(bookID, chapterID, rev, todo, remove); err != nil {
		return 0, err
	}
	if len(todo) > 0 || len(remove) > 0 {
		s.forget(bookID)
	} else {
		s.changed()
	}
	return len(todo), nil
}

// DropBook forgets a deleted book.
func (s *Service) DropBook(bookID string) error {
	s.forget(bookID)
	return s.idx.dropBook(bookID)
}

// IndexedBooks lists the books the index holds anything for.
func (s *Service) IndexedBooks() ([]string, error) { return s.idx.bookIDs() }

// EmbeddedBooks reports, for every book with chapters, whether the index
// has read all of them as they are now - what a book list asks to say a
// book is fully preprocessed. Two queries for the whole library, and the
// answer is kept for embeddedTTL.
func (s *Service) EmbeddedBooks() (map[string]bool, error) {
	s.mu.Lock()
	if s.embedded != nil && time.Since(s.embeddedAt) < embeddedTTL {
		defer s.mu.Unlock()
		return s.embedded, nil
	}
	s.mu.Unlock()

	books, err := s.lib.AllChapterTextRevs()
	if err != nil {
		return nil, err
	}
	indexed, err := s.idx.allChapterRevs()
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(books))
	for bookID, revs := range books {
		done := true
		for id, rev := range revs {
			if had, ok := indexed[id]; !ok || had != rev {
				done = false
				break
			}
		}
		out[bookID] = done
	}
	s.mu.Lock()
	s.embedded, s.embeddedAt = out, time.Now()
	s.mu.Unlock()
	return out, nil
}

// forget drops bookID's vectors from memory; the next search reads them
// again. Every write to the index comes through here or changed, so it
// also drops EmbeddedBooks' answer.
func (s *Service) forget(bookID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embedded = nil
	delete(s.cache, bookID)
	s.recent = remove(s.recent, bookID)
}

func remove(list []string, item string) []string {
	out := list[:0]
	for _, v := range list {
		if v != item {
			out = append(out, v)
		}
	}
	return out
}

// changed drops EmbeddedBooks' answer after a write that left the vectors
// themselves alone.
func (s *Service) changed() {
	s.mu.Lock()
	s.embedded = nil
	s.mu.Unlock()
}

// vectors returns bookID's vectors, from memory or the index.
func (s *Service) vectors(bookID string) (*bookVectors, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if bv, ok := s.cache[bookID]; ok {
		s.recent = append(remove(s.recent, bookID), bookID)
		return bv, nil
	}
	bv, err := s.idx.load(bookID)
	if err != nil {
		return nil, err
	}
	s.cache[bookID] = bv
	s.recent = append(s.recent, bookID)
	total := 0
	for _, c := range s.cache {
		total += len(c.data)
	}
	for total > cacheBytes && len(s.recent) > 1 {
		oldest := s.recent[0]
		total -= len(s.cache[oldest].data)
		delete(s.cache, oldest)
		s.recent = s.recent[1:]
	}
	return bv, nil
}

// Hit is one paragraph a search found.
type Hit struct {
	BookID       string
	ChapterID    string
	ChapterIdx   int
	ParagraphID  string
	ParagraphIdx int
	Text         string
	Speaker      string
	IsQuote      bool
	// Score is the cosine similarity of the paragraph and the search
	// phrase (to the precision vectors are stored at): higher is closer.
	// Only its order means much - an unrelated paragraph still scores
	// well above zero.
	Score float64
	// Lift is Score less the paragraph's similarity to the search's
	// baseline phrase (SearchAbove), 0 without one: how much closer it is
	// to the whole phrase than to the baseline alone.
	Lift float64
}

// candidate is a hit before its paragraph is read.
type candidate struct {
	bookID, chapterID, paragraphID string
	score                          float32
	vec                            []int8
}

// candidates is a min-heap on score: the worst kept candidate on top.
type candidates []candidate

func (h candidates) Len() int           { return len(h) }
func (h candidates) Less(i, j int) bool { return h[i].score < h[j].score }
func (h candidates) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *candidates) Push(x any)        { *h = append(*h, x.(candidate)) }
func (h *candidates) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// Search returns up to limit paragraphs of bookIDs closest in meaning to
// query, best first. Books the index hasn't read contribute nothing. The
// phrase is embedded on the CPU, so this is safe to call at any time.
func (s *Service) Search(ctx context.Context, bookIDs []string, query string, limit int) ([]Hit, error) {
	return s.SearchAbove(ctx, bookIDs, query, "", limit)
}

// SearchAbove is Search that also reports each hit's Lift over baseline,
// a phrase that is part of query: searching "Loth raspy" with baseline
// "Loth" tells the paragraphs about Loth's rasp (well above their
// similarity to "Loth" alone) from the ones found only for naming her.
// The hits are still the closest to query, in that order; what Lift is
// enough is the caller's call.
func (s *Service) SearchAbove(ctx context.Context, bookIDs []string, query, baseline string, limit int) ([]Hit, error) {
	query, baseline = strings.TrimSpace(query), strings.TrimSpace(baseline)
	if query == "" || limit <= 0 || len(bookIDs) == 0 {
		return nil, nil
	}
	texts := []string{queryPrefix + query}
	if baseline != "" {
		texts = append(texts, queryPrefix+baseline)
	}
	vecs, err := s.emb.Embed(ctx, texts, true)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}
	q := vecs[0]

	// A few spare, for paragraphs deleted since they were embedded.
	want := limit + limit/4 + 2
	best := &candidates{}
	for _, bookID := range bookIDs {
		bv, err := s.vectors(bookID)
		if err != nil {
			return nil, err
		}
		if bv.dim != len(q) {
			continue // empty, or built at another width
		}
		for i := range bv.ids {
			vec := bv.data[i*bv.dim : (i+1)*bv.dim]
			score := dot(q, vec)
			if best.Len() < want {
				heap.Push(best, candidate{bookID, bv.chapters[i], bv.ids[i], score, vec})
			} else if score > (*best)[0].score {
				(*best)[0] = candidate{bookID, bv.chapters[i], bv.ids[i], score, vec}
				heap.Fix(best, 0)
			}
		}
	}
	found := []candidate(*best)
	sort.Slice(found, func(i, j int) bool { return found[i].score > found[j].score })

	chapterIdx := map[string]map[string]int{}
	var out []Hit
	for _, c := range found {
		if len(out) == limit {
			break
		}
		p, err := s.lib.GetParagraph(c.paragraphID)
		if err != nil {
			return nil, err
		}
		if p == nil {
			continue
		}
		if chapterIdx[c.bookID] == nil {
			if chapterIdx[c.bookID], err = s.lib.ChapterIdxByID(c.bookID); err != nil {
				return nil, err
			}
		}
		var lift float64
		if len(vecs) > 1 {
			lift = float64(c.score-dot(vecs[1], c.vec)) / vectorScale
		}
		out = append(out, Hit{
			Lift:         lift,
			BookID:       c.bookID,
			ChapterID:    c.chapterID,
			ChapterIdx:   chapterIdx[c.bookID][c.chapterID],
			ParagraphID:  c.paragraphID,
			ParagraphIdx: p.Idx,
			Text:         p.Text,
			Speaker:      p.Speaker,
			IsQuote:      p.IsQuote,
			Score:        float64(c.score) / vectorScale,
		})
	}
	return out, nil
}

// dot is the inner product of a query vector and a stored one.
func dot(q []float32, v []int8) float32 {
	v = v[:len(q)]
	var a, b, c, d float32
	i := 0
	for ; i+4 <= len(q); i += 4 {
		a += q[i] * float32(v[i])
		b += q[i+1] * float32(v[i+1])
		c += q[i+2] * float32(v[i+2])
		d += q[i+3] * float32(v[i+3])
	}
	for ; i < len(q); i++ {
		a += q[i] * float32(v[i])
	}
	return a + b + c + d
}
