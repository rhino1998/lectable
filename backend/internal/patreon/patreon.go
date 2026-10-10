// Package patreon reads a creator's posts on Patreon as a websource.Source
// - where a web serial's advance chapters are usually posted.
//
// Patreon's documented API (docs.patreon.com) only lets a creator read
// their own campaign's posts; a member's token gets nothing. So this reads
// the JSON API the site's own pages use (www.patreon.com/api/...), which
// is undocumented and changes without notice - everything that depends on
// its shapes is in this package, and `WEBSOURCE_LIVE=<link> go test
// ./internal/websource -run TestLive -v` checks it against the real site.
//
// Without signing in, that API lists every post of a campaign, but gives
// the text only of the public ones: a patrons-only post has a title, a
// date and current_user_can_view false. So a campaign can be checked for
// what it has posted by anyone (those chapters are listed as locked), and
// read by a member - whose browser's session_id cookie is what
// SetSession takes. A cookie that has expired isn't refused: the site
// just answers as it would a stranger.
package patreon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"

	"golang.org/x/net/html"

	"github.com/rhino1998/lectable/backend/internal/websource"
)

// Kind is the book-source kind (store.BookSource.Kind) of a Patreon
// campaign or collection.
const Kind = "patreon"

const defaultBaseURL = "https://www.patreon.com"

// pageSize is how many posts are asked for at a time (the site's own
// pages ask for 20; it gives up to 100).
const pageSize = 100

// maxPages bounds one listing: 20,000 posts.
const maxPages = 200

// Source is patreon.com as a websource.Source. The zero value reads what
// the public can; SetSession signs it in. A work is a campaign (everything
// a creator has posted, oldest first; its id is the campaign's number) or
// one collection of a campaign ("<campaign>/<collection>", in the
// collection's own order) - what a creator with several serials files
// each one's chapters under.
type Source struct {
	// BaseURL replaces https://www.patreon.com (tests).
	BaseURL string

	once    sync.Once
	client  websource.Client
	mu      sync.RWMutex
	session string
}

func (s *Source) Kind() string { return Kind }
func (s *Source) Name() string { return "Patreon" }

func (s *Source) base() string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	return defaultBaseURL
}

// Fetch is the client for the live site: it sends the session cookie, if
// one is set, with requests to the site itself and nowhere else (a
// chapter's images come from other hosts).
func (s *Source) Fetch() websource.Fetcher {
	s.once.Do(func() {
		s.client.Prepare = func(req *http.Request) {
			req.Header.Set("Accept", "application/vnd.api+json, application/json;q=0.9, */*;q=0.5")
			base, err := url.Parse(s.base())
			if err != nil || req.URL.Host != base.Host {
				return
			}
			if session := s.Session(); session != "" {
				req.AddCookie(&http.Cookie{Name: "session_id", Value: session})
			}
		}
	})
	return &s.client
}

// SetSession sets the session_id cookie of a signed-in browser, which is
// what lets the posts that account can read be read here; "" signs out.
func (s *Source) SetSession(session string) {
	s.mu.Lock()
	s.session = strings.TrimSpace(session)
	s.mu.Unlock()
}

// Session is the cookie SetSession was given.
func (s *Source) Session() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.session
}

func (s *Source) Match(u *url.URL) bool {
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if s.BaseURL != "" {
		if base, err := url.Parse(s.BaseURL); err == nil && u.Host == base.Host {
			return true
		}
	}
	return host == "patreon.com"
}

var (
	postPath       = regexp.MustCompile(`/posts/(?:[^/]*-)?(\d+)/?$`)
	collectionPath = regexp.MustCompile(`^/collection/(\d+)/?$`)
	// creatorPath is a creator's page: /name, /c/name, /cw/name, and any
	// tab of it (/name/posts).
	creatorPath = regexp.MustCompile(`^/(?:c/|cw/)?([A-Za-z0-9_.-]+)(?:/(?:posts|home|about|collections|membership|shop|chats|recommendations)?)?/?$`)
)

