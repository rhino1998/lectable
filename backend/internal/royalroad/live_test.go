package royalroad

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/epub"
)

// TestLive fetches a real fiction's first chapters from royalroad.com and
// reads them back through the epub parser. Skipped unless ROYALROAD_LIVE
// names a fiction (a URL or id) - a manual check for when the site's
// markup changes:
//
//	ROYALROAD_LIVE=21220 go test ./internal/royalroad -run TestLive -v
func TestLive(t *testing.T) {
	target := os.Getenv("ROYALROAD_LIVE")
	if target == "" {
		t.Skip("ROYALROAD_LIVE not set")
	}
	id, err := ParseFictionURL(target)
	if err != nil {
		t.Fatal(err)
	}
	c := &Client{}
	ctx := context.Background()
	f, err := c.Fiction(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%q by %q, %d chapters, cover %s, url %s", f.Title, f.Author, len(f.Chapters), f.CoverURL, f.URL)
	if len(f.Chapters) == 0 {
		t.Fatal("no chapters")
	}
	meta := Meta{FictionID: f.ID, URL: f.URL, Title: f.Title, Author: f.Author}
	if f.CoverURL != "" {
		if meta.Cover, meta.CoverExt, err = c.Cover(ctx, f.CoverURL); err != nil {
			t.Fatal(err)
		}
	}
	var chapters []*Chapter
	for _, ref := range f.Chapters[:min(2, len(f.Chapters))] {
		ch, err := c.Chapter(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		chapters = append(chapters, ch)
	}
	var buf bytes.Buffer
	if err := WriteEPUB(&buf, meta, nil, chapters); err != nil {
		t.Fatal(err)
	}
	if out := os.Getenv("ROYALROAD_LIVE_OUT"); out != "" {
		if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	book, err := epub.ParseAs(bytes.NewReader(buf.Bytes()), int64(buf.Len()), epub.PlayOff)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len())); err != nil {
		t.Fatal(err)
	}
	t.Logf("parsed %q by %q, cover %d bytes (%s), %d chapters", book.Title, book.Author, len(book.CoverData), book.CoverMediaType, len(book.Chapters))
	for _, ch := range book.Chapters {
		t.Logf("chapter %q: %d blocks", ch.Title, len(ch.Blocks))
		for i, b := range ch.Blocks[:min(6, len(ch.Blocks))] {
			t.Logf("  %d %s quote=%v %.90q", i, b.Kind, b.IsQuote, b.Text)
		}
	}
	if len(book.Chapters) != len(chapters) {
		t.Fatalf("parsed %d chapters, wrote %d", len(book.Chapters), len(chapters))
	}
}
