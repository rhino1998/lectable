package httpapi

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
)

// testEpub builds a minimal epub whose chapters are the given XHTML body
// fragments, each titled by its own <h1>.
func testEpub(t *testing.T, bodies ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	write := func(name, content string) {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	write("mimetype", "application/epub+zip")
	write("META-INF/container.xml", `<?xml version="1.0"?><container version="1.0" xmlns="urn:oasis:names:tc:opendocument:xmlns:container"><rootfiles><rootfile full-path="OEBPS/content.opf" media-type="application/oebps-package+xml"/></rootfiles></container>`)
	var manifest, spine string
	for i, body := range bodies {
		manifest += fmt.Sprintf(`<item id="c%d" href="c%d.xhtml" media-type="application/xhtml+xml"/>`, i, i)
		spine += fmt.Sprintf(`<itemref idref="c%d"/>`, i)
		write(fmt.Sprintf("OEBPS/c%d.xhtml", i), `<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"><head><title>x</title></head><body>`+body+`</body></html>`)
	}
	write("OEBPS/content.opf", `<?xml version="1.0"?><package xmlns="http://www.idpf.org/2007/opf" version="2.0"><metadata xmlns:dc="http://purl.org/dc/elements/1.1/"><dc:title>Reimport Test</dc:title></metadata><manifest>`+manifest+`</manifest><spine>`+spine+`</spine></package>`)
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func postEpub(t *testing.T, url string, epub []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "book.epub")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(epub); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, mw.FormDataContentType(), &body)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var body struct{ Code string }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	return body.Code
}

func TestReimportChapter(t *testing.T) {
	srv, s, _, ts := newTestServer(t)

	original := testEpub(t,
		`<h1>One</h1><p>First chapter stays.</p>`,
		`<h1>Two</h1><p>“Hi,” Ann said.<br/><br/>“Bye,” Bob said.</p>`,
	)
	resp := postEpub(t, ts.URL+"/api/books", original)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	var book struct{ ID string }
	if err := json.NewDecoder(resp.Body).Decode(&book); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if _, err := os.Stat(audiopath.SourceEpubFile(srv.DataDir, book.ID)); err != nil {
		t.Fatalf("upload didn't keep the source epub: %v", err)
	}

	ch0, _ := s.GetChapterByIdx(book.ID, 0)
	ch1, _ := s.GetChapterByIdx(book.ID, 1)
	if err := s.SetParagraphSpeakers(ch0.ID, map[int]string{1: "Narrator"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphSpeakers(ch1.ID, map[int]string{1: "Ann"}); err != nil {
		t.Fatal(err)
	}

	// A corrected epub for chapter 1, uploaded with the request (becomes
	// the kept copy).
	fixed := testEpub(t,
		`<h1>One</h1><p>First chapter changed in the epub but not re-imported.</p>`,
		`<h1>Two</h1><p>“Hi,” Ann said.</p><p>“Bye,” Bob said.</p><p>The end.</p>`,
	)
	resp = postEpub(t, ts.URL+"/api/books/"+book.ID+"/chapters/1/reimport", fixed)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reimport: %d", resp.StatusCode)
	}
	resp.Body.Close()

	ps, _ := s.ListParagraphsRaw(ch1.ID)
	// The <h1> title is paragraph 0.
	if len(ps) != 6 || ps[5].Text != "The end." {
		t.Fatalf("chapter 1 not replaced: %+v", ps)
	}
	for _, p := range ps {
		if p.Speaker != "" {
			t.Errorf("chapter 1 paragraph %d kept speaker %q", p.Idx, p.Speaker)
		}
	}
	ps0, _ := s.ListParagraphsRaw(ch0.ID)
	if len(ps0) != 2 || ps0[1].Text != "First chapter stays." || ps0[1].Speaker != "Narrator" {
		t.Fatalf("chapter 0 should be untouched: %+v", ps0)
	}

	// No body: re-imports from the kept (now corrected) copy.
	resp, err := http.Post(ts.URL+"/api/books/"+book.ID+"/chapters/0/reimport", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reimport from kept copy: %d", resp.StatusCode)
	}
	resp.Body.Close()
	ps0, _ = s.ListParagraphsRaw(ch0.ID)
	if ps0[1].Text != "First chapter changed in the epub but not re-imported." {
		t.Fatalf("chapter 0 not re-imported from kept copy: %+v", ps0)
	}

	retitled := testEpub(t, `<h1>One</h1><p>a</p>`, `<h1>Deux</h1><p>b</p>`)
	resp = postEpub(t, ts.URL+"/api/books/"+book.ID+"/chapters/1/reimport", retitled)
	if resp.StatusCode != http.StatusConflict || errorCode(t, resp) != "title_mismatch" {
		t.Fatalf("want 409 title_mismatch, got %d", resp.StatusCode)
	}
	resp, _ = http.Post(ts.URL+"/api/books/"+book.ID+"/chapters/1/reimport?force=true", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("forced reimport: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if ch, _ := s.GetChapterByIdx(book.ID, 1); ch.Title != "Deux" {
		t.Fatalf("forced reimport didn't retitle: %q", ch.Title)
	}

	os.Remove(audiopath.SourceEpubFile(srv.DataDir, book.ID))
	resp, _ = http.Post(ts.URL+"/api/books/"+book.ID+"/chapters/1/reimport", "", nil)
	if resp.StatusCode != http.StatusConflict || errorCode(t, resp) != "no_source_epub" {
		t.Fatalf("want 409 no_source_epub, got %d", resp.StatusCode)
	}
}