// notCreators are first path segments that aren't a creator's name.
var notCreators = map[string]bool{
	"posts": true, "collection": true, "api": true, "home": true, "login": true, "signup": true, "search": true,
	"explore": true, "settings": true, "messages": true, "notifications": true, "policy": true, "user": true,
	"c": true, "cw": true, "join": true, "checkout": true, "bePatron": true, "about": true, "pricing": true,
}

// Resolve names the work a link points at: a collection's link is that
// collection, and a creator's page or one of their posts is everything
// they have posted.
func (s *Source) Resolve(ctx context.Context, f websource.Fetcher, u *url.URL) ([]string, error) {
	if m := collectionPath.FindStringSubmatch(u.Path); m != nil {
		var doc document[collectionAttrs]
		if err := s.get(ctx, f, "/api/collection/"+m[1]+"?include=campaign&fields[collection]=title&fields[campaign]=name", &doc); err != nil {
			return nil, fmt.Errorf("look up the collection: %w", err)
		}
		campaign := doc.Data.Relationships["campaign"].Data.ID
		if campaign == "" {
			return nil, errors.New("the site didn't say whose collection that is")
		}
		return []string{campaign + "/" + m[1]}, nil
	}
	if m := postPath.FindStringSubmatch(u.Path); m != nil {
		var doc document[postAttrs]
		if err := s.get(ctx, f, "/api/posts/"+m[1]+"?include=campaign&fields[post]=title&fields[campaign]=name", &doc); err != nil {
			return nil, fmt.Errorf("look up the post: %w", err)
		}
		campaign := doc.Data.Relationships["campaign"].Data.ID
		if campaign == "" {
			return nil, errors.New("the site didn't say whose post that is")
		}
		return []string{campaign}, nil
	}
	if u.Path == "/user" && u.Query().Get("u") != "" {
		return nil, errors.New("that link names a Patreon account, not a creator's page - use the link to the page, or to one of its posts")
	}
	m := creatorPath.FindStringSubmatch(u.Path)
	if m == nil || (notCreators[m[1]] && !strings.HasPrefix(u.Path, "/c/") && !strings.HasPrefix(u.Path, "/cw/")) {
		return nil, errors.New("that isn't a link to a Patreon creator, collection or post")
	}
	id, err := s.campaignByName(ctx, f, m[1])
	if err != nil {
		return nil, err
	}
	return []string{id}, nil
}

