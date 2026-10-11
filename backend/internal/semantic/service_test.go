package semantic

import (
	"context"
	"hash/fnv"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// wordEmbedder is a stand-in model: a text's vector is the sum of one
// fixed pseudo-random direction per word, so texts sharing words are
// close. It counts what it was asked for.
type wordEmbedder struct {
	bulk, queries int
}

func (e *wordEmbedder) Embed(_ context.Context, texts []string, query bool) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		if query {
			e.queries++
		} else {
			e.bulk++
		}
		text = strings.TrimPrefix(strings.TrimPrefix(text, queryPrefix), documentPrefix)
		v := make([]float32, 64)
		for _, w := range strings.Fields(strings.ToLower(strings.Trim(text, ".?!"))) {
			h := fnv.New64a()
			_, _ = h.Write([]byte(strings.Trim(w, ".,?!")))
			seed := h.Sum64()
			for j := range v {
				seed = seed*6364136223846793005 + 1442695040888963407
				v[j] += float32(int64(seed>>33)%2001-1000) / 1000
			}
		}
		var n float64
		for _, f := range v {
			n += float64(f) * float64(f)
		}
		for j := range v {
			v[j] /= float32(math.Sqrt(n))
		}
		out[i] = v
	}
	return out, nil
}

// newTestService makes a one-chapter book of paragraphs and a Service over
// it, answering them with the stand-in model, the data directory, and the
// book's and chapter's ids.
func newTestService(t *testing.T, paragraphs ...string) (svc *Service, st *store.SQLiteStore, emb *wordEmbedder, dir, bookID, chapterID string) {
	t.Helper()
	dir = t.TempDir()
	st, err := store.Open(filepath.Join(dir, "library.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blocks := make([]store.BlockInput, len(paragraphs))
	for i, p := range paragraphs {
		blocks[i] = store.BlockInput{Kind: store.BlockText, Text: p}
	}
	bookID, _, err = st.CreateBook("Book", "Author", "en", "", "", 0, []store.ChapterInput{{Title: "One", Blocks: blocks}})
	if err != nil {
		t.Fatal(err)
	}
	chapters, err := st.ChapterIdxByID(bookID)
	if err != nil {
		t.Fatal(err)
	}
	for id := range chapters {
		chapterID = id
	}
	emb = &wordEmbedder{}
	svc, err = Open(dir, "test-model.gguf", st, emb)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc, st, emb, dir, bookID, chapterID
}

func TestEmbedAndSearch(t *testing.T) {
	svc, st, emb, _, bookID, chapterID := newTestService(t,
		"Kobolds only live to fifteen years or so.",
		"The harbour city smelled of salt water and rotten fish.",
		"Too short.",
		"Loth spoke in her usual raspy whisper of a voice.",
	)
	ctx := context.Background()

	stale, err := svc.StaleChapters(bookID)
	if err != nil || !stale[chapterID] {
		t.Fatalf("a new chapter should be stale: %v, %v", stale, err)
	}
	if hits, err := svc.Search(ctx, []string{bookID}, "how long do kobolds live", 3); err != nil || len(hits) != 0 {
		t.Fatalf("search before embedding = %v, %v", hits, err)
	}

	n, err := svc.EmbedChapter(ctx, bookID, chapterID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("embedded %d paragraphs, want 3 (the short one is skipped)", n)
	}
	if stale, _ := svc.StaleChapters(bookID); len(stale) != 0 {
		t.Errorf("still stale after embedding: %v", stale)
	}

	hits, err := svc.Search(ctx, []string{bookID}, "how many years kobolds live", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || !strings.HasPrefix(hits[0].Text, "Kobolds only live") {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].ParagraphIdx != 0 || hits[0].ChapterID != chapterID || hits[0].Score <= hits[1].Score {
		t.Errorf("first hit = %+v, second score %.3f", hits[0], hits[1].Score)
	}
	if emb.queries != 2 {
		t.Errorf("%d texts embedded as queries, want one per search", emb.queries)
	}

	// Nothing changed: a second pass embeds nothing.
	if n, err := svc.EmbedChapter(ctx, bookID, chapterID); err != nil || n != 0 {
		t.Errorf("second pass embedded %d, %v", n, err)
	}

	if done, err := svc.EmbeddedBooks(); err != nil || !done[bookID] {
		t.Errorf("EmbeddedBooks = %v, %v, want the book done", done, err)
	}

	// Another speaker is not a change to the text: nothing to read again.
	p, err := st.GetParagraph(hits[0].ParagraphID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetParagraphSpeaker(p.ID, "Will"); err != nil {
		t.Fatal(err)
	}
	if stale, _ := svc.StaleChapters(bookID); len(stale) != 0 {
		t.Errorf("a speaker change left the chapter stale: %v", stale)
	}

	// New text is: only the paragraphs that changed are embedded again.
	if _, _, err := st.ReplaceChapterContent(chapterID, "One", []store.BlockInput{
		{Kind: store.BlockText, Text: "Kobolds only live to fifteen years or so."},
		{Kind: store.BlockText, Text: "The dwarves of the deep are long-lived and slow to breed."},
	}); err != nil {
		t.Fatal(err)
	}
	if stale, _ := svc.StaleChapters(bookID); !stale[chapterID] {
		t.Error("a reworded chapter should be stale")
	}
	svc.embeddedAt = time.Time{} // past the TTL
	if done, _ := svc.EmbeddedBooks(); done[bookID] {
		t.Error("EmbeddedBooks still calls a reworded book done")
	}
	if _, err := svc.EmbedChapter(ctx, bookID, chapterID); err != nil {
		t.Fatal(err)
	}
	hits, err = svc.Search(ctx, []string{bookID}, "long-lived dwarves", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || !strings.HasPrefix(hits[0].Text, "The dwarves") {
		t.Errorf("after rewording, hits = %+v", hits)
	}
	if done, _ := svc.EmbeddedBooks(); !done[bookID] {
		t.Error("EmbeddedBooks should call the re-embedded book done")
	}
}

func TestModelChangeEmptiesIndex(t *testing.T) {
	svc, st, emb, dir, bookID, chapterID := newTestService(t, "Kobolds only live to fifteen years or so.")
	if _, err := svc.EmbedChapter(context.Background(), bookID, chapterID); err != nil {
		t.Fatal(err)
	}
	_ = svc.Close()

	other, err := Open(dir, "another-model.gguf", st, emb)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if stale, _ := other.StaleChapters(bookID); !stale[chapterID] {
		t.Error("an index built with another model should start over")
	}
}

func TestQuantize(t *testing.T) {
	q := quantize([]float32{0, 0.1, -0.1, 0.9, -0.9})
	want := []int8{0, 40, -40, 127, -127}
	for i, w := range want {
		if int8(q[i]) != w {
			t.Errorf("quantize[%d] = %d, want %d", i, int8(q[i]), w)
		}
	}
}
