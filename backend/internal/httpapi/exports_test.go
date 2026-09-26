package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/bookexport"
)

func withExports(srv *Server) {
	srv.Exports = bookexport.NewManager(bookexport.Source{Store: srv.Store, Voices: srv.Narration, DataDir: srv.DataDir}, nil)
}

func waitForExports(t *testing.T, url string, done func([]bookExportDTO) bool) []bookExportDTO {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var list []bookExportDTO
		if resp := doJSON(t, http.MethodGet, url, nil, &list); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET exports: status %d", resp.StatusCode)
		}
		if done(list) {
			return list
		}
		if time.Now().After(deadline) {
			t.Fatalf("exports never settled: %+v", list)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// An export queued on an unrendered book renders it first (through the
// job queue, like any generation), then builds; the file downloads and
// deletes.
func TestExportRendersFirst(t *testing.T) {
	srv, s, fake, ts := newTestServer(t)
	withExports(srv)
	bookID := createTestBook(t, s, "", 0, "First paragraph.", "Second paragraph.")
	url := ts.URL + "/api/books/" + bookID + "/exports"

	if resp := doJSON(t, http.MethodPost, url, createExportRequest{WordLevel: true}, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST export: status %d", resp.StatusCode)
	}
	list := waitForExports(t, url, func(l []bookExportDTO) bool { return len(l) == 1 && (l[0].Ready || l[0].Error != "") })
	e := list[0]
	if e.Error != "" || !e.Full || e.MissingAudio != 0 || e.DownloadURL == "" || e.FileName != "Test Book [words].epub" {
		t.Fatalf("export: %+v", e)
	}
	if n := len(fake.GenerateCalls()); n != 2 {
		t.Errorf("expected both paragraphs rendered first, got %d generate calls", n)
	}

	resp, err := http.Get(ts.URL + e.DownloadURL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/epub+zip" ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") || int64(len(body)) != e.SizeBytes {
		t.Fatalf("download: %d %v (%d bytes)", resp.StatusCode, resp.Header, len(body))
	}

	if resp := doJSON(t, http.MethodDelete, url+"/"+e.ID, nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE export: status %d", resp.StatusCode)
	}
	waitForExports(t, url, func(l []bookExportDTO) bool { return len(l) == 0 })
	if resp := doJSON(t, http.MethodDelete, url+"/"+e.ID, nil, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second DELETE: status %d", resp.StatusCode)
	}
}

// As-is skips rendering: nothing is generated, the text goes in without
// audio.
func TestExportAsIs(t *testing.T) {
	srv, s, fake, ts := newTestServer(t)
	withExports(srv)
	bookID := createTestBook(t, s, "", 0, "Only paragraph.")
	url := ts.URL + "/api/books/" + bookID + "/exports"

	if resp := doJSON(t, http.MethodPost, url, createExportRequest{AsIs: true}, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST export: status %d", resp.StatusCode)
	}
	list := waitForExports(t, url, func(l []bookExportDTO) bool { return len(l) == 1 && (l[0].Ready || l[0].Error != "") })
	if e := list[0]; !e.Ready || e.MissingAudio != 1 {
		t.Fatalf("export: %+v", e)
	}
	if n := len(fake.GenerateCalls()); n != 0 {
		t.Errorf("as-is export generated %d paragraphs", n)
	}
	if resp := doJSON(t, http.MethodPost, url, createExportRequest{Chapters: []int{3}}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("out-of-range chapter: status %d", resp.StatusCode)
	}
}
