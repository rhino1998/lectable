package httpapi

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/websource"
)

// Changing a book's chapters after import (store's restructure.go): one
// put in mid-book from an epub, one moved, one removed. Only for a book
// imported from a website - its kept epub is written by this server, with
// an index of its chapters, so it can be rearranged to match and chapter
// re-import keeps working; an uploaded book's epub is the reader's own
// file, which re-import reads by position.

// epubOnlyPrefix starts the source id of a chapter put in from an epub by
// hand: the site never listed it, so no check asks about it.
const epubOnlyPrefix = "epub-"

// restructuring looks up the book a chapter change is for and holds off
// checks of its source meanwhile; done releases it. ok is false when the
// request has been answered.
func (s *Server) restructuring(w http.ResponseWriter, bookID string) (b *store.Book, done func(), ok bool) {
	b, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	if b == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return nil, nil, false
	}
	sources, err := s.Store.BookSources(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, nil, false
	}
	if len(sources) == 0 {
		writeError(w, http.StatusConflict, "only a book imported from a website can have its chapters rearranged")
		return nil, nil, false
	}
	busy := "chapters are being fetched for this book right now - try again when that finishes"
	if queued, running := s.Jobs.BulkState(bookID, sourceSyncJobKey); queued || running {
		writeError(w, http.StatusConflict, busy)
		return nil, nil, false
	}
	if _, held := s.sourceSyncs.LoadOrStore(bookID, struct{}{}); held {
		writeError(w, http.StatusConflict, busy)
		return nil, nil, false
	}
	s.sourceSyncChanged()
	return b, func() { s.sourceSyncs.Delete(bookID); s.sourceSyncChanged() }, true
}

// arrangeSourceEpub rewrites bookID's kept epub to hold the book's
// chapters as they now stand, in order, writing added from what is given.
// Best-effort: a kept epub that doesn't cover the book is left alone.
func (s *Server) arrangeSourceEpub(bookID string, added []*websource.Chapter) {
	err := func() error {
		chapters, err := s.Store.SourcedChapters(bookID)
		if err != nil {
			return err
		}
		dst := audiopath.SourceEpubFile(s.DataDir, bookID)
		old, err := zip.OpenReader(dst)
		if err != nil {
			return err
		}
		defer old.Close()
		ids, err := websource.ChapterIDs(&old.Reader)
		if err != nil {
			return err
		}
		known := make(map[string]bool, len(ids)+len(added))
		for _, id := range ids {
			known[id] = true
		}
		for _, ch := range added {
			known[ch.ID] = true
		}
		order := make([]string, 0, len(chapters))
		for _, sc := range chapters {
			// A chapter standing for several of the source's is in the
			// epub under all its ids, or the first it had.
			id := sc.SourceID
			if first := strings.Fields(id); !known[id] && len(first) > 0 {
				id = first[0]
			}
			if !known[id] {
				return fmt.Errorf("the epub has no chapter %q", sc.Title)
			}
			order = append(order, id)
		}
		f, err := os.CreateTemp(filepath.Dir(dst), bookID+"-*.tmp")
		if err != nil {
			return err
		}
		err = websource.ArrangeEPUB(f, &old.Reader, order, added)
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
	}()
	if err != nil {
		log.Printf("source: rearranging the epub of %s: %v", bookID, err)
	}
}

