// Package royalroad reads a fiction from royalroad.com: its metadata and
// chapter list from the fiction page, and each chapter's text from its own
// page. It only fetches and cleans up markup; what it returns is laid out
// as an epub (see epub.go) so the ordinary epub parser reads it like any
// other book.
package royalroad

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/html"
)

// Kind is the book-source kind (store.BookSource.Kind) of a fiction
// imported from Royal Road.
const Kind = "royalroad"

// ErrNotFound is a fiction or chapter the site answers 404 for: deleted,
// unpublished, or never there.
var ErrNotFound = errors.New("not found on Royal Road")

const (
	defaultBaseURL = "https://www.royalroad.com"
	// The site asks nothing more of a reader's browser than a page every
	// second or so; a 1,000-chapter serial takes about ten minutes.
	defaultMinInterval = 600 * time.Millisecond
	userAgent          = "Mozilla/5.0 (X11; Linux x86_64) lectable/1.0 (personal epub reader)"

	maxPageSize  = 16 << 20
	maxImageSize = 10 << 20
	// maxChapterImages bounds one chapter's embedded images - a map or a
	// status screenshot, not a gallery.
	maxChapterImages = 24
	maxAttempts      = 4
)

// Client fetches from Royal Road, one request at a time and never faster
// than MinInterval apart. The zero value works.
type Client struct {
	// HTTP makes the requests; nil uses a client that refuses to connect
	// to private addresses (see publicOnly).
	HTTP *http.Client
	// BaseURL replaces https://www.royalroad.com (tests).
	BaseURL string
	// MinInterval is the gap kept between requests; 0 means the default.
	// Negative disables it (tests).
	MinInterval time.Duration

	mu   sync.Mutex // one request at a time
	last time.Time
}

// ChapterRef is one entry of a fiction's chapter list.
type ChapterRef struct {
	// ID is the site's chapter id - stable across retitles and reorders.
	ID    string
	Title string
	URL   string
	// Unlocked is false for a chapter the site lists but won't serve to an
	// anonymous reader (early access).
	Unlocked bool
}

// Fiction is a fiction page's metadata and chapter list.
type Fiction struct {
	ID       string
	URL      string // canonical, with the slug
	Title    string
	Author   string
	CoverURL string
	Chapters []ChapterRef
}

// Image is one image embedded in a chapter.
type Image struct {
	// Name is the file name the chapter's markup refers to it by.
	Name string
	Data []byte
}

