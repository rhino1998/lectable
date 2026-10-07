package httpapi

import (
	"archive/zip"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/royalroad/royalroadtest"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/wanderinginn/wanderinginntest"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// fictionLink is what a user pastes: the test site's one fiction as a
// royalroad.com link (the server's client is pointed at the test site).
const fictionLink = "https://www.royalroad.com/fiction/" + royalroadtest.FictionID + "/the-long-road"

type sourceTestBook struct {
	ID           string
	Title        string
	Author       string
	CoverURL     string
	ChapterCount int
	Source       *bookSourceDTO
}

// waitSourceSynced polls the book until its source check is over.
func waitSourceSynced(t *testing.T, base, bookID string) sourceTestBook {
	t.Helper()
	var b sourceTestBook
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		b = sourceTestBook{}
		getJSONInto(t, base+"/api/books/"+bookID, &b)
		if b.Source != nil && !b.Source.Syncing {
			return b
		}
	}
	t.Fatalf("source check of %s never finished: %+v", bookID, b.Source)
	return b
}

func chapterTexts(t *testing.T, s *store.SQLiteStore, bookID string, idx int) (title string, texts []string) {
	t.Helper()
	ch, err := s.GetChapterByIdx(bookID, idx)
	if err != nil || ch == nil {
		t.Fatalf("chapter %d: %+v, %v", idx, ch, err)
	}
	ps, err := s.ListParagraphsRaw(ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		texts = append(texts, p.Text)
	}
	return ch.Title, texts
}

func TestImportBookURL(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	site := royalroadtest.New(t)
	srv.Sources = []websource.Source{site.Source()}
	site.AddChapter("1. Setting Out", `<p>The road was long.</p><p>“Are we there yet?” Sam asked.</p><p><img src="/img/pixel.png"></p>`)
	site.AddChapter("2. The Inn", `<p>The inn was warm.</p>`)
	site.AddChapter("3. Onward", `<p>They left at dawn.</p>`)
	site.AddLockedChapter("4. Early Access")

	var created sourceTestBook
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: fictionLink + "/chapter/9001/c"}, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	if created.Title != "The Long Road" || created.Author != "A. Writer" || created.CoverURL == "" {
		t.Errorf("created = %+v", created)
	}
	if created.Source == nil || created.Source.Kind != sourceRoyalRoad || created.Source.Name != "Royal Road" || created.Source.URL != site.URL() || created.Source.RemoteChapters != 4 {
		t.Fatalf("created source = %+v", created.Source)
	}

	book := waitSourceSynced(t, ts.URL, created.ID)
	// The check stops at the locked chapter and says which.
	if book.ChapterCount != 3 || book.Source.RemoteChapters != 3 || book.Source.Error != "" || book.Source.CheckedAt == 0 || book.Source.Blocked != "4. Early Access" {
		t.Fatalf("after import: %d chapters, source %+v", book.ChapterCount, book.Source)
	}
	title, texts := chapterTexts(t, s, book.ID, 0)
	if title != "1. Setting Out" || strings.Join(texts, "|") != "1. Setting Out|The road was long.|“Are we there yet?”|Sam asked." {
		t.Errorf("chapter 0 = %q %q", title, texts)
	}
	if title, texts := chapterTexts(t, s, book.ID, 2); title != "3. Onward" || strings.Join(texts, "|") != "3. Onward|They left at dawn." {
		t.Errorf("chapter 2 = %q %q", title, texts)
	}
	ch0, _ := s.GetChapterByIdx(book.ID, 0)
	if imgs, err := s.ListImages(ch0.ID); err != nil || len(imgs) != 1 {
		t.Errorf("chapter 0 images = %+v, %v", imgs, err)
	}

	// The same fiction again, by another of its links, is refused.
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: fictionLink}, nil)
	if code := respErrorCode(t, resp); resp.StatusCode != http.StatusConflict || code != string(errBookExists) {
		t.Errorf("second import: %d %q", resp.StatusCode, code)
	}
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: "https://example.com/book"}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("foreign link: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// New chapters: the locked one opens and another is published.
	site.Unlock(`<p>Now public.</p>`)
	site.AddChapter("5. Later", `<p>Much later.</p>`)
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/"+book.ID+"/source/sync", nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("sync: %d", resp.StatusCode)
	}
	resp.Body.Close()
	book = waitSourceSynced(t, ts.URL, book.ID)
	if book.ChapterCount != 5 || book.Source.RemoteChapters != 5 || book.Source.Error != "" || book.Source.Blocked != "" {
		t.Fatalf("after sync: %d chapters, source %+v", book.ChapterCount, book.Source)
	}
	if again, _ := s.GetChapterByIdx(book.ID, 0); again.ID != ch0.ID {
		t.Errorf("chapter 0 was replaced by the sync")
	}
	if title, texts := chapterTexts(t, s, book.ID, 4); title != "5. Later" || texts[len(texts)-1] != "Much later." {
		t.Errorf("chapter 4 = %q %q", title, texts)
	}

	// The kept epub has every chapter, so one can be re-imported from it.
	zr, err := zip.OpenReader(audiopath.SourceEpubFile(srv.DataDir, book.ID))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := websource.ChapterIDs(&zr.Reader)
	zr.Close()
	if err != nil || len(ids) != 5 {
		t.Fatalf("kept epub chapters = %v, %v", ids, err)
	}
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/"+book.ID+"/chapters/3/reimport", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("reimport from the kept epub: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if title, texts := chapterTexts(t, s, book.ID, 3); title != "4. Early Access" || texts[len(texts)-1] != "Now public." {
		t.Errorf("chapter 3 after reimport = %q %q", title, texts)
	}
}

