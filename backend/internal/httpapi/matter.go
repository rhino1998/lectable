package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// Front and back matter: a book's matter pass (one
// speakerattr.Client.FindMatter call over its chapter list) marks the
// chapters that aren't story - a title page, the copyright page, "About the
// Author" - as store.Chapter.Matter, applied at once; the reader changes
// any of them with PUT .../chapters/{idx}/matter, which the pass then
// leaves alone. See the `.../matter` entry in docs/api.md.

// MatterCheckEvery is how often the server looks for books whose matter
// pass hasn't run (EnqueueMissingMatter).
const MatterCheckEvery = 6 * time.Hour

// findBookMatter runs book's matter pass and stores its answer: every
// chapter the reader hasn't set is marked matter or story. A book from a
// website has none (store.BookNeedsMatterPass) unless force, the reader's
// "check again".
func (s *Server) findBookMatter(ctx context.Context, book *store.Book, force bool) error {
	if s.Speaker == nil {
		return nil
	}
	if !force {
		if need, err := s.Store.BookNeedsMatterPass(book.ID); err != nil || !need {
			return err
		}
	}
	chapters, err := s.Store.MatterSections(book.ID)
	if err != nil || len(chapters) == 0 {
		return err
	}
	sections := make([]speakerattr.MatterSection, len(chapters))
	for i, c := range chapters {
		sections[i] = speakerattr.MatterSection{Title: c.Title, Chars: c.Chars, Opening: speakerattr.MatterOpening(c.Title, c.Opening)}
	}
	matter, err := s.Speaker.FindMatter(ctx, sections)
	if err != nil {
		return fmt.Errorf("find matter: %w", err)
	}
	var ids []string
	for i, m := range matter {
		if m {
			ids = append(ids, chapters[i].ID)
		}
	}
	return s.Store.SetBookMatter(book.ID, ids)
}

// findBookMatterForJob is a jobs.KindBookMatter task's work when the queue
// itself asked for it (jobs.Manager.EnsureBookMatter).
func (s *Server) findBookMatterForJob(ctx context.Context, book *store.Book) error {
	return s.findBookMatter(ctx, book, false)
}

// enqueueBookMatter queues book's matter pass at tier if it needs one.
func (s *Server) enqueueBookMatter(book *store.Book, tier int) {
	if s.Speaker == nil {
		return
	}
	if need, err := s.Store.BookNeedsMatterPass(book.ID); err != nil || !need {
		return
	}
	s.Jobs.EnqueueBookMatter(book, tier, s.findBookMatterForJob)
}

// EnqueueMissingMatter queues the matter pass, at background priority, for
// every book that hasn't had it: a library from before the pass existed,
// and any book whose pass failed. Does nothing without the LLM.
func (s *Server) EnqueueMissingMatter(context.Context) error {
	if s.Speaker == nil {
		return nil
	}
	ids, err := s.Store.BooksNeedingMatterPass()
	if err != nil {
		return err
	}
	for _, id := range ids {
		book, err := s.Store.GetBook(id)
		if err != nil || book == nil {
			continue
		}
		s.Jobs.EnqueueBookMatter(book, jobs.TierBackground, s.findBookMatterForJob)
	}
	if len(ids) > 0 {
		log.Printf("httpapi: queued the matter pass for %d books", len(ids))
	}
	return nil
}

// storyChapterSummaries is chapters without the ones marked as front or
// back matter now - chapters may have been listed before the book's matter
// pass ran, so the marks are read again.
func (s *Server) storyChapterSummaries(bookID string, chapters []store.ChapterSummary) []store.ChapterSummary {
	sections, err := s.Store.ChapterExtents(bookID)
	if err != nil {
		log.Printf("httpapi: matter marks for book %s: %v", bookID, err)
		return chapters
	}
	matter := make(map[string]bool, len(sections))
	for _, e := range sections {
		matter[e.ID] = e.Matter
	}
	out := make([]store.ChapterSummary, 0, len(chapters))
	for _, c := range chapters {
		if !matter[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// storyPhase wraps a preprocessing phase over a book's chapters so that it
// runs over its story chapters only: it waits for the book's matter pass
// if that hasn't run, then leaves out what is marked.
func (s *Server) storyPhase(book *store.Book, chapters []store.ChapterSummary, phase func(*store.Book, []store.ChapterSummary) jobs.PipelinePhaseFunc) jobs.PipelinePhaseFunc {
	return func(ctx context.Context, tier func() int) error {
		s.Jobs.EnsureBookMatter(ctx, book.ID, tier())
		return phase(book, s.storyChapterSummaries(book.ID, chapters))(ctx, tier)
	}
}

// setChapterMatterRequest is PUT .../chapters/{idx}/matter's body.
type setChapterMatterRequest struct {
	// Matter is whether the chapter is front or back matter (true) or
	// story (false).
	Matter bool `json:"matter"`
}

// handleSetChapterMatter serves PUT /api/books/{id}/chapters/{idx}/matter:
// the reader marking a chapter as front or back matter, or as story after
// all - kept whatever the book's matter pass says later. Nothing generated
// is deleted either way; a chapter made story again is narrated and
// preprocessed with the rest the next time those run.
func (s *Server) handleSetChapterMatter(w http.ResponseWriter, r *http.Request) {
	var req setChapterMatterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	_, ch, ok := s.chapterTarget(w, r)
	if !ok {
		return
	}
	if err := s.Store.SetChapterMatter(ch.ID, req.Matter); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

// handleDetectBookMatter serves POST /api/books/{id}/matter/detect: queues
// the book's matter pass now (a jobs.KindBookMatter task at urgent
// priority), for any book - one from a website included - replacing what
// an earlier run marked but nothing the reader set. The book live topic
// carries the result.
func (s *Server) handleDetectBookMatter(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker attribution LLM not configured")
		return
	}
	s.Jobs.EnqueueBookMatter(book, jobs.TierUrgent, func(ctx context.Context, book *store.Book) error {
		return s.findBookMatter(ctx, book, true)
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}
