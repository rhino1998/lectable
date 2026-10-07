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
	"sort"
	"strings"
)

// The Internet Archive's Wayback Machine keeps dated copies of web pages.
// Two uses here: a link to one (web.archive.org/web/<time>/<page>) imports
// a work as it stood then, and a chapter the live site no longer serves
// to the public can be read from its last copy that was.

// Wayback reads pages from the Wayback Machine. The zero value works.
type Wayback struct {
	// Client reads archived pages, at its own pace apart from any
	// source's. Index is a second one for the questions a preview asks
	// (what the archive holds of a work), so that looking a link over
	// doesn't queue behind the chapters an import is reading.
	Client Client
	Index  Client
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
func (w *Wayback) At(timestamp string) Fetcher {
	return &waybackFetcher{w: w, client: &w.Client, timestamp: timestamp}
}

// LinkAt is the Wayback Machine link to a page's copy nearest timestamp.
func (w *Wayback) LinkAt(timestamp, pageURL string) string {
	return w.base() + "/web/" + timestamp + "/" + pageURL
}

type waybackFetcher struct {
	w         *Wayback
	client    *Client
	timestamp string
}

func (f *waybackFetcher) Get(ctx context.Context, rawURL string, limit int64) ([]byte, string, string, error) {
	// "id_" asks for the copy as saved, without the archive's banner or
	// its rewriting of the page's links.
	get := func(timestamp string) ([]byte, string, string, error) {
		return f.client.Get(ctx, f.w.base()+"/web/"+timestamp+"id_/"+rawURL, limit)
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
// newest archived copy of its page that read can make a chapter of - the
// last one saved while it was public. read is given a fetcher for each
// copy in turn. ErrLocked if the archive has none that reads.
func (w *Wayback) ArchivedChapter(ctx context.Context, pageURL string, read func(f Fetcher) (*Chapter, error)) (*Chapter, error) {
	times, err := w.Snapshots(ctx, pageURL)
	if err != nil {
		return nil, err
	}
	for _, ts := range times {
		ch, err := read(w.At(ts))
		if err == nil {
			return ch, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, ErrLocked
}

// delistedListSnapshots bounds how many archived copies of a work's page
// Delisted reads for chapter titles, beyond the first spread of them.
const delistedListSnapshots = 8

// Delisted finds the chapters of work its site no longer lists: every
// chapter page the archive has under the work's address (one question to
// its index), less the ones the site still lists. Returns the work's
// whole chapter list - listed and delisted together, in reading order,
// the delisted ones marked.
//
// A chapter's address gives its id but only a rough title, so archived
// copies of the work's own page are read for the real ones: a handful
// spread over its history, then ones from when chapters still untitled
// were captured, up to a limit. What stays untitled keeps the stand-in.
func (w *Wayback) Delisted(ctx context.Context, src Delister, work *Work) ([]ChapterRef, error) {
	q := url.Values{
		"url":       {src.ChapterPrefix(work)},
		"matchType": {"prefix"},
		"output":    {"json"},
		"fl":        {"original,timestamp"},
		"filter":    {"statuscode:200"},
		"collapse":  {"urlkey"},
		"limit":     {"50000"},
	}
	body, _, _, err := w.Index.Get(ctx, w.base()+"/cdx/search/cdx?"+q.Encode(), 32<<20)
	if err != nil {
		return nil, fmt.Errorf("list the archive's copies: %w", err)
	}
	var rows [][]string
	if len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, &rows); err != nil {
			return nil, fmt.Errorf("list the archive's copies: %w", err)
		}
	}
	listed := map[string]ChapterRef{}
	for _, ref := range work.Chapters {
		listed[ref.ID] = ref
	}
	// Every address the archive has each chapter id under, with when it
	// first saved it: usually one, more when the chapter's title changed.
	type saved struct {
		ref   ChapterRef
		first string
	}
	versions := map[string][]saved{}
	var ids []string
	for i, row := range rows {
		if i == 0 || len(row) < 2 { // row 0 is the header
			continue
		}
		u, err := url.Parse(row[0])
		if err != nil {
			continue
		}
		ref, ok := src.ArchivedChapter(work, u)
		if !ok {
			continue
		}
		list, known := versions[ref.ID], false
		for j := range list {
			if list[j].ref.URL == ref.URL {
				list[j].first, known = min(list[j].first, row[1]), true
			}
		}
		if !known {
			if len(list) == 0 {
				ids = append(ids, ref.ID)
			}
			versions[ref.ID] = append(list, saved{ref: ref, first: row[1]})
		}
	}
	found := map[string]*ChapterRef{}
	captured := map[string]string{} // id -> when its page was saved
	// replaced maps a listed chapter's id to the earlier chapter found
	// under it (see ChapterRef.ReplacedBy).
	replaced := map[string]*ChapterRef{}
	var order []string
	for _, id := range ids {
		// The address saved first is the chapter as first published.
		list := versions[id]
		sort.SliceStable(list, func(i, j int) bool { return list[i].first < list[j].first })
		now, isListed := listed[id]
		if !isListed {
			ref := list[0].ref
			ref.Delisted, ref.Unlocked, ref.Archived = true, true, list[0].first
			found[id] = &ref
			captured[id] = list[0].first
			order = append(order, id)
			continue
		}
		// Still listed - but under an address that says something else
		// than what is listed now, the site has put a different thing in
		// the chapter's place, and the chapter itself is only here.
		for _, v := range list {
			if sameAddress(v.ref.URL, now.URL) || alike(v.ref.Title, now.Title) {
				continue
			}
			ref := v.ref
			_, slug, _ := cutLast(ref.URL, "/")
			ref.ID = id + "@" + slug
			ref.Delisted, ref.Unlocked, ref.ReplacedBy, ref.Archived = true, true, now.Title, v.first
			found[ref.ID] = &ref
			captured[ref.ID] = v.first
			replaced[id] = found[ref.ID]
			order = append(order, ref.ID)
			break
		}
	}
	if len(found) > 0 {
		untitled := make(map[string]bool, len(found))
		for id := range found {
			untitled[id] = true
		}
		read := func(timestamp string) {
			refs, err := src.ListedChapters(ctx, &waybackFetcher{w: w, client: &w.Index, timestamp: timestamp}, work)
			if err != nil {
				return
			}
			for _, ref := range refs {
				if f := found[ref.ID]; f != nil && untitled[ref.ID] && ref.Title != "" {
					f.Title = ref.Title
					delete(untitled, ref.ID)
				}
				// A replaced chapter was listed under the same id, by its
				// own title, until it was replaced.
				if f := replaced[ref.ID]; f != nil && untitled[f.ID] && ref.Title != "" && !alike(ref.Title, f.ReplacedBy) {
					f.Title = ref.Title
					delete(untitled, f.ID)
				}
			}
		}
		times, _ := w.Snapshots(ctx, work.URL)
		for _, ts := range times {
			if len(untitled) == 0 || ctx.Err() != nil {
				break
			}
			read(ts)
		}
		for n := 0; n < delistedListSnapshots && len(untitled) > 0 && ctx.Err() == nil; n++ {
			// The page as it was when a still-untitled chapter was saved
			// most likely listed it, and its neighbours.
			var pick string
			for _, id := range order {
				if untitled[id] {
					pick = id
					break
				}
			}
			before := len(untitled)
			read(captured[pick])
			if len(untitled) == before {
				// That copy didn't help: don't ask for it again.
				delete(untitled, pick)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The site's list keeps its own order; each delisted chapter goes in
	// ahead of the first listed one it comes before.
	delisted := make([]ChapterRef, 0, len(order))
	for _, id := range order {
		delisted = append(delisted, *found[id])
	}
	sort.SliceStable(delisted, func(i, j int) bool { return src.Before(delisted[i], delisted[j]) })
	all := make([]ChapterRef, 0, len(work.Chapters)+len(delisted))
	for _, ref := range work.Chapters {
		for len(delisted) > 0 && src.Before(delisted[0], ref) {
			all = append(all, delisted[0])
			delisted = delisted[1:]
		}
		all = append(all, ref)
	}
	all = append(all, delisted...)
	return all, nil
}

// sameAddress compares two page addresses ignoring scheme, "www." and case.
func sameAddress(a, b string) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSuffix(s, "/"))
		s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
		return strings.TrimPrefix(s, "www.")
	}
	return norm(a) == norm(b)
}

// alike reports whether two titles are the same chapter's: at least half
// the words of the shorter are in the other. A corrected typo or a cut-off
// address still matches; "Chapter 37: Knowledge is Pain" and "Book 1
// Summary" don't.
func alike(a, b string) bool {
	words := func(s string) map[string]bool {
		out := map[string]bool{}
		for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9')
		}) {
			out[w] = true
		}
		return out
	}
	x, y := words(a), words(b)
	if len(x) > len(y) {
		x, y = y, x
	}
	if len(x) == 0 {
		return len(y) == 0
	}
	shared := 0
	for w := range x {
		// A word cut short by a truncated address still counts.
		if y[w] {
			shared++
			continue
		}
		for other := range y {
			if len(w) >= 2 && strings.HasPrefix(other, w) {
				shared++
				break
			}
		}
	}
	return shared*2 >= len(x)
}

func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}