// TestSyncBookSourceFailure: a check that can't fetch a chapter keeps what
// it has, says why, and the next one carries on; a chapter the site lists
// but no longer serves is passed over.
func TestSyncBookSourceFailure(t *testing.T) {
	srv, _, _, ts := newTestServer(t)
	site := royalroadtest.New(t)
	srv.Sources = []websource.Source{site.Source()}
	site.AddChapter("1. One", `<p>One.</p>`)

	var created sourceTestBook
	if resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: fictionLink}, &created); resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	if created.Source.Syncing {
		t.Errorf("a one-chapter fiction started a background fetch")
	}

	gone := site.AddChapter("2. Withdrawn", `<p>Gone.</p>`)
	site.Remove(gone)
	site.AddChapter("3. Three", `<p>Three.</p>`)
	site.SetFailing(true)
	doJSON(t, http.MethodPost, ts.URL+"/api/books/"+created.ID+"/source/sync", nil, nil).Body.Close()
	book := waitSourceSynced(t, ts.URL, created.ID)
	if book.ChapterCount != 1 || book.Source.Error == "" || book.Source.RemoteChapters != 3 {
		t.Fatalf("after failed sync: %d chapters, source %+v", book.ChapterCount, book.Source)
	}

	site.SetFailing(false)
	doJSON(t, http.MethodPost, ts.URL+"/api/books/"+created.ID+"/source/sync", nil, nil).Body.Close()
	book = waitSourceSynced(t, ts.URL, created.ID)
	if book.ChapterCount != 2 || book.Source.Error != "" || book.Source.RemoteChapters != 2 {
		t.Fatalf("after retry: %d chapters, source %+v", book.ChapterCount, book.Source)
	}
}

func TestSyncBookSourceNeedsSource(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "Hello.")
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/source/sync", nil, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("sync of an epub book: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/nope/source/sync", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("sync of a missing book: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestImportWanderingInnVolume: the link picks the site; a volume comes in
// as a book of the series, stops at a chapter that is for patrons only,
// and carries on from it once it opens. The site's own address imports the
// next volume the library doesn't have.
func TestImportWanderingInnVolume(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	inn := wanderinginntest.New(t)
	srv.Sources = []websource.Source{royalroadtest.New(t).Source(), inn.Source()}
	inn.AddChapter(1, "rw1-00", "1.00", `<p>The inn was dark.</p>`)
	locked := inn.AddChapter(1, "rw1-01", "1.01", `<p>Now open.</p>`)
	inn.AddChapter(1, "rw1-02", "1.02", `<p>After the wall.</p>`)
	inn.SetLocked(locked, true)
	inn.AddChapter(2, "2-00", "2.00", `<p>Volume two.</p>`)

	var created sourceTestBook
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: "wanderinginn.com"}, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	if created.Title != "The Wandering Inn: Volume 1" || created.Author != "pirateaba" || created.Source.Kind != sourceWanderingInn || created.Source.Name != "The Wandering Inn" {
		t.Errorf("created = %+v, source %+v", created, created.Source)
	}
	if b, _ := s.GetBook(created.ID); b == nil || b.SeriesName != "The Wandering Inn" || b.SeriesIndex != 1 {
		t.Errorf("series = %+v", b)
	}
	book := waitSourceSynced(t, ts.URL, created.ID)
	if book.ChapterCount != 1 || book.Source.Blocked != "1.01" || book.Source.RemoteChapters != 1 || book.Source.Error != "" {
		t.Fatalf("behind the wall: %d chapters, source %+v", book.ChapterCount, book.Source)
	}

	inn.SetLocked(locked, false)
	doJSON(t, http.MethodPost, ts.URL+"/api/books/"+book.ID+"/source/sync", nil, nil).Body.Close()
	book = waitSourceSynced(t, ts.URL, book.ID)
	if book.ChapterCount != 3 || book.Source.Blocked != "" {
		t.Fatalf("after it opened: %d chapters, source %+v", book.ChapterCount, book.Source)
	}
	if title, texts := chapterTexts(t, s, book.ID, 1); title != "1.01" || texts[len(texts)-1] != "Now open." {
		t.Errorf("chapter 1 = %q %q", title, texts)
	}

	// The site's address again: volume 1 is here, so volume 2.
	var second sourceTestBook
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: "https://wanderinginn.com/table-of-contents/"}, &second)
	if resp.StatusCode != http.StatusCreated || second.Title != "The Wandering Inn: Volume 2" {
		t.Fatalf("second import: %d %+v", resp.StatusCode, second)
	}
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: "https://wanderinginn.com/"}, nil)
	if code := respErrorCode(t, resp); resp.StatusCode != http.StatusConflict || code != string(errBookExists) {
		t.Errorf("third import: %d %q", resp.StatusCode, code)
	}
}
