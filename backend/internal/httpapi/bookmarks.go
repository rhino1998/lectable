package httpapi

import (
	"encoding/json"
	"net/http"
)

type bookmarkDTO struct {
	ID           string `json:"id"`
	ChapterIdx   int    `json:"chapterIdx"`
	ChapterTitle string `json:"chapterTitle"`
	ParagraphIdx int    `json:"paragraphIdx"`
	Text         string `json:"text"`
	Note         string `json:"note"`
	CreatedAt    int64  `json:"createdAt"`
}

func (s *Server) handleListBookmarks(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")

	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	bookmarks, err := s.Store.ListBookmarks(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make([]bookmarkDTO, len(bookmarks))
	for i, b := range bookmarks {
		out[i] = bookmarkDTO{
			ID: b.ID, ChapterIdx: b.ChapterIdx, ChapterTitle: b.ChapterTitle,
			ParagraphIdx: b.ParagraphIdx, Text: b.Text, Note: b.Note, CreatedAt: b.CreatedAt,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

type createBookmarkRequest struct {
	ChapterIdx   int    `json:"chapterIdx"`
	ParagraphIdx int    `json:"paragraphIdx"`
	Note         string `json:"note"`
}

// handleCreateBookmark flags a paragraph (addressed the same way the rest
// of the public API does, by chapterIdx/paragraphIdx rather than its
// internal id) as bookmarked, or updates its note if it already is - see
// Store.UpsertBookmark.
func (s *Server) handleCreateBookmark(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")

	var req createBookmarkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ch, err := s.Store.GetChapterByIdx(bookID, req.ChapterIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}

	paragraphID, err := s.Store.GetParagraphIDByIdx(ch.ID, req.ParagraphIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if paragraphID == "" {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return
	}

	id, err := s.Store.UpsertBookmark(paragraphID, req.Note)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

type updateBookmarkRequest struct {
	Note string `json:"note"`
}

func (s *Server) handleUpdateBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req updateBookmarkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if err := s.Store.UpdateBookmarkNote(id, req.Note); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.DeleteBookmark(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
