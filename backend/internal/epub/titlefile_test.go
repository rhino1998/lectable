package epub

import (
	"archive/zip"
	"bytes"
	"fmt"
	"testing"
)

// A chapter whose TOC entry points at a decoration-only file, with the
// heading and text in the next, unlisted file, is one chapter.
func TestTitleOnlyFileJoinsItsChapter(t *testing.T) {
	titles := []string{"Chapter One – Negotiations", "Chapter Two – Fallback Plans"}
	files := map[string]string{
		"META-INF/container.xml": `<container><rootfiles><rootfile full-path="content.opf"/></rootfiles></container>`,
	}
	manifest, spine, nav := "", "", ""
	for i, title := range titles {
		deco, text := fmt.Sprintf("c%d_split_000.xhtml", i), fmt.Sprintf("c%d_split_001.xhtml", i)
		files[deco] = `<html><head><title>` + title + `</title></head><body><div class="chapter-title"><div id="pb"></div></div></body></html>`
		files[text] = `<html><head><title>` + title + `</title></head><body><h1>` + title + `</h1><p>Some text of chapter ` + fmt.Sprint(i) + `.</p></body></html>`
		for _, name := range []string{deco, text} {
			manifest += `<item id="` + name + `" href="` + name + `" media-type="application/xhtml+xml"/>`
			spine += `<itemref idref="` + name + `"/>`
		}
		nav += `<navPoint><navLabel><text>` + title + `</text></navLabel><content src="` + deco + `"/></navPoint>`
	}
	files["content.opf"] = `<package><metadata><title>Book</title></metadata><manifest>` + manifest +
		`<item id="ncx" href="toc.ncx" media-type="application/x-dtbncx+xml"/></manifest><spine toc="ncx">` + spine + `</spine></package>`
	files["toc.ncx"] = `<ncx><navMap>` + nav + `</navMap></ncx>`

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	book, err := Parse(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(book.Chapters) != len(titles) {
		t.Fatalf("got %d chapters, want %d", len(book.Chapters), len(titles))
	}
	for i, ch := range book.Chapters {
		if ch.Title != titles[i] {
			t.Errorf("chapter %d: title %q, want %q", i, ch.Title, titles[i])
		}
		if len(ch.Blocks) != 2 || ch.Blocks[0].Text != titles[i] {
			t.Errorf("chapter %d: blocks %+v, want the title then one paragraph", i, ch.Blocks)
		}
	}
}
