// Package httpapi wires the HTTP surface of the backend: library
// management, chapter/paragraph reading, TTS generation triggers, and
// audio/cover file serving.
package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/live"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsworker"
)

type Server struct {
	Store       *store.Store
	TTS         *ttsworker.Manager
	Jobs        *jobs.Manager
	DataDir     string
	AllowOrigin string
	// Live serves GET /api/events, the subscribe-to-topics WebSocket the
	// frontend keeps its state in sync through (see package live and
	// registerLiveTopics). nil leaves the route unregistered.
	Live *live.Hub
	// Narration resolves a paragraph's effective narration voice (a
	// character's assigned voice, when book.MultiVoice is on, overriding
	// the book's own) - see internal/narration.
	Narration *narration.Resolver
	// Speaker runs LLM-based speaker attribution (internal/speakerattr) -
	// nil if SPEAKER_LLM_BASE_URL isn't configured, in which case
	// handleAttributeSpeakers reports 503 rather than the rest of the app
	// failing to start; speaker/character data already in the store still
	// works (listing, voice assignment), only running new attribution
	// requires this.
	Speaker *speakerattr.Client
	// InstanceID is stable across restarts/network-address changes as long as DataDir
	// persists (see internal/instanceid) - lets a client that can be pointed at different
	// backends (the Android app - see android/CLAUDE.md) tell them apart reliably, rather
	// than assuming two backends are the same just because a book ID happens to match (IDs
	// are only unique within one backend's own database) or keying on the backend's URL
	// (which breaks on every DHCP lease change for a LAN-hosted backend).
	InstanceID string
	// LibraryName is an optional human-readable label (LIBRARY_NAME env var, see cmd/server) for
	// telling backends apart in a client's UI - purely cosmetic, never used for the cache-scoping
	// InstanceID is for. "" (the default) means the deployer never set one.
	LibraryName string

	// provisionLocks is a keyed mutex (characterID -> *sync.Mutex) used by
	// lockCharacterProvision to serialize provisionCharacterVoice per
	// character - see its own doc comment. sync.Map's zero value is ready
	// to use, matching Server's own construction as a plain struct
	// literal (see cmd/server/main.go) rather than through a constructor.
	provisionLocks sync.Map

	// syncTreePrune drops stored offline-sync tree nodes left by other
	// builds, once per process, on the first manifest request - see
	// syncTreeVersion.
	syncTreePrune sync.Once
}

// cancelPipeline cancels bookID's in-progress preprocessing pipeline run,
// if any - a thin wrapper around jobs.Manager.CancelPipeline, kept as its
// own Server method so handleDeleteBook's own call site (and doc comment,
// explaining *why* a book being deleted needs this) didn't have to change
// when pipeline runs moved from a hand-tracked sync.Map of CancelFuncs
// (Server.pipelineRunning, now gone) to real dependency-ordered tasks in
// jobs.Manager's own pipelineQueue. See CancelPipeline's own doc comment
// for what "best-effort" means here.
func (s *Server) cancelPipeline(bookID string) {
	s.Jobs.CancelPipeline(bookID)
}

