package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// worldFactDTO is one keyed fact about a story's world (store.WorldFact):
// what the casting prompt is told whenever Key or one of Aliases comes up
// in a character's material.
type worldFactDTO struct {
	ID string `json:"id"`
	// Key is the word the fact is filed under ("dwarf").
	Key string `json:"key"`
	// Aliases are other words that bring it up ("dwarven"); a plain
	// plural of Key needs none.
	Aliases []string `json:"aliases"`
	Fact    string   `json:"fact"`
	// Global: the fact holds in every book, rather than being one
	// series' own.
	Global bool `json:"global"`
	// Superseded: a global fact the book's series has its own fact for,
	// under a shared word - not in force in that series. Only set in a
	// book's list.
	Superseded bool `json:"superseded,omitempty"`
}

type upsertWorldFactRequest struct {
	// ID rewrites that fact, key included; without it the fact is added,
	// or replaces the one already filed under Key.
	ID      string   `json:"id,omitempty"`
	Key     string   `json:"key"`
	Aliases []string `json:"aliases"`
	Fact    string   `json:"fact"`
}

func (s *Server) handleListGlobalFacts(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildGlobalFacts()
	writeBuilt(w, v, err)
}

func worldFactDTOFrom(f store.WorldFact) worldFactDTO {
	aliases := []string(f.Aliases)
	if aliases == nil {
		aliases = []string{}
	}
	return worldFactDTO{ID: f.ID, Key: f.Key, Aliases: aliases, Fact: f.Fact, Global: f.Scope == store.GlobalFactScope}
}

// buildGlobalFacts lists the facts that hold in every book
// (store.GlobalFactScope), alphabetical by key.
func (s *Server) buildGlobalFacts() ([]worldFactDTO, error) {
	facts, err := s.Store.ListWorldFacts(store.GlobalFactScope)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	out := make([]worldFactDTO, 0, len(facts))
	for _, f := range facts {
		out = append(out, worldFactDTOFrom(f))
	}
	return out, nil
}

func (s *Server) handleListBookFacts(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildBookFacts(r.PathValue("id"))
	writeBuilt(w, v, err)
}

// buildBookFacts lists the facts bookID's series has of its own
// (store.SeriesScope), then the global ones, each marked Superseded when
// a series fact shares a word with it - what store.BookWorldFacts leaves
// out for this book.
func (s *Server) buildBookFacts(bookID string) ([]worldFactDTO, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return nil, httpError(http.StatusNotFound, "book not found")
	}
	series, err := s.Store.ListWorldFacts(store.SeriesScope(book))
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	global, err := s.Store.ListWorldFacts(store.GlobalFactScope)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	out := make([]worldFactDTO, 0, len(series)+len(global))
	taken := map[string]bool{}
	for _, f := range series {
		out = append(out, worldFactDTOFrom(f))
		if f.Status == store.WorldFactActive {
			for _, w := range f.Words() {
				taken[w] = true
			}
		}
	}
	for _, f := range global {
		dto := worldFactDTOFrom(f)
		dto.Superseded = slices.ContainsFunc(f.Words(), func(w string) bool { return taken[w] })
		out = append(out, dto)
	}
	return out, nil
}

// handleUpsertGlobalFact saves a fact for every book. Nothing is recast:
// a fact reaches a character the next time their voice is cast.
func (s *Server) handleUpsertGlobalFact(w http.ResponseWriter, r *http.Request) {
	s.upsertFact(w, r, store.GlobalFactScope)
}

// handleUpsertBookFact saves a fact for the book's series (or the book
// alone, outside a series), where it replaces any global fact sharing a
// word with it. Nothing is recast.
func (s *Server) handleUpsertBookFact(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	s.upsertFact(w, r, store.SeriesScope(book))
}

func (s *Server) upsertFact(w http.ResponseWriter, r *http.Request, scope string) {
	var req upsertWorldFactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	fact := store.WorldFact{ID: req.ID, Key: strings.TrimSpace(req.Key), Fact: strings.TrimSpace(req.Fact)}
	if fact.Key == "" || fact.Fact == "" {
		writeError(w, http.StatusBadRequest, "key and fact are required")
		return
	}
	for _, a := range req.Aliases {
		a = strings.TrimSpace(a)
		if a == "" || strings.EqualFold(a, fact.Key) || slices.ContainsFunc(fact.Aliases, func(have string) bool { return strings.EqualFold(have, a) }) {
			continue
		}
		fact.Aliases = append(fact.Aliases, a)
	}
	saved, err := s.Store.UpsertWorldFact(scope, fact)
	switch {
	case errors.Is(err, store.ErrFactKeyTaken):
		writeError(w, http.StatusConflict, "another fact is already filed under "+fact.Key)
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	case saved == nil:
		writeError(w, http.StatusNotFound, "fact not found")
	default:
		writeJSON(w, http.StatusOK, idResponse{ID: saved.ID})
	}
}

func (s *Server) handleDeleteGlobalFact(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.DeleteWorldFact(store.GlobalFactScope, r.PathValue("factId")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

// handleDeleteBookFact removes one of the series' own facts; a global
// fact's id is left alone.
func (s *Server) handleDeleteBookFact(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	if _, err := s.Store.DeleteWorldFact(store.SeriesScope(book), r.PathValue("factId")); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

// affectedCharacterDTO is one character whose casting the facts in force
// bear on: the facts their material brings up.
type affectedCharacterDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Role: a pooled role label ("Guard") rather than one person.
	Role bool `json:"role"`
	// Notes are the "key: fact" lines their casting is shown.
	Notes []string `json:"notes"`
	// Summary is their casting as it stands.
	Summary string `json:"summary"`
}

// handleFactsAffected lists the characterized characters of the book's
// series whose material brings up a fact in force - the ones a recast
// would now read differently. It searches the series once per character,
// so it is slow on a long roster, and is not a live topic.
func (s *Server) handleFactsAffected(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	books, err := s.booksInScope(book)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	bookIDs := make([]string, len(books))
	for i, b := range books {
		bookIDs[i] = b.ID
	}
	characters, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []affectedCharacterDTO{}
	for _, char := range characters {
		if char.Invalid || char.Summary == "" {
			continue
		}
		if r.Context().Err() != nil {
			return
		}
		evidence, err := s.describedEvidence(books, bookIDs, char)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		notes, err := s.worldNotes(book, char, evidence)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(notes) > 0 {
			out = append(out, affectedCharacterDTO{ID: char.ID, Name: char.Name, Role: char.IsRole, Notes: notes, Summary: char.Summary})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
