package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voicecheck"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
)

// The voice check: an optional pass over a roster that listens to each
// character's reference clip - their own, and the one each age stage
// speaks in - with the worker's audio-language model and lists the voices
// that clearly don't sound like their casting (internal/voicecheck has the
// questions and the rules). Emotion variants aren't heard: they are cloned
// from the neutral clip and rendered again whenever it is. Nothing is
// re-rendered here - a finding is the reader's to act on, by rendering the
// voice afresh (POST .../speakers/variants/regenerate) or dismissing it.

// voiceTrait is a trait a voice can be found to disagree with its casting
// on.
type voiceTrait = voicecheck.Trait

// voiceTraitsDTO is what a voice sounds like, or is cast to: each field
// omitted when unknown.
type voiceTraitsDTO struct {
	// Gender is "man" or "woman".
	Gender string `json:"gender,omitempty"`
	// Age is "child", "teenager", "young adult", "middle-aged" or
	// "elderly".
	Age string `json:"age,omitempty"`
	// Pitch is "low", "medium" or "high".
	Pitch string `json:"pitch,omitempty"`
	// Accent is the casting's accent by name, or the listener's own words
	// for what it heard. Shown, never compared: the listener can't tell
	// regional accents apart.
	Accent string `json:"accent,omitempty"`
}

func traitsDTO(t voicecheck.Traits) voiceTraitsDTO {
	return voiceTraitsDTO{Gender: t.Gender, Age: t.Age, Pitch: t.Pitch, Accent: t.Accent}
}

// voiceCheckDTO is one voice as the check heard it.
type voiceCheckDTO struct {
	// PresetID names the voice; what POST .../voice-check/dismiss takes.
	PresetID    string `json:"presetId"`
	CharacterID string `json:"characterId"`
	// Speaker is the character's name, as the regenerate route takes it.
	Speaker string `json:"speaker"`
	// StageID and StageAge say which age stage's voice this is; omitted
	// for the character's own.
	StageID  string `json:"stageId,omitempty"`
	StageAge int    `json:"stageAge,omitempty"`
	// Instruct is the casting the clip was designed from.
	Instruct    string         `json:"instruct"`
	RefAudioURL string         `json:"refAudioUrl,omitempty"`
	Cast        voiceTraitsDTO `json:"cast"`
	Heard       voiceTraitsDTO `json:"heard"`
	// Mismatches is the traits on which the two clearly disagree.
	Mismatches []voiceTrait `json:"mismatches"`
	Dismissed  bool         `json:"dismissed,omitempty"`
	CheckedAt  int64        `json:"checkedAt"`
}

// voiceCheckResponse is GET /api/books/{id}/voice-check.
type voiceCheckResponse struct {
	// Voices is how many of the roster's voices have a reference clip to
	// hear, Checked how many of those have been heard as they are now.
	Voices  int `json:"voices"`
	Checked int `json:"checked"`
	// Findings is the checked voices that disagree with their casting and
	// haven't been dismissed, most lines first - or, with ?all=1, every
	// checked voice.
	Findings []voiceCheckDTO `json:"findings"`
}

type dismissVoiceCheckRequest struct {
	PresetID string `json:"presetId"`
}

// voiceCheckTarget is one voice the check can hear.
type voiceCheckTarget struct {
	char     store.Character
	stage    *store.CharacterStage
	presetID string
	instruct string
	path     string
}

// voiceCheckTargets lists the roster's voices that have a reference clip
// on disk: each valid character's own voice under book's clone model, and
// the voice of each of their age stages anywhere in the series. A
// character still on the narrator's voice has none; a role's numbered
// members follow the role's voice and aren't listed apart.
func (s *Server) voiceCheckTargets(book *store.Book) ([]voiceCheckTarget, error) {
	if !book.MultiVoice() {
		return nil, nil
	}
	scope := store.SeriesScope(book)
	chars, err := s.Store.ListCharacters(scope)
	if err != nil {
		return nil, err
	}
	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		return nil, err
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)
	ids := make([]string, len(chars))
	for i, c := range chars {
		ids[i] = c.ID
	}
	presets, err := s.Store.CharacterVoicesForModel(ids, cloneModel)
	if err != nil {
		return nil, err
	}
	stages, err := s.Store.ListCharacterStages(scope)
	if err != nil {
		return nil, err
	}
	var out []voiceCheckTarget
	add := func(c store.Character, st *store.CharacterStage, v narration.ResolvedVoice) {
		path := audiopath.VoicePresetRefFile(s.DataDir, v.PresetID)
		if fileExists(path) {
			out = append(out, voiceCheckTarget{char: c, stage: st, presetID: v.PresetID, instruct: v.Instruct, path: path})
		}
	}
	for _, c := range chars {
		if c.Invalid || presets[c.ID] == "" {
			continue
		}
		own, err := s.Narration.ResolveCharacterVoice(book, bookVoice, c, cloneModel, presets[c.ID])
		if err != nil {
			return nil, err
		}
		// Styled per line from the narrator's clip: no clip of their own.
		if own.PresetID == "" || own.CloneInstruct != "" || own.VoiceID() == bookVoice.VoiceID() {
			continue
		}
		add(c, nil, own)
		for i := range stages {
			st := &stages[i]
			if st.CharacterID != c.ID {
				continue
			}
			if v := narration.StageVoice(own, bookVoice, c, st); v.PresetID != own.PresetID {
				add(c, st, v)
			}
		}
	}
	return out, nil
}

