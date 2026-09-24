package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsworker/ttsworkertest"
)

// newTestServer wires a full httpapi.Server against a temp DuckDB store, a
// temp data directory, and a real jobs.Manager pointed at a fake ttsworker
// (see ttsworkertest) instead of a real one - no GPU, no audio.cpp, no
// network beyond loopback. Speaker is deliberately left nil, matching a
// deployment with SPEAKER_LLM_MODEL_PATH unset (see Server.Speaker's own
// doc comment) - LLM-backed endpoints are expected to report 503 in this
// configuration, exercised explicitly below rather than left untested.
func newTestServer(t *testing.T) (*Server, *store.DuckStore, *ttsworkertest.Server, *httptest.Server) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "library.duckdb"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// Everything reads through the cache, as in cmd/server, so these tests
	// also catch a write that fails to invalidate what it changed.
	cached := store.NewCached(s)

	fake := ttsworkertest.New(t)
	dataDir := t.TempDir()
	tts := fake.Manager()
	mgr := jobs.NewManager(cached, tts, dataDir)

	srv := &Server{
		Store:       cached,
		TTS:         tts,
		Jobs:        mgr,
		DataDir:     dataDir,
		AllowOrigin: "http://localhost:5173",
		Narration:   narration.NewResolver(cached),
		InstanceID:  "test-instance",
	}
	router := NewRouter(srv)
	ts := httptest.NewServer(router)
	t.Cleanup(ts.Close)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	mgr.Start(ctx)

	return srv, s, fake, ts
}

func createTestBook(t *testing.T, s *store.DuckStore, seriesName string, seriesIndex float64, paragraphs ...string) string {
	t.Helper()
	blocks := make([]store.BlockInput, len(paragraphs))
	for i, p := range paragraphs {
		blocks[i] = store.BlockInput{Kind: store.BlockText, Text: p}
	}
	bookID, _, err := s.CreateBook("Test Book", "Test Author", "en", "", seriesName, seriesIndex, []store.ChapterInput{
		{Title: "Chapter One", Blocks: blocks},
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	return bookID
}

func doJSON(t *testing.T, method, url string, body any, out any) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	if out != nil {
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode response from %s %s: %v", method, url, err)
		}
	}
	return resp
}

func TestBookLifecycle(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "p1")

	var list []bookSummaryDTO
	resp := doJSON(t, http.MethodGet, ts.URL+"/api/books", nil, &list)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/books: status %d", resp.StatusCode)
	}
	if len(list) != 1 || list[0].ID != bookID {
		t.Fatalf("unexpected book list: %+v", list)
	}

	var detail bookDetailDTO
	resp = doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID, nil, &detail)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/books/{id}: status %d", resp.StatusCode)
	}
	if len(detail.Chapters) != 1 || detail.Chapters[0].ParagraphCount != 1 {
		t.Fatalf("unexpected book detail: %+v", detail)
	}

	resp = doJSON(t, http.MethodGet, ts.URL+"/api/books/does-not-exist", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for a missing book, got %d", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodDelete, ts.URL+"/api/books/"+bookID, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /api/books/{id}: status %d", resp.StatusCode)
	}
	resp = doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID, nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", resp.StatusCode)
	}
}

func TestVoiceSettingsRoundTrip(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "p1")

	var got voiceSettingsDTO
	resp := doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/voice", nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET voice: status %d", resp.StatusCode)
	}
	if got.PresetID == "" {
		t.Fatalf("expected a default preset id, got empty")
	}

	update := voiceSettingsDTO{PresetID: "", Instruct: "speak like a robot", Language: "English", CharacterVoiceMode: string(store.CharacterVoiceModeAssigned)}
	var updated voiceSettingsDTO
	resp = doJSON(t, http.MethodPut, ts.URL+"/api/books/"+bookID+"/voice", update, &updated)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT voice: status %d", resp.StatusCode)
	}
	if updated.Instruct != "speak like a robot" || updated.CharacterVoiceMode != string(store.CharacterVoiceModeAssigned) {
		t.Fatalf("update didn't apply: %+v", updated)
	}

	resp = doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/voice", nil, &got)
	if resp.StatusCode != http.StatusOK || got.Instruct != "speak like a robot" {
		t.Fatalf("voice update didn't persist: %+v", got)
	}
}

func TestPositionRoundTrip(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "p1", "p2")

	update := positionDTO{ChapterIdx: 0, ParagraphIdx: 1, Seconds: 12.5}
	resp := doJSON(t, http.MethodPut, ts.URL+"/api/books/"+bookID+"/position", update, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT position: status %d", resp.StatusCode)
	}

	var got positionDTO
	resp = doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/position", nil, &got)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET position: status %d", resp.StatusCode)
	}
	if got != update {
		t.Fatalf("expected %+v, got %+v", update, got)
	}
}

