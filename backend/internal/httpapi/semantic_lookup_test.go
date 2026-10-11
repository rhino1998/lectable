package httpapi

import (
	"context"
	"hash/fnv"
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/semantic"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// wordVectors stands in for the embedding model: texts sharing words get
// vectors that are close.
type wordVectors struct{}

func (wordVectors) Embed(_ context.Context, texts []string, _ bool) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, 64)
		for _, w := range strings.Fields(strings.ToLower(text)) {
			h := fnv.New64a()
			_, _ = h.Write([]byte(strings.Trim(w, ".,?!:’'s")))
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

// TestSemanticExcerpts: a lookup's search by meaning keeps only paragraphs
// that name the character, and leaves out what the exact search showed.
func TestSemanticExcerpts(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "library.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	paragraphs := []string{
		"Loth spoke in a raspy whisper of a voice.",
		"The merchant had a raspy voice and a raspy whisper too.",
		"Loth walked down to the harbour before dawn.",
		"The raspy voice of Loth carried across the hall.",
	}
	blocks := make([]store.BlockInput, len(paragraphs))
	for i, p := range paragraphs {
		blocks[i] = store.BlockInput{Kind: store.BlockText, Text: p}
	}
	bookID, _, err := st.CreateBook("Book", "Author", "en", "", "", 0, []store.ChapterInput{{Title: "One", Blocks: blocks}})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := semantic.Open(dir, "model.gguf", st, wordVectors{})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	chapters, err := st.ChapterIdxByID(bookID)
	if err != nil {
		t.Fatal(err)
	}
	for chapterID := range chapters {
		if _, err := svc.EmbedChapter(context.Background(), bookID, chapterID); err != nil {
			t.Fatal(err)
		}
	}

	s := &Server{Store: st, Semantic: svc}
	have := []string{paragraphs[3]}
	got := s.semanticExcerpts(context.Background(), []string{bookID}, []string{"Loth"}, []string{"Loth", "raspy"}, have)
	if len(got) == 0 || got[0] != paragraphs[0] {
		t.Fatalf("excerpts = %q, want the raspy whisper line first", got)
	}
	for _, e := range got {
		if strings.Contains(e, "merchant") {
			t.Errorf("an excerpt that doesn't name the character: %q", e)
		}
		if e == paragraphs[3] {
			t.Errorf("an excerpt the exact search already showed: %q", e)
		}
	}

	if got := (&Server{Store: st}).semanticExcerpts(context.Background(), []string{bookID}, []string{"Loth"}, []string{"Loth", "raspy"}, nil); got != nil {
		t.Errorf("without an index: %q", got)
	}
}
