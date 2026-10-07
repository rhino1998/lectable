package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// Books imported from websites are checked for new entries without being
// asked: CheckSources (a periodic task, cmd/server) queues a check for
// each book whose last one is older than the library's interval
// (store.SourceCheckHours, 24 by default, 0 for never). A check doesn't
// import what it finds - new entries wait for the reader's say
// (source_review.go), which is what the clients mark a book with - except
// chapters already agreed to that weren't readable before. "Check all now"
// (handleCheckSources) does the same for every book regardless of when it
// was last checked.

// SourceCheckEvery is how often CheckSources is run. Each pass only
// queues the books that are due.
const SourceCheckEvery = 30 * time.Minute

// sourceSettingsDTO is the library-wide setting for checking sources
// (GET|PUT /api/settings/sources, live topic sourceSettings).
type sourceSettingsDTO struct {
	// CheckEveryHours is how often each book imported from a website is
	// checked for new entries by itself: 0 never.
	CheckEveryHours int `json:"checkEveryHours"`
}

func (s *Server) buildSourceSettings() (sourceSettingsDTO, error) {
	hours, err := s.Store.SourceCheckHours()
	if err != nil {
		return sourceSettingsDTO{}, httpError(http.StatusInternalServerError, err.Error())
	}
	return sourceSettingsDTO{CheckEveryHours: hours}, nil
}

// handleGetSourceSettings serves GET /api/settings/sources.
func (s *Server) handleGetSourceSettings(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildSourceSettings()
	writeBuilt(w, v, err)
}

// handleSetSourceSettings serves PUT /api/settings/sources.
func (s *Server) handleSetSourceSettings(w http.ResponseWriter, r *http.Request) {
	var req sourceSettingsDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.CheckEveryHours < 0 || req.CheckEveryHours > 24*365 {
		writeError(w, http.StatusBadRequest, "checkEveryHours must be 0 (never) or a number of hours")
		return
	}
	if err := s.Store.SetSourceCheckHours(req.CheckEveryHours); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

// CheckSources is one pass of the periodic check: a check is queued for
// every book with a source whose last one is older than the library's
// interval.
func (s *Server) CheckSources(_ context.Context) error {
	hours, err := s.Store.SourceCheckHours()
	if err != nil || hours <= 0 {
		return err
	}
	_, err = s.checkSources(time.Now().Add(-time.Duration(hours) * time.Hour).Unix())
	return err
}

// checkSources queues a check for every sourced book last checked before
// the given time (startSourceSync - one already queued or running is left
// to it), and returns how many it queued.
func (s *Server) checkSources(before int64) (int, error) {
	books, err := s.Store.SourcedBooks()
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, b := range books {
		if b.CheckedAt >= before {
			continue
		}
		if src, err := s.Store.GetBookSource(b.BookID); err != nil || src == nil {
			continue
		} else if _, _, ok := s.readerForBook(src); !ok {
			continue
		}
		if s.startSourceSync(b.BookID, nil) {
			queued++
		}
	}
	return queued, nil
}

// handleCheckSources queues a check for new entries on every book that has
// a source, whenever each was last checked. 202 with how many it queued
// (books already being checked aren't counted).
func (s *Server) handleCheckSources(w http.ResponseWriter, r *http.Request) {
	queued, err := s.checkSources(time.Now().Unix() + 1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: queued})
}
