package httpapi

import (
	"encoding/json"
	"net/http"
)

type positionDTO struct {
	ChapterIdx   int     `json:"chapterIdx"`
	ParagraphIdx int     `json:"paragraphIdx"`
	Seconds      float64 `json:"seconds"`
}

func (s *Server) handleGetPosition(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.Store.GetBook(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	writeJSON(w, http.StatusOK, positionDTO{
		ChapterIdx:   b.PosChapterIdx,
		ParagraphIdx: b.PosParagraphIdx,
		Seconds:      b.PosSeconds,
	})
}

func (s *Server) handleUpdatePosition(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.Store.GetBook(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	var req positionDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.Store.UpdatePosition(id, req.ChapterIdx, req.ParagraphIdx, req.Seconds); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

type markReadRequest struct {
	Read bool `json:"read"`
}

// handleMarkRead marks a book read or unread from the libraries' own
// button, without opening it: read moves the saved position to the book's
// last paragraph, unread back to its start (see Store.MarkBookRead).
func (s *Server) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	b, err := s.Store.GetBook(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if b == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	var req markReadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := s.Store.MarkBookRead(id, req.Read); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}
