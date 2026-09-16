package httpapi

import (
	"net/http"
	"strings"
)

// searchResultLimit caps how many paragraphs a single search returns -
// plenty for a "jump to where this occurs" search, and cheap to hit even
// on a common short query against a full-length novel.
const searchResultLimit = 200

type searchResultDTO struct {
	ChapterIdx   int    `json:"chapterIdx"`
	ChapterTitle string `json:"chapterTitle"`
	ParagraphIdx int    `json:"paragraphIdx"`
	Text         string `json:"text"`
}

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