func TestBookmarkLifecycle(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "p1")

	create := createBookmarkRequest{ChapterIdx: 0, ParagraphIdx: 0, Note: "remember this"}
	// handleCreateBookmark's own response is just {"id": ...} - the full
	// bookmark (Note included) is only ever returned by the list endpoint,
	// which this test checks right after.
	var created map[string]string
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/bookmarks", create, &created)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST bookmark: status %d", resp.StatusCode)
	}
	if created["id"] == "" {
		t.Fatalf("expected a non-empty bookmark id, got %+v", created)
	}

	var list []bookmarkDTO
	resp = doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/bookmarks", nil, &list)
	if resp.StatusCode != http.StatusOK || len(list) != 1 {
		t.Fatalf("unexpected bookmark list: status=%d list=%+v", resp.StatusCode, list)
	}
	if list[0].Note != "remember this" {
		t.Fatalf("unexpected bookmark note: %+v", list[0])
	}

	resp = doJSON(t, http.MethodDelete, ts.URL+"/api/bookmarks/"+created["id"], nil, nil)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE bookmark: status %d", resp.StatusCode)
	}
	resp = doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/bookmarks", nil, &list)
	if resp.StatusCode != http.StatusOK || len(list) != 0 {
		t.Fatalf("expected no bookmarks after delete, got %+v", list)
	}
}

func TestChapterGenerateEndToEnd(t *testing.T) {
	_, s, fake, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "First paragraph.", "Second paragraph.")

	var chapter chapterDetailDTO
	resp := doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/chapters/0", nil, &chapter)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET chapter: status %d", resp.StatusCode)
	}
	if len(chapter.Paragraphs) != 2 {
		t.Fatalf("expected 2 paragraphs, got %d", len(chapter.Paragraphs))
	}
	for _, p := range chapter.Paragraphs {
		if p.AudioStatus != store.AudioPending {
			t.Fatalf("expected a fresh paragraph to be pending, got %q", p.AudioStatus)
		}
	}

	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/chapters/0/generate", nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST generate: status %d", resp.StatusCode)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp = doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/chapters/0", nil, &chapter)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET chapter (polling): status %d", resp.StatusCode)
		}
		allReady := true
		for _, p := range chapter.Paragraphs {
			if p.AudioStatus != store.AudioReady {
				allReady = false
			}
		}
		if allReady {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("chapter did not finish generating within 5s: %+v", chapter.Paragraphs)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, p := range chapter.Paragraphs {
		if p.AudioURL == "" {
			t.Fatalf("expected an audio URL for a ready paragraph, got %+v", p)
		}
	}
	if len(fake.GenerateCalls()) != 2 {
		t.Fatalf("expected 2 Generate calls (one per paragraph), got %d", len(fake.GenerateCalls()))
	}
}

func TestJobsSnapshotAndCancelAll(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "p1", "p2", "p3")

	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/chapters/0/generate", nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST generate: status %d", resp.StatusCode)
	}

	// There's necessarily a queued/in-flight task immediately after
	// enqueueing (maxInFlight=4 well exceeds this test's 3 paragraphs), so
	// don't race the worker goroutine actually draining the queue - just
	// confirm the snapshot endpoint responds with the right shape and that
	// cancel-all reports success, rather than depending on exact timing of
	// what's still queued vs. already finished.
	var snap jobsSnapshotDTO
	resp = doJSON(t, http.MethodGet, ts.URL+"/api/jobs", nil, &snap)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/jobs: status %d", resp.StatusCode)
	}

	var cancelResult map[string]int
	resp = doJSON(t, http.MethodDelete, ts.URL+"/api/jobs", nil, &cancelResult)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /api/jobs: status %d", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodDelete, ts.URL+"/api/jobs/nonexistent-id", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 canceling a nonexistent job, got %d", resp.StatusCode)
	}
	_ = srv
}

