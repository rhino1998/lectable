package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/epub"
	"github.com/rhino1998/lectable/backend/internal/royalroad/royalroadtest"
	"github.com/rhino1998/lectable/backend/internal/wanderinginn/wanderinginntest"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// fakeWayback stands in for web.archive.org: it lists and serves the
// copies it was given, and for a page it has no copy list for, passes the
// request through to the page's own (test) site - an archive that has
// everything as it is now.
type fakeWayback struct {
	server *httptest.Server
	mu     sync.Mutex
	// copies maps a page address to its saved copies by timestamp.
	copies map[string]map[string]string
}

func newFakeWayback(t *testing.T) (*fakeWayback, *websource.Wayback) {
	t.Helper()
	f := &fakeWayback{copies: map[string]map[string]string{}}
	// Not a ServeMux: it would "clean" the address inside the path.
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f, &websource.Wayback{Client: websource.Client{HTTP: f.server.Client(), MinInterval: -1}, BaseURL: f.server.URL}
}

func (f *fakeWayback) save(pageURL, timestamp, page string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.copies[pageURL] == nil {
		f.copies[pageURL] = map[string]string{}
	}
	f.copies[pageURL][timestamp] = page
}

func (f *fakeWayback) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/cdx/search/cdx" && r.URL.Query().Get("matchType") == "prefix" {
		// Everything saved under an address: "royalroad.com/fiction/4242/".
		_, path, _ := strings.Cut(r.URL.Query().Get("url"), "/")
		rows := [][]string{{"original", "timestamp"}}
		for pageURL, copies := range f.copies {
			if !strings.Contains(pageURL, "/"+path) {
				continue
			}
			for ts := range copies {
				rows = append(rows, []string{pageURL, ts})
				break
			}
		}
		_ = json.NewEncoder(w).Encode(rows)
		return
	}
	if r.URL.Path == "/cdx/search/cdx" {
		rows := [][]string{{"timestamp"}}
		var times []string
		for ts := range f.copies[r.URL.Query().Get("url")] {
			times = append(times, ts)
		}
		// Oldest first, as the real index answers.
		for i := range times {
			for j := i + 1; j < len(times); j++ {
				if times[j] < times[i] {
					times[i], times[j] = times[j], times[i]
				}
			}
		}
		for _, ts := range times {
			rows = append(rows, []string{ts})
		}
		_ = json.NewEncoder(w).Encode(rows)
		return
	}
	rest, ok := strings.CutPrefix(r.RequestURI, "/web/")
	timestamp, pageURL, found := strings.Cut(rest, "id_/")
	if !ok || !found {
		http.NotFound(w, r)
		return
	}
	if copies, listed := f.copies[pageURL]; listed {
		page, have := copies[timestamp]
		if !have {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, page)
		return
	}
	resp, err := http.Get(pageURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// oldLayoutChapter is a chapter page as the site served it before its
// redesign.
func oldLayoutChapter(body string) string {
	return `<!DOCTYPE html><html><head><title>1.01 – The Wandering Inn</title></head><body>
<article><div class="entry-content">` + body + `
<p>&nbsp;</p><hr><p><a href="/x/">Previous Chapter</a> <a href="/y/"><span style="float: right">Next Chapter</span></a></p>
<div id="jp-post-flair" class="sharedaddy"><h3>Share this:</h3></div>
</div></article></body></html>`
}

// TestLockedChapterFromWayback: a chapter the site lists but keeps for
// patrons is read from its last archived copy that was public, so the
// book doesn't stop at it.
func TestLockedChapterFromWayback(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	inn := wanderinginntest.New(t)
	fake, wayback := newFakeWayback(t)
	srv.Sources = []websource.Source{inn.Source()}
	srv.Wayback = wayback
	inn.AddChapter(1, "rw1-00", "1.00", `<p>The inn was dark.</p>`)
	locked := inn.AddChapter(1, "rw1-01", "1.01", `<p>Never served.</p>`)
	inn.AddChapter(1, "rw1-02", "1.02", `<p>After the wall.</p>`)
	inn.SetLocked(locked, true)
	lockedURL := inn.Server.URL + "/" + locked + "/"
	// The newest copy was already behind the wall; the one before wasn't.
	fake.save(lockedURL, "20260901000000", `<html><body><div class="patreon-protected-post">Patreon Exclusive</div></body></html>`)
	fake.save(lockedURL, "20250401000000", oldLayoutChapter(`<p>From the archive.</p>`))
	fake.save(lockedURL, "20240101000000", oldLayoutChapter(`<p>An older draft.</p>`))

	var created sourceTestBook
	if resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: "wanderinginn.com"}, &created); resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	book := waitSourceSynced(t, ts.URL, created.ID)
	if book.ChapterCount != 3 || book.Source.Blocked != "" || book.Source.Error != "" || book.Source.RemoteChapters != 3 {
		t.Fatalf("%d chapters, source %+v", book.ChapterCount, book.Source)
	}
	if title, texts := chapterTexts(t, s, book.ID, 1); title != "1.01" || strings.Join(texts, "|") != "1.01|From the archive." {
		t.Errorf("chapter 1 = %q %q", title, texts)
	}
	if _, texts := chapterTexts(t, s, book.ID, 2); texts[len(texts)-1] != "After the wall." {
		t.Errorf("chapter 2 = %q", texts)
	}
}