// voiceCheckQuestionTokens caps each answer: the questions ask for a word
// or two.
const voiceCheckQuestionTokens = 24

// CheckVoices is the voice-check pass for book's roster - the
// jobs.KindVoiceCheck task. It hears every voice whose current clip it
// hasn't heard yet and stores each as a store.VoiceCheck; a voice it can't
// hear is logged and left for the next run.
func (s *Server) CheckVoices(ctx context.Context, book *store.Book) error {
	if s.TTS == nil {
		return fmt.Errorf("tts worker not configured")
	}
	scope := store.SeriesScope(book)
	targets, err := s.voiceCheckTargets(book)
	if err != nil {
		return err
	}
	stored, err := s.Store.ListVoiceChecks(scope)
	if err != nil {
		return err
	}
	heardAs := make(map[string]string, len(stored))
	for _, c := range stored {
		heardAs[c.PresetID] = c.RefIdentity
	}
	checked, flagged, failed := 0, 0, 0
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		identity := voicerefs.RefIdentity(s.DataDir, t.presetID, "")
		if heardAs[t.presetID] == identity {
			continue
		}
		clip, err := os.ReadFile(t.path)
		if err != nil {
			log.Printf("httpapi: voice check %s (%s): %v", t.char.Name, t.presetID, err)
			failed++
			continue
		}
		answers := make(map[string]string, len(voicecheck.Questions))
		for _, q := range voicecheck.Questions {
			a, err := s.TTS.Listen(ctx, clip, q.Text, voiceCheckQuestionTokens)
			if err != nil {
				// The listener itself is down or missing: every other voice
				// would fail the same way.
				return fmt.Errorf("listen to %s's voice: %w", t.char.Name, err)
			}
			answers[q.Key] = a
		}
		years, stageID := 0, ""
		if t.stage != nil {
			years, stageID = t.stage.Age, t.stage.ID
		}
		cast := voicecheck.Cast(t.instruct, t.char.Pronouns, years, speakerattr.CastingAccent(t.instruct))
		heard := voicecheck.Heard(answers)
		mismatches := voicecheck.Mismatches(cast, heard)
		if err := s.Store.UpsertVoiceCheck(store.VoiceCheck{
			Scope: scope, PresetID: t.presetID, CharacterID: t.char.ID, StageID: stageID, RefIdentity: identity,
			Cast: cast, Heard: heard, Mismatches: mismatches,
		}); err != nil {
			return err
		}
		checked++
		if len(mismatches) > 0 {
			flagged++
		}
	}
	log.Printf("httpapi: voice check %s: %d voices, heard %d, flagged %d, unreadable %d", scope, len(targets), checked, flagged, failed)
	return nil
}

