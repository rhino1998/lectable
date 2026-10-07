// Package websource is what importing a book from a website shares across
// sites: the Source interface each site implements, a paced HTTP client
// (Client), and the epub a fetched work is kept as (WriteEPUB), which the
// ordinary epub parser reads like any other book. A site's own package
// (internal/royalroad, internal/wanderinginn) only knows its pages'
// markup; creating the book and appending chapters is httpapi/source.go.
package websource

import (
	"context"
	"errors"
	"net/url"
)

// ErrNotFound is a work or chapter the site answers 404 for: deleted,
// unpublished, or never there.
var ErrNotFound = errors.New("not found at the source")

// ErrLocked is a chapter the site lists but doesn't serve to an anonymous
// reader (early access, patrons only).
var ErrLocked = errors.New("chapter is locked at the source")

// Fetcher gets a page or an image by its address: the body (an error if
// it exceeds limit bytes), the address after redirects, and the content
// type. A Client fetches from the live web; a Wayback fetcher gets the
// same addresses as the Internet Archive saved them.
type Fetcher interface {
	Get(ctx context.Context, rawURL string, limit int64) (body []byte, finalURL, mediaType string, err error)
}

// Source is one website books can be imported from. Every method that
// reads the site takes the Fetcher to read it through - the source's own
// (Fetch) for the live site, or an archive's - and so has to cope with the
// site's pages as they were laid out in the past too.
type Source interface {
	// Kind is the source's stable id, stored with each book imported
	// from it ("royalroad").
	Kind() string
	// Name is the site's name as shown to the user ("Royal Road").
	Name() string
	// Fetch is the source's fetcher for the live site, shared by every
	// import from it so they keep one request pace.
	Fetch() Fetcher
	// Match reports whether a link is to this site.
	Match(u *url.URL) bool
	// Resolve names the work a link points at. A link to the site as a
	// whole rather than one work may return several, in the order they
	// should be offered: the import takes the first the library doesn't
	// have.
	Resolve(ctx context.Context, f Fetcher, u *url.URL) (workIDs []string, err error)
	// Work fetches a work's metadata and chapter list.
	Work(ctx context.Context, f Fetcher, workID string) (*Work, error)
	// Chapter fetches one chapter's text; ErrLocked or ErrNotFound when
	// the site won't serve it.
	Chapter(ctx context.Context, f Fetcher, ref ChapterRef) (*Chapter, error)
}

// ChapterRef is one entry of a work's chapter list.
type ChapterRef struct {
	// ID is the source's own id for the chapter - stable across retitles
	// and reorders.
	ID    string
	Title string
	URL   string
	// Unlocked is false for a chapter the list itself marks as not
	// readable by an anonymous visitor. A site whose list doesn't say
	// reports ErrLocked from Chapter instead.
	Unlocked bool
	// Aliases are the titles this chapter goes by in the work's published
	// ebook, where the source says - one web chapter can be several ebook
	// chapters, or share one with its neighbours. What an uploaded epub's
	// chapters are matched against when it fills in for chapters the site
	// won't serve; empty means match on Title.
	Aliases []string
}

// Work is one importable book at a source: its metadata and chapter list.
type Work struct {
	ID       string
	URL      string // the work's page, canonical
	Title    string
	Author   string
	CoverURL string // "" for none
	// SeriesName and SeriesIndex place the work in a series, for a source
	// that splits one serial into several books; "" and 0 otherwise.
	SeriesName  string
	SeriesIndex float64
	Chapters    []ChapterRef // in reading order
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
	// with each image's src rewritten to its Images name.
	Body   string
	Images []Image
}
