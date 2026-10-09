// Package royalroad reads a fiction from royalroad.com for import by link
// (a websource.Source): its metadata and chapter list from the fiction
// page, and each chapter's text from its own page. parse.go holds
// everything that depends on the site's markup.
package royalroad

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/websource"
	"golang.org/x/net/html"
)

// Kind is the book-source kind (store.BookSource.Kind) of a fiction
// imported from Royal Road.
const Kind = "royalroad"

const defaultBaseURL = "https://www.royalroad.com"

// Fiction is a fiction page's metadata and chapter list.
type Fiction = websource.Work

// ChapterRef is one entry of a fiction's chapter list.
type ChapterRef = websource.ChapterRef

// Source is royalroad.com as a websource.Source. The zero value works; a
// work's id is the fiction's number.
type Source struct {
	// Client fetches the live site.
	Client websource.Client
	// BaseURL replaces https://www.royalroad.com (tests).
	BaseURL string
}

func (s *Source) Fetch() websource.Fetcher { return &s.Client }

func (s *Source) Kind() string { return Kind }
func (s *Source) Name() string { return "Royal Road" }

func (s *Source) Match(u *url.URL) bool {
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	return host == "royalroad.com" || host == "royalroadl.com"
}

// Resolve returns the fiction a link points at - the fiction's own page or
// any of its chapters.
func (s *Source) Resolve(_ context.Context, _ websource.Fetcher, u *url.URL) ([]string, error) {
	id, err := ParseFictionURL(u.String())
	if err != nil {
		return nil, err
	}
	return []string{id}, nil
}

// ParseFictionURL returns the fiction id a Royal Road link points at - the
// fiction's own page or any of its chapters. A bare id is accepted too.
func ParseFictionURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("no URL given")
	}
	if _, err := strconv.ParseUint(raw, 10, 63); err == nil {
		return raw, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("not a valid URL")
	}
	if !(&Source{}).Match(u) {
		return "", errors.New("not a Royal Road link")
	}
	// /fiction/<id>[/<slug>[/chapter/...]]
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "fiction" {
		return "", errors.New("not a link to a Royal Road fiction (expected royalroad.com/fiction/<number>/...)")
	}
	if _, err := strconv.ParseUint(parts[1], 10, 63); err != nil {
		return "", errors.New("not a link to a Royal Road fiction (expected royalroad.com/fiction/<number>/...)")
	}
	return parts[1], nil
}

// Work fetches fictionID's page.
func (s *Source) Work(ctx context.Context, f websource.Fetcher, fictionID string) (*websource.Work, error) {
	base := defaultBaseURL
	if s.BaseURL != "" {
		base = strings.TrimRight(s.BaseURL, "/")
	}
	body, finalURL, _, err := f.Get(ctx, base+"/fiction/"+fictionID, websource.MaxPageSize)
	if err != nil {
		return nil, fmt.Errorf("fetch fiction page: %w", err)
	}
	fic, err := parseFiction(body, finalURL)
	if err != nil {
		return nil, err
	}
	fic.ID = fictionID
	return fic, nil
}

// Chapter fetches and cleans up one chapter, downloading the images its
// text embeds.
func (s *Source) Chapter(ctx context.Context, f websource.Fetcher, ref ChapterRef) (*websource.Chapter, error) {
	if !ref.Unlocked {
		return nil, websource.ErrLocked
	}
	page, finalURL, _, err := f.Get(ctx, ref.URL, websource.MaxPageSize)
	if err != nil {
		return nil, fmt.Errorf("fetch chapter %q: %w", ref.Title, err)
	}
	base, err := url.Parse(finalURL)
	if err != nil {
		return nil, err
	}
	// A chapter found in an archive is known by a title made from its
	// address; its page has the real one.
	if ref.Delisted {
		if t := pageHeading(page); t != "" && sameTitle(ref.Title, t) {
			ref.Title = t
		}
	}
	content, imgs, err := cleanChapter(page, ref.Title)
	if err != nil {
		return nil, fmt.Errorf("chapter %q: %w", ref.Title, err)
	}
	return websource.RenderChapter(ctx, f, ref, base, content, imgs)
}

