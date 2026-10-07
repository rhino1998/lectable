// Package royalroadtest is a stand-in for royalroad.com in tests: a local
// server laying one fiction's pages out the way the site does.
package royalroadtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/royalroad"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// FictionID is the id of the one fiction a Site serves.
const FictionID = "4242"

// PNG is a 1x1 image, served at /img/pixel.png.
var PNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\xcf\xc0\x00\x00\x03\x01\x01\x00\xc9\xfe\x92\xef\x00\x00\x00\x00IEND\xaeB`\x82")

type chapter struct {
	id       int
	title    string
	body     string
	unlocked bool
	gone     bool
}

// Site is the fake site. Add chapters, then point a client at it.
type Site struct {
	Server *httptest.Server
	Title  string
	Author string

	mu       sync.Mutex
	chapters []*chapter
	// failing makes every chapter page answer 503 while set.
	failing bool
}

// New starts a site serving one fiction with no chapters yet.
func New(t *testing.T) *Site {
	t.Helper()
	s := &Site{Title: "The Long Road", Author: "A. Writer"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /fiction/{id}", s.fiction)
	mux.HandleFunc("GET /fiction/{id}/{slug}", s.fiction)
	mux.HandleFunc("GET /fiction/{id}/{slug}/chapter/{chapter}/{chapterSlug}", s.chapter)
	mux.HandleFunc("GET /img/pixel.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(PNG)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Server.Close)
	return s
}

// Source returns a source reading from the site, with no pacing.
func (s *Site) Source() *royalroad.Source {
	return &royalroad.Source{Client: websource.Client{HTTP: s.Server.Client(), MinInterval: -1}, BaseURL: s.Server.URL}
}

// URL is the fiction's page.
func (s *Site) URL() string { return s.Server.URL + "/fiction/" + FictionID + "/the-long-road" }

// AddChapter publishes a chapter whose text is body (an HTML fragment) and
// returns its id.
func (s *Site) AddChapter(title, body string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := 9000 + len(s.chapters)
	s.chapters = append(s.chapters, &chapter{id: id, title: title, body: body, unlocked: true})
	return fmt.Sprint(id)
}

// AddLockedChapter lists a chapter the site won't serve yet.
func (s *Site) AddLockedChapter(title string) string {
	id := s.AddChapter(title, "")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chapters[len(s.chapters)-1].unlocked = false
	return id
}

// Unlock makes every listed chapter readable, with body as the text of
// those that were locked.
func (s *Site) Unlock(body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.chapters {
		if !c.unlocked {
			c.unlocked, c.body = true, body
		}
	}
}

// SetFailing makes chapter pages answer 503 (true) or work again (false).
func (s *Site) SetFailing(failing bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = failing
}

// Remove makes a listed chapter's page answer 404.
func (s *Site) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.chapters {
		if fmt.Sprint(c.id) == id {
			c.gone = true
		}
	}
}

func (s *Site) fiction(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != FictionID {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	type entry struct {
		ID         int    `json:"id"`
		Title      string `json:"title"`
		Slug       string `json:"slug"`
		Order      int    `json:"order"`
		IsUnlocked bool   `json:"isUnlocked"`
		URL        string `json:"url"`
	}
	list := []entry{}
	for i, c := range s.chapters {
		list = append(list, entry{ID: c.id, Title: c.title, Slug: "c", Order: i, IsUnlocked: c.unlocked,
			URL: fmt.Sprintf("/fiction/%s/the-long-road/chapter/%d/c", FictionID, c.id)})
	}
	raw, _ := json.Marshal(list)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html><html><head>
<title>%[1]s | Royal Road</title>
<meta name="twitter:title" content="%[1]s">
<link rel="canonical" href="%[4]s"/>
<meta property="og:image" content="%[3]s/img/pixel.png">
<meta property="og:url" content="%[4]s">
<meta property="books:author" content="%[2]s"/>
</head><body>
<div class="fic-header"><h1 class="font-white">%[1]s</h1></div>
<script>
window.fictionCover = "x";
window.chapters = %[5]s;
window.volumes = [];
</script>
</body></html>`, s.Title, s.Author, s.Server.URL, s.URL(), raw)
}

func (s *Site) chapter(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing {
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	for _, c := range s.chapters {
		if fmt.Sprint(c.id) != r.PathValue("chapter") {
			continue
		}
		if c.gone {
			break
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if !c.unlocked {
			fmt.Fprint(w, `<!DOCTYPE html><html><body><h1>Locked</h1><p>Subscribe to read.</p></body></html>`)
			return
		}
		// One real paragraph is followed by a hidden notice, as on the site.
		body := c.body
		if i := strings.Index(body, "</p>"); i >= 0 {
			body = body[:i+4] + `<span class="cmHIDDEN0"><br>Stolen content; report it.<br></span>` + body[i+4:]
		}
		fmt.Fprintf(w, `<!DOCTYPE html><html><head><title>%[1]s</title>
<style>
            .cmHIDDEN0{
                display: none;
                speak: never;
            }
</style></head><body>
<h1 class="font-white break-word">%[1]s</h1>
<div class="portlet solid author-note-portlet"><div class="portlet-body author-note"><p>Note before: join my Patreon.</p></div></div>
<div class="chapter-inner chapter-content">
%[2]s
</div>
<div class="portlet solid author-note-portlet"><div class="portlet-body author-note"><p>Note after: thanks for reading.</p></div></div>
</body></html>`, c.title, body)
		return
	}
	http.NotFound(w, r)
}
