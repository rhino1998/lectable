// Package wanderinginn reads The Wandering Inn (wanderinginn.com) for
// import by link (a websource.Source). The serial is one site-wide story
// of a dozen million words, split by its table of contents into volumes;
// each volume is imported as its own book in the series "The Wandering
// Inn", so a work's id is a volume's ("vol-3"). parse.go holds everything
// that depends on the site's markup.
package wanderinginn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rhino1998/lectable/backend/internal/websource"
)

// Kind is the book-source kind (store.BookSource.Kind) of a volume
// imported from The Wandering Inn.
const Kind = "wanderinginn"

const (
	defaultBaseURL = "https://wanderinginn.com"
	tocPath        = "/table-of-contents/"
	seriesName     = "The Wandering Inn"
	author         = "pirateaba"
	// tocTTL is how long a fetched table of contents is reused: an import
	// resolves the link and then reads the volume, one page fetch for both.
	tocTTL = time.Minute
)

// Source is wanderinginn.com as a websource.Source. The zero value works.
type Source struct {
	// Client fetches the live site.
	Client websource.Client
	// BaseURL replaces https://wanderinginn.com (tests).
	BaseURL string
	// Authorize, if set, answers Patreon's consent page for the site's
	// "Unlock with Patreon" as the account the library is signed in to
	// Patreon as (patreon.Source.Authorize), which is what lets chapters
	// for patrons only be read - see unlock.go.
	Authorize func(ctx context.Context, authorizeURL, redirectHost string) (callbackURL string, err error)

	once     sync.Once
	unlockMu sync.Mutex // one unlock at a time
	// cookies are the site's own login, from the last unlock, and
	// unlockAt when that was tried; under loginMu (mu is held across
	// fetches of the table of contents).
	loginMu  sync.Mutex
	cookies  map[string]string
	unlockAt time.Time

	mu       sync.Mutex
	toc      []volume
	tocAt    time.Time
	tocClock func() time.Time // tests; nil means time.Now
}

// Fetch is the client for the live site. It sends the site's login
// cookies, once an unlock has got them, to the site itself and nowhere
// else.
func (s *Source) Fetch() websource.Fetcher {
	s.once.Do(func() {
		s.Client.Prepare = func(req *http.Request) {
			if base, err := url.Parse(s.base()); err != nil || req.URL.Host != base.Host {
				return
			}
			s.loginMu.Lock()
			defer s.loginMu.Unlock()
			for name, value := range s.cookies {
				req.AddCookie(&http.Cookie{Name: name, Value: value})
			}
		}
	})
	return &s.Client
}

func (s *Source) Kind() string { return Kind }
func (s *Source) Name() string { return seriesName }

func (s *Source) Match(u *url.URL) bool {
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") == "wanderinginn.com"
}

func (s *Source) base() string {
	if s.BaseURL != "" {
		return strings.TrimRight(s.BaseURL, "/")
	}
	return defaultBaseURL
}

// volumes returns the table of contents as f serves it. The live site's
// is reused for a minute; an archive's is fetched each time.
func (s *Source) volumes(ctx context.Context, f websource.Fetcher) ([]volume, error) {
	live := f == websource.Fetcher(&s.Client)
	now := time.Now
	if s.tocClock != nil {
		now = s.tocClock
	}
	if live {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.toc != nil && now().Sub(s.tocAt) < tocTTL {
			return s.toc, nil
		}
	}
	page, finalURL, _, err := f.Get(ctx, s.base()+tocPath, websource.MaxPageSize)
	if err != nil {
		return nil, fmt.Errorf("fetch table of contents: %w", err)
	}
	vols, err := parseTOC(page, finalURL)
	if err != nil {
		return nil, err
	}
	if live {
		s.toc, s.tocAt = vols, now()
	}
	return vols, nil
}

var (
	// chapterPath is a chapter's address path, also its id.
	chapterPath = regexp.MustCompile(`^\d{4}/\d{2}/\d{2}/[^/]+$`)
	volumeName  = regexp.MustCompile(`^vol(?:ume)?-?(\d+)$`)
)

// Resolve maps a link to the volume it means: a chapter's link to the
// volume holding that chapter, a table-of-contents link with a volume's
// anchor (#vol-3) to that volume, and the site or its table of contents
// as a whole to every volume in order - so pasting the site's address
// again imports the next volume the library doesn't have.
func (s *Source) Resolve(ctx context.Context, f websource.Fetcher, u *url.URL) ([]string, error) {
	vols, err := s.volumes(ctx, f)
	if err != nil {
		return nil, err
	}
	if m := volumeName.FindStringSubmatch(strings.ToLower(u.Fragment)); m != nil {
		id := "vol-" + m[1]
		for _, v := range vols {
			if v.id == id {
				return []string{id}, nil
			}
		}
		return nil, fmt.Errorf("there is no volume %s of The Wandering Inn", m[1])
	}
	path := strings.Trim(u.Path, "/")
	switch {
	case path == "" || path == strings.Trim(tocPath, "/"):
		ids := make([]string, len(vols))
		for i, v := range vols {
			ids[i] = v.id
		}
		return ids, nil
	case chapterPath.MatchString(path):
		for _, v := range vols {
			for _, c := range v.chapters {
				if c.ID == path {
					return []string{v.id}, nil
				}
			}
		}
		return nil, errors.New("that chapter isn't in The Wandering Inn's table of contents")
	}
	return nil, errors.New("not a link to The Wandering Inn's table of contents or one of its chapters")
}

// Work returns one volume as a book.
func (s *Source) Work(ctx context.Context, f websource.Fetcher, volumeID string) (*websource.Work, error) {
	vols, err := s.volumes(ctx, f)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		if v.id != volumeID {
			continue
		}
		return &websource.Work{
			ID:          v.id,
			URL:         s.base() + tocPath + "#" + v.id,
			Title:       seriesName + ": " + v.title,
			Author:      author,
			CoverURL:    v.coverURL,
			SeriesName:  seriesName,
			SeriesIndex: v.number,
			Chapters:    v.chapters,
		}, nil
	}
	return nil, fmt.Errorf("%w: %s is no longer in the table of contents", websource.ErrNotFound, volumeID)
}

// Chapter fetches and cleans up one chapter. The table of contents doesn't
// say which chapters are for patrons only, so that is found out here
// (websource.ErrLocked) - after trying the site's "Unlock with Patreon"
// once, when the library is signed in to Patreon (unlock.go).
func (s *Source) Chapter(ctx context.Context, f websource.Fetcher, ref websource.ChapterRef) (*websource.Chapter, error) {
	page, finalURL, _, err := f.Get(ctx, ref.URL, websource.MaxPageSize)
	if err != nil {
		return nil, fmt.Errorf("fetch chapter %q: %w", ref.Title, err)
	}
	base, err := url.Parse(finalURL)
	if err != nil {
		return nil, err
	}
	content, imgs, err := cleanChapter(page)
	if errors.Is(err, websource.ErrLocked) && s.unlock(ctx, page, base) {
		// Again, as a member.
		if page, finalURL, _, err = f.Get(ctx, ref.URL, websource.MaxPageSize); err != nil {
			return nil, fmt.Errorf("fetch chapter %q: %w", ref.Title, err)
		}
		if base, err = url.Parse(finalURL); err != nil {
			return nil, err
		}
		content, imgs, err = cleanChapter(page)
	}
	if err != nil {
		return nil, fmt.Errorf("chapter %q: %w", ref.Title, err)
	}
	return websource.RenderChapter(ctx, f, ref, base, content, imgs)
}
