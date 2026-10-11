package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// matterLLM answers the matter pass by calling everything that isn't
// titled "Chapter ..." matter - the first chapter included, which the
// pass's guard must not let through.
type matterLLM struct {
	fakeLLMBackend
	calls *int
}

func (l matterLLM) LLMGenerate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error) {
	if !strings.HasPrefix(systemPrompt, "You sort the sections of an ebook") {
		return l.fakeLLMBackend.LLMGenerate(ctx, systemPrompt, userPrompt, temp, maxTokens)
	}
	*l.calls++
	return `{"skip": [{"n": 0, "title": "Title Page"}, {"n": 1, "title": "Contents [20 chars] - Chapter One"},
		{"n": 2, "title": "Chapter One"}, {"n": 4, "title": "About the Author"}]}`, nil
}

// TestBookMatter runs the matter pass over a book with a stand-in model:
// the chapters it marks are left out of the book's progress and its
// start, the reader's own mark outlives a second run, and a book that has
// had its pass isn't asked about again.
func TestBookMatter(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	calls := 0
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, matterLLM{calls: &calls})
	text := func(lines ...string) []store.BlockInput {
		var out []store.BlockInput
		for _, l := range lines {
			out = append(out, store.BlockInput{Kind: store.BlockText, Text: l})
		}
		return out
	}
	bookID, _, err := s.CreateBook("Book", "Author", "en", "", "", 0, []store.ChapterInput{
		{Title: "Title Page", Blocks: text("Book")},
		{Title: "Contents", Blocks: text("Chapter One", "Chapter Two")},
		{Title: "Chapter One", Blocks: text("It began.", "It went on.")},
		{Title: "Chapter Two", Blocks: text("It ended.", "Really.")},
		{Title: "About the Author", Blocks: text("She writes.")},
	})
	if err != nil {
		t.Fatal(err)
	}
	book, _ := s.GetBook(bookID)
	if err := srv.findBookMatter(t.Context(), book, false); err != nil {
		t.Fatal(err)
	}
	get := func() bookDetailDTO {
		t.Helper()
		var dto bookDetailDTO
		if resp := doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID, nil, &dto); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET book: %d", resp.StatusCode)
		}
		return dto
	}
	marks := func(dto bookDetailDTO) string {
		var sb strings.Builder
		for _, c := range dto.Chapters {
			if c.Matter {
				sb.WriteByte('m')
			} else {
				sb.WriteByte('s')
			}
		}
		return sb.String()
	}
	dto := get()
	if got := marks(dto); got != "mmssm" {
		t.Fatalf("marks = %s", got)
	}
	// Never opened: it starts at its first story chapter.
	if dto.PosChapterIdx != 2 || dto.ProgressPercent != 0 || dto.Finished {
		t.Fatalf("a new book: pos %d, %.0f%%, finished %v", dto.PosChapterIdx, dto.ProgressPercent, dto.Finished)
	}
	var ch chapterDetailDTO
	doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/chapters/0", nil, &ch)
	if !ch.Matter {
		t.Fatal("the title page's own detail isn't marked")
	}

	// Halfway through the story is half the book, whatever came before.
	if err := s.UpdatePosition(bookID, 3, 0, 0); err != nil {
		t.Fatal(err)
	}
	if dto = get(); dto.ProgressPercent != 50 || dto.Finished {
		t.Fatalf("at chapter two: %.0f%%, finished %v", dto.ProgressPercent, dto.Finished)
	}
	if err := s.UpdatePosition(bookID, 3, 1, 0); err != nil {
		t.Fatal(err)
	}
	if dto = get(); !dto.Finished {
		t.Fatal("the end of the last story chapter is the end of the book")
	}

	// A judged book isn't asked about again; "check again" asks, and
	// leaves the reader's own mark alone.
	if err := srv.findBookMatter(t.Context(), book, false); err != nil || calls != 1 {
		t.Fatalf("second run: %v, %d calls", err, calls)
	}
	if resp := doJSON(t, http.MethodPut, ts.URL+"/api/books/"+bookID+"/chapters/4/matter", setChapterMatterRequest{Matter: false}, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT matter: %d", resp.StatusCode)
	}
	if err := srv.findBookMatter(t.Context(), book, true); err != nil || calls != 2 {
		t.Fatalf("forced run: %v, %d calls", err, calls)
	}
	if dto = get(); marks(dto) != "mmsss" || dto.Finished {
		t.Fatalf("after the reader's mark: %s, finished %v", marks(dto), dto.Finished)
	}

	// The recap counts none of the front matter as read.
	if err := s.UpdatePosition(bookID, 2, 0, 0); err != nil {
		t.Fatal(err)
	}
	book, _ = s.GetBook(bookID)
	if plan, err := srv.planRecap(book); err != nil || plan != nil {
		t.Fatalf("a recap at the story's first line: %+v, %v", plan, err)
	}
}