// TestSetJobTier exercises PUT /api/jobs/{id}/tier end to end: a freshly
// queued paragraph task defaults to "background" (see jobs.TierBackground/
// EnqueueChapter), bumping it to "urgent" succeeds and is reflected back
// on the next GET /api/jobs, a second bump to a tier that isn't strictly
// more urgent (same tier, or "background" - a downgrade) is rejected
// (409), an invalid tier string is rejected (400), and an unknown id is
// rejected (404) - see jobs.Manager.PromoteTier's own doc comment for why
// this only ever allows raising urgency.
func TestSetJobTier(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "p1", "p2", "p3")

	// Paused first so the fake ttsworker (which resolves near-instantly -
	// see manager_test.go's own TestPauseStopsNewDispatchButNotInFlight)
	// can't drain the queue out from under this test before it gets a
	// chance to look up a still-queued task's own id.
	var pauseResult map[string]bool
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/jobs/pause", nil, &pauseResult)
	if resp.StatusCode != http.StatusOK || !pauseResult["paused"] {
		t.Fatalf("POST /api/jobs/pause: status %d body %v", resp.StatusCode, pauseResult)
	}

	resp = doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/chapters/0/generate", nil, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST generate: status %d", resp.StatusCode)
	}

	// EnqueueChapter resolves and pushes its paragraphs from a detached
	// goroutine (see its own doc comment), so poll briefly rather than
	// assuming they've landed the instant the 202 above returns.
	var snap jobsSnapshotDTO
	var all []queueTaskDTO
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp = doJSON(t, http.MethodGet, ts.URL+"/api/jobs", nil, &snap)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/jobs: status %d", resp.StatusCode)
		}
		all = append(append([]queueTaskDTO{}, snap.InFlight...), snap.Queued...)
		if len(all) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected at least one queued/in-flight task within 2s, got none")
		}
		time.Sleep(10 * time.Millisecond)
	}
	id := all[0].ID
	if all[0].Tier != "background" {
		t.Fatalf("expected a freshly enqueued task to start at background tier, got %q", all[0].Tier)
	}

	var promoteResult map[string]bool
	resp = doJSON(t, http.MethodPut, ts.URL+"/api/jobs/"+id+"/tier", map[string]string{"tier": "urgent"}, &promoteResult)
	if resp.StatusCode != http.StatusOK || !promoteResult["promoted"] {
		t.Fatalf("PUT /api/jobs/%s/tier: status %d body %v", id, resp.StatusCode, promoteResult)
	}

	resp = doJSON(t, http.MethodGet, ts.URL+"/api/jobs", nil, &snap)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/jobs: status %d", resp.StatusCode)
	}
	all = append(append([]queueTaskDTO{}, snap.InFlight...), snap.Queued...)
	var promoted *queueTaskDTO
	for i := range all {
		if all[i].ID == id {
			promoted = &all[i]
		}
	}
	if promoted == nil || promoted.Tier != "urgent" {
		t.Fatalf("expected task %s to report tier=urgent after promotion, got %+v", id, promoted)
	}

	// Re-requesting the same (already-current) tier isn't an upgrade.
	resp = doJSON(t, http.MethodPut, ts.URL+"/api/jobs/"+id+"/tier", map[string]string{"tier": "urgent"}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 re-requesting the current tier, got %d", resp.StatusCode)
	}

	// A downgrade is rejected the same way.
	resp = doJSON(t, http.MethodPut, ts.URL+"/api/jobs/"+id+"/tier", map[string]string{"tier": "background"}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 downgrading a task's tier, got %d", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodPut, ts.URL+"/api/jobs/"+id+"/tier", map[string]string{"tier": "not-a-real-tier"}, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid tier string, got %d", resp.StatusCode)
	}

	resp = doJSON(t, http.MethodPut, ts.URL+"/api/jobs/nonexistent-id/tier", map[string]string{"tier": "urgent"}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 promoting a nonexistent job, got %d", resp.StatusCode)
	}
}

func TestJobsPauseAndResume(t *testing.T) {
	_, _, _, ts := newTestServer(t)

	var snap jobsSnapshotDTO
	resp := doJSON(t, http.MethodGet, ts.URL+"/api/jobs", nil, &snap)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/jobs: status %d", resp.StatusCode)
	}
	if snap.Paused {
		t.Fatalf("expected the queue not to start paused")
	}

	var pauseResult map[string]bool
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/jobs/pause", nil, &pauseResult)
	if resp.StatusCode != http.StatusOK || !pauseResult["paused"] {
		t.Fatalf("POST /api/jobs/pause: status %d body %v", resp.StatusCode, pauseResult)
	}

	resp = doJSON(t, http.MethodGet, ts.URL+"/api/jobs", nil, &snap)
	if resp.StatusCode != http.StatusOK || !snap.Paused {
		t.Fatalf("expected GET /api/jobs to report paused=true after pausing, got %+v", snap)
	}

	var resumeResult map[string]bool
	resp = doJSON(t, http.MethodPost, ts.URL+"/api/jobs/resume", nil, &resumeResult)
	if resp.StatusCode != http.StatusOK || resumeResult["paused"] {
		t.Fatalf("POST /api/jobs/resume: status %d body %v", resp.StatusCode, resumeResult)
	}

	resp = doJSON(t, http.MethodGet, ts.URL+"/api/jobs", nil, &snap)
	if resp.StatusCode != http.StatusOK || snap.Paused {
		t.Fatalf("expected GET /api/jobs to report paused=false after resuming, got %+v", snap)
	}
}