// handleInsertChapters puts chapters of an uploaded epub into a book at a
// chosen place (multipart "file"; "epubChapter" the first one's index as
// handleListEpubChapters lists it, "count" how many consecutive ones,
// default 1; "at" the idx the first takes - the chapters from there on
// move down, and the chapter count appends; "title" renames a single
// one). For chapters a book skipped past and can now get - nothing
// already in the book changes but its numbering. Answers how many went
// in. 409 for a book that wasn't imported from a website, or while its
// source is being checked.
func (s *Server) handleInsertChapters(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	if err := r.ParseMultipartForm(200 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "could not parse upload: "+err.Error())
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer file.Close()
	fill, err := readEpubFill(file)
	if err != nil {
		writeError(w, http.StatusBadRequest, "could not parse epub: "+err.Error())
		return
	}
	first, err := strconv.Atoi(r.FormValue("epubChapter"))
	count := 1
	if raw := r.FormValue("count"); raw != "" && err == nil {
		count, err = strconv.Atoi(raw)
	}
	if err != nil || first < 0 || count < 1 || first+count > len(fill.chapters) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("epubChapter and count must name chapters among the epub's %d", len(fill.chapters)))
		return
	}
	at, err := strconv.Atoi(r.FormValue("at"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "at must be the idx the first chapter takes")
		return
	}
	b, done, ok := s.restructuring(w, bookID)
	if !ok {
		return
	}
	defer done()
	have, err := s.Store.CountChapters(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if at < 0 || at > have {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("at must be between 0 and %d", have))
		return
	}

	var (
		added      []*websource.Chapter
		inputs     []store.ChapterInput
		imageBytes [][]byte
	)
	for i := range count {
		epubCh := fill.chapters[first+i]
		title := epubCh.Title
		if t := strings.TrimSpace(r.FormValue("title")); t != "" && count == 1 {
			title = t
		}
		id := epubOnlyPrefix + store.NewID()
		ch := epubChapterAsSource(epubCh, title, id, websource.FileKey(id))
		input, images, err := sourceChapterInput(websource.Meta{Title: b.Title}, ch)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		added = append(added, ch)
		inputs = append(inputs, input)
		imageBytes = append(imageBytes, images...)
	}
	created, err := s.Store.InsertChapters(bookID, at, inputs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeChapterImages(bookID, created, imageBytes)
	s.arrangeSourceEpub(bookID, added)
	for _, ch := range added {
		ref := websource.ChapterRef{ID: ch.ID, Title: ch.Title}
		s.writeRawChapter(bookID, sourced{ch: ch, raw: &websource.RawChapter{Ref: ref, Via: "epub", FetchedAt: time.Now().Unix(), Chapter: ch}})
	}
	writeJSON(w, http.StatusOK, insertChaptersResponse{Inserted: len(added)})
}

// handleMoveChapter moves one chapter to another place in its book
// (`{"to": idx}`); the chapters between shift by one. The chapter itself -
// text, speakers, audio - is untouched. 409 as handleInsertChapters.
func (s *Server) handleMoveChapter(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	var req moveChapterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	_, done, ok := s.restructuring(w, bookID)
	if !ok {
		return
	}
	defer done()
	have, err := s.Store.CountChapters(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if idx < 0 || idx >= have || req.To < 0 || req.To >= have {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("the book has chapters 0 to %d", have-1))
		return
	}
	if err := s.Store.MoveChapter(bookID, idx, req.To); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.arrangeSourceEpub(bookID, nil)
	writeNoContent(w)
}

