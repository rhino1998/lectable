package httpapi

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
)

// Paragraphs on a shared clip play from the clip's own URL - but two in a
// row never get the same one, since clients read that as a merge group.
func TestSharedClipURLs(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "…", "…", "Between.", "…")
	book, err := s.GetBook(bookID)
	if err != nil || book == nil {
		t.Fatalf("GetBook: %v", err)
	}
	voice, err := srv.Narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	chapters, err := s.ListChapterSummaries(bookID, "", nil)
	if err != nil || len(chapters) == 0 {
		t.Fatalf("ListChapterSummaries: %v", err)
	}
	paragraphs, err := s.ListParagraphsRaw(chapters[0].ID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	const key = "0123456789abcdef0123456789abcdef"
	shared := audiopath.SharedClipFile(srv.DataDir, key)
	if err := audiopath.EnsureDir(shared); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shared, []byte("clip"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 1, 3} {
		if err := s.SetParagraphReadyClip(paragraphs[i].ID, voice.VoiceID(), key, 1, ""); err != nil {
			t.Fatal(err)
		}
	}

	var chapter chapterDetailDTO
	resp := doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/chapters/0", nil, &chapter)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get chapter: %d", resp.StatusCode)
	}
	urls := make([]string, len(chapter.Paragraphs))
	for i, p := range chapter.Paragraphs {
		urls[i] = p.AudioURL
	}
	for _, i := range []int{0, 1, 3} {
		if !strings.HasPrefix(urls[i], "/api/clips/"+key+"?v=") {
			t.Errorf("paragraph %d plays from %q, want the shared clip's URL", i, urls[i])
		}
	}
	if urls[0] == urls[1] {
		t.Errorf("paragraphs 0 and 1, in a row, share the URL %q", urls[0])
	}
	if urls[0] != urls[3] {
		t.Errorf("paragraphs 0 and 3 are on one clip but play from %q and %q", urls[0], urls[3])
	}

	for _, u := range []string{urls[0], urls[1]} {
		got, err := http.Get(ts.URL + u)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(got.Body)
		got.Body.Close()
		if got.StatusCode != http.StatusOK || string(body) != "clip" {
			t.Errorf("GET %s: %d %q, want the clip", u, got.StatusCode, body)
		}
		if cc := got.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("GET %s: Cache-Control %q, want a versioned clip cached for good", u, cc)
		}
	}
	if got, err := http.Get(ts.URL + "/api/clips/..%2f..%2flibrary.sqlite"); err != nil || got.StatusCode != http.StatusNotFound {
		t.Errorf("a key that isn't one: %v %v, want 404", got, err)
	}
}
