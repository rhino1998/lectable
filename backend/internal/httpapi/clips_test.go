package httpapi

import (
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/store"
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
		if err := s.SetParagraphReadyClip(paragraphs[i].ID, voice.VoiceID(), key, 1, "", nil); err != nil {
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

// A ready music region's audioUrl names its render, so it can be cached for
// good and a regenerated region is fetched again.
func TestMusicRegionAudioURLIsVersioned(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "One.", "Two.")
	ch, err := s.GetChapterByIdx(bookID, 0)
	if err != nil || ch == nil {
		t.Fatalf("GetChapterByIdx: %v, %v", ch, err)
	}
	regions, err := s.AppendMusicRegions(ch.ID, []store.MusicRegionInput{{StartIdx: 0, Mood: "calm", Prompt: "p", Transition: store.MusicTransitionCut}}, 1)
	if err != nil || len(regions) != 1 {
		t.Fatalf("AppendMusicRegions: %v, %v", regions, err)
	}
	clip := audiopath.MusicRegionFile(srv.DataDir, bookID, ch.ID, regions[0].ID)
	if err := audiopath.EnsureDir(clip); err != nil {
		t.Fatal(err)
	}
	render := func(body string, mtime time.Time) string {
		t.Helper()
		if err := os.WriteFile(clip, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(clip, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		if err := s.SetMusicRegionReady(regions[0].ID, 1); err != nil {
			t.Fatal(err)
		}
		var music chapterMusicDTO
		if resp := doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/chapters/0/music", nil, &music); resp.StatusCode != http.StatusOK {
			t.Fatalf("get music: %d", resp.StatusCode)
		}
		return music.Regions[0].AudioURL
	}

	base := "/api/music-regions/" + regions[0].ID + "/audio"
	first := render("one", time.Unix(1000, 0))
	if !strings.HasPrefix(first, base+"?v=") {
		t.Fatalf("audioUrl %q, want %s versioned", first, base)
	}
	if second := render("two", time.Unix(2000, 0)); second == first {
		t.Errorf("a regenerated region kept the URL %q", first)
	}
	for u, want := range map[string]string{first: "immutable", base: "no-cache"} {
		got, err := http.Get(ts.URL + u)
		if err != nil {
			t.Fatal(err)
		}
		got.Body.Close()
		if cc := got.Header.Get("Cache-Control"); got.StatusCode != http.StatusOK || !strings.Contains(cc, want) {
			t.Errorf("GET %s: %d, Cache-Control %q, want %s", u, got.StatusCode, cc, want)
		}
	}
}