// Royal Road takes chapters off a fiction's list when the author "stubs"
// it - its early volumes published elsewhere - so the fiction's page then
// lists only what is left. websource.Delister: how to find the rest in an
// archive. A chapter's page is /fiction/<id>/<slug>/chapter/<chapter id>/
// <chapter slug>, and chapter ids are handed out in order of writing.

// The slug is lowercase words and hyphens; the archive also holds mangled
// addresses (".../chapter/1764028/0, 0, 0, .5"), which aren't a chapter's.
var chapterPagePath = regexp.MustCompile(`^/fiction/(\d+)/[^/]+/chapter/(\d+)/([a-z0-9][a-z0-9-]*)/?$`)

func (s *Source) ChapterPrefix(work *websource.Work) string {
	return "royalroad.com/fiction/" + work.ID + "/"
}

func (s *Source) ArchivedChapter(work *websource.Work, pageURL *url.URL) (websource.ChapterRef, bool) {
	m := chapterPagePath.FindStringSubmatch(pageURL.Path)
	if m == nil || m[1] != work.ID {
		return websource.ChapterRef{}, false
	}
	// As the site would link it, whatever the archive saved it under
	// (a comment's address, the old domain).
	clean := url.URL{Scheme: "https", Host: "www.royalroad.com", Path: strings.TrimSuffix(pageURL.Path, "/")}
	return websource.ChapterRef{ID: m[2], Title: titleFromSlug(m[3]), URL: clean.String()}, true
}

// titleFromSlug makes a stand-in title of a chapter's address slug:
// "chapter-670-sadness-porridge" reads "Chapter 670 Sadness Porridge".
func titleFromSlug(slug string) string {
	words := strings.FieldsFunc(slug, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	if len(words) == 0 {
		return "Untitled chapter"
	}
	return strings.Join(words, " ")
}

func (s *Source) ListedChapters(ctx context.Context, f websource.Fetcher, work *websource.Work) ([]websource.ChapterRef, error) {
	body, finalURL, _, err := f.Get(ctx, work.URL, websource.MaxPageSize)
	if err != nil {
		return nil, err
	}
	fic, err := parseFiction(body, finalURL)
	if err != nil {
		return nil, err
	}
	return fic.Chapters, nil
}

// Before orders chapters by id. Two kinds of entry share a real chapter's
// id: an earlier chapter the site has since replaced ("<id>@<slug>"),
// which comes first - it is where the story was - and a chapter known only
// to be missing after it ("<id>+<n>"), which comes after, in order of n.
func (s *Source) Before(a, b websource.ChapterRef) bool {
	rank := func(id string) (base uint64, rank int, ok bool) {
		plain := id
		if i := strings.IndexAny(id, "@+"); i >= 0 {
			plain = id[:i]
			rank = -1
			if id[i] == '+' {
				n, _ := strconv.Atoi(id[i+1:])
				rank = max(n, 1)
			}
		}
		base, err := strconv.ParseUint(plain, 10, 64)
		return base, rank, err == nil
	}
	x, xr, okA := rank(a.ID)
	y, yr, okB := rank(b.ID)
	if !okA || !okB {
		return a.ID < b.ID
	}
	if x != y {
		return x < y
	}
	return xr < yr
}

// pageHeading returns a chapter page's first <h1>, which is the chapter's
// title (the page's second is the author's name).
func pageHeading(page []byte) string {
	z := html.NewTokenizer(bytes.NewReader(page))
	var text strings.Builder
	in := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken:
			if name, _ := z.TagName(); string(name) == "h1" {
				in = true
			}
		case html.TextToken:
			if in {
				text.Write(z.Text())
			}
		case html.EndTagToken:
			if name, _ := z.TagName(); in && string(name) == "h1" {
				return strings.Join(strings.Fields(html.UnescapeString(text.String())), " ")
			}
		}
	}
}

// sameTitle reports whether heading is the full form of a title made from
// an address slug: the same letters and digits, the slug's possibly cut
// short. A slug of a letter or less ("x") says nothing, and any heading
// stands for it.
func sameTitle(fromSlug, heading string) bool {
	key := func(s string) string {
		return strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, strings.ToLower(s))
	}
	a, b := key(fromSlug), key(heading)
	return len(a) <= 1 || strings.HasPrefix(b, a)
}
