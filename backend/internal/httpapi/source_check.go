package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/patreon"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/wanderinginn"
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
	// PatreonAccount is the name of the Patreon account the library reads
	// patrons-only posts as (PUT /api/settings/patreon), "" when it isn't
	// signed in to one. Read-only here.
	PatreonAccount string `json:"patreonAccount,omitempty"`
}

type patreonSessionRequest struct {
	// SessionID is the session_id cookie of a browser signed in to
	// patreon.com; "" signs the library out.
	SessionID string `json:"sessionId"`
}

func (s *Server) buildSourceSettings() (sourceSettingsDTO, error) {
	hours, err := s.Store.SourceCheckHours()
	if err != nil {
		return sourceSettingsDTO{}, httpError(http.StatusInternalServerError, err.Error())
	}
	_, account, err := s.Store.PatreonSession()
	if err != nil {
		return sourceSettingsDTO{}, httpError(http.StatusInternalServerError, err.Error())
	}
	return sourceSettingsDTO{CheckEveryHours: hours, PatreonAccount: account}, nil
}

// patreonSource is the Patreon source among this server's, nil if it has
// none.
func (s *Server) patreonSource() *patreon.Source {
	for _, src := range s.sources() {
		if p, ok := src.(*patreon.Source); ok {
			return p
		}
	}
	return nil
}

// handleSetPatreonSession signs the library in to Patreon with a browser's
// session_id cookie, so posts that account can read can be imported - or,
// with none, signs it out. The cookie is checked with Patreon first: 400
// if it isn't a session Patreon knows. Books that track Patreon - or The
// Wandering Inn, which is unlocked with the same account - are then
// checked again, since what they can read has changed.
func (s *Server) handleSetPatreonSession(w http.ResponseWriter, r *http.Request) {
	var req patreonSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	src := s.patreonSource()
	if src == nil {
		writeError(w, http.StatusConflict, "this server can't read Patreon")
		return
	}
	// Pasted as the browser shows it, name and all.
	session := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.SessionID), "session_id="))
	if strings.ContainsAny(session, "; \t\r\n") {
		writeError(w, http.StatusBadRequest, "that isn't a session_id cookie's value - paste only what follows \"session_id=\"")
		return
	}
	previous := src.Session()
	account := ""
	if session != "" {
		src.SetSession(session)
		name, err := src.Account(r.Context())
		if err != nil || name == "" {
			src.SetSession(previous)
			if err != nil {
				writeError(w, http.StatusBadGateway, "could not ask Patreon about that session: "+err.Error())
				return
			}
			writeError(w, http.StatusBadRequest, "Patreon doesn't know that session - it may have expired, or been copied short. Sign in to patreon.com in a browser and copy its session_id cookie again")
			return
		}
		account = name
	}
	if err := s.Store.SetPatreonSession(session, account); err != nil {
		src.SetSession(previous)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	src.SetSession(session)
	// The Wandering Inn's own login came from the account before.
	for _, other := range s.sources() {
		if inn, ok := other.(*wanderinginn.Source); ok {
			inn.SignOut()
		}
	}
	if books, err := s.Store.SourcedBooks(); err == nil {
		for _, b := range books {
			sources, _ := s.Store.BookSources(b.BookID)
			if slices.ContainsFunc(sources, func(src store.BookSource) bool {
				return src.Kind == patreon.Kind || src.Kind == wanderinginn.Kind
			}) {
				s.startSourceSync(b.BookID, nil)
			}
		}
	}
	writeNoContent(w)
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
		if sources, err := s.Store.BookSources(b.BookID); err != nil || !s.anyReadable(sources) {
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