func NewRouter(s *Server) http.Handler {
	// Wired here (rather than by cmd/server/main.go directly) since
	// provisionCharacterVoice/characterizeCharacterForJob/directChapterForJob/
	// attributeChapterForJob are unexported - they need Store/Speaker/TTS
	// access jobs.Manager doesn't have (see jobs.CharacterVoiceProvisioner/
	// CharacterCharacterizer/ChapterDirector/ChapterAttributor's own doc
	// comments), but s.Jobs itself is already populated by the time a
	// caller has a *Server to pass here. Safe to set any time before the
	// HTTP server actually starts accepting requests, since the background
	// worker (already started by cmd/server/main.go by this point) can't
	// call any of them until some paragraph's generation is enqueued.
	if s.Jobs != nil {
		s.Jobs.SetCharacterVoiceProvisioner(s.provisionCharacterVoice)
		// characterizeCharacterForJob/directChapterForJob/
		// attributeChapterForJob all go straight to s.Speaker
		// (characterizeVoice/directChapter/attributeChapter, no
		// book.MultiVoice gate the way provisionCharacterVoice's own
		// caller has - see jobs.Manager.characterizationDependency/
		// speechDirectionDependency/attributionOrderDependency, which fire
		// for any attributed speaker/chapter regardless of MultiVoice), so
		// leaving these three unwired when Speaker isn't configured (same
		// as handleCharacterizeSpeakers/handleTagDirections'/
		// handleAttributeSpeakers' own explicit-endpoint guards) is what
		// keeps characterizationDependency/speechDirectionDependency/
		// attributionOrderDependency's own nil-hook no-op path correct
		// rather than crashing the worker goroutine the first time any of
		// them fires.
		if s.Speaker != nil {
			s.Jobs.SetCharacterCharacterizer(s.characterizeCharacterForJob)
			s.Jobs.SetChapterDirector(s.directChapterForJob)
			s.Jobs.SetChapterAttributor(s.attributeChapterForJob)
			s.Jobs.SetChapterScareQuoter(s.scareQuoteChapterForJob)
			s.Jobs.SetChapterPronouncer(s.pronounceChapterForJob)
			// scoreChapterMusic itself calls s.Speaker.ScoreMusic - same
			// s.Speaker-configured gate as the three above, and the same
			// reason jobs.Manager.maybeScoreChapterMusic's own nil-scorer
			// check must stay correct rather than panicking the worker
			// goroutine the first time a chapter generates with MusicEnabled
			// on but no speaker LLM configured.
			s.Jobs.SetMusicScorer(s.scoreChapterMusic)
		}
	}

	mux := http.NewServeMux()

	if s.Live != nil {
		s.registerLiveTopics(s.Live)
		mux.HandleFunc("GET /api/events", s.handleEvents)
	}

	mux.HandleFunc("GET /api/instance", s.handleGetInstance)

	mux.HandleFunc("POST /api/books", s.handleUploadBook)
	mux.HandleFunc("GET /api/books", s.handleListBooks)
	mux.HandleFunc("GET /api/books/{id}", s.handleGetBook)
	mux.HandleFunc("DELETE /api/books/{id}", s.handleDeleteBook)
	mux.HandleFunc("DELETE /api/books/{id}/audio", s.handleDeleteBookAudio)
	mux.HandleFunc("GET /api/books/{id}/cover", s.handleGetCover)
	mux.HandleFunc("GET /api/books/{id}/manifest", s.handleBookManifest)

	mux.HandleFunc("GET /api/books/{id}/voice", s.handleGetVoice)
	mux.HandleFunc("PUT /api/books/{id}/voice", s.handleUpdateVoice)

	mux.HandleFunc("GET /api/books/{id}/position", s.handleGetPosition)
	mux.HandleFunc("PUT /api/books/{id}/position", s.handleUpdatePosition)

	mux.HandleFunc("GET /api/books/{id}/search", s.handleSearchBook)

	mux.HandleFunc("GET /api/books/{id}/bookmarks", s.handleListBookmarks)
	mux.HandleFunc("POST /api/books/{id}/bookmarks", s.handleCreateBookmark)
	mux.HandleFunc("PUT /api/bookmarks/{id}", s.handleUpdateBookmark)
	mux.HandleFunc("DELETE /api/bookmarks/{id}", s.handleDeleteBookmark)

	mux.HandleFunc("GET /api/books/{id}/chapters/{idx}", s.handleGetChapter)
	mux.HandleFunc("POST /api/books/{id}/generate", s.handleGenerateBook)
	mux.HandleFunc("POST /api/books/{id}/generate-remaining", s.handleGenerateRemaining)
	mux.HandleFunc("POST /api/books/{id}/chapters/{idx}/generate", s.handleGenerateChapter)
	mux.HandleFunc("POST /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/regenerate", s.handleRegenerateParagraph)
	mux.HandleFunc("PUT /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/speaker", s.handleSetParagraphSpeaker)
	mux.HandleFunc("PUT /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/description", s.handleSetParagraphDescription)
	mux.HandleFunc("PUT /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/scare-quote", s.handleSetParagraphScareQuote)
	mux.HandleFunc("PUT /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/emotion", s.handleSetParagraphEmotion)
	mux.HandleFunc("PUT /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/sfx-prompt", s.handleSetParagraphSFXPrompt)
	mux.HandleFunc("POST /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/generate-sfx", s.handleGenerateParagraphSFX)
	mux.HandleFunc("POST /api/books/{id}/chapters/{idx}/attribute-speakers", s.handleAttributeSpeakers)
	mux.HandleFunc("POST /api/books/{id}/chapters/{idx}/retag-descriptions", s.handleRetagDescriptions)
	mux.HandleFunc("POST /api/books/{id}/chapters/{idx}/retag-scare-quotes", s.handleRetagScareQuotes)
	mux.HandleFunc("POST /api/books/{id}/chapters/{idx}/tag-directions", s.handleTagDirections)
	mux.HandleFunc("POST /api/books/{id}/chapters/{idx}/resolve-pronunciation", s.handleResolvePronunciation)
	mux.HandleFunc("GET /api/books/{id}/chapters/{idx}/music", s.handleGetChapterMusic)
	mux.HandleFunc("POST /api/books/{id}/chapters/{idx}/score-music", s.handleScoreChapterMusic)
	mux.HandleFunc("POST /api/books/{id}/chapters/{idx}/generate-music", s.handleGenerateChapterMusic)
	mux.HandleFunc("GET /api/music-regions/{id}/audio", s.handleGetMusicRegionAudio)
	mux.HandleFunc("POST /api/music-regions/{id}/regenerate", s.handleRegenerateMusicRegion)
	mux.HandleFunc("POST /api/books/{id}/preprocess", s.handlePreprocessBook)
	mux.HandleFunc("POST /api/books/{id}/bulk/{action}", s.handleBulkAction)
	mux.HandleFunc("POST /api/books/{id}/reset/{pass}", s.handleResetPass)
	mux.HandleFunc("DELETE /api/books/{id}/chapters/{idx}/audio", s.handleDeleteChapterAudio)
	mux.HandleFunc("POST /api/books/{id}/chapters/{idx}/reimport", s.handleReimportChapter)
	mux.HandleFunc("POST /api/books/{id}/lookahead", s.handleLookahead)

	mux.HandleFunc("GET /api/books/{id}/speakers", s.handleListSpeakers)
	mux.HandleFunc("DELETE /api/books/{id}/speakers", s.handleDeleteBookSpeakerData)
	mux.HandleFunc("POST /api/books/{id}/speakers/reattribute", s.handleReattributeSpeaker)
	mux.HandleFunc("POST /api/books/{id}/speakers/variants/regenerate", s.handleRegenerateVariant)
	mux.HandleFunc("GET /api/voices/variants/{presetId}/{emotion}/audio", s.handleVariantAudio)
	mux.HandleFunc("PUT /api/books/{id}/characters/{characterId}/voice", s.handleSetCharacterVoice)
	mux.HandleFunc("PUT /api/books/{id}/characters/{characterId}/invalid", s.handleSetCharacterInvalid)
	mux.HandleFunc("DELETE /api/books/{id}/characters/{characterId}", s.handleDeleteCharacter)
	mux.HandleFunc("POST /api/books/{id}/characters/{characterId}/merge", s.handleMergeCharacter)
	mux.HandleFunc("POST /api/books/{id}/characters/{characterId}/generate-voice", s.handleGenerateCharacterVoice)
	mux.HandleFunc("POST /api/books/{id}/characters/generate-voice", s.handleGenerateCharacterVoices)
	mux.HandleFunc("POST /api/books/{id}/characters/regenerate-voice", s.handleRegenerateCharacterVoices)
	mux.HandleFunc("GET /api/books/{id}/characters/{characterId}/appearances", s.handleCharacterAppearances)
	mux.HandleFunc("GET /api/books/{id}/characters/{characterId}/descriptions", s.handleCharacterDescriptions)
	mux.HandleFunc("POST /api/books/{id}/characters/{characterId}/characterize", s.handleCharacterizeSpeaker)
	mux.HandleFunc("POST /api/books/{id}/characters/characterize", s.handleCharacterizeSpeakers)

	mux.HandleFunc("GET /api/jobs", s.handleJobsSnapshot)
	mux.HandleFunc("DELETE /api/jobs", s.handleCancelAllJobs)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.handleCancelJob)
	mux.HandleFunc("PUT /api/jobs/{id}/tier", s.handleSetJobTier)
	mux.HandleFunc("POST /api/jobs/pause", s.handlePauseJobs)
	mux.HandleFunc("POST /api/jobs/resume", s.handleResumeJobs)
	mux.HandleFunc("POST /api/jobs/restart-worker", s.handleRestartWorker)

	mux.HandleFunc("GET /api/paragraphs/{id}/audio", s.handleGetAudio)
	mux.HandleFunc("GET /api/paragraphs/{id}/sfx-audio", s.handleGetParagraphSFXAudio)
	mux.HandleFunc("POST /api/sfx/generate", s.handleGenerateSFX)
	mux.HandleFunc("POST /api/llm/test", s.handleTestLLM)
	mux.HandleFunc("GET /api/images/{id}", s.handleGetImage)

	mux.HandleFunc("GET /api/voices/presets", s.handleVoicePresets)
	mux.HandleFunc("GET /api/voices/presets/{id}/audio", s.handleGetPresetAudio)
	mux.HandleFunc("POST /api/voices/presets/{id}/test", s.handleTestPreset)
	mux.HandleFunc("POST /api/voices/presets/{id}/regenerate", s.handleRegeneratePreset)
	mux.HandleFunc("POST /api/voices/design-test", s.handleTestVoiceDesign)
	mux.HandleFunc("POST /api/voices/clone-instruct-test", s.handleTestCloneInstruct)
	mux.HandleFunc("GET /api/voices/languages", s.handleVoiceLanguages)
	mux.HandleFunc("GET /api/voices/default", s.handleGetDefaultVoice)
	mux.HandleFunc("PUT /api/voices/default", s.handleUpdateDefaultVoice)

	mux.HandleFunc("GET /api/voices/custom-presets", s.handleListCustomVoicePresets)
	mux.HandleFunc("POST /api/voices/custom-presets", s.handleCreateCustomVoicePreset)
	mux.HandleFunc("PUT /api/voices/custom-presets/{id}", s.handleUpdateCustomVoicePreset)
	mux.HandleFunc("DELETE /api/voices/custom-presets/{id}", s.handleDeleteCustomVoicePreset)
	mux.HandleFunc("GET /api/voices/custom-presets/{id}/audio", s.handleGetCustomVoiceAudio)
	mux.HandleFunc("POST /api/voices/custom-presets/{id}/test", s.handleTestCustomVoicePreset)
	mux.HandleFunc("POST /api/voices/custom-presets/{id}/regenerate", s.handleRegenerateCustomVoicePreset)

	return withCORS(s.AllowOrigin, withLogging(mux))
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		log.Printf("%s %s", r.Method, r.URL.Path)
	})
}