// campaignByName finds a creator's campaign by the name in their page's
// address, through the site's search - the page itself only says in
// markup that changes every few months.
func (s *Source) campaignByName(ctx context.Context, f websource.Fetcher, name string) (string, error) {
	var found struct {
		Data []struct {
			ID         string `json:"id"`
			Type       string `json:"type"`
			Attributes struct {
				URL string `json:"url"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := s.get(ctx, f, "/api/search?q="+url.QueryEscape(name)+"&page[size]=10", &found); err != nil {
		return "", fmt.Errorf("look up the creator: %w", err)
	}
	for _, d := range found.Data {
		if d.Type != "campaign-document" {
			continue
		}
		at, err := url.Parse(d.Attributes.URL)
		if err != nil || !strings.EqualFold(strings.Trim(at.Path, "/"), name) {
			continue
		}
		if id := strings.TrimPrefix(d.ID, "campaign_"); id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("no creator at /%s in the site's search - try a link to one of their posts: %w", name, websource.ErrNotFound)
}

// Work lists a campaign's posts (or one collection's) as chapters. Posts
// this reader can't open are listed too, not Unlocked. Polls are left out.
func (s *Source) Work(ctx context.Context, f websource.Fetcher, workID string) (*websource.Work, error) {
	campaignID, collectionID, _ := strings.Cut(workID, "/")
	var campaign document[campaignAttrs]
	if err := s.get(ctx, f, "/api/campaigns/"+campaignID+"?include=creator&fields[campaign]=name,url,avatar_photo_url&fields[user]=full_name", &campaign); err != nil {
		return nil, fmt.Errorf("fetch the creator's page: %w", err)
	}
	work := &websource.Work{
		ID:       workID,
		URL:      campaign.Data.Attributes.URL,
		Title:    campaign.Data.Attributes.Name,
		Author:   campaign.Data.Attributes.Name,
		CoverURL: campaign.Data.Attributes.AvatarPhotoURL,
	}
	for _, inc := range campaign.Included {
		if inc.Type == "user" && inc.Attributes.FullName != "" {
			work.Author = inc.Attributes.FullName
		}
	}
	if work.URL == "" {
		work.URL = s.base() + "/user?u=" + campaignID
	}
	query := "/api/posts?fields[post]=title,published_at,current_user_can_view,post_type,url" +
		"&filter[campaign_id]=" + campaignID + "&filter[is_draft]=false&filter[contains_exclusive_posts]=true" +
		"&sort=published_at&page[count]=" + fmt.Sprint(pageSize)
	if collectionID != "" {
		var collection document[collectionAttrs]
		if err := s.get(ctx, f, "/api/collection/"+collectionID+"?fields[collection]=title", &collection); err != nil {
			return nil, fmt.Errorf("fetch the collection: %w", err)
		}
		if t := collection.Data.Attributes.Title; t != "" {
			work.Title = t
		}
		work.URL = s.base() + "/collection/" + collectionID
		query = "/api/posts?fields[post]=title,published_at,current_user_can_view,post_type,url" +
			"&filter[campaign_id]=" + campaignID + "&filter[collection_id]=" + collectionID +
			"&filter[is_draft]=false&filter[contains_exclusive_posts]=true&filter[include_drops]=true" +
			"&sort=collection_order&page[count]=" + fmt.Sprint(pageSize)
	}
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < maxPages; page++ {
		var posts listing
		at := query
		if cursor != "" {
			at += "&page[cursor]=" + url.QueryEscape(cursor)
		}
		if err := s.get(ctx, f, at, &posts); err != nil {
			return nil, fmt.Errorf("fetch the list of posts: %w", err)
		}
		for _, p := range posts.Data {
			if p.ID == "" || seen[p.ID] || p.Attributes.PostType == "poll" {
				continue
			}
			seen[p.ID] = true
			title := strings.TrimSpace(p.Attributes.Title)
			if title == "" {
				title = "Untitled post"
			}
			link := p.Attributes.URL
			if link == "" {
				link = s.base() + "/posts/" + p.ID
			}
			work.Chapters = append(work.Chapters, websource.ChapterRef{ID: p.ID, Title: title, URL: link, Unlocked: p.Attributes.CanView})
		}
		cursor = posts.Meta.Pagination.Cursors.Next
		if cursor == "" || len(posts.Data) == 0 {
			break
		}
	}
	return work, nil
}

// Chapter fetches one post's text. ErrLocked for one this reader can't
// open: not a member, or not of a tier that includes it.
func (s *Source) Chapter(ctx context.Context, f websource.Fetcher, ref websource.ChapterRef) (*websource.Chapter, error) {
	if !ref.Unlocked {
		return nil, websource.ErrLocked
	}
	var doc document[postAttrs]
	if err := s.get(ctx, f, "/api/posts/"+ref.ID+"?fields[post]=title,content,content_json_string,current_user_can_view,post_type,url", &doc); err != nil {
		return nil, fmt.Errorf("fetch post %q: %w", ref.Title, err)
	}
	post := doc.Data.Attributes
	if !post.CanView {
		return nil, websource.ErrLocked
	}
	// Posts written in the site's current editor carry their text as a
	// document tree; older ones as HTML.
	markup := post.Content
	if post.ContentJSON != "" {
		converted, err := tiptapHTML(post.ContentJSON)
		if err != nil {
			return nil, fmt.Errorf("post %q: %w", ref.Title, err)
		}
		markup = converted
	}
	if strings.TrimSpace(markup) == "" {
		return nil, fmt.Errorf("post %q has no text (it may be only a link, a file or a video)", ref.Title)
	}
	content, images, err := parseBody(markup)
	if err != nil {
		return nil, fmt.Errorf("post %q: %w", ref.Title, err)
	}
	base, err := url.Parse(s.base())
	if err != nil {
		return nil, err
	}
	return websource.RenderChapter(ctx, f, ref, base, content, images)
}

// Account is who the session cookie signs in as: their name, and "" with
// no error when the site doesn't know the session (expired, or mistyped).
func (s *Source) Account(ctx context.Context) (string, error) {
	var doc document[userAttrs]
	err := s.get(ctx, s.Fetch(), "/api/current_user?fields[user]=full_name", &doc)
	if err != nil {
		// Signed out is 401, which the client reports as an error.
		if strings.Contains(err.Error(), "401") {
			return "", nil
		}
		return "", err
	}
	name := doc.Data.Attributes.FullName
	if name == "" && doc.Data.ID != "" {
		name = "account " + doc.Data.ID
	}
	return name, nil
}

// ErrSignedOut is an exchange that needs the library signed in to Patreon
// (SetSession) when it isn't, or its session has lapsed.
var ErrSignedOut = errors.New("the library isn't signed in to Patreon")

// Authorize answers a site's "Log in with Patreon" as the signed-in
// account: authorizeURL is that site's link to Patreon's consent page,
// and the answer is where Patreon sends the browser back to - the site's
// own address, carrying the one-time code the site finishes its login
// with. It says "Allow" on the consent page, as clicking the button
// would. redirectHost is the host that link must lead back to: a link
// that would hand the code to anywhere else is refused.
func (s *Source) Authorize(ctx context.Context, authorizeURL, redirectHost string) (string, error) {
	session := s.Session()
	if session == "" {
		return "", ErrSignedOut
	}
	u, err := url.Parse(authorizeURL)
	base, _ := url.Parse(s.base())
	if err != nil || base == nil || !strings.EqualFold(u.Host, base.Host) || u.Path != "/oauth2/authorize" {
		return "", errors.New("not a link to Patreon's consent page")
	}
	if back, err := url.Parse(u.Query().Get("redirect_uri")); err != nil || !strings.EqualFold(back.Host, redirectHost) {
		return "", fmt.Errorf("the consent page would send the login to %q, not %s", u.Query().Get("redirect_uri"), redirectHost)
	}
	s.Fetch()
	client := s.client.NoRedirect()
	do := func(method, target string, form url.Values) (*http.Response, []byte, error) {
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, method, target, body)
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("User-Agent", websource.UserAgent)
		req.AddCookie(&http.Cookie{Name: "session_id", Value: session})
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Origin", base.Scheme+"://"+base.Host)
			req.Header.Set("Referer", authorizeURL)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		page, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return resp, page, err
	}
	sentBack := func(resp *http.Response) (string, error) {
		to, err := resp.Location()
		if err != nil || !strings.EqualFold(to.Host, redirectHost) {
			return "", fmt.Errorf("the consent page answered %s without sending the login back to %s", resp.Status, redirectHost)
		}
		return to.String(), nil
	}
	resp, page, err := do(http.MethodGet, authorizeURL, nil)
	if err != nil {
		return "", fmt.Errorf("open the consent page: %w", err)
	}
	// An account that has allowed the site before may be sent straight back.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return sentBack(resp)
	}
	action, form := consentForm(page)
	if action == "" {
		// Signed out, the page is a login form instead.
		return "", fmt.Errorf("%w (its session has lapsed - sign in again on the Settings page)", ErrSignedOut)
	}
	to, err := u.Parse(action)
	if err != nil || !strings.EqualFold(to.Host, base.Host) {
		return "", errors.New("the consent page's form posts somewhere other than Patreon")
	}
	form.Set("confirm", "allow")
	resp, _, err = do(http.MethodPost, to.String(), form)
	if err != nil {
		return "", fmt.Errorf("answer the consent page: %w", err)
	}
	return sentBack(resp)
}

// consentForm finds the consent page's form - the one with the "confirm"
// buttons - and its hidden fields; "" when the page has none.
func consentForm(page []byte) (action string, fields url.Values) {
	doc, err := html.Parse(strings.NewReader(string(page)))
	if err != nil {
		return "", nil
	}
	attr := func(n *html.Node, key string) string {
		for _, a := range n.Attr {
			if a.Key == key {
				return a.Val
			}
		}
		return ""
	}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if action != "" {
			return
		}
		if n.Type == html.ElementNode && n.Data == "form" {
			got, confirm := url.Values{}, false
			var inside func(n *html.Node)
			inside = func(n *html.Node) {
				if n.Type == html.ElementNode {
					switch {
					case n.Data == "input" && attr(n, "name") != "":
						got.Set(attr(n, "name"), attr(n, "value"))
					case n.Data == "button" && attr(n, "name") == "confirm":
						confirm = true
					}
				}
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					inside(c)
				}
			}
			inside(n)
			if confirm && attr(n, "action") != "" {
				action, fields = attr(n, "action"), got
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return action, fields
}

// get reads one of the site's JSON API answers.
func (s *Source) get(ctx context.Context, f websource.Fetcher, pathAndQuery string, into any) error {
	body, _, _, err := f.Get(ctx, s.base()+pathAndQuery+"&json-api-version=1.0&json-api-use-default-includes=false", websource.MaxPageSize)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("the answer isn't the JSON its API gives (%w) - the API may have changed", err)
	}
	return nil
}

// The JSON:API shapes read here: only the fields asked for.

type related struct {
	Data struct {
		ID string `json:"id"`
	} `json:"data"`
}

type resource[A any] struct {
	ID            string             `json:"id"`
	Type          string             `json:"type"`
	Attributes    A                  `json:"attributes"`
	Relationships map[string]related `json:"relationships"`
}

type document[A any] struct {
	Data     resource[A]           `json:"data"`
	Included []resource[userAttrs] `json:"included"`
}

type listing struct {
	Data []resource[postAttrs] `json:"data"`
	Meta struct {
		Pagination struct {
			Cursors struct {
				Next string `json:"next"`
			} `json:"cursors"`
		} `json:"pagination"`
	} `json:"meta"`
}

type postAttrs struct {
	Title       string `json:"title"`
	Content     string `json:"content"`
	ContentJSON string `json:"content_json_string"`
	CanView     bool   `json:"current_user_can_view"`
	PostType    string `json:"post_type"`
	URL         string `json:"url"`
}

type campaignAttrs struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	AvatarPhotoURL string `json:"avatar_photo_url"`
}

type collectionAttrs struct {
	Title string `json:"title"`
}

type userAttrs struct {
	FullName string `json:"full_name"`
}

// parseBody parses a post's markup into a container element and the
// images in it.
func parseBody(markup string) (content *html.Node, images []*html.Node, err error) {
	doc, err := html.Parse(strings.NewReader("<html><body><div>" + markup + "</div></body></html>"))
	if err != nil {
		return nil, nil, err
	}
	var drop []*html.Node
	var find func(n *html.Node)
	find = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "div":
				if content == nil {
					content = n
				}
			case "img":
				images = append(images, n)
			case "script", "style", "iframe":
				drop = append(drop, n)
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			find(c)
		}
	}
	find(doc)
	for _, n := range drop {
		n.Parent.RemoveChild(n)
	}
	if content == nil {
		return nil, nil, errors.New("no text found")
	}
	return content, images, nil
}