func TestVoicePresetsAndLanguages(t *testing.T) {
	_, _, _, ts := newTestServer(t)

	var presets map[string]any
	resp := doJSON(t, http.MethodGet, ts.URL+"/api/voices/presets", nil, &presets)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/voices/presets: status %d", resp.StatusCode)
	}
	if presets["default"] == nil {
		t.Fatalf("expected a default preset id in the response: %+v", presets)
	}

	var langs map[string]any
	resp = doJSON(t, http.MethodGet, ts.URL+"/api/voices/languages", nil, &langs)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/voices/languages: status %d", resp.StatusCode)
	}
	list, ok := langs["languages"].([]any)
	if !ok || len(list) == 0 || list[0] != "Auto" {
		t.Fatalf("expected languages to start with Auto, got %+v", langs)
	}
}

func TestCustomVoicePresetCreateAndDelete(t *testing.T) {
	_, _, fake, ts := newTestServer(t)

	create := customVoicePresetRequest{Name: "My Voice", Instruct: "speak warmly", RefText: "a reference line"}
	var created customVoicePresetDTO
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/voices/custom-presets", create, &created)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST custom preset: status %d", resp.StatusCode)
	}
	if created.ID == "" || created.Name != "My Voice" {
		t.Fatalf("unexpected created preset: %+v", created)
	}
	if len(fake.DesignCalls()) != 1 {
		t.Fatalf("expected creating a preset to render its reference clip via Design, got %d calls", len(fake.DesignCalls()))
	}

	var list []customVoicePresetDTO
	resp = doJSON(t, http.MethodGet, ts.URL+"/api/voices/custom-presets", nil, &list)
	if resp.StatusCode != http.StatusOK || len(list) != 1 {
		t.Fatalf("unexpected preset list: status=%d list=%+v", resp.StatusCode, list)
	}

	resp = doJSON(t, http.MethodDelete, ts.URL+"/api/voices/custom-presets/"+created.ID, nil, nil)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE custom preset: status %d", resp.StatusCode)
	}
	resp = doJSON(t, http.MethodGet, ts.URL+"/api/voices/custom-presets", nil, &list)
	if resp.StatusCode != http.StatusOK || len(list) != 0 {
		t.Fatalf("expected no presets after delete, got %+v", list)
	}
}

func TestAttributeSpeakersUnavailableWithoutSpeakerConfigured(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "p1")

	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/chapters/0/attribute-speakers", nil, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when Speaker is unconfigured, got %d", resp.StatusCode)
	}
}

// fakeLLMBackend is a minimal stand-in for speakerattr's own unexported
// llmBackend interface (satisfied structurally - LLMGenerate is exported,
// so this works across packages without naming that type directly),
// always returning a fixed, validly-shaped CharacterizeVoice response
// (long enough to clear parseCharacterization's own minCharacterizeWords/
// minRefLineWords floors) regardless of the prompt it's given.
type fakeLLMBackend struct{}

func (fakeLLMBackend) LLMGenerate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error) {
	return `{"instruct": "Speak as a calm, warm middle-aged narrator with a neutral General American accent, measured pace, and steady, unhurried delivery.", "refLine": "The old house stood quietly at the end of the lane, its windows dark and its garden overgrown, waiting patiently for someone to remember it was still there."}`, nil
}

// TestCharacterizeSpeakerDispatchesThroughJobsManager confirms
// handleCharacterizeSpeaker (the reader-view annotation legend's and
// SpeakersPage's own single-character "Regenerate" button) actually runs
// its work through jobs.Manager.RunCharacterization as a real
// KindSpeakerCharacterization task, rather than calling characterizeVoice
// directly against the request's own context the way an earlier version of
// this handler did (see its own doc comment for why that was a real bug:
// no dedup against a concurrent duplicate run, invisible on the Jobs
// dashboard, and no respect for poolGeneration/poolLLM's mutual
// exclusion). Uses fakeLLMBackend rather than a nil one - a character with
// no attributed dialogue or descriptions yet still actually characterizes
// now (from its name alone - see speakerattr.Client.CharacterizeVoice's
// own doc comment), so this needs a real (fake) backend to reach - by the
// time RunCharacterization's blocking call returns (and so this handler's
// own HTTP response), the task it created must already be finished and
// cleared, not left behind.
func TestCharacterizeSpeakerDispatchesThroughJobsManager(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, fakeLLMBackend{})
	bookID := createTestBook(t, s, "", 0, "Alice said hello.")
	book, err := s.GetBook(bookID)
	if err != nil || book == nil {
		t.Fatalf("GetBook: %v", err)
	}
	char, _, err := s.UpsertCharacter(store.SeriesScope(book), "Alice", false)
	if err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}

	var body map[string]any
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/characters/"+char.ID+"/characterize", nil, &body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	// voiceInvalidated stays false here even though summary is now real -
	// recharacterizeAndInvalidate only flips it true when there was an
	// existing voice preset to actually invalidate (see its own doc
	// comment), and this character has never been provisioned yet.
	if body["summary"] == "" || body["voiceInvalidated"] != false {
		t.Fatalf("expected a real characterization result (name-only, no dialogue/descriptions) with nothing yet to invalidate, got %+v", body)
	}

	// recharacterizeAndInvalidate (see its own doc comment) always fires a
	// follow-up KindVoiceProvision task in "regenerate" mode after a
	// successful characterization, to eagerly pre-warm the character's
	// voice - fire-and-forget (EnqueueVoiceProvision, not RunVoiceProvision),
	// so it's still legitimately in flight/queued right as this blocking
	// request returns; poll for it to actually finish too (against
	// ttsworkertest's fake worker, same as this file's other end-to-end
	// generation tests) rather than asserting an empty snapshot immediately.
	deadline := time.Now().Add(5 * time.Second)
	for {
		inFlight, queued := srv.Jobs.Snapshot()
		if len(inFlight) == 0 && len(queued) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected the follow-up voice-provision task to drain within 5s, got inFlight=%+v queued=%+v", inFlight, queued)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReattributeSpeakerUnavailableWithoutSpeakerConfigured(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "p1")

	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/speakers/reattribute", map[string]any{"name": "Bogus"}, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when Speaker is unconfigured, got %d", resp.StatusCode)
	}
}

