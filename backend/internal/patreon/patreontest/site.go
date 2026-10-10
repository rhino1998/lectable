// Package patreontest is a stand-in for patreon.com in tests: a local
// server answering the parts of the site's JSON API internal/patreon
// reads, for one creator's campaign.
package patreontest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rhino1998/lectable/backend/internal/patreon"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

const (
	// CampaignID is the id of the one campaign a Site serves, Vanity the
	// name in its page's address, and CollectionID a collection of it.
	CampaignID   = "777"
	Vanity       = "longroad"
	CollectionID = "31"
	// MemberSession is the session_id cookie of an account that can read
	// the campaign's patrons-only posts; any other reads as a stranger.
	MemberSession = "member-session"
	// PageSize is how many posts a listing answers with at a time.
	PageSize = 3
)

type post struct {
	id           int
	title        string
	html, tiptap string
	kind         string
	patronsOnly  bool
	collected    bool
}

// Site is the fake site. Add posts, then point a source at it.
type Site struct {
	Server *httptest.Server
	Name   string

	mu    sync.Mutex
	posts []*post
}

// New starts a site serving one campaign with no posts yet.
func New(t *testing.T) *Site {
	t.Helper()
	s := &Site{Name: "A. Writer"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/search", s.search)
	mux.HandleFunc("GET /api/campaigns/{id}", s.campaign)
	mux.HandleFunc("GET /api/collection/{id}", s.collection)
	mux.HandleFunc("GET /api/posts", s.list)
	mux.HandleFunc("GET /api/posts/{id}", s.post)
	mux.HandleFunc("GET /api/current_user", s.currentUser)
	mux.HandleFunc("GET /oauth2/authorize", s.consent)
	mux.HandleFunc("POST /api/oauth2/authorize", s.allow)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Server.Close)
	return s
}

// Source returns a source reading from the site, with no pacing.
func (s *Site) Source() *patreon.Source {
	src := &patreon.Source{BaseURL: s.Server.URL}
	c := src.Fetch().(*websource.Client)
	c.HTTP, c.MinInterval = s.Server.Client(), -1
	return src
}

// Link is the creator's page as a patreon.com link.
func (s *Site) Link() string { return "https://www.patreon.com/" + Vanity }

func (s *Site) add(p *post) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.id = 5000 + len(s.posts)
	if p.kind == "" {
		p.kind = "text_only"
	}
	s.posts = append(s.posts, p)
	return fmt.Sprint(p.id)
}

// AddPublic posts something anyone can read, its text an HTML fragment
// (as posts from before the site's current editor are kept).
func (s *Site) AddPublic(title, body string) string {
	return s.add(&post{title: title, html: body})
}

// AddPatronsOnly posts a chapter only a member can read, its text a
// Tiptap document of the given paragraphs (the current editor's format),
// filed in the campaign's collection.
func (s *Site) AddPatronsOnly(title string, paragraphs ...string) string {
	type node map[string]any
	doc := node{"type": "doc"}
	var content []node
	for _, text := range paragraphs {
		content = append(content, node{"type": "paragraph", "content": []node{{"type": "text", "text": text}}})
	}
	doc["content"] = content
	raw, _ := json.Marshal(doc)
	return s.add(&post{title: title, tiptap: string(raw), patronsOnly: true, collected: true})
}

// AddPoll posts a poll.
func (s *Site) AddPoll(title string) string {
	return s.add(&post{title: title, kind: "poll", html: "<p>Vote.</p>"})
}

func member(r *http.Request) bool {
	c, err := r.Cookie("session_id")
	return err == nil && c.Value == MemberSession
}

func writeAPI(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/vnd.api+json")
	_ = json.NewEncoder(w).Encode(v)
}

func missing(w http.ResponseWriter, what string) {
	w.Header().Set("Content-Type", "application/vnd.api+json")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, `{"errors":[{"code_name":"ResourceMissing","title":"%s was not found.","status":"404"}]}`, what)
}

type obj = map[string]any

func (s *Site) search(w http.ResponseWriter, r *http.Request) {
	data := []obj{{"id": "campaign_1", "type": "campaign-document", "attributes": obj{"url": "https://www.patreon.com/someoneelse"}}}
	if strings.EqualFold(r.URL.Query().Get("q"), Vanity) {
		data = append(data, obj{"id": "campaign_" + CampaignID, "type": "campaign-document", "attributes": obj{"url": "https://www.patreon.com/" + Vanity}})
	}
	writeAPI(w, obj{"data": data})
}

func (s *Site) campaign(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != CampaignID {
		missing(w, "campaign")
		return
	}
	writeAPI(w, obj{
		"data": obj{"id": CampaignID, "type": "campaign",
			"attributes":    obj{"name": "The Long Road", "url": "https://www.patreon.com/" + Vanity, "avatar_photo_url": nil},
			"relationships": obj{"creator": obj{"data": obj{"id": "9", "type": "user"}}}},
		"included": []obj{{"id": "9", "type": "user", "attributes": obj{"full_name": s.Name}}},
	})
}

