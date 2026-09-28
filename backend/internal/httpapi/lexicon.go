package httpapi

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// lexiconEntryDTO is one pronunciation rule (store.LexiconEntry) - shared
// by every book in the book's series (store.SeriesScope), like the
// character roster.
type lexiconEntryDTO struct {
	ID            string `json:"id"`
	Term          string `json:"term"`
	Replacement   string `json:"replacement"`
	CaseSensitive bool   `json:"caseSensitive"`
	// Matches is how many paragraphs in this book the rule applies to.
	Matches int `json:"matches"`
}

func (s *Server) handleListLexicon(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildLexicon(r.PathValue("id"))
	writeBuilt(w, v, err)
}

// buildLexicon lists bookID's scope's pronunciation rules, each with how
// many of this book's paragraphs it matches (so a typo'd rule that never
// fires is visible).
func (s *Server) buildLexicon(bookID string) ([]lexiconEntryDTO, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return nil, httpError(http.StatusNotFound, "book not found")
	}
	entries, err := s.Store.ListLexicon(store.SeriesScope(book))
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	out := make([]lexiconEntryDTO, len(entries))
	if len(entries) == 0 {
		return out, nil
	}
	paragraphs, err := s.Store.ListParagraphsRawForBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	for i, e := range entries {
		out[i] = lexiconEntryDTO{
			ID: e.ID, Term: e.Term, Replacement: e.Replacement, CaseSensitive: e.CaseSensitive,
			Matches: len(matchingParagraphs(paragraphs, e.LexiconEntry)),
		}
	}
	return out, nil
}

type upsertLexiconRequest struct {
	Term          string `json:"term"`
	Replacement   string `json:"replacement"`
	CaseSensitive bool   `json:"caseSensitive"`
}

// handleUpsertLexicon adds a pronunciation rule to the book's series, or
// replaces the one for the same term (case-insensitively), then clears
// generated audio for every paragraph in the series that the old or new
// rule matches, so it's re-rendered with the new pronunciation.
func (s *Server) handleUpsertLexicon(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	var req upsertLexiconRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Term = strings.TrimSpace(req.Term)
	req.Replacement = strings.TrimSpace(req.Replacement)
	if req.Term == "" || req.Replacement == "" {
		writeError(w, http.StatusBadRequest, "term and replacement are required")
		return
	}
	entry, previous, err := s.Store.UpsertLexiconEntry(store.SeriesScope(book), pronounce.LexiconEntry{
		Term: req.Term, Replacement: req.Replacement, CaseSensitive: req.CaseSensitive,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rules := []pronounce.LexiconEntry{entry.LexiconEntry}
	if previous != nil {
		rules = append(rules, previous.LexiconEntry)
	}
	s.invalidateLexiconMatches(book, rules)
	writeJSON(w, http.StatusOK, idResponse{ID: entry.ID})
}

// handleDeleteLexicon removes a pronunciation rule and clears generated
// audio for every paragraph in its series that the rule matched.
func (s *Server) handleDeleteLexicon(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	entry, err := s.Store.DeleteLexiconEntry(r.PathValue("entryId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if entry != nil && entry.Scope == store.SeriesScope(book) {
		s.invalidateLexiconMatches(book, []pronounce.LexiconEntry{entry.LexiconEntry})
	}
	writeNoContent(w)
}

func (s *Server) lexiconBook(w http.ResponseWriter, bookID string) (*store.Book, bool) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return nil, false
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return nil, false
	}
	return book, true
}

// invalidateLexiconMatches resets generated audio for every paragraph, in
// every book sharing book's lexicon scope, that any of rules matches.
// Best-effort like invalidateParagraphAudio: the rule itself is already
// saved, so a failure here is logged, not returned.
func (s *Server) invalidateLexiconMatches(book *store.Book, rules []pronounce.LexiconEntry) {
	books, err := s.booksInScope(book)
	if err != nil {
		log.Printf("lexicon: list books in scope of %s: %v", book.ID, err)
		return
	}
	for i := range books {
		b := &books[i]
		paragraphs, err := s.Store.ListParagraphsRawForBook(b.ID)
		if err != nil {
			log.Printf("lexicon: list paragraphs of %s: %v", b.ID, err)
			continue
		}
		var hit []store.Paragraph
		for _, rule := range rules {
			hit = append(hit, matchingParagraphs(paragraphs, rule)...)
		}
		s.invalidateParagraphAudio(b, dedupParagraphs(hit))
	}
}

func matchingParagraphs(paragraphs []store.Paragraph, rule pronounce.LexiconEntry) []store.Paragraph {
	var out []store.Paragraph
	rules := []pronounce.LexiconEntry{rule}
	for _, p := range paragraphs {
		if len(pronounce.LexiconSubstitutions(p.Text, rules)) > 0 {
			out = append(out, p)
		}
	}
	return out
}

func dedupParagraphs(ps []store.Paragraph) []store.Paragraph {
	seen := make(map[string]bool, len(ps))
	out := ps[:0]
	for _, p := range ps {
		if !seen[p.ID] {
			seen[p.ID] = true
			out = append(out, p)
		}
	}
	return out
}