// fakeReattributeLLMBackend always reassigns idx 0 to "Alice" (an existing,
// real character) and idx 1 back to "Bogus" itself - the speaker "Auto
// Split" is trying to eliminate - regardless of the actual prompt, so
// TestReattributeSpeakerDispatchesThroughJobsManager can assert both the
// happy path (idx 0 genuinely moves to a different real character) and
// reattributeChapterSpeaker's own defensive backstop (idx 1's result is
// forced to "Unknown" rather than persisted as a no-op reattribution back
// onto "Bogus" - see that function's own doc comment).
type fakeReattributeLLMBackend struct{}

func (fakeReattributeLLMBackend) LLMGenerate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (string, error) {
	return `[{"idx": 0, "speaker": "Alice"}, {"idx": 1, "speaker": "Bogus"}]`, nil
}

// TestReattributeSpeakerDispatchesThroughJobsManager confirms
// handleReattributeSpeaker (the Speakers page's "Auto Split" button) fans
// out into a real KindSpeakerReattribution task per chapter, actually
// reassigns only the target speaker's own paragraphs (idx 2, attributed to
// "Alice" from the start, is left untouched even though it's re-judged by
// the same LLM call for context), and never persists the excluded speaker
// back onto their own former paragraphs even if the model itself still
// emits that name.
func TestReattributeSpeakerDispatchesThroughJobsManager(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, fakeReattributeLLMBackend{})

	bookID, _, err := s.CreateBook("Test Book", "Test Author", "en", "", "", 0, []store.ChapterInput{
		{Title: "Chapter One", Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Hello there.”", IsQuote: true},
			{Kind: store.BlockText, Text: "“Good day.”", IsQuote: true},
			{Kind: store.BlockText, Text: "“Still here.”", IsQuote: true},
		}},
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	book, err := s.GetBook(bookID)
	if err != nil || book == nil {
		t.Fatalf("GetBook: %v", err)
	}
	ch, err := s.GetChapterByIdx(bookID, 0)
	if err != nil || ch == nil {
		t.Fatalf("GetChapterByIdx: %v", err)
	}
	if _, _, err := s.UpsertCharacter(store.SeriesScope(book), "Alice", false); err != nil {
		t.Fatalf("UpsertCharacter(Alice): %v", err)
	}
	if _, _, err := s.UpsertCharacter(store.SeriesScope(book), "Bogus", false); err != nil {
		t.Fatalf("UpsertCharacter(Bogus): %v", err)
	}
	if err := s.SetParagraphSpeakers(ch.ID, map[int]string{0: "Bogus", 1: "Bogus", 2: "Alice"}); err != nil {
		t.Fatalf("SetParagraphSpeakers: %v", err)
	}

	var body map[string]any
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/speakers/reattribute", map[string]any{"name": "Bogus"}, &body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %+v", resp.StatusCode, body)
	}
	if body["queued"] != float64(1) {
		t.Fatalf("expected exactly one chapter queued, got %+v", body)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		inFlight, queued := srv.Jobs.Snapshot()
		if len(inFlight) == 0 && len(queued) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected the reattribution task to drain within 5s, got inFlight=%+v queued=%+v", inFlight, queued)
		}
		time.Sleep(10 * time.Millisecond)
	}

	all, err := s.ListParagraphsRaw(ch.ID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	got := map[int]string{}
	for _, p := range all {
		got[p.Idx] = p.Speaker
	}
	if got[0] != "Alice" {
		t.Fatalf("expected idx 0 reattributed to Alice, got %q", got[0])
	}
	if got[1] != "Unknown" {
		t.Fatalf("expected idx 1's echoed-back \"Bogus\" result forced to Unknown, got %q", got[1])
	}
	if got[2] != "Alice" {
		t.Fatalf("expected idx 2 (never Bogus's to begin with) left untouched, got %q", got[2])
	}
}

