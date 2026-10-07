// Package wanderinginntest is a stand-in for wanderinginn.com in tests: a
// local server with a table of contents and chapter pages laid out the way
// the site's are.
package wanderinginntest

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/wanderinginn"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

type chapter struct {
	volume int
	path   string // 2017/03/03/slug
	title  string
	body   string
	locked bool
}

// Site is the fake site.
type Site struct {
	Server *httptest.Server

	mu       sync.Mutex
	chapters []*chapter
}

// New starts a site with an empty table of contents.
func New(t *testing.T) *Site {
	t.Helper()
	s := &Site{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /table-of-contents/", s.toc)
	mux.HandleFunc("GET /", s.chapter)
	mux.HandleFunc("GET /wp-content/cover.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\xcf\xc0\x00\x00\x03\x01\x01\x00\xc9\xfe\x92\xef\x00\x00\x00\x00IEND\xaeB`\x82"))
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Server.Close)
	return s
}

// Source returns a source reading from the site, with no pacing.
func (s *Site) Source() *wanderinginn.Source {
	return &wanderinginn.Source{Client: websource.Client{HTTP: s.Server.Client(), MinInterval: -1}, BaseURL: s.Server.URL}
}

// AddChapter lists a chapter under a volume; body is its text (an HTML
// fragment). Returns its id (its address's path).
func (s *Site) AddChapter(volume int, slug, title, body string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &chapter{volume: volume, path: "2017/03/0" + fmt.Sprint(volume) + "/" + slug, title: title, body: body}
	s.chapters = append(s.chapters, c)
	return c.path
}

// SetLocked puts a chapter behind (or takes it out from behind) the
// patrons-only notice.
func (s *Site) SetLocked(id string, locked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.chapters {
		if c.path == id {
			c.locked = locked
		}
	}
}

func (s *Site) toc(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><head><title>Table of Contents - The Wandering Inn</title></head><body>
<ul class="menu"><li><a href="` + s.Server.URL + `/2018/07/09/glossary/">Glossary</a></li></ul>
<div id="table-of-contents">`)
	var volumes []int
	seen := map[int]bool{}
	for _, c := range s.chapters {
		if !seen[c.volume] {
			seen[c.volume] = true
			volumes = append(volumes, c.volume)
		}
	}
	for _, v := range volumes {
		fmt.Fprintf(&b, `<div id="vol-%[1]d" class="volume-wrapper">
<div class="volume-header-buffer"><div class="volume-header"><h2 class="volume-title">Volume %[1]d</h2>
<button class="collapse-button btn-border"></button></div>
<div class="volume-books-wrapper"><div class="volume-books">
<a class="volume-book-card" href="/book/x/"><img class="book-cover" src="%[2]s/wp-content/cover-150x150.png" alt="Book">
<div class="book-card-meta"><div class="book-card-title">Book</div></div></a></div></div></div>
<div class="volume-body"><div class="book-wrapper" data-book-number="%[1]d"><div class="book-body table-body">
`, v, s.Server.URL)
		for _, c := range s.chapters {
			if c.volume != v {
				continue
			}
			fmt.Fprintf(&b, `<div class="chapter-entry table-row is-mobile" data-book-number="%d">
<div class="body-web table-cell">
  <a href="%s/%s/">
      %s    </a>
</div>
<div class="body-audiobook table-cell">Chapter 1</div><div class="body-ebook table-cell">%s</div></div>
`, v, s.Server.URL, c.path, c.title, c.title)
		}
		b.WriteString("</div></div></div></div>\n")
	}
	b.WriteString("</div></body></html>")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(b.String()))
}

func (s *Site) chapter(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.Trim(r.URL.Path, "/")
	for _, c := range s.chapters {
		if c.path != path {
			continue
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		content := `<main id="main-content" role="main"><article class="twi-article" role="article">` + c.body + `
<p>&nbsp;</p>
<hr />
<p><a href="` + s.Server.URL + `/2017/03/01/before/">Previous Chapter</a> <span style="float:right"><a href="` + s.Server.URL + `/2017/03/09/after">Next Chapter</a></span></p>
</article></main>`
		if c.locked {
			content = `<div class="patreon-protected-post" style="background:#000"><h2>Patreon Exclusive</h2><p>Support us on Patreon to access this content.</p></div>`
		}
		fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>%[1]s - The Wandering Inn</title></head><body>
<h2 class="elementor-heading-title">%[1]s</h2>
<div class="reader-container"><div id="reader-content"><div class="elementor-widget-container">
%[2]s
</div></div></div><div class="comments"><p>A comment.</p></div></body></html>`, c.title, content)
		return
	}
	http.NotFound(w, r)
}
