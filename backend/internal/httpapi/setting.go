package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// bookSettingDTO is a book's setting profile (store.BookSetting) - the world
// its story takes place in, which the ambience, lighting and
// characterization prompts are shown. Field-for-field store.BookSetting, so
// the two convert directly. Derived once per book by one LLM call
// the first time a pass needs it; a reader can edit it.
type bookSettingDTO struct {
	Era        string `json:"era"`
	Technology string `json:"technology"`
	Place      string `json:"place"`
	// Accent is the main characters' accent, "" when the text doesn't make
	// it clear.
	Accent string `json:"accent"`
	Genre  string `json:"genre"`
	// Narrator is a first-person book's narrator (the "I" - attribution
	// credits "I said" lines to them), "" for a third-person book.
	Narrator string `json:"narrator"`
}

// bookSettingResponse is GET/PUT/POST .../setting's reply. Setting is nil
// until the book has one.
type bookSettingResponse struct {
	Setting *bookSettingDTO `json:"setting"`
	// Source is who wrote it - store.BookSettingAuto or BookSettingManual.
	Source    store.BookSettingSource `json:"source,omitempty"`
	UpdatedAt int64                   `json:"updatedAt,omitempty"`
}

func bookSettingDTOFrom(s store.BookSetting) *bookSettingDTO {
	d := bookSettingDTO(s)
	return &d
}

func speakerattrSetting(s store.BookSetting) *speakerattr.BookSetting {
	out := speakerattr.BookSetting(s)
	return &out
}

func bookSettingResponseFrom(st *store.StoredBookSetting) bookSettingResponse {
	if st == nil {
		return bookSettingResponse{}
	}
	return bookSettingResponse{Setting: bookSettingDTOFrom(st.Setting), Source: st.Source, UpdatedAt: st.UpdatedAt}
}

// lockBookSetting serializes deriving one book's setting, so concurrent
// ambience/characterization tasks for the same book make one LLM call
// between them rather than one each.
func (s *Server) lockBookSetting(bookID string) func() {
	muAny, _ := s.settingLocks.LoadOrStore(bookID, &sync.Mutex{})
	mu := muAny.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// bookSetting is book's setting profile for a pass's prompt, nil when it
// has none. Never derives one itself: the setting pass is its own
// jobs.KindBookSetting task, which the passes reading it wait on while the
// book has none (jobs.Manager.bookSettingDependency) - if it failed, they
// simply run without it, as before setting profiles existed.
func (s *Server) bookSetting(_ context.Context, book *store.Book) *speakerattr.BookSetting {
	st, err := s.Store.GetBookSetting(book.ID)
	if err != nil {
		log.Printf("httpapi: book setting for %s: %v", book.ID, err)
		return nil
	}
	if st == nil {
		return nil
	}
	return speakerattrSetting(st.Setting)
}

// deriveBookSetting runs the setting pass for book - one
// speakerattr.Client.DescribeSetting call over the whole book's technology
// word counts and the start of chapters spread across it - and stores the
// result. Unless force, a setting that
// appeared while waiting for the lock (another task derived it) is
// returned as is. A manual setting is only ever replaced by force.
func (s *Server) deriveBookSetting(ctx context.Context, book *store.Book, force bool) (*store.StoredBookSetting, error) {
	if s.Speaker == nil {
		return nil, nil
	}
	defer s.lockBookSetting(book.ID)()
	if !force {
		if st, err := s.Store.GetBookSetting(book.ID); err != nil || st != nil {
			return st, err
		}
	}
	// One whole-book read: TechCounts covers every paragraph, IdiomCounts
	// every dialogue line, and the samples come from the same rows grouped
	// by chapter.
	all, err := s.Store.ListParagraphsRawForBook(book.ID)
	if err != nil {
		return nil, err
	}
	counts := speakerattr.TechCounts{}
	idioms := speakerattr.IdiomCounts{}
	var chapters [][]speakerattr.ParagraphInput
	for i, p := range all {
		counts.Add(p.Text)
		if p.IsQuote {
			idioms.Add(p.Text)
		}
		if i == 0 || p.ChapterID != all[i-1].ChapterID {
			chapters = append(chapters, nil)
		}
		chapters[len(chapters)-1] = append(chapters[len(chapters)-1], speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote})
	}
	var samples []string
	for _, idx := range speakerattr.SettingSampleChapters(len(chapters)) {
		if len(samples) == speakerattr.SettingSampleCount {
			break
		}
		if sample := speakerattr.SettingSample(chapters[idx]); sample != "" {
			samples = append(samples, sample)
		}
	}
	if len(samples) == 0 {
		return nil, nil
	}
	setting, err := s.Speaker.DescribeSetting(ctx, book.Title, book.Author, counts, idioms, samples)
	if err != nil {
		return nil, fmt.Errorf("describe setting: %w", err)
	}
	if err := s.Store.SetBookSetting(book.ID, store.BookSetting(setting), store.BookSettingAuto); err != nil {
		return nil, err
	}
	return s.Store.GetBookSetting(book.ID)
}

// buildBookSetting is the bookSetting live topic's value and GET
// .../setting's reply.
func (s *Server) buildBookSetting(bookID string) (bookSettingResponse, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return bookSettingResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return bookSettingResponse{}, httpError(http.StatusNotFound, "book not found")
	}
	st, err := s.Store.GetBookSetting(bookID)
	if err != nil {
		return bookSettingResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	return bookSettingResponseFrom(st), nil
}

// deriveBookSettingForJob is a lazily-created jobs.KindBookSetting task's
// work: derive book's setting unless it already has one.
func (s *Server) deriveBookSettingForJob(ctx context.Context, book *store.Book) error {
	_, err := s.deriveBookSetting(ctx, book, false)
	return err
}

// handleGetBookSetting serves GET /api/books/{id}/setting.
func (s *Server) handleGetBookSetting(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildBookSetting(r.PathValue("id"))
	writeBuilt(w, v, err)
}

// handleSetBookSetting serves PUT /api/books/{id}/setting - a reader's
// edit, which a later derive won't replace unless asked to. Already-run
// passes keep what they wrote; re-run them to apply it.
func (s *Server) handleSetBookSetting(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req bookSettingDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.Store.SetBookSetting(book.ID, store.BookSetting(req), store.BookSettingManual); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	st, err := s.Store.GetBookSetting(book.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, bookSettingResponseFrom(st))
}

// handleDeriveBookSetting serves POST /api/books/{id}/setting/derive:
// queues the setting pass now (a jobs.KindBookSetting task at urgent
// priority), replacing whatever the book had - a manual edit included.
// The bookSetting live topic carries the result.
func (s *Server) handleDeriveBookSetting(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker attribution LLM not configured")
		return
	}
	s.Jobs.EnqueueBookSetting(book, func(ctx context.Context, book *store.Book) error {
		_, err := s.deriveBookSetting(ctx, book, true)
		return err
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}