// TestReattributeSpeakerAllowsUnknown confirms "Auto Split" can target the
// literal "Unknown" sentinel - which, unlike a real character, has no
// characters-table row to look up (see reattributeSpeakerRequest's own doc
// comment) - without 404ing the way a nonexistent character name would.
func TestReattributeSpeakerAllowsUnknown(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, fakeReattributeLLMBackend{})

	bookID, _, err := s.CreateBook("Test Book", "Test Author", "en", "", "", 0, []store.ChapterInput{
		{Title: "Chapter One", Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Hello there.”", IsQuote: true},
		}},
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	ch, err := s.GetChapterByIdx(bookID, 0)
	if err != nil || ch == nil {
		t.Fatalf("GetChapterByIdx: %v", err)
	}
	if err := s.SetParagraphSpeakers(ch.ID, map[int]string{0: "Unknown"}); err != nil {
		t.Fatalf("SetParagraphSpeakers: %v", err)
	}

	var body map[string]any
	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/speakers/reattribute", map[string]any{"name": "Unknown"}, &body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 for Unknown (no character row required), got %d: %+v", resp.StatusCode, body)
	}
	if body["queued"] != float64(1) {
		t.Fatalf("expected exactly one chapter queued, got %+v", body)
	}
}

func TestReattributeSpeakerCharacterNotFound(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	srv.Speaker = speakerattr.NewClient(speakerattr.Config{}, fakeReattributeLLMBackend{})
	bookID := createTestBook(t, s, "", 0, "p1")

	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/speakers/reattribute", map[string]any{"name": "NoSuchCharacter"}, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for a nonexistent character name, got %d", resp.StatusCode)
	}
}

// TestPreprocessBookUnavailableWithoutSpeakerConfigured mirrors
// TestAttributeSpeakersUnavailableWithoutSpeakerConfigured for the
// attribution/characterization/voice-provisioning/direction-tagging
// meta-task (POST .../preprocess) - it needs Speaker configured just like
// each of the individual steps it chains, so it should refuse up front
// (503) rather than kick off a background goroutine that would immediately
// have nothing to do.
func TestPreprocessBookUnavailableWithoutSpeakerConfigured(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "p1")

	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/"+bookID+"/preprocess", nil, nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when Speaker is unconfigured, got %d", resp.StatusCode)
	}
}

func TestPreprocessBookNotFound(t *testing.T) {
	srv, _, _, ts := newTestServer(t)
	// Give the handler a non-nil Speaker so the 503 short-circuit above
	// doesn't mask this 404 check - a real *speakerattr.Client isn't
	// needed since the book lookup fails before it would ever be used.
	srv.Speaker = &speakerattr.Client{}

	resp := doJSON(t, http.MethodPost, ts.URL+"/api/books/does-not-exist/preprocess", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for a nonexistent book, got %d", resp.StatusCode)
	}
}

func TestResolveAudioIDReturnsOwnIDWithoutAPointer(t *testing.T) {
	p := store.Paragraph{ID: "p-5", Idx: 5}
	state := store.AudioState{Status: store.AudioReady}
	byIdx := map[int]string{5: "p-5"}
	if got := resolveAudioID(p, state, byIdx); got != "p-5" {
		t.Fatalf("expected own id p-5, got %q", got)
	}
}

// TestResolveAudioIDResolvesToAnchor confirms resolveAudioID returns the
// EXACT SAME id string a merge group's own anchor paragraph would - not
// just an id that happens to serve the same bytes - since the frontend
// player relies on that exact string match to know not to reload/restart
// audio already mid-playback across a merge group's own internal
// boundaries (see resolveAudioID's own doc comment).
func TestResolveAudioIDResolvesToAnchor(t *testing.T) {
	byIdx := map[int]string{3: "anchor-id", 4: "scare-quote-id", 5: "tail-id"}

	anchor := store.Paragraph{ID: "anchor-id", Idx: 3}
	if got := resolveAudioID(anchor, store.AudioState{Status: store.AudioReady, PointerOffset: 0}, byIdx); got != "anchor-id" {
		t.Fatalf("anchor: expected anchor-id, got %q", got)
	}

	scareQuote := store.Paragraph{ID: "scare-quote-id", Idx: 4}
	if got := resolveAudioID(scareQuote, store.AudioState{Status: store.AudioReady, PointerOffset: 1}, byIdx); got != "anchor-id" {
		t.Fatalf("pointer at offset 1: expected anchor-id, got %q", got)
	}

	tail := store.Paragraph{ID: "tail-id", Idx: 5}
	if got := resolveAudioID(tail, store.AudioState{Status: store.AudioReady, PointerOffset: 2}, byIdx); got != "anchor-id" {
		t.Fatalf("pointer at offset 2: expected anchor-id, got %q", got)
	}
}

