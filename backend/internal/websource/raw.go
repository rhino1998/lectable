package websource

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// What a chapter was made from is kept beside the book: the pages and
// images exactly as fetched, before any of a source's cleaning up. A
// chapter can then be read again by newer code - a fixed parser, a
// different idea of what to drop - without asking the site for anything
// (Replay), which matters most for pages that came from an archive or are
// no longer served at all.

// Fetched is one response as a Fetcher returned it.
type Fetched struct {
	// URL is the address asked for; FinalURL what the Fetcher reported
	// after redirects.
	URL       string `json:"url"`
	FinalURL  string `json:"finalUrl"`
	MediaType string `json:"mediaType,omitempty"`
	Body      []byte `json:"body"`
}

// RawChapter is everything one chapter was made from.
type RawChapter struct {
	// Ref is the chapter as the source listed it when it was read.
	Ref ChapterRef `json:"ref"`
	// Via says where it was read: "live", "wayback" (an archived copy,
	// whichever the Fetches came from) or "epub" (an uploaded epub).
	Via string `json:"via"`
	// FetchedAt is when, unix seconds.
	FetchedAt int64 `json:"fetchedAt"`
	// Fetches are the chapter's page and the images it embeds, in the
	// order they were got. Empty for a chapter from an epub.
	Fetches []Fetched `json:"fetches,omitempty"`
	// Chapter is the chapter itself when it wasn't fetched: one taken from
	// an uploaded epub, which has no page to read again.
	Chapter *Chapter `json:"chapter,omitempty"`
}

// Recorder is a Fetcher that keeps what another one returns.
type Recorder struct {
	Inner Fetcher

	mu      sync.Mutex
	fetches []Fetched
}

func (r *Recorder) Get(ctx context.Context, rawURL string, limit int64) ([]byte, string, string, error) {
	body, finalURL, mediaType, err := r.Inner.Get(ctx, rawURL, limit)
	if err == nil {
		r.mu.Lock()
		r.fetches = append(r.fetches, Fetched{URL: rawURL, FinalURL: finalURL, MediaType: mediaType, Body: body})
		r.mu.Unlock()
	}
	return body, finalURL, mediaType, err
}

// Fetches returns what has been fetched so far.
func (r *Recorder) Fetches() []Fetched {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Fetched(nil), r.fetches...)
}

// ErrNotRecorded is an address a Replay has no response for.
var ErrNotRecorded = errors.New("not among the chapter's stored pages")

// Replay is a Fetcher that answers from recorded responses and never
// touches the network. An address that wasn't recorded - an image that
// couldn't be fetched at the time - fails, as it did then.
func Replay(fetches []Fetched) Fetcher {
	r := replay{}
	for _, f := range fetches {
		if _, dup := r[f.URL]; !dup {
			r[f.URL] = f
		}
	}
	return r
}

type replay map[string]Fetched

func (r replay) Get(_ context.Context, rawURL string, limit int64) ([]byte, string, string, error) {
	f, ok := r[rawURL]
	if !ok {
		return nil, "", "", fmt.Errorf("%s: %w", rawURL, ErrNotRecorded)
	}
	if int64(len(f.Body)) > limit {
		return nil, "", "", fmt.Errorf("stored response larger than %d MiB", limit>>20)
	}
	return f.Body, f.FinalURL, f.MediaType, nil
}

// WriteRaw stores raw at path (gzipped JSON), replacing what is there, via
// a temp file.
func WriteRaw(path string, raw *RawChapter) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(raw); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(buf.Bytes())
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// ReadRaw loads what WriteRaw stored; os.ErrNotExist if nothing was.
func ReadRaw(path string) (*RawChapter, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	var raw RawChapter
	if err := json.NewDecoder(io.LimitReader(zr, 1<<30)).Decode(&raw); err != nil {
		return nil, err
	}
	return &raw, nil
}