func (s *Site) collection(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") != CollectionID {
		missing(w, "collection")
		return
	}
	writeAPI(w, obj{"data": obj{"id": CollectionID, "type": "collection",
		"attributes":    obj{"title": "The Long Road: Advance Chapters"},
		"relationships": obj{"campaign": obj{"data": obj{"id": CampaignID, "type": "campaign"}}}}})
}

// attributes is a post as the API gives it to this visitor: a
// patrons-only one has no text for a stranger.
func (p *post) attributes(r *http.Request) obj {
	canView := !p.patronsOnly || member(r)
	a := obj{
		"title": p.title, "post_type": p.kind, "current_user_can_view": canView,
		"url": fmt.Sprintf("https://www.patreon.com/posts/post-%d", p.id), "published_at": "2026-01-01T00:00:00.000+00:00",
	}
	if canView {
		a["content"], a["content_json_string"] = nil, nil
		if p.html != "" {
			a["content"] = p.html
		}
		if p.tiptap != "" {
			a["content_json_string"] = p.tiptap
		}
	}
	return a
}

func (s *Site) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("filter[campaign_id]") != CampaignID {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var posts []*post
	for _, p := range s.posts {
		if q.Get("filter[collection_id]") == "" || (q.Get("filter[collection_id]") == CollectionID && p.collected) {
			posts = append(posts, p)
		}
	}
	start, _ := strconv.Atoi(q.Get("page[cursor]"))
	end := min(start+PageSize, len(posts))
	data := []obj{}
	for _, p := range posts[min(start, len(posts)):end] {
		data = append(data, obj{"id": fmt.Sprint(p.id), "type": "post", "attributes": p.attributes(r)})
	}
	cursors := obj{"next": nil}
	if end < len(posts) {
		cursors["next"] = fmt.Sprint(end)
	}
	writeAPI(w, obj{"data": data, "meta": obj{"pagination": obj{"total": len(posts), "cursors": cursors}}})
}

func (s *Site) post(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.posts {
		if fmt.Sprint(p.id) == r.PathValue("id") {
			writeAPI(w, obj{"data": obj{"id": fmt.Sprint(p.id), "type": "post", "attributes": p.attributes(r),
				"relationships": obj{"campaign": obj{"data": obj{"id": CampaignID, "type": "campaign"}}}}})
			return
		}
	}
	missing(w, "post")
}

// LoginCode is the one-time code the consent page sends a site back with.
const LoginCode = "one-time-code"

const csrfToken = "csrf-token-value"

// consent is the "Log in with Patreon" page a site sends its readers to:
// for a signed-in account, a form to allow or deny it; otherwise a login
// form.
func (s *Site) consent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if !member(r) {
		fmt.Fprint(w, `<html><body><form action="/login" method="POST"><input name="email"><button type="submit">Log in</button></form></body></html>`)
		return
	}
	q := r.URL.Query()
	fmt.Fprintf(w, `<html><body><p>A site would like to view your identity.</p>
<form action="%s/api/oauth2/authorize" method="POST">
<input type="hidden" id="csrf_token" name="CSRFToken[token]" value="%s"/>
<input type="hidden" id="csrf_time" name="CSRFToken[time]" value=""/>
<input type="hidden" value="%s" name="client_id" id="client_id">
<input type="hidden" value="%s" name="redirect_uri" id="redirect_uri">
<input type="hidden" value="code" name="response_type">
<input type="hidden" name="state" value="%s">
<button type="submit" name="confirm" value="allow">Allow</button>
<button type="submit" name="confirm" value="deny">Deny</button>
</form></body></html>`, s.Server.URL, csrfToken, q.Get("client_id"), q.Get("redirect_uri"), q.Get("state"))
}

// allow is the consent form's answer: "allow" from the signed-in account,
// with the form's token, sends the browser back to the site with a code.
func (s *Site) allow(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil || !member(r) || r.PostForm.Get("CSRFToken[token]") != csrfToken || r.PostForm.Get("confirm") != "allow" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	back := r.PostForm.Get("redirect_uri") + "?code=" + LoginCode + "&state=" + r.PostForm.Get("state")
	http.Redirect(w, r, back, http.StatusFound)
}

func (s *Site) currentUser(w http.ResponseWriter, r *http.Request) {
	if !member(r) {
		w.Header().Set("Content-Type", "application/vnd.api+json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"errors":[{"code_name":"LoginRequired","status":"401"}]}`)
		return
	}
	writeAPI(w, obj{"data": obj{"id": "42", "type": "user", "attributes": obj{"full_name": "Sam Reader"}}})
}