// TestImportWaybackLink: a Wayback Machine link imports the work as
// archived, as its own book beside the live one.
func TestImportWaybackLink(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	inn := wanderinginntest.New(t)
	_, wayback := newFakeWayback(t)
	srv.Sources = []websource.Source{inn.Source()}
	srv.Wayback = wayback
	inn.AddChapter(1, "rw1-00", "1.00", `<p>The inn was dark.</p>`)
	inn.AddChapter(1, "rw1-01", "1.01", `<p>Second.</p>`)

	link := "https://web.archive.org/web/20240516122358/https://wanderinginn.com/table-of-contents/#vol-1"
	var created sourceTestBook
	if resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: link}, &created); resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	if created.Title != "The Wandering Inn: Volume 1 (archived 2024-05-16)" || created.Source.Kind != sourceWayback ||
		created.Source.Name != "The Wandering Inn (Wayback Machine)" || !strings.Contains(created.Source.URL, "/web/20240516122358/") {
		t.Errorf("created = %+v, source %+v", created, created.Source)
	}
	book := waitSourceSynced(t, ts.URL, created.ID)
	if book.ChapterCount != 2 || book.Source.Error != "" {
		t.Fatalf("%d chapters, source %+v", book.ChapterCount, book.Source)
	}
	if _, texts := chapterTexts(t, s, book.ID, 1); texts[len(texts)-1] != "Second." {
		t.Errorf("chapter 1 = %q", texts)
	}
	// The same copy again is refused; the live volume is a different book.
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: link}, nil)
	if code := respErrorCode(t, resp); resp.StatusCode != http.StatusConflict || code != string(errBookExists) {
		t.Errorf("second import: %d %q", resp.StatusCode, code)
	}
	var live sourceTestBook
	if resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: "wanderinginn.com"}, &live); resp.StatusCode != http.StatusCreated || live.Source.Kind != sourceWanderingInn {
		t.Errorf("live import: %d %+v", resp.StatusCode, live.Source)
	}
	// Checking the archived book again reads the same copy.
	doJSON(t, http.MethodPost, ts.URL+"/api/books/"+book.ID+"/source/sync", nil, nil).Body.Close()
	if again := waitSourceSynced(t, ts.URL, book.ID); again.ChapterCount != 2 || again.Source.Error != "" {
		t.Errorf("after a check: %d chapters, source %+v", again.ChapterCount, again.Source)
	}
}

