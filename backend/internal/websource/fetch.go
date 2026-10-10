package websource

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

const (
	// A reader's browser asks for a page every second or so and no site
	// minds; a 1,000-chapter serial takes about ten minutes.
	defaultMinInterval = 600 * time.Millisecond
	userAgent          = "Mozilla/5.0 (X11; Linux x86_64) lectable/1.0 (personal epub reader)"

	// MaxPageSize bounds an HTML page passed to Client.Get.
	MaxPageSize  = 16 << 20
	maxImageSize = 10 << 20
	// maxChapterImages bounds one chapter's embedded images - a map or a
	// status screenshot, not a gallery.
	maxChapterImages = 24
	maxAttempts      = 4
)

// errRedirectLoop is a page that only ever redirects.
var errRedirectLoop = errors.New("the page redirects in a loop")

// Client is the Fetcher for the live web: one request at a time (unless
// Parallel says more) and never started faster than MinInterval apart.
// The zero value works.
type Client struct {
	// HTTP makes the requests; nil uses a client that refuses to connect
	// to private addresses (see publicOnly).
	HTTP *http.Client
	// MinInterval is the gap kept between requests; 0 means the default.
	// Negative disables it (tests).
	MinInterval time.Duration

	// Parallel is how many requests may be in flight at once; 0 means
	// one. They still start MinInterval apart.
	Parallel int
	// Prepare, if set, is given each request before it is sent - for a
	// site that wants a header of its own (a session cookie).
	Prepare func(*http.Request)

	mu    sync.Mutex
	slots chan struct{} // one token per request in flight
	last  time.Time     // when the latest request started, or will
}

// UserAgent is what a Client calls itself.
const UserAgent = userAgent

// NoRedirect is the HTTP client c makes its requests with, set not to
// follow redirects - for the few exchanges that aren't "get a page", like
// a sign-in handshake whose every hop sets cookies. Requests made with it
// aren't paced or retried.
func (c *Client) NoRedirect() *http.Client {
	base := c.HTTP
	if base == nil {
		base = publicOnly
	}
	plain := *base
	plain.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &plain
}

// RenderChapter turns a chapter's cleaned content element into a Chapter:
// it downloads (through f) each of images (the <img> elements left in content; base
// resolves a relative src), rewrites it to point at the downloaded copy,
// and renders content's children. Best-effort per image: one that can't
// be fetched, or isn't an image this app stores, is removed.
func RenderChapter(ctx context.Context, f Fetcher, ref ChapterRef, base *url.URL, content *html.Node, images []*html.Node) (*Chapter, error) {
	ch := &Chapter{ID: ref.ID, Title: ref.Title}
	key := FileKey(ref.ID)
	for _, img := range images {
		var src, alt string
		for _, a := range img.Attr {
			switch a.Key {
			case "src":
				src = a.Val
			case "alt":
				alt = a.Val
			}
		}
		u, err := base.Parse(src)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || len(ch.Images) >= maxChapterImages {
			detach(img)
			continue
		}
		data, _, mediaType, err := f.Get(ctx, u.String(), maxImageSize)
		ext := imageExt(mediaType, data)
		if err != nil || ext == "" {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			detach(img)
			continue
		}
		name := fmt.Sprintf("%s-%d%s", key, len(ch.Images), ext)
		ch.Images = append(ch.Images, Image{Name: name, Data: data})
		img.Attr = []html.Attribute{{Key: "src", Val: "../img/" + name}, {Key: "alt", Val: alt}}
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

func detach(n *html.Node) {
	if n.Parent != nil {
		n.Parent.RemoveChild(n)
	}
}

// Cover fetches a work's cover image; ext is its file extension
// (".jpg"), "" if it isn't an image this app stores.
func Cover(ctx context.Context, f Fetcher, coverURL string) (data []byte, ext string, err error) {
	data, _, mediaType, err := f.Get(ctx, coverURL, maxImageSize)
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

// Get fetches one URL, waiting out the request gap first and retrying a
// rate-limit or server error a few times. Returns the body (an error if it
// exceeds limit), the URL after redirects, and the Content-Type.
func (c *Client) Get(ctx context.Context, rawURL string, limit int64) (body []byte, finalURL, mediaType string, err error) {
	// At most Parallel requests at once.
	c.mu.Lock()
	if c.slots == nil {
		c.slots = make(chan struct{}, max(c.Parallel, 1))
	}
	slots := c.slots
	c.mu.Unlock()
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, "", "", ctx.Err()
	}

	gap := c.MinInterval
	if gap == 0 {
		gap = defaultMinInterval
	}
	client := c.HTTP
	if client == nil {
		client = publicOnly
	}
	for attempt := 1; ; attempt++ {
		// Requests start no closer together than the gap, however many
		// are in flight: each books the next free moment.
		c.mu.Lock()
		start := time.Now()
		if next := c.last.Add(gap); next.After(start) {
			start = next
		}
		c.last = start
		c.mu.Unlock()
		if err := sleep(ctx, time.Until(start)); err != nil {
			return nil, "", "", err
		}
		var retryAfter time.Duration
		body, finalURL, mediaType, retryAfter, err = fetch(ctx, client, rawURL, limit, c.Prepare)
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
func fetch(ctx context.Context, client *http.Client, rawURL string, limit int64, prepare func(*http.Request)) (body []byte, finalURL, mediaType string, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", "", -1, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,image/*;q=0.9,*/*;q=0.5")
	if prepare != nil {
		prepare(req)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", "", -1, ctx.Err()
		}
		// A redirect loop is the same every time.
		if strings.Contains(err.Error(), "redirects") {
			return nil, "", "", -1, errRedirectLoop
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
