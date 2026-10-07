package websource

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
)

// The Internet Archive's Wayback Machine keeps dated copies of web pages.
// Two uses here: a link to one (web.archive.org/web/<time>/<page>) imports
// a work as it stood then, and a chapter the live site no longer serves
// to the public can be read from its last copy that was.

// Wayback reads pages from the Wayback Machine. The zero value works.
type Wayback struct {
	// Client makes the requests, at its own pace apart from any source's.
	Client Client
	// BaseURL replaces https://web.archive.org (tests).
	BaseURL string
}

const (
	waybackBase = "https://web.archive.org"
	// snapshotTries bounds how many copies of one page ChapterSnapshots
	// offers: each is a request, and a chapter that was never public has
	// none worth finding.
	snapshotTries = 6
)

func (w *Wayback) base() string {
	if w.BaseURL != "" {
		return strings.TrimRight(w.BaseURL, "/")
	}
	return waybackBase
}

var waybackPath = regexp.MustCompile(`^/web/(\d{4,14})[a-z_]*/(.+)$`)

// ParseWaybackLink splits a Wayback Machine link into the time of the
// copy (4 to 14 digits, YYYY[MMDDhhmmss]) and the page's own address.
func ParseWaybackLink(u *url.URL) (timestamp string, original *url.URL, ok bool) {
	host := strings.ToLower(u.Hostname())
	if host != "web.archive.org" && host != "wayback.archive.org" {
		return "", nil, false
	}
	// The original address is the rest of the link as written, query
	// and all.
	rest := u.EscapedPath()
	if u.RawQuery != "" {
		rest += "?" + u.RawQuery
	}
	m := waybackPath.FindStringSubmatch(rest)
	if m == nil {
		return "", nil, false
	}
	raw := m[2]
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	orig, err := url.Parse(raw)
	if err != nil || orig.Hostname() == "" {
		return "", nil, false
	}
	orig.Fragment = u.Fragment
	return m[1], orig, true
}

// At returns a Fetcher that gets every address as the archive's copy
// nearest timestamp: pages come back as they were served then, unaltered,
// and report the address they were saved from, so a source parses them
// as it would its live site's.
func (w *Wayback) At(timestamp string) Fetcher { return &waybackFetcher{w: w, timestamp: timestamp} }

// LinkAt is the Wayback Machine link to a page's copy nearest timestamp.
func (w *Wayback) LinkAt(timestamp, pageURL string) string {
	return w.base() + "/web/" + timestamp + "/" + pageURL
}

type waybackFetcher struct {
	w         *Wayback
	timestamp string
}

func (f *waybackFetcher) Get(ctx context.Context, rawURL string, limit int64) ([]byte, string, string, error) {
	// "id_" asks for the copy as saved, without the archive's banner or
	// its rewriting of the page's links.
	get := func(timestamp string) ([]byte, string, string, error) {
		return f.w.Client.Get(ctx, f.w.base()+"/web/"+timestamp+"id_/"+rawURL, limit)
	}
	body, finalURL, mediaType, err := get(f.timestamp)
	if errors.Is(err, errRedirectLoop) || errors.Is(err, ErrNotFound) {
		// The copy nearest that time can be a saved redirect, or one of a
		// pair of copies the archive bounces between without serving
		// either. Ask its index for the nearest copies that were pages,
		// and take the first that comes back.
		times, _ := f.w.closest(ctx, rawURL, f.timestamp)
		for _, ts := range times {
			if body, finalURL, mediaType, err = get(ts); err == nil || ctx.Err() != nil {
				break
			}
		}
	}
	if err != nil {
		return nil, "", "", err
	}
	// A copy saved compressed comes back compressed, with nothing in the
	// response saying so.
	if len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b {
		if zr, zerr := gzip.NewReader(bytes.NewReader(body)); zerr == nil {
			plain, rerr := io.ReadAll(io.LimitReader(zr, limit+1))
			if rerr != nil {
				return nil, "", "", fmt.Errorf("unpack archived copy: %w", rerr)
			}
			if int64(len(plain)) > limit {
				return nil, "", "", fmt.Errorf("archived copy larger than %d MiB", limit>>20)
			}
			body = plain
		}
	}
	// The archive redirects to the copy it has; report the page's own
	// address, which is what links in it are relative to.
	original := rawURL
	if u, perr := url.Parse(finalURL); perr == nil {
		if m := waybackPath.FindStringSubmatch(u.EscapedPath()); m != nil {
			original = m[2]
			if u.RawQuery != "" {
				original += "?" + u.RawQuery
			}
		}
	}
	return body, original, mediaType, nil
}

// closest returns the times of the archive's good copies of a page
// nearest timestamp, nearest first - a few of them.
func (w *Wayback) closest(ctx context.Context, pageURL, timestamp string) ([]string, error) {
	q := url.Values{
		"url":      {pageURL},
		"output":   {"json"},
		"fl":       {"timestamp"},
		"filter":   {"statuscode:200"},
		"collapse": {"digest"},
		"closest":  {timestamp},
		"sort":     {"closest"},
		"limit":    {"5"},
	}
	body, _, _, err := w.Client.Get(ctx, w.base()+"/cdx/search/cdx?"+q.Encode(), 1<<20)
	if err != nil {
		return nil, err
	}
	var rows [][]string
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, err
		}
	}
	var times []string
	for i, row := range rows {
		if i > 0 && len(row) > 0 { // row 0 is the header
			times = append(times, row[0])
		}
	}
	return times, nil
}

// Snapshots lists the times of the archive's distinct good copies of a
// page, newest first, thinned to at most a handful spread over the page's
// history - enough to step back past a stretch when it wasn't readable.
func (w *Wayback) Snapshots(ctx context.Context, pageURL string) ([]string, error) {
	q := url.Values{
		"url":      {pageURL},
		"output":   {"json"},
		"fl":       {"timestamp"},
		"filter":   {"statuscode:200"},
		"collapse": {"digest"},
		"limit":    {"-200"},
	}
	body, _, _, err := w.Client.Get(ctx, w.base()+"/cdx/search/cdx?"+q.Encode(), 4<<20)
	if err != nil {
		return nil, fmt.Errorf("list archived copies: %w", err)
	}
	var rows [][]string
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, fmt.Errorf("list archived copies: %w", err)
		}
	}
	var all []string
	for i, row := range rows {
		if i == 0 || len(row) == 0 { // the header row
			continue
		}
		all = append(all, row[0])
	}
	// Newest first.
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if len(all) <= snapshotTries {
		return all, nil
	}
	// The newest, then evenly back to the oldest.
	picked := make([]string, 0, snapshotTries)
	for k := range snapshotTries {
		picked = append(picked, all[k*(len(all)-1)/(snapshotTries-1)])
	}
	return picked, nil
}

// ArchivedChapter reads a chapter the live site won't serve from the
// newest archived copy of its page that src can read as a chapter - the
// last one saved while it was public. ErrLocked if the archive has none.
func (w *Wayback) ArchivedChapter(ctx context.Context, src Source, ref ChapterRef) (*Chapter, error) {
	times, err := w.Snapshots(ctx, ref.URL)
	if err != nil {
		return nil, err
	}
	for _, ts := range times {
		ch, err := src.Chapter(ctx, w.At(ts), ref)
		if err == nil {
			return ch, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, ErrLocked
}