// TestResolveAudioIDFallsBackWhenAnchorMissing covers the "should never
// happen" gap defensively (a dangling pointer_offset with no paragraph at
// that idx, e.g. stale data) - falls back to the paragraph's own id rather
// than an empty/invalid URL.
func TestResolveAudioIDFallsBackWhenAnchorMissing(t *testing.T) {
	p := store.Paragraph{ID: "p-9", Idx: 9}
	state := store.AudioState{Status: store.AudioReady, PointerOffset: 3}
	byIdx := map[int]string{9: "p-9"} // idx 6 (9-3) missing
	if got := resolveAudioID(p, state, byIdx); got != "p-9" {
		t.Fatalf("expected fallback to own id p-9, got %q", got)
	}
}

func TestBookManifestHashTree(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "p1", "\"Hello,\" she said.")

	getManifest := func(query string) bookManifestDTO {
		t.Helper()
		var m bookManifestDTO
		resp := doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/manifest"+query, nil, &m)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET manifest: status %d", resp.StatusCode)
		}
		return m
	}
	getChapter := func() chapterDetailDTO {
		t.Helper()
		var c chapterDetailDTO
		resp := doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/chapters/0", nil, &c)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET chapter: status %d", resp.StatusCode)
		}
		return c
	}

	before := getManifest("")
	if before.Hash == "" || len(before.Chapters) != 1 {
		t.Fatalf("unexpected manifest: %+v", before)
	}
	if again := getManifest(""); again.Hash != before.Hash {
		t.Fatalf("root hash not stable: %s vs %s", before.Hash, again.Hash)
	}
	chBefore := getChapter()
	if chBefore.Hash != before.Chapters[0].Hash {
		t.Fatalf("chapter hash %s doesn't match manifest's %s", chBefore.Hash, before.Chapters[0].Hash)
	}

	// Reading position is deliberately outside the tree.
	doJSON(t, http.MethodPut, ts.URL+"/api/books/"+bookID+"/position", positionDTO{ChapterIdx: 0, ParagraphIdx: 1, Seconds: 3}, nil)
	if m := getManifest(""); m.Hash != before.Hash || m.PosParagraphIdx != 1 {
		t.Fatalf("position change: hash %s -> %s, pos %d", before.Hash, m.Hash, m.PosParagraphIdx)
	}

	// A content-only change moves that paragraph's content hash (not its
	// audio hash), its chapter's hash and the root.
	ch, err := s.GetChapterByIdx(bookID, 0)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := s.GetParagraphIDByIdx(ch.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphSpeaker(pid, "Alice"); err != nil {
		t.Fatal(err)
	}
	chAfter := getChapter()
	if chAfter.Paragraphs[0].ContentHash != chBefore.Paragraphs[0].ContentHash {
		t.Fatal("untouched paragraph's content hash changed")
	}
	if chAfter.Paragraphs[1].ContentHash == chBefore.Paragraphs[1].ContentHash {
		t.Fatal("re-attributed paragraph's content hash didn't change")
	}
	if chAfter.Hash == chBefore.Hash {
		t.Fatal("chapter hash didn't change")
	}
	if m := getManifest(""); m.Hash == before.Hash || m.Chapters[0].Hash != chAfter.Hash {
		t.Fatalf("root/chapter hash not updated: %+v", m)
	}

	// ?chapters= limits the tree to the listed chapters.
	if m := getManifest("?chapters=5"); len(m.Chapters) != 0 {
		t.Fatalf("expected no chapters for an unknown idx, got %+v", m.Chapters)
	}
	if resp := doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/manifest?chapters=x", nil, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad chapters list: status %d", resp.StatusCode)
	}
}

