package httpapi

// Small fixed-shape responses and request bodies shared by several
// handlers. Named (rather than map literals or function-local structs) so
// cmd/apigen can generate the frontend's and Android client's copies of
// them.
//
// Response conventions: a GET returns its resource; a request that
// enqueues background work returns 202 queuedResponse; a create returns
// 201 with the new resource (or idResponse); a PUT replacing a whole
// resource may return it; a mutation whose result is a count or computed
// value returns that (cancel-all, reset, characterize); every other
// mutation returns 204 (writeNoContent) - clients see the effect through
// the live topics. Errors are always errorResponse.

import "net/http"

// writeNoContent answers a successful mutation with nothing to report.
func writeNoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// errorResponse is every non-2xx JSON body (see writeError/writeErrorCode).
type errorResponse struct {
	Error string `json:"error"`
	// Code is set only for errors the client is expected to recover from.
	Code errorCode `json:"code,omitempty"`
}

// errorCode is a machine-readable error reason a client can branch on
// (errorResponse.Code).
type errorCode string

const (
	// errNoSourceEpub: the book's source epub isn't stored - resend the
	// request with the file attached.
	errNoSourceEpub errorCode = "no_source_epub"
	// errTitleMismatch: the epub's chapter title differs from the
	// library's - retry with force=true if it's the right chapter.
	errTitleMismatch errorCode = "title_mismatch"
	// errBookExists: an uploaded lectable export is a book this library
	// already has - delete it first to replace it.
	errBookExists errorCode = "book_exists"
)

// queuedResponse is every 202 body: how many background tasks the request
// enqueued (1 for a single-item enqueue, even if it joined one already
// queued). A mutation with nothing to report answers 204 instead - see
// writeNoContent.
type queuedResponse struct {
	Queued int `json:"queued"`
}

type idResponse struct {
	ID string `json:"id"`
}

type instanceDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type llmTestResponse struct {
	Text string `json:"text"`
}

type canceledCountResponse struct {
	Canceled int `json:"canceled"`
}

type invalidatedResponse struct {
	Invalidated int `json:"invalidated"`
}

type languagesResponse struct {
	Languages []string `json:"languages"`
}

type aliasesResponse struct {
	Aliases []string `json:"aliases"`
}

type characterizeResponse struct {
	Summary string `json:"summary"`
	// VoiceInvalidated is true when this run changed the characterization
	// enough that the character's voice was discarded for re-rendering.
	VoiceInvalidated bool `json:"voiceInvalidated"`
}

type generateVoiceResponse struct {
	VoicePresetID string `json:"voicePresetId"`
	AudioURL      string `json:"audioUrl"`
}

type reimportChapterResponse struct {
	Title      string `json:"title"`
	Paragraphs int    `json:"paragraphs"`
}

// voicePresetsDTO is GET /api/voices/presets: the built-in presets plus
// which one is the factory default.
type voicePresetsDTO struct {
	Default string              `json:"default"`
	Presets []voicePresetDTOOut `json:"presets"`
}

// characterIDsRequest names a set of characters for a bulk speaker action.
type characterIDsRequest struct {
	CharacterIDs []string `json:"characterIds"`
}

// generateParagraphSFXRequest is POST .../sfx/generate's optional body.
type generateParagraphSFXRequest struct {
	Prompt          string   `json:"prompt"`
	DurationSeconds *float64 `json:"durationSeconds"`
}
