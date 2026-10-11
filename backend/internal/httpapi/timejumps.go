package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// timeJumpDTO is one of a book's time jumps (store.TimeJump): years pass
// in the story before this chapter, for everyone. A jump at the book's
// first chapter sets the whole book that long after the one before it in
// its series.
type timeJumpDTO struct {
	ID           string `json:"id"`
	ChapterID    string `json:"chapterId"`
	ChapterIdx   int    `json:"chapterIdx"`
	ChapterTitle string `json:"chapterTitle"`
	Years        int    `json:"years"`
	// Voices is how many characters the jump has given an older voice so
	// far (the stages made for it) - it grows while the jump is applied,
	// and most characters never need one.
	Voices int `json:"voices"`
}

func (s *Server) handleListTimeJumps(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildTimeJumps(r.PathValue("id"))
	writeBuilt(w, v, err)
}

func (s *Server) buildTimeJumps(bookID string) ([]timeJumpDTO, error) {
	jumps, err := s.Store.ListTimeJumps(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	counts, err := s.Store.TimeJumpStageCounts(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	out := make([]timeJumpDTO, len(jumps))
	for i, j := range jumps {
		out[i] = timeJumpDTO{ID: j.ID, ChapterID: j.ChapterID, ChapterIdx: j.ChapterIdx, Years: j.Years, Voices: counts[j.ID]}
		if ch, err := s.chapterByID(j.ChapterID); err == nil && ch != nil {
			out[i].ChapterTitle = ch.Title
		}
	}
	return out, nil
}

type timeJumpRequest struct {
	// ChapterIdx is the chapter the jump falls before; 0 for the whole
	// book.
	ChapterIdx int `json:"chapterIdx"`
	// Years is how long has passed.
	Years int `json:"years"`
}

// maxJumpYears bounds a time jump.
const maxJumpYears = 1000

// handleCreateTimeJump records that years pass before one of this book's
// chapters and ages the cast across it: every character who speaks on
// both sides of the jump is asked about in the background
// (ageAcrossJump), and each one whose age group changes gets a stage
// there (store.CharacterStage, marked with the jump). Most characters
// come back unchanged. 202 with how many are being asked about.
func (s *Server) handleCreateTimeJump(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "ageing voices across a time jump needs the speaker LLM (set LECTABLE_SPEAKER_LLM_MODEL_PATH)")
		return
	}
	var req timeJumpRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Years < 1 || req.Years > maxJumpYears {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("years must be between 1 and %d", maxJumpYears))
		return
	}
	book, err := s.Store.GetBook(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	ch, err := s.Store.GetChapterByIdx(book.ID, req.ChapterIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	jump, created, err := s.Store.CreateTimeJump(book.ID, ch.ID, req.Years)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !created {
		writeError(w, http.StatusConflict, "that chapter already has a time jump - remove it first")
		return
	}
	queued, err := s.applyTimeJump(book, *jump)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: queued})
}

// handleDeleteTimeJump removes a time jump and the stages made for it;
// those lines go back to the voice they had before it.
func (s *Server) handleDeleteTimeJump(w http.ResponseWriter, r *http.Request) {
	jump, err := s.Store.GetTimeJump(r.PathValue("jumpId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if jump == nil || jump.BookID != r.PathValue("id") {
		writeError(w, http.StatusNotFound, "time jump not found")
		return
	}
	if err := s.Store.DeleteTimeJump(jump.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

// applyTimeJump queues, for every character who speaks on both sides of
// jump, the question of whether they sound different after it
// (ageAcrossJump), and returns how many it queued. A character heard only
// before it has no later lines to voice, and one heard only after it was
// cast from those lines already. Roles, invalid names and characters not
// yet characterized are left out.
func (s *Server) applyTimeJump(book *store.Book, jump store.TimeJump) (int, error) {
	books, err := s.booksInScope(book)
	if err != nil {
		return 0, err
	}
	before, after := map[string]bool{}, map[string]bool{}
	seen := false
	for _, b := range books {
		into := before
		if seen {
			into = after
		}
		from, to := 0, math.MaxInt32
		if b.ID == book.ID {
			seen = true
			// This book's chapters fall on both sides.
			names, err := s.Store.SpeakersInChapters(b.ID, 0, jump.ChapterIdx)
			if err != nil {
				return 0, err
			}
			for _, n := range names {
				before[n] = true
			}
			into, from = after, jump.ChapterIdx
		}
		names, err := s.Store.SpeakersInChapters(b.ID, from, to)
		if err != nil {
			return 0, err
		}
		for _, n := range names {
			into[n] = true
		}
	}
	characters, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, c := range characters {
		if c.IsRole || c.Invalid || c.Summary == "" || !before[c.Name] || !after[c.Name] {
			continue
		}
		// Keyed per jump: a task with the same key joins the one queued.
		s.Jobs.EnqueueCharacterization(book.ID, c.ID+"~jump:"+jump.ID, fmt.Sprintf("%s, %d years on", c.Name, jump.Years), func(ctx context.Context) error {
			return s.ageAcrossJump(ctx, book.ID, c.ID, jump.ID)
		})
		queued++
	}
	return queued, nil
}

// ageAcrossJump gives one character a stage at a time jump when the years
// change how they sound (speakerattr.Client.AgeCastingBy), aged from the
// voice they had going into it: their latest stage before the jump, else
// their own casting. A character who already has a stage at that chapter
// - set by hand - keeps it. Everything is re-read here: the jump may have
// been removed, or the character recast, while this waited.
func (s *Server) ageAcrossJump(ctx context.Context, bookID, characterID, jumpID string) error {
	jump, err := s.Store.GetTimeJump(jumpID)
	if err != nil || jump == nil {
		return err
	}
	book, err := s.Store.GetBook(bookID)
	if err != nil || book == nil {
		return err
	}
	char, err := s.Store.GetCharacter(characterID)
	if err != nil || char == nil || char.Summary == "" || char.IsRole {
		return err
	}
	stages, err := s.Narration.BookStages(book)
	if err != nil {
		return err
	}
	base := char.Summary
	for _, span := range stages[char.ID] {
		st := span.Stage
		if st.BookID == book.ID && st.ChapterIdx == jump.ChapterIdx {
			return nil
		}
		if st.BookID == book.ID && st.ChapterIdx > jump.ChapterIdx {
			break
		}
		if st.Instruct != "" {
			base = st.Instruct
		}
	}
	aged, err := s.Speaker.AgeCastingBy(ctx, base, jump.Years, s.castingWorldNotes(book, *char, base))
	if err != nil || !aged.Changed {
		return err
	}
	// Removed while the model was thinking: don't leave a stage behind.
	if again, err := s.Store.GetTimeJump(jumpID); err != nil || again == nil {
		return err
	}
	_, err = s.Store.SetCharacterStage(char.ID, book.ID, jump.ChapterID, aged.Age, aged.Instruct, jump.ID)
	return err
}
