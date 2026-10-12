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
	// errNoEdits: re-attributing around corrections (?scope=edits), but
	// the chapter has no corrected speakers.
	errNoEdits errorCode = "no_edits"
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

type pronounsResponse struct {
	Pronouns string `json:"pronouns"`
}

type characterizeResponse struct {
	Summary string `json:"summary"`
	// VoiceInvalidated is true when this run changed the characterization
	// enough that the character's voice was discarded for re-rendering.
	VoiceInvalidated bool `json:"voiceInvalidated"`
}

// characterizePreviewResponse is a characterization that was run but not
// stored.
type characterizePreviewResponse struct {
	Summary string `json:"summary"`
	RefLine string `json:"refLine"`
	// Lookups are the book searches the casting call made, each with how
	// many sentences it found.
	Lookups []string `json:"lookups"`
	// LookupExcerpts are the sentences those searches showed the casting
	// call, each under its search ("Scylla + Greek: ...").
	LookupExcerpts []string `json:"lookupExcerpts"`
	// Accent is the accent decided before the casting call, with the
	// sentence it rests on; absent when the step was off or failed.
	Accent *castAccentDTO `json:"accent,omitempty"`
	// CurrentSummary is the character's stored casting, for comparison.
	CurrentSummary string `json:"currentSummary"`
}

// castAccentDTO is the accent a casting was held to
// (speakerattr.AccentDecision).
type castAccentDTO struct {
	Accent string `json:"accent"`
	// Evidence is the sentence it rests on ("none" for the setting's),
	// Whose who the model read that sentence as being about, and Basis the
	// rule that decided it ("origin", "idiom", "setting", ...).
	Evidence string `json:"evidence"`
	Whose    string `json:"whose"`
	Basis    string `json:"basis"`
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

// epubChaptersResponse lists an uploaded epub's chapters
// (handleListEpubChapters).
type epubChaptersResponse struct {
	Chapters []epubChapterDTO `json:"chapters"`
}

type epubChapterDTO struct {
	// Index is the chapter's place in the epub, what "epubChapter" and
	// "first" take.
	Index      int    `json:"index"`
	Title      string `json:"title"`
	Opening    string `json:"opening"`
	Paragraphs int    `json:"paragraphs"`
	// BookChapter is the idx of the book's chapter that opens with the
	// same text, when a book was named and one does.
	BookChapter *int `json:"bookChapter,omitempty"`
}

// insertChaptersResponse answers handleInsertChapters.
type insertChaptersResponse struct {
	Inserted int `json:"inserted"`
}