func postFile(t *testing.T, url string, data []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "book.epub")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write(data)
	_ = mw.Close()
	resp, err := http.Post(url, mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestBackfillFromEpub: chapters neither the site nor the archive will
// give are taken from an uploaded epub, in place, and the site's chapters
// carry on after them.
func TestBackfillFromEpub(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	inn := wanderinginntest.New(t)
	_, wayback := newFakeWayback(t)
	srv.Sources = []websource.Source{inn.Source()}
	srv.Wayback = wayback
	inn.AddChapter(1, "rw1-00", "1.00", `<p>From the site.</p>`)
	for _, slug := range []string{"rw1-01", "rw1-02"} {
		id := inn.AddChapter(1, slug, strings.Replace(slug, "rw1-", "1.", 1), `<p>Never served.</p>`)
		inn.SetLocked(id, true)
	}
	inn.AddChapter(1, "rw1-03", "1.03", `<p>Public again.</p>`)

	var created sourceTestBook
	if resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: "wanderinginn.com"}, &created); resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	book := waitSourceSynced(t, ts.URL, created.ID)
	if book.ChapterCount != 1 || book.Source.Blocked != "1.01" {
		t.Fatalf("before: %d chapters, source %+v", book.ChapterCount, book.Source)
	}

	// An epub of something else fills nothing.
	resp := postFile(t, ts.URL+"/api/books/"+book.ID+"/source/backfill", testEpub(t, `<h1>Prologue</h1><p>Other book.</p>`))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("unrelated epub: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = postFile(t, ts.URL+"/api/books/"+book.ID+"/source/backfill", testEpub(t,
		`<h1>1.00</h1><p>The ebook's own first chapter, not needed.</p>`,
		`<h1>Chapter 1.01</h1><p>From the <em>ebook</em>.</p><p>“Hello,” she said.</p>`,
		`<h1>1.02</h1><p>Also from the ebook.</p>`,
	))
	var queued queuedResponse
	if err := json.NewDecoder(resp.Body).Decode(&queued); err != nil || resp.StatusCode != http.StatusAccepted || queued.Queued != 2 {
		t.Fatalf("backfill: %d %+v %v", resp.StatusCode, queued, err)
	}
	resp.Body.Close()
	book = waitSourceSynced(t, ts.URL, book.ID)
	if book.ChapterCount != 4 || book.Source.Blocked != "" || book.Source.Error != "" || book.Source.RemoteChapters != 4 {
		t.Fatalf("after: %d chapters, source %+v", book.ChapterCount, book.Source)
	}
	var got []string
	for idx := range 4 {
		title, texts := chapterTexts(t, s, book.ID, idx)
		got = append(got, title+": "+texts[len(texts)-1])
	}
	want := "1.00: From the site.|Chapter 1.01: she said.|1.02: Also from the ebook.|1.03: Public again."
	if strings.Join(got, "|") != want {
		t.Errorf("chapters = %q\nwant %q", strings.Join(got, "|"), want)
	}
	// A filled chapter re-imports from the kept epub like any other.
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/"+book.ID+"/chapters/1/reimport", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("reimport of a filled chapter: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if _, texts := chapterTexts(t, s, book.ID, 1); strings.Join(texts, "|") != "Chapter 1.01|From the ebook.|“Hello,”|she said." {
		t.Errorf("filled chapter after reimport = %q", texts)
	}
	// Nothing left to fill.
	resp = postFile(t, ts.URL+"/api/books/"+book.ID+"/source/backfill", testEpub(t, `<h1>1.02</h1><p>x</p>`))
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("backfill with nothing missing: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestEpubFillAliases: the site's list names the ebook chapters a web
// chapter corresponds to - several, or one it shares with its neighbour.
func TestEpubFillAliases(t *testing.T) {
	book := &epub.Book{Chapters: []epub.Chapter{
		{Title: "1.00", Blocks: []epub.Block{{Kind: epub.BlockText, Text: "1.00"}, {Kind: epub.BlockText, Text: "a"}}},
		{Title: "1.01", Blocks: []epub.Block{{Kind: epub.BlockText, Text: "b"}}},
		{Title: "1.02", Blocks: []epub.Block{{Kind: epub.BlockText, Text: "c"}}},
	}}
	fill := newEpubFill(book)
	two, ok := fill.take(websource.ChapterRef{ID: "w0", Title: "1.00", Aliases: []string{"1.00", "1.01"}})
	if !ok || len(two) != 2 || two[0].ID != "w0" || two[1].ID == "w0" || two[0].Body != "<p>a</p>\n" {
		t.Fatalf("two ebook chapters for one: %+v %v", two, ok)
	}
	// The web numbering runs one behind the ebook's.
	one, ok := fill.take(websource.ChapterRef{ID: "w1", Title: "1.01", Aliases: []string{"1.02"}})
	if !ok || len(one) != 1 || one[0].Title != "1.02" {
		t.Fatalf("renumbered: %+v %v", one, ok)
	}
	shared, ok := fill.take(websource.ChapterRef{ID: "w2", Title: "1.02", Aliases: []string{"1.02"}})
	if !ok || len(shared) != 0 {
		t.Fatalf("an ebook chapter already taken: %+v %v", shared, ok)
	}
	if _, ok := fill.take(websource.ChapterRef{ID: "w3", Title: "1.03", Aliases: []string{"1.03"}}); ok {
		t.Error("a chapter the epub lacks was filled")
	}
}

// TestImportArchiveOrgItem: an archive.org item link downloads the item's
// epub and imports it as an uploaded book.
func TestImportArchiveOrgItem(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	data := testEpub(t, `<h1>One</h1><p>From the archive.</p>`)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metadata/{item}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("item") {
		case "goodbook":
			fmt.Fprint(w, `{"metadata":{"title":"Good Book"},"files":[
				{"name":"goodbook_lcp.epub","format":"LCP Encrypted EPUB","private":"true"},
				{"name":"goodbook.pdf","format":"Text PDF"},
				{"name":"good book.epub","format":"EPUB"}]}`)
		case "lentonly":
			fmt.Fprint(w, `{"metadata":{"title":"Lent"},"files":[{"name":"lentonly_lcp.epub","format":"LCP Encrypted EPUB","private":"true"}]}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	})
	mux.HandleFunc("GET /download/goodbook/{file}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("file") != "good book.epub" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	})
	fake := httptest.NewServer(mux)
	defer fake.Close()
	oldBase, oldClient := archiveOrgBase, archiveOrgClient
	archiveOrgBase, archiveOrgClient = fake.URL, &websource.Client{HTTP: fake.Client(), MinInterval: -1}
	defer func() { archiveOrgBase, archiveOrgClient = oldBase, oldClient }()

	var created sourceTestBook
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: "https://archive.org/details/goodbook"}, &created)
	if resp.StatusCode != http.StatusCreated || created.ChapterCount != 1 || created.Source != nil {
		t.Fatalf("import: %d %+v", resp.StatusCode, created)
	}
	if _, texts := chapterTexts(t, s, created.ID, 0); texts[len(texts)-1] != "From the archive." {
		t.Errorf("chapter = %q", texts)
	}
	for link, want := range map[string]int{
		"https://archive.org/details/lentonly": http.StatusUnprocessableEntity,
		"https://archive.org/details/nosuch":   http.StatusNotFound,
	} {
		resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: link}, nil)
		if resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", link, resp.StatusCode, want)
		}
		resp.Body.Close()
	}
}

// TestReparseFromStoredPages: a chapter's page is kept as fetched, so it
// can be read again - by different import code - with the site gone, and
// a chapter that now reads differently re-imported from the result.
func TestReparseFromStoredPages(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	inn := wanderinginntest.New(t)
	srv.Sources = []websource.Source{inn.Source()}
	inn.AddChapter(1, "rw1-00", "1.00", `<p>The inn was dark.</p>`)
	second := inn.AddChapter(1, "rw1-01", "1.01", `<p>Second chapter.</p><p><img src="/wp-content/cover.png" alt="Map"></p>`)

	var created sourceTestBook
	if resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: "wanderinginn.com"}, &created); resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	book := waitSourceSynced(t, ts.URL, created.ID)
	if book.ChapterCount != 2 {
		t.Fatalf("%d chapters, source %+v", book.ChapterCount, book.Source)
	}
	rawPath := audiopath.SourceRawFile(srv.DataDir, book.ID, websource.FileKey(second))
	raw, err := websource.ReadRaw(rawPath)
	if err != nil {
		t.Fatalf("the chapter's fetched pages weren't kept: %v", err)
	}
	// The page whole, navigation and all, and the image it embeds.
	if raw.Via != "live" || len(raw.Fetches) != 2 || !strings.Contains(string(raw.Fetches[0].Body), "Next Chapter") || raw.Fetches[1].MediaType != "image/png" {
		t.Fatalf("raw = via %q, %d fetches", raw.Via, len(raw.Fetches))
	}

	// A chapter imported before pages were kept gets its own at the next
	// check, without the chapter changing.
	firstPath := audiopath.SourceRawFile(srv.DataDir, book.ID, "2017_03_01_rw1-00")
	if err := os.Remove(firstPath); err != nil {
		t.Fatal(err)
	}
	ch0, _ := s.GetChapterByIdx(book.ID, 0)
	doJSON(t, http.MethodPost, ts.URL+"/api/books/"+book.ID+"/source/sync", nil, nil).Body.Close()
	waitSourceSynced(t, ts.URL, book.ID)
	if _, err := websource.ReadRaw(firstPath); err != nil {
		t.Errorf("the check didn't fetch the missing pages: %v", err)
	}
	if again, _ := s.GetChapterByIdx(book.ID, 0); again.ID != ch0.ID {
		t.Errorf("fetching its pages replaced the chapter")
	}

	// From here on there is no site to ask.
	inn.Server.Close()

	var first reparseSourceResponse
	if resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+book.ID+"/source/reparse", nil, &first); resp.StatusCode != http.StatusOK {
		t.Fatalf("reparse: %d", resp.StatusCode)
	}
	if first.Reparsed != 2 || first.Missing != 0 || first.Failed != 0 || len(first.Changed) != 0 {
		t.Fatalf("reparse of unchanged pages = %+v", first)
	}

	// Stand in for a change to the import code: the stored page reads
	// differently now.
	raw.Fetches[0].Body = bytes.Replace(raw.Fetches[0].Body, []byte("Second chapter."), []byte("Second chapter, read anew."), 1)
	if err := websource.WriteRaw(rawPath, raw); err != nil {
		t.Fatal(err)
	}
	var again reparseSourceResponse
	doJSON(t, http.MethodPost, ts.URL+"/api/books/"+book.ID+"/source/reparse", nil, &again)
	if again.Reparsed != 2 || len(again.Changed) != 1 || again.Changed[0] != 1 {
		t.Fatalf("reparse after the change = %+v", again)
	}
	// Nothing the reader sees has changed yet.
	if _, texts := chapterTexts(t, s, book.ID, 1); texts[len(texts)-1] != "Second chapter." {
		t.Errorf("reparse alone changed the chapter: %q", texts)
	}
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+book.ID+"/chapters/1/reimport", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reimport: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if _, texts := chapterTexts(t, s, book.ID, 1); texts[len(texts)-1] != "Second chapter, read anew." {
		t.Errorf("chapter after reimport = %q", texts)
	}
	ch1, _ := s.GetChapterByIdx(book.ID, 1)
	if imgs, _ := s.ListImages(ch1.ID); len(imgs) != 1 {
		t.Errorf("the chapter's image didn't survive: %d", len(imgs))
	}
	var settled reparseSourceResponse
	doJSON(t, http.MethodPost, ts.URL+"/api/books/"+book.ID+"/source/reparse", nil, &settled)
	if len(settled.Changed) != 0 {
		t.Errorf("still changed after reimport: %+v", settled)
	}

	// Deleting the book takes its stored pages with it.
	doJSON(t, http.MethodDelete, ts.URL+"/api/books/"+book.ID, nil, nil).Body.Close()
	if _, err := os.Stat(audiopath.SourceRawDir(srv.DataDir, book.ID)); !os.IsNotExist(err) {
		t.Errorf("stored pages left behind: %v", err)
	}
}

func fetchPage(t *testing.T, pageURL string) string {
	t.Helper()
	resp, err := http.Get(pageURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d %v", pageURL, resp.StatusCode, err)
	}
	return string(body)
}

func postPreview(t *testing.T, base, query string, epubData []byte) (importPreviewResponse, int) {
	t.Helper()
	var resp *http.Response
	if epubData != nil {
		resp = postFile(t, base+"/api/books/import-url/preview?"+query, epubData)
	} else {
		var err error
		resp, err = http.Post(base+"/api/books/import-url/preview?"+query, "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer resp.Body.Close()
	var out importPreviewResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	return out, resp.StatusCode
}

func previewLine(p importPreviewResponse) string {
	var parts []string
	for _, c := range p.Chapters {
		parts = append(parts, c.Title+"="+string(c.Source))
	}
	return strings.Join(parts, "|")
}

// TestImportChosenChaptersOfStubbedFiction: a fiction whose early chapters
// were taken off the site is previewed, widened with the archive's copies
// of them, and imported in part; later checks add what is new and leave
// out what was left out.
func TestImportChosenChaptersOfStubbedFiction(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	site := royalroadtest.New(t)
	fake, wayback := newFakeWayback(t)
	srv.Sources = []websource.Source{site.Source()}
	srv.Wayback = wayback
	one := site.AddChapter("1. One", `<p>First.</p>`)
	two := site.AddChapter("2. Two", `<p>Second, since delisted.</p>`)
	three := site.AddChapter("3. Three", `<p>Third, since delisted.</p>`)
	four := site.AddChapter("4. Four", `<p>Fourth.</p>`)

	// The archive saved the fiction's page and two chapters while they
	// were up, under the site's real addresses.
	real := func(id string) string {
		return "https://www.royalroad.com/fiction/" + royalroadtest.FictionID + "/the-long-road/chapter/" + id + "/c"
	}
	fake.save(site.URL(), "20230101000000", fetchPage(t, site.URL()))
	for _, id := range []string{two, three} {
		fake.save(real(id), "20230102000000", fetchPage(t, site.URL()+"/chapter/"+id+"/c"))
	}
	site.Delist(two)
	site.Delist(three)

	first, status := postPreview(t, ts.URL, "url="+url.QueryEscape(fictionLink), nil)
	if status != http.StatusOK || previewLine(first) != "1. One=site|4. Four=site" || !first.CanSearchArchive || first.ArchiveSearched || first.Site != "Royal Road" {
		t.Fatalf("preview: %d %q %+v", status, previewLine(first), first)
	}
	wide, status := postPreview(t, ts.URL, "preview="+first.ID+"&archive=true", nil)
	if status != http.StatusOK || previewLine(wide) != "1. One=site|2. Two=archive|3. Three=archive|4. Four=site" || wide.CanSearchArchive || !wide.ArchiveSearched || wide.ID != first.ID {
		t.Fatalf("with the archive: %d %q %+v", status, previewLine(wide), wide)
	}

	var created sourceTestBook
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: fictionLink, PreviewID: wide.ID, Chapters: []string{two, four}}, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	book := waitSourceSynced(t, ts.URL, created.ID)
	if book.ChapterCount != 2 || book.Source.Error != "" || book.Source.Blocked != "" || book.Source.RemoteChapters != 2 {
		t.Fatalf("after import: %d chapters, source %+v", book.ChapterCount, book.Source)
	}
	if title, texts := chapterTexts(t, s, book.ID, 0); title != "2. Two" || texts[len(texts)-1] != "Second, since delisted." {
		t.Errorf("chapter 0 = %q %q", title, texts)
	}
	if title, _ := chapterTexts(t, s, book.ID, 1); title != "4. Four" {
		t.Errorf("chapter 1 = %q", title)
	}
	if plan, err := s.GetBookSourcePlan(book.ID); err != nil || strings.Join(plan.Skip, ",") != one+","+three || len(plan.Extra) != 1 || plan.Extra[0].ID != two {
		t.Errorf("plan = %+v, %v", plan, err)
	}
	// The preview is spent.
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: fictionLink, PreviewID: wide.ID}, nil)
	if resp.StatusCode != http.StatusGone {
		t.Errorf("import from a used preview: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A new chapter is added; the ones left out stay out.
	site.AddChapter("5. Five", `<p>Fifth.</p>`)
	doJSON(t, http.MethodPost, ts.URL+"/api/books/"+book.ID+"/source/sync", nil, nil).Body.Close()
	book = waitSourceSynced(t, ts.URL, book.ID)
	if book.ChapterCount != 3 || book.Source.Error != "" {
		t.Fatalf("after a check: %d chapters, source %+v", book.ChapterCount, book.Source)
	}
	if title, _ := chapterTexts(t, s, book.ID, 2); title != "5. Five" {
		t.Errorf("chapter 2 = %q", title)
	}
}

// TestPreviewWithEpub: an epub attached to a preview marks the chapters it
// would stand in for, and the import takes them from it.
func TestPreviewWithEpub(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	site := royalroadtest.New(t)
	srv.Sources = []websource.Source{site.Source()}
	site.AddChapter("1. One", `<p>First.</p>`)
	site.AddLockedChapter("2. Two")
	site.AddLockedChapter("3. Three")

	plain, _ := postPreview(t, ts.URL, "url="+url.QueryEscape(fictionLink), nil)
	if previewLine(plain) != "1. One=site|2. Two=locked|3. Three=locked" {
		t.Fatalf("preview = %q", previewLine(plain))
	}
	with, status := postPreview(t, ts.URL, "preview="+plain.ID, testEpub(t, `<h1>1. One</h1><p>x</p>`, `<h1>2. Two</h1><p>From the ebook.</p>`))
	if status != http.StatusOK || previewLine(with) != "1. One=site|2. Two=epub|3. Three=locked" || with.EpubChapters != 2 {
		t.Fatalf("with the epub: %d %q %+v", status, previewLine(with), with)
	}
	var created sourceTestBook
	if resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: fictionLink, PreviewID: with.ID}, &created); resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	book := waitSourceSynced(t, ts.URL, created.ID)
	if book.ChapterCount != 2 || book.Source.Blocked != "3. Three" {
		t.Fatalf("%d chapters, source %+v", book.ChapterCount, book.Source)
	}
	if _, texts := chapterTexts(t, s, book.ID, 1); texts[len(texts)-1] != "From the ebook." {
		t.Errorf("chapter 1 = %q", texts)
	}
	if _, status := postPreview(t, ts.URL, "preview=nope", nil); status != http.StatusGone {
		t.Errorf("unknown preview: %d", status)
	}
}

// TestPreviewFindsReplacedChapter: an author has put a notice where a
// chapter was, under the same id. The archive still has the chapter under
// its old address; the preview offers it ahead of the notice, and it
// imports from the archive's copy.
func TestPreviewFindsReplacedChapter(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	site := royalroadtest.New(t)
	fake, wayback := newFakeWayback(t)
	srv.Sources = []websource.Source{site.Source()}
	srv.Wayback = wayback
	site.AddChapter("1. One", `<p>First.</p>`)
	two := site.AddChapter("2. The Long Night", `<p>The real second chapter.</p>`)
	site.AddChapter("3. Three", `<p>Third.</p>`)

	base := "https://www.royalroad.com/fiction/" + royalroadtest.FictionID + "/the-long-road"
	fake.save(site.URL(), "20230101000000", fetchPage(t, site.URL()))
	fake.save(base+"/chapter/"+two+"/2-the-long-night", "20230102000000", fetchPage(t, site.URL()+"/chapter/"+two+"/c"))
	site.Replace(two, "Book 1 Summary", `<p>Book one is on Kindle now.</p>`)
	// Saved again since, under what it is now - and once under a mangled
	// address, which is nothing.
	fake.save(base+"/chapter/"+two+"/book-1-summary", "20240102000000", fetchPage(t, site.URL()+"/chapter/"+two+"/c"))
	fake.save(base+"/chapter/"+two+"/0, 0, .5", "20240103000000", "junk")

	p, _ := postPreview(t, ts.URL, "url="+url.QueryEscape(fictionLink)+"&archive=true", nil)
	if previewLine(p) != "1. One=site|2. The Long Night=archive|Book 1 Summary=site|3. Three=site" {
		t.Fatalf("preview = %q", previewLine(p))
	}
	if p.Chapters[1].ReplacedBy != "Book 1 Summary" || p.Chapters[1].ID == two || p.Chapters[2].ID != two {
		t.Fatalf("replaced chapter = %+v, then %+v", p.Chapters[1], p.Chapters[2])
	}

	// Take the story, leave the notice.
	var created sourceTestBook
	chosen := []string{p.Chapters[0].ID, p.Chapters[1].ID, p.Chapters[3].ID}
	if resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/import-url", importBookURLRequest{URL: fictionLink, PreviewID: p.ID, Chapters: chosen}, &created); resp.StatusCode != http.StatusCreated {
		t.Fatalf("import: %d", resp.StatusCode)
	}
	book := waitSourceSynced(t, ts.URL, created.ID)
	if book.ChapterCount != 3 || book.Source.Error != "" {
		t.Fatalf("%d chapters, source %+v", book.ChapterCount, book.Source)
	}
	if title, texts := chapterTexts(t, s, book.ID, 1); title != "2. The Long Night" || texts[len(texts)-1] != "The real second chapter." {
		t.Errorf("chapter 1 = %q %q", title, texts)
	}
	if title, _ := chapterTexts(t, s, book.ID, 2); title != "3. Three" {
		t.Errorf("chapter 2 = %q", title)
	}
	// A later check doesn't bring the notice in.
	doJSON(t, http.MethodPost, ts.URL+"/api/books/"+book.ID+"/source/sync", nil, nil).Body.Close()
	if again := waitSourceSynced(t, ts.URL, book.ID); again.ChapterCount != 3 || again.Source.Error != "" {
		t.Errorf("after a check: %d chapters, source %+v", again.ChapterCount, again.Source)
	}
}
