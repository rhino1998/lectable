package wanderinginn

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// The site keeps its newest chapters for the author's patrons: such a
// page has a notice where the text would be, with an "Unlock with
// Patreon" link. That link is Patreon's login-with-Patreon consent page
// for the site; allowing it sends the browser back to the site's
// /patreon-authorization/ with a one-time code, and the site answers with
// cookies of its own (patreon_verified and the like) that say who the
// reader is and what their membership covers. They last a day.
//
// So a locked chapter is unlocked the way a reader's browser would do it,
// with the Patreon session the library already holds (Source.Authorize):
// follow the link, take the cookies, read the page again. A membership
// that doesn't cover the chapter leaves it locked, as it would in a
// browser.

// unlockEvery is how long an unlock stands before another is tried. The
// site's cookies outlast it; a page still locked sooner than this is one
// the membership doesn't cover, not a login that has lapsed.
const unlockEvery = 10 * time.Minute

// browserAgent is how the request that finishes the login identifies
// itself. The site answers that one address differently to anything that
// doesn't look like a browser following a link: a redirect that drops the
// code (observed 2026-10-09).
const browserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// SignOut forgets the site's login, so the next locked chapter is
// unlocked afresh - for when the Patreon account behind it changes.
func (s *Source) SignOut() {
	s.loginMu.Lock()
	s.cookies, s.unlockAt = nil, time.Time{}
	s.loginMu.Unlock()
}

// unlock logs the library in to the site through the "Unlock with
// Patreon" link on a locked chapter's page (fetched from at). It reports
// whether the page is worth reading again: false when there is no way to
// (not signed in to Patreon, no link, an archived copy of the page), or
// the login just made left it locked.
func (s *Source) unlock(ctx context.Context, page []byte, at *url.URL) bool {
	base, err := url.Parse(s.base())
	if s.Authorize == nil || err != nil || at.Host != base.Host {
		return false
	}
	link := unlockLink(page)
	if link == "" {
		return false
	}
	s.unlockMu.Lock()
	defer s.unlockMu.Unlock()
	s.loginMu.Lock()
	recent, had := time.Since(s.unlockAt) < unlockEvery, len(s.cookies) > 0
	s.loginMu.Unlock()
	if recent {
		// Another chapter's unlock, finished while this one waited, may
		// have been after this page was fetched.
		return had && !sentWith(page)
	}
	s.loginMu.Lock()
	s.unlockAt = time.Now()
	s.loginMu.Unlock()

	callback, err := s.Authorize(ctx, link, base.Host)
	if err != nil {
		log.Printf("wanderinginn: unlocking with Patreon: %v", err)
		return false
	}
	cookies, err := s.finishLogin(ctx, callback, base.Host)
	if err != nil || len(cookies) == 0 {
		log.Printf("wanderinginn: unlocking with Patreon: the site set no login (%v)", err)
		return false
	}
	s.loginMu.Lock()
	s.cookies = cookies
	s.loginMu.Unlock()
	log.Printf("wanderinginn: unlocked with Patreon")
	return true
}

// sentWith reports whether a locked page was served to a logged-in
// reader: it then names their access level rather than "none".
func sentWith(page []byte) bool {
	return !bytes.Contains(page, []byte(">none<"))
}

// finishLogin follows the address Patreon sent the login back to, and
// whatever the site redirects to from there, and returns the cookies the
// site set on the way.
func (s *Source) finishLogin(ctx context.Context, callback, host string) (map[string]string, error) {
	s.Fetch()
	client := s.Client.NoRedirect()
	s.loginMu.Lock()
	cookies := map[string]string{}
	for name, value := range s.cookies {
		cookies[name] = value
	}
	s.loginMu.Unlock()
	login := false
	next := callback
	for hop := 0; hop < 5; hop++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", browserAgent)
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "en-US,en;q=0.9")
		req.Header.Set("Referer", "https://www.patreon.com/")
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Upgrade-Insecure-Requests", "1")
		for name, value := range cookies {
			req.AddCookie(&http.Cookie{Name: name, Value: value})
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		for _, c := range resp.Cookies() {
			cookies[c.Name] = c.Value
			login = login || strings.HasPrefix(c.Name, "patreon_")
		}
		to, err := resp.Location()
		if resp.StatusCode < 300 || resp.StatusCode >= 400 || err != nil || !strings.EqualFold(to.Host, host) {
			break
		}
		next = to.String()
	}
	if !login {
		return nil, nil
	}
	return cookies, nil
}

// unlockLink is the "Unlock with Patreon" link of a locked chapter's
// page: the link to Patreon's consent page inside the patrons-only
// notice. "" when the page has none.
func unlockLink(page []byte) string {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return ""
	}
	notice := first(doc, func(n *html.Node) bool { return hasClass(n, "patreon-protected-post") })
	if notice == nil {
		return ""
	}
	link := first(notice, func(n *html.Node) bool {
		if n.Data != "a" {
			return false
		}
		for _, a := range n.Attr {
			if a.Key == "href" && strings.Contains(a.Val, "/oauth2/authorize") {
				return true
			}
		}
		return false
	})
	if link == nil {
		return ""
	}
	for _, a := range link.Attr {
		if a.Key == "href" {
			return a.Val
		}
	}
	return ""
}
