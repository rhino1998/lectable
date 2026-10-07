package royalroad_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/royalroad"
	"github.com/rhino1998/lectable/backend/internal/royalroad/royalroadtest"
	"github.com/rhino1998/lectable/backend/internal/websource"
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
	c := site.Source()
	ctx := context.Background()

	f, err := c.Work(ctx, c.Fetch(), royalroadtest.FictionID)
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "The Long Road" || f.Author != "A. Writer" || f.URL != site.URL() || !strings.HasSuffix(f.CoverURL, "/img/pixel.png") {
		t.Fatalf("fiction = %+v", f)
	}
	if len(f.Chapters) != 2 || f.Chapters[0].Title != "1. Setting Out" || !f.Chapters[0].Unlocked || f.Chapters[1].Unlocked {
		t.Fatalf("chapters = %+v", f.Chapters)
	}

	ch, err := c.Chapter(ctx, c.Fetch(), f.Chapters[0])
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

	if _, err := c.Chapter(ctx, c.Fetch(), f.Chapters[1]); !errors.Is(err, websource.ErrLocked) {
		t.Errorf("locked chapter: err = %v, want ErrLocked", err)
	}
	site.Remove(f.Chapters[0].ID)
	if _, err := c.Chapter(ctx, c.Fetch(), f.Chapters[0]); !errors.Is(err, websource.ErrNotFound) {
		t.Errorf("removed chapter: err = %v, want ErrNotFound", err)
	}
	if _, err := c.Work(ctx, c.Fetch(), "1"); !errors.Is(err, websource.ErrNotFound) {
		t.Errorf("unknown fiction: err = %v, want ErrNotFound", err)
	}
}