func TestBookManifestReusesStoredTree(t *testing.T) {
	srv, s, _, ts := newTestServer(t)
	bookID := createTestBook(t, s, "", 0, "p1", "\"Hi,\" he said.")

	manifestFrom := func(base string) bookManifestDTO {
		t.Helper()
		var m bookManifestDTO
		if resp := doJSON(t, http.MethodGet, base+"/api/books/"+bookID+"/manifest", nil, &m); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET manifest: status %d", resp.StatusCode)
		}
		return m
	}
	manifest := func() bookManifestDTO { t.Helper(); return manifestFrom(ts.URL) }
	real := manifest().Chapters[0].Hash
	if nodes, err := s.SyncTreeNodes(bookID, syncTreeVersion()); err != nil || nodes[0].Hash != real {
		t.Fatalf("built node not stored: %v %+v", err, nodes)
	}

	// Poison the stored node under the current fingerprints: if a request
	// serves it, nothing was rebuilt.
	bookFP, chapterFPs, err := s.SyncFingerprints(bookID)
	if err != nil {
		t.Fatal(err)
	}
	poison := []store.SyncTreeNode{{ChapterIdx: 0, BookFP: bookFP, ChapterFP: chapterFPs[0], Hash: "poisoned"}}
	if err := s.PutSyncTreeNodes(bookID, syncTreeVersion(), poison); err != nil {
		t.Fatal(err)
	}
	if got := manifest().Chapters[0].Hash; got != "poisoned" {
		t.Fatalf("unchanged chapter was rebuilt: got %s", got)
	}

	// Survives a restart: a fresh Server on the same database reuses it.
	fresh := httptest.NewServer(NewRouter(&Server{Store: s, TTS: srv.TTS, Jobs: srv.Jobs, DataDir: srv.DataDir, Narration: srv.Narration}))
	defer fresh.Close()
	if got := manifestFrom(fresh.URL).Chapters[0].Hash; got != "poisoned" {
		t.Fatalf("stored node not reused after restart: got %s", got)
	}

	// Nodes hashed by another build are never reused, and get pruned.
	if err := s.PutSyncTreeNodes(bookID, "other-build", poison); err != nil {
		t.Fatal(err)
	}
	if err := s.PruneSyncTree(syncTreeVersion()); err != nil {
		t.Fatal(err)
	}
	if nodes, _ := s.SyncTreeNodes(bookID, "other-build"); len(nodes) != 0 {
		t.Fatal("other build's nodes survived pruning")
	}
	if got := manifest().Chapters[0].Hash; got == "poisoned" {
		t.Fatal("served a node from another build")
	}
	if err := s.PutSyncTreeNodes(bookID, syncTreeVersion(), poison); err != nil {
		t.Fatal(err)
	}

	// Reading-position writes keep serving the stored node.
	doJSON(t, http.MethodPut, ts.URL+"/api/books/"+bookID+"/position", positionDTO{ChapterIdx: 0, ParagraphIdx: 1}, nil)
	if got := manifest().Chapters[0].Hash; got != "poisoned" {
		t.Fatalf("position write invalidated the stored node: got %s", got)
	}

	// A real change to the chapter's rows forces a rebuild.
	ch, err := s.GetChapterByIdx(bookID, 0)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := s.GetParagraphIDByIdx(ch.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetParagraphSpeaker(pid, "Bob"); err != nil {
		t.Fatal(err)
	}
	got := manifest().Chapters[0].Hash
	if got == "poisoned" || got == real {
		t.Fatalf("changed chapter not rebuilt: got %s", got)
	}
	var chapter chapterDetailDTO
	doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/chapters/0", nil, &chapter)
	if got != chapter.Hash {
		t.Fatalf("rebuilt hash %s doesn't match the chapter's own %s", got, chapter.Hash)
	}

	// Deleting the book drops its nodes.
	doJSON(t, http.MethodDelete, ts.URL+"/api/books/"+bookID, nil, nil)
	if nodes, _ := s.SyncTreeNodes(bookID, syncTreeVersion()); len(nodes) != 0 {
		t.Fatal("deleted book's tree was kept")
	}
}

// TestSetParagraphEmotion covers the manual emotion override: dialogue
// accepts a known emotion (and "" to clear it) and reports it on the
// chapter; narration and unknown emotions are rejected.
func TestSetParagraphEmotion(t *testing.T) {
	_, s, _, ts := newTestServer(t)
	bookID, _, err := s.CreateBook("Test Book", "Test Author", "en", "", "", 0, []store.ChapterInput{{
		Title: "Chapter One",
		Blocks: []store.BlockInput{
			{Kind: store.BlockText, Text: "“Get out!”", IsQuote: true},
			{Kind: store.BlockText, Text: "she shouted.", Inline: true},
		},
	}})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	url := func(pidx int) string {
		return ts.URL + "/api/books/" + bookID + "/chapters/0/paragraphs/" + strconv.Itoa(pidx) + "/emotion"
	}
	emotionOf := func(pidx int) string {
		var ch chapterDetailDTO
		doJSON(t, http.MethodGet, ts.URL+"/api/books/"+bookID+"/chapters/0", nil, &ch)
		return ch.Paragraphs[pidx].Emotion
	}

	if resp := doJSON(t, http.MethodPut, url(0), map[string]string{"emotion": "shout"}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("set dialogue emotion: status %d", resp.StatusCode)
	}
	if got := emotionOf(0); got != "shout" {
		t.Fatalf("emotion after set = %q, want shout", got)
	}
	if resp := doJSON(t, http.MethodPut, url(0), map[string]string{"emotion": "elated"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown emotion: status %d, want 400", resp.StatusCode)
	}
	if resp := doJSON(t, http.MethodPut, url(1), map[string]string{"emotion": "angry"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("narration emotion: status %d, want 400", resp.StatusCode)
	}
	if resp := doJSON(t, http.MethodPut, url(0), map[string]string{"emotion": ""}, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("clear emotion: status %d", resp.StatusCode)
	}
	if got := emotionOf(0); got != "" {
		t.Fatalf("emotion after clear = %q, want neutral", got)
	}
}
