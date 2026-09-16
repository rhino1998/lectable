package httpapi

import "net/http"

// handleGetInstance reports this backend's stable identity (see
// internal/instanceid) - a client caching data locally (the Android app's
// offline downloads - see android/CLAUDE.md) uses this to scope that cache
// per-backend, since book IDs alone are only unique within one backend.
// Name is the optional LIBRARY_NAME operators can set purely so a client
// juggling multiple backends has something human-readable to show, distinct
// from id (never meant to be read by a person).
func (s *Server) handleGetInstance(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"id": s.InstanceID, "name": s.LibraryName})
}
