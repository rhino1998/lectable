// Package royalroad reads a fiction from royalroad.com for import by link
// (a websource.Source): its metadata and chapter list from the fiction
// page, and each chapter's text from its own page. parse.go holds
// everything that depends on the site's markup.
package royalroad

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/websource"
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
	content, imgs, err := cleanChapter(page, ref.Title)
	if err != nil {
		return nil, fmt.Errorf("chapter %q: %w", ref.Title, err)
	}
	return websource.RenderChapter(ctx, f, ref, base, content, imgs)
}
