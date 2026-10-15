package wanderinginn_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/wanderinginn"
	"github.com/rhino1998/lectable/backend/internal/wanderinginn/wanderinginntest"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

func TestSource(t *testing.T) {
	site := wanderinginntest.New(t)
	first := site.AddChapter(1, "rw1-00", "1.00", `<p>The inn was dark.</p>
<p><span style="color:#96be50;">“Same to you!”</span></p>
<p><strong>Author’s Note:</strong> an aside in the first half, kept.</p>
<p>More story.</p><p>Still more.</p><p>And more.</p><p>The end of it.</p>
<p>&nbsp;</p>
<p><strong>Author’s Note: </strong>And we’re done.</p>
<p>Thanks for reading.</p>
<p><img class="alignnone" src="/wp-content/cover.png" alt="Fan art" data-image-meta="{}" /></p>`)
	locked := site.AddChapter(1, "rw1-01", "1.01", `<p>Patrons only.</p>`)
	site.SetLocked(locked, true)
	site.AddChapter(3, "3-00-e", "3.00 E", `<p>Volume three.</p><p><img src="/wp-content/cover.png" alt="Map"></p>`)
	src := site.Source()
	ctx := context.Background()

	resolve := func(link string) string {
		t.Helper()
		u, err := url.Parse(link)
		if err != nil {
			t.Fatal(err)
		}
		if !src.Match(mustParse(t, "https://wanderinginn.com/")) || src.Match(mustParse(t, "https://example.com/")) {
			t.Fatal("Match is wrong")
		}
		ids, err := src.Resolve(ctx, src.Fetch(), u)
		if err != nil {
			return "error"
		}
		return strings.Join(ids, ",")
	}
	for link, want := range map[string]string{
		"https://wanderinginn.com/":                          "vol-1,vol-3",
		"https://wanderinginn.com/table-of-contents/":        "vol-1,vol-3",
		"https://wanderinginn.com/table-of-contents/#vol-3":  "vol-3",
		"https://wanderinginn.com/table-of-contents/#vol-9":  "error",
		"https://wanderinginn.com/2017/03/03/3-00-e/":        "vol-3",
		"https://wanderinginn.com/2017/03/01/rw1-00/#c":      "vol-1",
		"https://wanderinginn.com/2017/03/03/not-a-chapter/": "error",
		"https://wanderinginn.com/about/":                    "error",
	} {
		if got := resolve(link); got != want {
			t.Errorf("Resolve(%s) = %s, want %s", link, got, want)
		}
	}

	w, err := src.Work(ctx, src.Fetch(), "vol-1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Title != "The Wandering Inn: Volume 1" || w.Author != "pirateaba" || w.SeriesName != "The Wandering Inn" || w.SeriesIndex != 1 ||
		!strings.HasSuffix(w.CoverURL, "/wp-content/cover.png") || !strings.HasSuffix(w.URL, "/table-of-contents/#vol-1") {
		t.Errorf("work = %+v", w)
	}
	if len(w.Chapters) != 2 || w.Chapters[0].ID != first || w.Chapters[0].Title != "1.00" || !w.Chapters[1].Unlocked {
		t.Fatalf("chapters = %+v", w.Chapters)
	}
	if _, err := src.Work(ctx, src.Fetch(), "vol-2"); !errors.Is(err, websource.ErrNotFound) {
		t.Errorf("missing volume: err = %v", err)
	}

	ch, err := src.Chapter(ctx, src.Fetch(), w.Chapters[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"Previous Chapter", "Next Chapter", "<hr", "we’re done", "Thanks for reading", "Fan art", "style=", "A comment"} {
		if strings.Contains(ch.Body, unwanted) {
			t.Errorf("body still has %q:\n%s", unwanted, ch.Body)
		}
	}
	for _, wanted := range []string{"The inn was dark.", "“Same to you!”", "an aside in the first half", "The end of it."} {
		if !strings.Contains(ch.Body, wanted) {
			t.Errorf("body lacks %q:\n%s", wanted, ch.Body)
		}
	}
	if len(ch.Images) != 0 {
		t.Errorf("the note's art was kept: %d images", len(ch.Images))
	}

	if _, err := src.Chapter(ctx, src.Fetch(), w.Chapters[1]); !errors.Is(err, websource.ErrLocked) {
		t.Errorf("locked chapter: err = %v, want ErrLocked", err)
	}

	w3, err := src.Work(ctx, src.Fetch(), "vol-3")
	if err != nil {
		t.Fatal(err)
	}
	ch, err = src.Chapter(ctx, src.Fetch(), w3.Chapters[0])
	if err != nil {
		t.Fatal(err)
	}
	// An image in the story itself is kept, under a name safe for a file.
	if len(ch.Images) != 1 || ch.Images[0].Name != "2017_03_03_3-00-e-0.png" || !strings.Contains(ch.Body, `src="../img/2017_03_03_3-00-e-0.png"`) {
		t.Errorf("images = %+v, body:\n%s", ch.Images, ch.Body)
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestTableOfContentsFailureIsShared: a table of contents that can't be
// fetched is asked for once by the volumes checked together, not once a
// volume.
func TestTableOfContentsFailureIsShared(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	src := &wanderinginn.Source{Client: websource.Client{HTTP: srv.Client(), MinInterval: -1}, BaseURL: srv.URL}
	for _, vol := range []string{"vol-1", "vol-2", "vol-3"} {
		if _, err := src.Work(context.Background(), src.Fetch(), vol); err == nil || !strings.Contains(err.Error(), "403") {
			t.Fatalf("%s: err = %v", vol, err)
		}
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("the table of contents was asked for %d times, want once", n)
	}
}
