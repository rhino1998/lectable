package royalroad_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/epub"
	"github.com/rhino1998/lectable/backend/internal/royalroad"
	"github.com/rhino1998/lectable/backend/internal/royalroad/royalroadtest"
)

func TestParseFictionURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.royalroad.com/fiction/21220/mother-of-learning":                   "21220",
		"https://www.royalroad.com/fiction/21220":                                      "21220",
		"royalroad.com/fiction/21220/mother-of-learning/chapter/301778/1-good-morning": "21220",
		" http://royalroadl.com/fiction/7/x ":                                          "7",
		"21220":                                                                        "21220",
		"https://www.royalroad.com/fictions/best-rated":                                "",
		"https://www.royalroad.com/profile/21220":                                      "",
		"https://example.com/fiction/21220":                                            "",
		"https://royalroad.com.evil.example/fiction/21220":                             "",
		"": "",
	} {
		got, err := royalroad.ParseFictionURL(in)
		if got != want || (err == nil) != (want != "") {
			t.Errorf("ParseFictionURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestFictionAndChapter(t *testing.T) {
	site := royalroadtest.New(t)
	site.AddChapter("1. Setting Out", `<p class="cnAAA" style="text-align: center"><strong>Chapter 001<br></strong><strong>Setting Out</strong></p>
<p class="cnBBB">The road was long.</p>
<p class="cnCCC">“Are we there yet?” Sam asked.</p>
<p><img src="/img/pixel.png" data-x="1"><img src="/img/missing.png"></p>
<table><tr><td>Name</td><td>Sam</td></tr><tr><td>Class</td><td>None</td></tr></table>
<script>alert(1)</script>`)
	site.AddLockedChapter("2. Early Access")
	c := site.Client()
	ctx := context.Background()

	f, err := c.Fiction(ctx, royalroadtest.FictionID)
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "The Long Road" || f.Author != "A. Writer" || f.URL != site.URL() || !strings.HasSuffix(f.CoverURL, "/img/pixel.png") {
		t.Fatalf("fiction = %+v", f)
	}
	if len(f.Chapters) != 2 || f.Chapters[0].Title != "1. Setting Out" || !f.Chapters[0].Unlocked || f.Chapters[1].Unlocked {
		t.Fatalf("chapters = %+v", f.Chapters)
	}

	ch, err := c.Chapter(ctx, f.Chapters[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"Stolen", "Patreon", "thanks for reading", "alert", "cnBBB", "style=", "data-x", "Chapter 001", "missing"} {
		if strings.Contains(ch.Body, unwanted) {
			t.Errorf("body still has %q:\n%s", unwanted, ch.Body)
		}
	}
	for _, wanted := range []string{"The road was long.", "Are we there yet?", "<table>", `src="../img/` + ch.ID + `-0.png"`} {
		if !strings.Contains(ch.Body, wanted) {
			t.Errorf("body lacks %q:\n%s", wanted, ch.Body)
		}
	}
	if len(ch.Images) != 1 || !bytes.Equal(ch.Images[0].Data, royalroadtest.PNG) {
		t.Fatalf("images = %d", len(ch.Images))
	}

	if _, err := c.Chapter(ctx, f.Chapters[1]); err == nil {
		t.Error("a locked chapter's page read as a chapter")
	}
	site.Remove(f.Chapters[0].ID)
	if _, err := c.Chapter(ctx, f.Chapters[0]); !errors.Is(err, royalroad.ErrNotFound) {
		t.Errorf("removed chapter: err = %v, want ErrNotFound", err)
	}
	if _, err := c.Fiction(ctx, "1"); !errors.Is(err, royalroad.ErrNotFound) {
		t.Errorf("unknown fiction: err = %v, want ErrNotFound", err)
	}
}

// TestEPUBGrows checks that the written epub parses to one chapter per
// fetched chapter - an empty one included - and that adding chapters to it
// keeps the earlier ones.
func TestEPUBGrows(t *testing.T) {
	meta := royalroad.Meta{FictionID: "1", URL: "https://example.test/fiction/1", Title: "T & <Co>", Author: "Me", Cover: royalroadtest.PNG, CoverExt: ".png"}
	first := []*royalroad.Chapter{
		{ID: "10", Title: "One & Only", Body: `<p><img src="../img/10-0.png" alt=""/></p><p>First.</p>`, Images: []royalroad.Image{{Name: "10-0.png", Data: royalroadtest.PNG}}},
		{ID: "11", Title: "Empty"},
	}
	var a bytes.Buffer
	if err := royalroad.WriteEPUB(&a, meta, nil, first); err != nil {
		t.Fatal(err)
	}
	prev, err := zip.NewReader(bytes.NewReader(a.Bytes()), int64(a.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	meta.Cover, meta.CoverExt = nil, ""
	more := []*royalroad.Chapter{{ID: "11", Title: "Empty"}, {ID: "12", Title: "Three", Body: "<p>Third.</p>"}}
	if err := royalroad.WriteEPUB(&b, meta, prev, more); err != nil {
		t.Fatal(err)
	}

	zr, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := royalroad.ChapterIDs(zr)
	if err != nil || strings.Join(ids, ",") != "10,11,12" {
		t.Fatalf("ChapterIDs = %v, %v", ids, err)
	}
	book, err := epub.ParseAs(bytes.NewReader(b.Bytes()), int64(b.Len()), epub.PlayOff)
	if err != nil {
		t.Fatal(err)
	}
	if book.Title != "T & <Co>" || book.Author != "Me" || !bytes.Equal(book.CoverData, royalroadtest.PNG) {
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
	if err := royalroad.WriteEPUB(&bytes.Buffer{}, meta, fr, more); !errors.Is(err, royalroad.ErrForeignEpub) {
		t.Errorf("adding to an epub without an index: err = %v", err)
	}
}
