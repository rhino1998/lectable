package httpapi

import (
	"github.com/rhino1998/lectable/backend/internal/store"
	"net/http"
	"strings"
)

// searchResultLimit caps how many paragraphs a single search returns -
// plenty for a "jump to where this occurs" search, and cheap to hit even
// on a common short query against a full-length novel.
const searchResultLimit = 200

// meaningResultLimit caps a search by meaning, which always finds as many
// paragraphs as it is asked for: past the first few dozen they are no
// longer about the phrase.
const meaningResultLimit = 30

type searchResultDTO struct {
	ChapterIdx   int    `json:"chapterIdx"`
	ChapterTitle string `json:"chapterTitle"`
	ParagraphIdx int    `json:"paragraphIdx"`
	Text         string `json:"text"`
}

// handleSearchBook finds paragraphs of a book: those containing ?q= as
// written, in reading order, or with ?mode=meaning the ones closest to it
// in meaning, best first (semantic.Service.Search - "how long do kobolds
// live" finds "Kobolds only live to fifteen or so"). A search by meaning
// answers nothing for a book whose chapters haven't been embedded (a
// preprocessing phase), and 503 when there is no embedding model.
func (s *Server) handleSearchBook(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusOK, []searchResultDTO{})
		return
	}

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	if r.URL.Query().Get("mode") == "meaning" {
		s.searchBookByMeaning(w, r, book, q)
		return
	}

	results, err := s.Store.SearchParagraphs(bookID, q, searchResultLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]searchResultDTO, len(results))
	for i, res := range results {
		out[i] = searchResultDTO{
			ChapterIdx:   res.ChapterIdx,
			ChapterTitle: res.ChapterTitle,
			ParagraphIdx: res.ParagraphIdx,
			Text:         res.Text,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// searchBookByMeaning is handleSearchBook's ?mode=meaning.
func (s *Server) searchBookByMeaning(w http.ResponseWriter, r *http.Request, book *store.Book, q string) {
	if s.Semantic == nil {
		writeError(w, http.StatusServiceUnavailable, "search by meaning needs an embedding model")
		return
	}
	hits, err := s.Semantic.Search(r.Context(), []string{book.ID}, q, meaningResultLimit)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	titles := map[string]string{}
	out := make([]searchResultDTO, len(hits))
	for i, h := range hits {
		title, ok := titles[h.ChapterID]
		if !ok {
			if ch, err := s.Store.GetChapterByID(h.ChapterID); err == nil && ch != nil {
				title = ch.Title
			}
			titles[h.ChapterID] = title
		}
		out[i] = searchResultDTO{ChapterIdx: h.ChapterIdx, ChapterTitle: title, ParagraphIdx: h.ParagraphIdx, Text: h.Text}
	}
	writeJSON(w, http.StatusOK, out)
}
