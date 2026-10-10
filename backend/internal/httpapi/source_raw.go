package httpapi

import (
	"archive/zip"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// Every chapter imported from a website keeps what it was made from - its
// page and images exactly as fetched (websource.RawChapter, at
// audiopath.SourceRawFile) - so a change to how a site's pages are cleaned
// up can be applied to a book without fetching anything again: reparse
// reads each chapter afresh from its stored page and rewrites the book's
// kept epub with the result. That alone changes no chapter the reader
// sees; it reports which chapters now read differently, and re-importing
// one of those (the ordinary chapter re-import, from the kept epub) is
// what takes the new text - and resets that chapter's speakers and audio,
// which is why it stays a separate, per-chapter step.

// writeRawChapter stores what a chapter was made from, named by its
// source id. Best-effort: without it the chapter just can't be reparsed.
func (s *Server) writeRawChapter(bookID string, f sourced) {
	if f.raw == nil || f.ch == nil {
		return
	}
	path := audiopath.SourceRawFile(s.DataDir, bookID, websource.FileKey(f.ch.ID))
	if err := websource.WriteRaw(path, f.raw); err != nil {
		log.Printf("source: keeping the fetched pages of %q: %v", f.ch.Title, err)
	}
}

// addLastChapterSourceID records that bookID's last chapter also stands
// for another of the source's chapters, and renames its stored pages to
// match: they are named by the chapter's source id.
func (s *Server) addLastChapterSourceID(bookID, sourceID string) error {
	before, err := s.Store.SourcedChapters(bookID)
	if err != nil {
		return err
	}
	if err := s.Store.AddLastChapterSourceID(bookID, sourceID); err != nil {
		return err
	}
	if len(before) == 0 {
		return nil
	}
	old := before[len(before)-1].SourceID
	_ = os.Rename(
		audiopath.SourceRawFile(s.DataDir, bookID, websource.FileKey(old)),
		audiopath.SourceRawFile(s.DataDir, bookID, websource.FileKey(old+" "+sourceID)))
	return nil
}

// fetchMissingRaw fetches the pages of chapters that have none stored -
// ones imported before pages were kept - so they too can be reparsed. Part
// of every check of a book's source, after its new chapters; a book is
// only ever behind once. The chapters themselves aren't touched: what is
// fetched now may differ from what was imported, which a reparse then
// reports. Best-effort: a chapter the source no longer lists or serves
// (and the archive doesn't have) stays without, and a failure stops the
// pass until the next check.
func (s *Server) fetchMissingRaw(ctx context.Context, bookID string, reader sourceReader, work *websource.Work) {
	chapters, err := s.Store.SourcedChapters(bookID)
	if err != nil {
		return
	}
	refs := make(map[string]websource.ChapterRef, len(work.Chapters))
	for _, ref := range work.Chapters {
		refs[ref.ID] = ref
	}
	fetched := 0
	for _, sc := range chapters {
		path := audiopath.SourceRawFile(s.DataDir, bookID, websource.FileKey(sc.SourceID))
		if _, err := os.Stat(path); err == nil {
			continue
		}
		// A chapter standing for several of the source's is the first.
		ids := strings.Fields(sc.SourceID)
		ref, listed := refs[ids[0]]
		if !listed {
			continue
		}
		got, err := s.readChapter(ctx, reader, ref)
		if errors.Is(err, websource.ErrLocked) || errors.Is(err, websource.ErrNotFound) {
			continue
		}
		if err != nil {
			log.Printf("source: fetching the pages of %q for keeping: %v", sc.Title, err)
			break
		}
		got.ch.ID = sc.SourceID
		s.writeRawChapter(bookID, got)
		fetched++
	}
	if fetched > 0 {
		log.Printf("source: kept the pages of %d chapters of %s imported before they were stored", fetched, bookID)
	}
}

type reparseSourceResponse struct {
	// Reparsed is how many chapters were read again from their stored
	// pages and written to the book's kept epub.
	Reparsed int `json:"reparsed"`
	// Missing is how many chapters have no stored pages (imported before
	// they were kept) and Failed how many stored pages no longer read as a
	// chapter; both keep what the kept epub had.
	Missing int `json:"missing"`
	Failed  int `json:"failed"`
	// Changed lists the chapters (by idx) whose text now differs from the
	// library's - the ones a chapter re-import would change.
	Changed []int `json:"changed"`
}

// handleReparseBookSource reads every chapter of a book imported from a
// website again from its stored pages - no fetching - with the current
// import code, and rewrites the book's kept epub with the results. The
// library's chapters are untouched: the answer lists the ones that now
// read differently, for re-importing one by one (POST
// .../chapters/{idx}/reimport). 409 for a book with no source, no kept
// epub of its own, or a check running.
func (s *Server) handleReparseBookSource(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	b, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	sources, err := s.Store.BookSources(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(sources) == 0 {
		writeError(w, http.StatusConflict, "this book wasn't imported from a website, so it has no stored pages to read again")
		return
	}
	if !s.anyReadable(sources) {
		writeError(w, http.StatusConflict, "this server can't read the site this book came from ("+sources[0].Kind+")")
		return
	}
	// Holds off checks for new chapters, which add to the same epub.
	if queued, running := s.Jobs.BulkState(bookID, sourceSyncJobKey); queued || running {
		writeError(w, http.StatusConflict, "chapters are being fetched for this book right now - try again when that finishes")
		return
	}
	if _, running := s.sourceSyncs.LoadOrStore(bookID, struct{}{}); running {
		writeError(w, http.StatusConflict, "chapters are being fetched for this book right now - try again when that finishes")
		return
	}
	s.sourceSyncChanged()
	defer func() { s.sourceSyncs.Delete(bookID); s.sourceSyncChanged() }()

	chapters, err := s.Store.SourcedChapters(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := reparseSourceResponse{Changed: []int{}}
	var fresh []*websource.Chapter
	for _, sc := range chapters {
		raw, err := websource.ReadRaw(audiopath.SourceRawFile(s.DataDir, bookID, websource.FileKey(sc.SourceID)))
		if errors.Is(err, os.ErrNotExist) {
			resp.Missing++
			continue
		}
		var ch *websource.Chapter
		if err == nil {
			if reader, ok := s.readerForChapter(sources, sc.SourceID); ok {
				ch, err = rereadChapter(r.Context(), reader.site, raw)
			} else {
				err = errors.New("no reader for the site it came from")
			}
		}
		if err != nil {
			log.Printf("source: reading %q again from its stored pages: %v", sc.Title, err)
			resp.Failed++
			continue
		}
		// As stored: a chapter standing for several of the source's
		// carries all their ids.
		ch.ID = sc.SourceID
		input, _, err := sourceChapterInput(websource.Meta{Title: b.Title}, ch)
		if err != nil {
			log.Printf("source: parsing %q again: %v", sc.Title, err)
			resp.Failed++
			continue
		}
		fresh = append(fresh, ch)
		resp.Reparsed++

		current, err := s.Store.GetChapterByIdx(bookID, sc.Idx)
		if err != nil || current == nil {
			continue
		}
		paragraphs, err := s.Store.ListParagraphsRaw(current.ID)
		if err != nil {
			continue
		}
		var texts []string
		for _, blk := range input.Blocks {
			if blk.Kind == store.BlockText {
				texts = append(texts, blk.Text)
			}
		}
		same := len(texts) == len(paragraphs) && input.Title == current.Title
		for i := 0; same && i < len(texts); i++ {
			same = texts[i] == paragraphs[i].Text
		}
		if !same {
			resp.Changed = append(resp.Changed, sc.Idx)
		}
	}
	if len(fresh) > 0 {
		if err := s.rewriteSourceEpub(bookID, fresh); err != nil {
			if errors.Is(err, websource.ErrForeignEpub) {
				writeError(w, http.StatusConflict, "this book's kept epub was replaced by an uploaded one, so there is nothing to write the chapters back to")
				return
			}
			writeError(w, http.StatusInternalServerError, "could not rewrite the book's epub: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// rereadChapter makes a chapter again from what it was first made from,
// with the source's current code and no network.
func rereadChapter(ctx context.Context, site websource.Source, raw *websource.RawChapter) (*websource.Chapter, error) {
	if raw.Chapter != nil {
		return raw.Chapter, nil
	}
	ref := raw.Ref
	// It was readable when these pages were fetched, whatever the list
	// said of it then.
	ref.Unlocked = true
	return site.Chapter(ctx, websource.Replay(raw.Fetches), ref)
}

// rewriteSourceEpub replaces chapters of bookID's kept source epub in
// place, via a temp file.
func (s *Server) rewriteSourceEpub(bookID string, chapters []*websource.Chapter) error {
	dst := audiopath.SourceEpubFile(s.DataDir, bookID)
	old, err := zip.OpenReader(dst)
	if err != nil {
		return websource.ErrForeignEpub
	}
	defer old.Close()
	f, err := os.CreateTemp(filepath.Dir(dst), bookID+"-*.tmp")
	if err != nil {
		return err
	}
	err = websource.RewriteEPUB(f, &old.Reader, chapters)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), dst)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}