func withCORS(allowOrigin string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowOrigin != "" {
			w.Header().Set("Access-Control-Allow-Origin", allowOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// Every JSON response is dynamic and often polled on a fixed URL (the
	// Jobs page's GET /api/jobs every 2s, a chapter's own 1.5s poll while
	// paragraphs are pending, etc.) - without this, a browser can serve a
	// stale cached response instead of hitting the network on some of
	// those polls (no Cache-Control at all lets heuristic caching kick
	// in), making a live view look frozen even though the server side is
	// actually progressing.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// writeErrorCode is writeError plus a machine-readable "code" the client
// can branch on (e.g. handleReimportChapter's "no_source_epub"), for an
// error it's expected to recover from rather than just display.
func writeErrorCode(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}

// httpError is how a build* function (the shared core of a GET handler and
// its live topic - see live.go) reports a failure with a specific status;
// any other error it returns is a 500.
func httpError(status int, message string) error {
	return &live.Error{Status: status, Message: message}
}

// writeBuilt writes a build* function's (value, error) result as a GET
// response - usage: writeBuilt(w)(s.buildBook(id)).
func writeBuilt(w http.ResponseWriter) func(any, error) {
	return func(v any, err error) {
		if err != nil {
			var le *live.Error
			if errors.As(err, &le) {
				writeError(w, le.Status, le.Message)
			} else {
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

func writeLoggedWarning(r *http.Request, format string, args ...any) {
	log.Printf("warn: "+format+" (request: %s %s)", append(append([]any{}, args...), r.Method, r.URL.Path)...)
}

func (s *Server) chapterByID(id string) (*store.Chapter, error) {
	return s.Store.GetChapterByID(id)
}

func (s *Server) paragraphAudioPath(bookID, chapterID, voiceID string, idx int) string {
	return audiopath.ParagraphFile(s.DataDir, bookID, chapterID, voiceID, idx)
}