// handleDeleteChapter removes one chapter from its book for good - a
// notice that was imported as a chapter, a duplicate - with its audio,
// and closes the gap. The chapter's jobs are cancelled first (409 if one
// won't stop), and its source ids join the book's skip list, so a later
// check doesn't fetch it again. 409 as handleInsertChapters.
func (s *Server) handleDeleteChapter(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	_, done, ok := s.restructuring(w, bookID)
	if !ok {
		return
	}
	defer done()
	ch, err := s.Store.GetChapterByIdx(bookID, idx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	sourceID := ""
	sourcedChapters, err := s.Store.SourcedChapters(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(sourcedChapters) <= 1 {
		writeError(w, http.StatusConflict, "a book keeps at least one chapter")
		return
	}
	for _, sc := range sourcedChapters {
		if sc.Idx == idx {
			sourceID = sc.SourceID
		}
	}
	cancelCtx, cancel := context.WithTimeout(r.Context(), reimportCancelWait)
	defer cancel()
	if !s.Jobs.CancelChapter(cancelCtx, ch.ID) {
		writeError(w, http.StatusConflict, "this chapter still has a job running - try again in a moment")
		return
	}
	// Before the chapter goes: once it has, only the skip list stops the
	// next check from fetching it again.
	if ids := strings.Fields(sourceID); len(ids) > 0 {
		sources, err := s.Store.BookSources(bookID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// In the plan of every source the ids could be listed by.
		for i := range sources {
			src := &sources[i]
			plan, err := s.Store.GetBookSourcePlan(src.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			changed := false
			for _, id := range ids {
				if ownsID(src, id) && !slices.Contains(plan.Skip, id) {
					plan.Skip = append(plan.Skip, id)
					changed = true
				}
			}
			if !changed {
				continue
			}
			plan.Extra = slices.DeleteFunc(plan.Extra, func(p store.PlannedChapter) bool { return slices.Contains(ids, p.ID) })
			if err := s.Store.SetBookSourcePlan(src.ID, plan); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	chapterID, images, err := s.Store.DeleteChapter(bookID, idx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.RemoveAll(fmt.Sprintf("%s/audio/%s/%s", s.DataDir, bookID, chapterID))
	for _, img := range images {
		_ = os.Remove(audiopath.ImageFile(s.DataDir, bookID, chapterID, img.ImageID, img.Ext))
	}
	if sourceID != "" {
		if err := os.Remove(audiopath.SourceRawFile(s.DataDir, bookID, websource.FileKey(sourceID))); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("source: removing the stored pages of %q: %v", ch.Title, err)
		}
	}
	s.arrangeSourceEpub(bookID, nil)
	writeNoContent(w)
}

type moveChapterRequest struct {
	// To is the idx the chapter takes.
	To int `json:"to"`
}

type renameChapterRequest struct {
	// Title is the chapter's new title; it can't be blank.
	Title string `json:"title"`
}

// handleRenameChapter gives one chapter a new title (`{"title"}`). The
// title is also the chapter's opening line, so that line's audio is made
// again; the rest of the chapter is untouched. For a book imported from a
// website the kept epub and the chapter's stored pages take the title
// too, so a re-import gives it back (a chapter renamed this way is no
// longer re-read from its pages by a reparse - it is kept as it now is).
func (s *Server) handleRenameChapter(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	var req renameChapterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	title := strings.Join(strings.Fields(req.Title), " ")
	if title == "" {
		writeError(w, http.StatusBadRequest, "a chapter needs a title")
		return
	}
	ch, err := s.Store.GetChapterByIdx(bookID, idx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	if err := s.Store.RenameChapter(ch.ID, title); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.renameInSource(r.Context(), bookID, idx, title)
	writeNoContent(w)
}

// renameInSource carries a chapter's new title into what a book imported
// from a website keeps of it: its stored pages and its epub. Best-effort.
func (s *Server) renameInSource(ctx context.Context, bookID string, idx int, title string) {
	sources, err := s.Store.BookSources(bookID)
	if err != nil || len(sources) == 0 {
		return
	}
	chapters, err := s.Store.SourcedChapters(bookID)
	if err != nil {
		return
	}
	for _, sc := range chapters {
		if sc.Idx != idx {
			continue
		}
		reader, ok := s.readerForChapter(sources, sc.SourceID)
		if !ok {
			return
		}
		path := audiopath.SourceRawFile(s.DataDir, bookID, websource.FileKey(sc.SourceID))
		raw, err := websource.ReadRaw(path)
		if err != nil {
			return
		}
		ch, err := rereadChapter(ctx, reader.site, raw)
		if err != nil {
			log.Printf("source: renaming %q: %v", sc.Title, err)
			return
		}
		ch.ID, ch.Title = sc.SourceID, title
		raw.Chapter = ch
		if err := websource.WriteRaw(path, raw); err != nil {
			log.Printf("source: renaming %q: %v", sc.Title, err)
		}
		if err := s.rewriteSourceEpub(bookID, []*websource.Chapter{ch}); err != nil && !errors.Is(err, websource.ErrForeignEpub) {
			log.Printf("source: renaming %q in the epub: %v", sc.Title, err)
		}
	}
}
