package websource_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/epub"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// png is a 1x1 image.
var png = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\xcf\xc0\x00\x00\x03\x01\x01\x00\xc9\xfe\x92\xef\x00\x00\x00\x00IEND\xaeB`\x82")

// TestEPUBGrows checks that the written epub parses to one chapter per
// fetched chapter - an empty one included - and that adding chapters to it
// keeps the earlier ones.
func TestEPUBGrows(t *testing.T) {
	meta := websource.Meta{Kind: "test", WorkID: "1", URL: "https://example.test/fiction/1", Title: "T & <Co>", Author: "Me", Cover: png, CoverExt: ".png"}
	first := []*websource.Chapter{
		{ID: "10", Title: "One & Only", Body: `<p><img src="../img/10-0.png" alt=""/></p><p>First.</p>`, Images: []websource.Image{{Name: "10-0.png", Data: png}}},
		{ID: "11", Title: "Empty"},
	}
	var a bytes.Buffer
	if err := websource.WriteEPUB(&a, meta, nil, first); err != nil {
		t.Fatal(err)
	}
	prev, err := zip.NewReader(bytes.NewReader(a.Bytes()), int64(a.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	meta.Cover, meta.CoverExt = nil, ""
	more := []*websource.Chapter{{ID: "11", Title: "Empty"}, {ID: "2017/03/03/rw1-00", Title: "Three", Body: "<p>Third.</p>"}}
	if err := websource.WriteEPUB(&b, meta, prev, more); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := websource.ChapterIDs(zr)
	if err != nil || strings.Join(ids, ",") != "10,11,2017/03/03/rw1-00" {
		t.Fatalf("ChapterIDs = %v, %v", ids, err)
	}
	book, err := epub.ParseAs(bytes.NewReader(b.Bytes()), int64(b.Len()), epub.PlayOff)
	if err != nil {
		t.Fatal(err)
	}
	if book.Title != "T & <Co>" || book.Author != "Me" || !bytes.Equal(book.CoverData, png) {
		t.Errorf("book = %q by %q, cover %d bytes", book.Title, book.Author, len(book.CoverData))
	}
	var titles []string
	for _, ch := range book.Chapters {
		titles = append(titles, ch.Title)
	}
	if strings.Join(titles, "|") != "One & Only|Empty|Three" {
		t.Fatalf("chapters = %q", titles)
	}
	// Title, image, text - the title spoken once.
	if got := book.Chapters[0].Blocks; len(got) != 3 || got[0].Text != "One & Only" || got[1].Kind != epub.BlockImage || got[2].Text != "First." {
		t.Errorf("chapter 0 blocks = %+v", got)
	}

	var foreign bytes.Buffer
	zw := zip.NewWriter(&foreign)
	_, _ = zw.Create("mimetype")
	_ = zw.Close()
	fr, _ := zip.NewReader(bytes.NewReader(foreign.Bytes()), int64(foreign.Len()))
	if err := websource.WriteEPUB(&bytes.Buffer{}, meta, fr, more); !errors.Is(err, websource.ErrForeignEpub) {
		t.Errorf("adding to an epub without an index: err = %v", err)
	}
}