// voiceCheck builds GET /api/books/{id}/voice-check.
func (s *Server) voiceCheck(bookID string, all bool) (voiceCheckResponse, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return voiceCheckResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return voiceCheckResponse{}, httpError(http.StatusNotFound, "book not found")
	}
	out := voiceCheckResponse{Findings: []voiceCheckDTO{}}
	stored, err := s.Store.ListVoiceChecks(store.SeriesScope(book))
	if err != nil {
		return voiceCheckResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	targets, err := s.voiceCheckTargets(book)
	if err != nil {
		return voiceCheckResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	out.Voices = len(targets)
	if len(stored) == 0 {
		return out, nil
	}
	byPreset := make(map[string]store.VoiceCheck, len(stored))
	for _, c := range stored {
		byPreset[c.PresetID] = c
	}
	books, err := s.booksInScope(book)
	if err != nil {
		return voiceCheckResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	lines, err := s.rosterLineCounts(books)
	if err != nil {
		return voiceCheckResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	for _, t := range targets {
		c, ok := byPreset[t.presetID]
		// A clip rendered since it was heard hasn't been checked.
		if !ok || c.RefIdentity != voicerefs.RefIdentity(s.DataDir, t.presetID, "") {
			continue
		}
		out.Checked++
		if !all && (c.Dismissed || len(c.Mismatches) == 0) {
			continue
		}
		dto := voiceCheckDTO{
			PresetID: t.presetID, CharacterID: t.char.ID, Speaker: t.char.Name, Instruct: t.instruct,
			Cast: traitsDTO(c.Cast), Heard: traitsDTO(c.Heard), Mismatches: c.Mismatches, Dismissed: c.Dismissed, CheckedAt: c.CheckedAt,
		}
		if dto.Mismatches == nil {
			dto.Mismatches = []voiceTrait{}
		}
		if t.stage != nil {
			dto.StageID, dto.StageAge = t.stage.ID, t.stage.Age
			dto.RefAudioURL = withClipVersion("/api/voices/role-variants/"+t.presetID+"/audio", t.path)
		} else {
			dto.RefAudioURL = s.versionedPresetAudioURL(t.presetID)
		}
		out.Findings = append(out.Findings, dto)
	}
	sort.SliceStable(out.Findings, func(i, j int) bool {
		return lines[out.Findings[i].Speaker] > lines[out.Findings[j].Speaker]
	})
	return out, nil
}

// buildVoiceCheck is the voiceCheck live topic's value.
func (s *Server) buildVoiceCheck(bookID string) (voiceCheckResponse, error) {
	return s.voiceCheck(bookID, false)
}

// handleGetVoiceCheck serves GET /api/books/{id}/voice-check - see
// docs/api.md.
func (s *Server) handleGetVoiceCheck(w http.ResponseWriter, r *http.Request) {
	v, err := s.voiceCheck(r.PathValue("id"), r.URL.Query().Get("all") == "1")
	writeBuilt(w, v, err)
}

// handleRunVoiceCheck serves POST /api/books/{id}/voice-check/run: queues
// a check of the book's roster's voices. The voiceCheck live topic carries
// what it finds.
func (s *Server) handleRunVoiceCheck(w http.ResponseWriter, r *http.Request) {
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	if s.TTS == nil {
		writeError(w, http.StatusServiceUnavailable, "tts worker not configured")
		return
	}
	s.Jobs.EnqueueVoiceCheck(book, store.SeriesScope(book), func(ctx context.Context) error {
		return s.CheckVoices(ctx, book)
	})
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// handleDismissVoiceCheck serves POST /api/books/{id}/voice-check/dismiss:
// the reader says a flagged voice is fine as it is.
func (s *Server) handleDismissVoiceCheck(w http.ResponseWriter, r *http.Request) {
	var req dismissVoiceCheckRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	book, ok := s.lexiconBook(w, r.PathValue("id"))
	if !ok {
		return
	}
	found, err := s.Store.DismissVoiceCheck(store.SeriesScope(book), req.PresetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "no check of that voice")
		return
	}
	writeNoContent(w)
}

type listenRequest struct {
	// PresetID is the voice whose reference clip is asked about: a preset,
	// or a derived one (a numbered role member's, an age stage's).
	PresetID string `json:"presetId"`
	// Question is asked of the clip as written.
	Question string `json:"question"`
	// MaxTokens caps the answer; 0 takes the worker's default.
	MaxTokens int `json:"maxTokens,omitempty"`
}

// handleListen serves POST /api/voices/listen: one question asked of one
// voice's reference clip by the worker's audio-language model - the voice
// check's building block, for trying a question out. Run as a
// jobs.KindListenPreview task, so the listener takes its turn on the GPU
// like any other audio model.
func (s *Server) handleListen(w http.ResponseWriter, r *http.Request) {
	var req listenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	question := strings.TrimSpace(req.Question)
	if question == "" {
		writeError(w, http.StatusBadRequest, "question is required")
		return
	}
	// A preset id is a file name here.
	if req.PresetID == "" || strings.ContainsAny(req.PresetID, `/\`) || strings.Contains(req.PresetID, "..") {
		writeError(w, http.StatusBadRequest, "presetId is required")
		return
	}
	if s.TTS == nil {
		writeError(w, http.StatusServiceUnavailable, "tts worker not configured")
		return
	}
	clip, err := os.ReadFile(audiopath.VoicePresetRefFile(s.DataDir, req.PresetID))
	if err != nil {
		writeError(w, http.StatusNotFound, "that voice has no reference clip")
		return
	}
	text, err := s.Jobs.RunListenPreview(r.Context(), question, func(ctx context.Context) (string, error) {
		return s.TTS.Listen(ctx, clip, question, req.MaxTokens)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "ttsworker unavailable: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, llmTestResponse{Text: text})
}
