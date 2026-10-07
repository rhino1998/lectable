package websource_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/epub"
	"github.com/rhino1998/lectable/backend/internal/royalroad"
	"github.com/rhino1998/lectable/backend/internal/wanderinginn"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// TestLive fetches a real work's first chapters from its site and reads
// them back through the epub parser. Skipped unless WEBSOURCE_LIVE is a
// link to one - a manual check for when a site's markup changes:
//
//	WEBSOURCE_LIVE=https://www.royalroad.com/fiction/21220 go test ./internal/websource -run TestLive -v
//	WEBSOURCE_LIVE=https://wanderinginn.com/table-of-contents/#vol-1 go test ./internal/websource -run TestLive -v
//	WEBSOURCE_LIVE=https://web.archive.org/web/2022/https://wanderinginn.com/table-of-contents/ go test ./internal/websource -run TestLive -v
func TestLive(t *testing.T) {
	target := os.Getenv("WEBSOURCE_LIVE")
	if target == "" {
		t.Skip("WEBSOURCE_LIVE not set")
	}
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	// A Wayback Machine link reads the site as archived then.
	wayback := &websource.Wayback{}
	timestamp, original, archived := websource.ParseWaybackLink(u)
	if archived {
		u = original
	}
	var src websource.Source
	for _, s := range []websource.Source{&royalroad.Source{}, &wanderinginn.Source{}} {
		if s.Match(u) {
			src = s
		}
	}
	if src == nil {
		t.Fatalf("no source for %s", target)
	}
	f := src.Fetch()
	if archived {
		f = wayback.At(timestamp)
	}
	ctx := context.Background()
	ids, err := src.Resolve(ctx, f, u)
	if err != nil || len(ids) == 0 {
		t.Fatalf("Resolve = %v, %v", ids, err)
	}
	w, err := src.Work(ctx, f, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s %s (of %d): %q by %q, series %q #%v, %d chapters, cover %s, url %s", src.Name(), w.ID, len(ids), w.Title, w.Author, w.SeriesName, w.SeriesIndex, len(w.Chapters), w.CoverURL, w.URL)
	if len(w.Chapters) == 0 {
		t.Fatal("no chapters")
	}
	// WEBSOURCE_LIVE_DELISTED=1 also looks in the archive for chapters the
	// site has taken off the work's list.
	if d, ok := src.(websource.Delister); ok && os.Getenv("WEBSOURCE_LIVE_DELISTED") != "" && !archived {
		all, err := wayback.Delisted(ctx, d, w)
		if err != nil {
			t.Fatal(err)
		}
		delisted := 0
		for _, ref := range all {
			if ref.Delisted {
				delisted++
			}
		}
		t.Logf("with the archive: %d chapters, %d of them delisted", len(all), delisted)
		for _, i := range []int{0, 1, len(all) / 3, len(all) / 2, len(all) - 2, len(all) - 1} {
			t.Logf("  %4d %s delisted=%v %q", i, all[i].ID, all[i].Delisted, all[i].Title)
		}
		// Chapters the site has put something else in the place of, with
		// their neighbours.
		replaced := false
		for i, ref := range all {
			if ref.ReplacedBy == "" {
				continue
			}
			replaced = true
			for j := max(0, i-1); j < min(len(all), i+3); j++ {
				t.Logf("  %4d %s delisted=%v %q replacedBy=%q", j, all[j].ID, all[j].Delisted, all[j].Title, all[j].ReplacedBy)
			}
		}
		// Read one delisted chapter from its archived copy: a replaced one
		// if there is any, else the first.
		for _, ref := range all {
			if !ref.Delisted || (replaced && ref.ReplacedBy == "") {
				continue
			}
			ch, err := wayback.ArchivedChapter(ctx, ref.URL, func(f websource.Fetcher) (*websource.Chapter, error) {
				return src.Chapter(ctx, f, ref)
			})
			if err != nil {
				t.Fatalf("delisted chapter %q: %v", ref.Title, err)
			}
			t.Logf("  read %q from the archive: %d bytes: %.160q", ref.Title, len(ch.Body), ch.Body)
			break
		}
	}
	meta := websource.Meta{Kind: src.Kind(), WorkID: w.ID, URL: w.URL, Title: w.Title, Author: w.Author}
	if w.CoverURL != "" {
		if meta.Cover, meta.CoverExt, err = websource.Cover(ctx, f, w.CoverURL); err != nil {
			t.Fatal(err)
		}
	}
	var chapters []*websource.Chapter
	for _, ref := range w.Chapters[:min(2, len(w.Chapters))] {
		ch, err := src.Chapter(ctx, f, ref)
		if errors.Is(err, websource.ErrLocked) && !archived {
			t.Logf("chapter %q is locked at the site; looking in the archive", ref.Title)
			ch, err = wayback.ArchivedChapter(ctx, ref.URL, func(f websource.Fetcher) (*websource.Chapter, error) {
				return src.Chapter(ctx, f, ref)
			})
		}
		if err != nil {
			t.Fatal(err)
		}
		chapters = append(chapters, ch)
	}
	var buf bytes.Buffer
	if err := websource.WriteEPUB(&buf, meta, nil, chapters); err != nil {
		t.Fatal(err)
	}
	if out := os.Getenv("WEBSOURCE_LIVE_OUT"); out != "" {
		if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	book, err := epub.ParseAs(bytes.NewReader(buf.Bytes()), int64(buf.Len()), epub.PlayOff)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("parsed %q by %q, cover %d bytes (%s), %d chapters", book.Title, book.Author, len(book.CoverData), book.CoverMediaType, len(book.Chapters))
	for _, ch := range book.Chapters {
		t.Logf("chapter %q: %d blocks", ch.Title, len(ch.Blocks))
		for i, b := range ch.Blocks[:min(6, len(ch.Blocks))] {
			t.Logf("  %d %s quote=%v %.90q", i, b.Kind, b.IsQuote, b.Text)
		}
		for i, b := range ch.Blocks[max(0, len(ch.Blocks)-3):] {
			t.Logf("  -%d %s quote=%v %.90q", 3-i, b.Kind, b.IsQuote, b.Text)
		}
	}
	if len(book.Chapters) != len(chapters) {
		t.Fatalf("parsed %d chapters, wrote %d", len(book.Chapters), len(chapters))
	}
}