// Chapter is one fetched chapter, cleaned up.
type Chapter struct {
	ID    string
	Title string
	// Body is the chapter's markup as an XHTML fragment: the text only,
	// with hidden paragraphs, scripts and presentational attributes gone
	// and each image's src rewritten to its Images name.
	Body   string
	Images []Image
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
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host != "royalroad.com" && host != "royalroadl.com" {
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

// Fiction fetches fictionID's page.
func (c *Client) Fiction(ctx context.Context, fictionID string) (*Fiction, error) {
	pageURL := c.base() + "/fiction/" + fictionID
	body, finalURL, _, err := c.get(ctx, pageURL, maxPageSize)
	if err != nil {
		return nil, fmt.Errorf("fetch fiction page: %w", err)
	}
	f, err := parseFiction(body, finalURL)
	if err != nil {
		return nil, err
	}
	f.ID = fictionID
	return f, nil
}

// Chapter fetches and cleans up one chapter, downloading the images its
// text embeds (best-effort: one that can't be fetched is dropped).
func (c *Client) Chapter(ctx context.Context, ref ChapterRef) (*Chapter, error) {
	page, finalURL, _, err := c.get(ctx, ref.URL, maxPageSize)
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
	ch := &Chapter{ID: ref.ID, Title: ref.Title}
	for _, img := range imgs {
		src := attr(img, "src")
		u, err := base.Parse(src)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || len(ch.Images) >= maxChapterImages {
			remove(img)
			continue
		}
		data, _, mediaType, err := c.get(ctx, u.String(), maxImageSize)
		ext := imageExt(mediaType, data)
		if err != nil || ext == "" {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			remove(img)
			continue
		}
		name := fmt.Sprintf("%s-%d%s", ref.ID, len(ch.Images), ext)
		ch.Images = append(ch.Images, Image{Name: name, Data: data})
		img.Attr = []html.Attribute{{Key: "src", Val: "../img/" + name}, {Key: "alt", Val: attr(img, "alt")}}
	}
	var buf bytes.Buffer
	for n := content.FirstChild; n != nil; n = n.NextSibling {
		if err := html.Render(&buf, n); err != nil {
			return nil, err
		}
	}
	ch.Body = buf.String()
	return ch, nil
}

// Cover fetches a fiction's cover image; ext is its file extension
// (".jpg"), "" if it isn't an image this app stores.
func (c *Client) Cover(ctx context.Context, coverURL string) (data []byte, ext string, err error) {
	data, _, mediaType, err := c.get(ctx, coverURL, maxImageSize)
	if err != nil {
		return nil, "", err
	}
	return data, imageExt(mediaType, data), nil
}

// imageExt is the extension for an image response, by its declared type or
// else its bytes; "" for anything but JPEG, PNG, GIF and WebP.
func imageExt(mediaType string, data []byte) string {
	for _, mt := range []string{mediaType, http.DetectContentType(data)} {
		mt, _, _ = strings.Cut(mt, ";")
		switch strings.TrimSpace(strings.ToLower(mt)) {
		case "image/jpeg", "image/jpg":
			return ".jpg"
		case "image/png":
			return ".png"
		case "image/gif":
			return ".gif"
		case "image/webp":
			return ".webp"
		}
	}
	return ""
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return defaultBaseURL
}

// get fetches one URL, waiting out the request gap first and retrying a
// rate-limit or server error a few times. Returns the body (an error if it
// exceeds limit), the URL after redirects, and the Content-Type.
func (c *Client) get(ctx context.Context, rawURL string, limit int64) (body []byte, finalURL, mediaType string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	gap := c.MinInterval
	if gap == 0 {
		gap = defaultMinInterval
	}
	client := c.HTTP
	if client == nil {
		client = publicOnly
	}
	for attempt := 1; ; attempt++ {
		if wait := gap - time.Since(c.last); wait > 0 {
			if err := sleep(ctx, wait); err != nil {
				return nil, "", "", err
			}
		}
		var retryAfter time.Duration
		body, finalURL, mediaType, retryAfter, err = fetch(ctx, client, rawURL, limit)
		c.last = time.Now()
		if err == nil || retryAfter < 0 || attempt == maxAttempts {
			return body, finalURL, mediaType, err
		}
		if gap > 0 {
			retryAfter = max(retryAfter, time.Duration(attempt)*5*time.Second)
		}
		if err := sleep(ctx, retryAfter); err != nil {
			return nil, "", "", err
		}
	}
}

// fetch makes one request. retryAfter is negative when the failure isn't
// worth retrying, otherwise how long the server asked to wait (0 if it
// didn't say).
func fetch(ctx context.Context, client *http.Client, rawURL string, limit int64) (body []byte, finalURL, mediaType string, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", "", -1, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,image/*;q=0.9,*/*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", "", -1, ctx.Err()
		}
		return nil, "", "", 0, err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, "", "", -1, ErrNotFound
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		secs, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return nil, "", "", time.Duration(min(max(secs, 0), 120)) * time.Second, fmt.Errorf("server answered %s", resp.Status)
	case resp.StatusCode != http.StatusOK:
		return nil, "", "", -1, fmt.Errorf("server answered %s", resp.Status)
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", "", 0, err
	}
	if int64(len(body)) > limit {
		return nil, "", "", -1, fmt.Errorf("response larger than %d MiB", limit>>20)
	}
	return body, resp.Request.URL.String(), resp.Header.Get("Content-Type"), -1, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// publicOnly is the default HTTP client. A chapter's images are fetched
// from whatever address its author wrote, so it refuses to connect to
// loopback, private and link-local addresses: a chapter must not be able
// to make the backend read something off the home network.
var publicOnly = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout: 15 * time.Second,
			Control: func(_, address string, _ syscall.RawConn) error {
				host, _, err := net.SplitHostPort(address)
				if err != nil {
					return err
				}
				ip := net.ParseIP(host)
				if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
					return fmt.Errorf("refusing to connect to non-public address %s", host)
				}
				return nil
			},
		}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
	},
}
