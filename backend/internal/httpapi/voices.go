package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

type voicePresetDTOOut struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Instruct        string  `json:"instruct"`
	Seed            int     `json:"seed"`
	RefText         string  `json:"ref_text"`
	SpeedMultiplier float64 `json:"speed_multiplier"`
	CloneModel      string  `json:"cloneModel"`
	DesignModel     string  `json:"designModel"`
	AudioURL        string  `json:"audioUrl"`
	// RefError is only ever set by handleRegeneratePreset's own response -
	// handleVoicePresets never renders a preset's clip as part of listing,
	// so it has nothing to report here and always leaves this "" (omitted).
	RefError string `json:"refError,omitempty"`
}

func (s *Server) handleVoicePresets(w http.ResponseWriter, r *http.Request) {
	out := make([]voicePresetDTOOut, len(voices.Presets))
	for i, p := range voices.Presets {
		out[i] = voicePresetDTOOut{
			ID: p.ID, Name: p.Name, Instruct: p.Instruct, Seed: p.Seed, RefText: p.RefText,
			SpeedMultiplier: p.SpeedMultiplier,
			CloneModel:      DefaultCloneModel,
			DesignModel:     p.DesignModel,
			AudioURL:        "/api/voices/presets/" + p.ID + "/audio",
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"default": voices.DefaultPresetID, "presets": out})
}

func (s *Server) handleVoiceLanguages(w http.ResponseWriter, r *http.Request) {
	langs := append([]string{"Auto"}, voices.DisplayLanguages...)
	writeJSON(w, http.StatusOK, map[string]any{"languages": langs})
}

type voiceSettingsDTO struct {
	PresetID string `json:"presetId"`
	Instruct string `json:"instruct"`
	Language string `json:"language"`
	Seed     int    `json:"seed"`
	// CharacterVoiceMode - see store.CharacterVoiceMode's own doc comment
	// for the four possible values ("narrator"/"assigned"/
	// "instruct_unassigned"/"instruct_all") and what each means. Characters
	// and their voice assignments (GET/PUT /api/books/{id}/characters/...)
	// can be set up regardless of this setting; it's what actually turns
	// those assignments (or, for the two instruct_* modes, a character's
	// own characterization) into different narration voices during
	// generation. The two instruct_* modes only actually change anything
	// for a book whose resolved clone model is breeze_tts - a no-op
	// otherwise (same as "assigned" for that character).
	CharacterVoiceMode string `json:"characterVoiceMode"`
	// SpeechDirection opts this book into waiting for speech-direction
	// tagging (store.Passes.Direction) before generating a chapter's
	// audio - see store.Book.SpeechDirection and jobs.Manager's
	// speechDirectionDependency. Off by default: most books never touch
	// direction tagging, so nothing should pay an LLM-pass delay before
	// its first audio unless a reader explicitly opts in.
	SpeechDirection bool `json:"speechDirection"`
	// MusicEnabled is the reader-facing background-music toggle - see
	// store.Book.MusicEnabled's own doc comment for why this is book-wide
	// rather than per-chapter like Passes.Music itself. Off by default.
	MusicEnabled bool `json:"musicEnabled"`
}

func (s *Server) handleGetVoice(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, voiceSettingsDTO{
		PresetID:           b.VoicePresetID,
		Instruct:           b.VoiceInstruct,
		Language:           b.VoiceLanguage,
		Seed:               b.VoiceSeed,
		CharacterVoiceMode: string(b.CharacterVoiceMode),
		SpeechDirection:    b.SpeechDirection,
		MusicEnabled:       b.MusicEnabled,
	})
}

// resolveVoiceSeed looks up presetID's seed - first among the curated
// built-in presets, then among this library's custom ones - so a book's
// stored voice_seed always matches whichever preset it names. Zero means
// "no preset backs this voice" (a fully custom instruct with no preset
// selected), which the worker treats as "no seed anchor" too.
func (s *Server) resolveVoiceSeed(presetID string) (int, error) {
	if presetID == "" {
		return 0, nil
	}
	if p, ok := voices.PresetsByID[presetID]; ok {
		return p.Seed, nil
	}
	custom, err := s.Store.GetVoicePreset(presetID)
	if err != nil {
		return 0, err
	}
	if custom != nil {
		return custom.Seed, nil
	}
	// Unknown preset id (e.g. a custom preset that's since been deleted) -
	// fall back to no anchor rather than failing the whole request; the
	// book's instruct text still carries through as-is.
	return 0, nil
}

// resolveVoiceRecipe looks up id's full render recipe - among the curated
// built-in presets first, then this library's custom ones, the same
// "built-in id space, then custom" precedence resolveVoiceSeed uses. A
// built-in's own CloneModel is deliberately ignored in favor of
// DefaultCloneModel here, matching handleTestPreset's own established
// "always preview a built-in through the default clone model" convention
// (voices.FastPresetID is the sole built-in with its own CloneModel
// override, and it exists to be cloned through specifically for length
// estimation, not for a reader to preview).
func (s *Server) resolveVoiceRecipe(id string) (name, instruct string, seed int, refText string, speedMultiplier float64, cloneModel, designModel string, err error) {
	if p, found := voices.PresetsByID[id]; found {
		return p.Name, p.Instruct, p.Seed, p.RefText, p.SpeedMultiplier, DefaultCloneModel, p.DesignModel, nil
	}
	custom, err := s.Store.GetVoicePreset(id)
	if err != nil {
		return "", "", 0, "", 0, "", "", err
	}
	if custom == nil {
		return "", "", 0, "", 0, "", "", fmt.Errorf("voice %q not found", id)
	}
	return custom.Name, custom.Instruct, custom.Seed, custom.RefText, custom.SpeedMultiplier, custom.CloneModel, custom.DesignModel, nil
}

type testCloneInstructRequest struct {
	// BaseID is an existing voice (built-in or custom) whose already-
	// rendered reference clip is cloned from - the timbre/identity this
	// preview keeps constant.
	BaseID string `json:"baseId"`
	// Instruct is the clone-time style instruction sent alongside BaseID's
	// reference clip - what this preview is actually testing.
	Instruct string `json:"instruct"`
	Text     string `json:"text"`
	// CloneModel, when set, overrides BaseID's own saved/resolved cloning
	// model - lets the editor preview breeze_tts specifically (the one
	// family that actually honors Instruct alongside a reference clip -
	// see audioworker.cloneFamily.instructOption) regardless of which
	// model BaseID itself normally clones through. "" defers to BaseID's
	// own resolved clone model, same as testVoiceRequest.CloneModel.
	CloneModel string `json:"cloneModel"`
	// GuidanceScale, when set, overrides the breeze_tts clone family's own
	// configured instruct-time guidance_scale (LECTABLE_AUDIOCPP_BREEZE_CLONE_GUIDANCE_SCALE,
	// audioworker.breezeCloneGuidanceScale) for this one preview call - lets
	// the editor's own "guidance scale" test field experiment with values
	// live, without a server restart/env var change. "" defers to that
	// configured default, same as every other caller of Worker.Generate.
	GuidanceScale string `json:"guidanceScale"`
}

// handleTestCloneInstruct previews "instructed voice cloning" -
// breeze_tts's own ability to clone a reference clip *and* apply a style
// instruction in one call - given an existing voice to clone the reference
// clip from (BaseID) and a free-form instruction to style it with, without
// saving anything as a preset first. Backs the voice/character editor's
// "test with another voice as a base" control - the same thing a reader
// would use to preview what store.Book.InstructCharacterVoices actually
// sounds like for one character before turning it on for a whole book.
func (s *Server) handleTestCloneInstruct(w http.ResponseWriter, r *http.Request) {
	var req testCloneInstructRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	instruct := strings.TrimSpace(req.Instruct)
	if instruct == "" {
		writeError(w, http.StatusBadRequest, "instruct is required")
		return
	}
	baseName, baseInstruct, baseSeed, baseRefText, baseSpeed, baseCloneModel, baseDesignModel, err := s.resolveVoiceRecipe(req.BaseID)
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	cloneModel := baseCloneModel
	if req.CloneModel != "" {
		cloneModel = req.CloneModel
	}

	refPath, err := s.ensureVoiceRef(r.Context(), req.BaseID, baseName, baseInstruct, baseSeed, baseRefText, baseSpeed, cloneModel, baseDesignModel)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ttsworker unavailable: "+err.Error())
		return
	}
	refAudio, err := os.ReadFile(refPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	audio, err := s.Jobs.RunVoiceDesignPreview(r.Context(), instruct, func(ctx context.Context) ([]byte, error) {
		return s.TTS.Generate(ctx, text, cloneModel, refAudio, baseRefText, "Auto", instruct, req.GuidanceScale)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "ttsworker unavailable: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Write(audio)
}

func (s *Server) handleUpdateVoice(w http.ResponseWriter, r *http.Request) {
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

	var req voiceSettingsDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Language == "" {
		req.Language = "Auto"
	}
	characterVoiceMode := store.CharacterVoiceMode(req.CharacterVoiceMode)
	if characterVoiceMode == "" {
		characterVoiceMode = store.DefaultCharacterVoiceMode
	}
	switch characterVoiceMode {
	case store.CharacterVoiceModeNarrator, store.CharacterVoiceModeAssigned,
		store.CharacterVoiceModeInstructUnassigned, store.CharacterVoiceModeInstructAll:
	default:
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown characterVoiceMode %q", req.CharacterVoiceMode))
		return
	}

	seed, err := s.resolveVoiceSeed(req.PresetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.Store.UpdateVoice(id, req.PresetID, req.Instruct, req.Language, seed, characterVoiceMode, req.SpeechDirection, req.MusicEnabled); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Nothing to invalidate for a voice/language/CharacterVoiceMode change:
	// each voice's generated audio is cached under its own voice_id (a hash
	// of the full config), so switching to a previously-used voice for this
	// book just picks that cache back up, and switching to a new one
	// starts it generating independently. Same for switching
	// CharacterVoiceMode - a character's voice_id (or the book's own)
	// either already has cached audio or doesn't; nothing to reset.
	//
	// SpeechDirection is different: turning it on (false -> true) doesn't
	// change any voice_id, so already-ready audio under the book's current
	// voice_id would otherwise just keep being served forever, even though
	// it was generated with no guarantee its chapter was ever actually
	// tagged first (see store.Book.SpeechDirection's own doc comment) -
	// jobs.Manager.speechDirectionDependency only ever gates a *new* clone
	// task, it can't retroactively un-ready one that already finished.
	// Deleting the book's audio here forces every paragraph to regenerate
	// under the newly-enabled dependency, the same explicit invalidation
	// handleDeleteBookAudio already does for a manual "clear generation".
	if req.SpeechDirection && !b.SpeechDirection {
		if err := s.Store.DeleteBookAudio(id); err != nil {
			log.Printf("httpapi: enable speech direction for book %s: delete stale audio: %v", id, err)
		} else if err := os.RemoveAll(fmt.Sprintf("%s/audio/%s", s.DataDir, id)); err != nil {
			log.Printf("httpapi: enable speech direction for book %s: remove audio dir: %v", id, err)
		}
	}
	req.Seed = seed
	writeJSON(w, http.StatusOK, req)
}

// handleGetDefaultVoice reports the voice new books are created with.
func (s *Server) handleGetDefaultVoice(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.GetDefaultVoice()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, voiceSettingsDTO{PresetID: v.PresetID, Instruct: v.Instruct, Language: v.Language, Seed: v.Seed})
}

// handleUpdateDefaultVoice sets the voice new books are created with -
// "selecting" a voice on the Voices page. Existing books are unaffected;
// this only changes what a freshly uploaded book starts with.
func (s *Server) handleUpdateDefaultVoice(w http.ResponseWriter, r *http.Request) {
	var req voiceSettingsDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Language == "" {
		req.Language = "Auto"
	}

	seed, err := s.resolveVoiceSeed(req.PresetID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if err := s.Store.SetDefaultVoice(req.PresetID, req.Instruct, req.Language, seed); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	req.Seed = seed
	writeJSON(w, http.StatusOK, req)
}

type customVoicePresetDTO struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Instruct        string  `json:"instruct"`
	RefText         string  `json:"refText"`
	Seed            int     `json:"seed"`
	SpeedMultiplier float64 `json:"speedMultiplier"`
	CloneModel      string  `json:"cloneModel"`
	DesignModel     string  `json:"designModel"`
	CreatedAt       int64   `json:"createdAt"`
	AudioURL        string  `json:"audioUrl"`
	// Set only when tts-service failed to (re)build this preset's stable
	// reference clip; the preset is still saved and usable, just via the
	// unclamped instruct+seed fallback until this is retried (the
	// "Regenerate" button - see handleRegenerateCustomVoicePreset - or an
	// edit that saves unchanged, or waiting for tts-service's clone model
	// to come up).
	RefError string `json:"refError,omitempty"`
	// GroupLabel is set only by handleListCustomVoicePresets, from
	// Store.VoicePresetGroups - the series name (or book title, if no
	// series) this preset's assigned character belongs to, so the Voices
	// page can group auto-created speaker voices by series/book instead of
	// listing every custom preset in one flat list. "" for a preset never
	// assigned to any character.
	GroupLabel string `json:"groupLabel,omitempty"`
}

func voicePresetDTO(p store.VoicePreset, refErr error) customVoicePresetDTO {
	dto := customVoicePresetDTO{
		ID: p.ID, Name: p.Name, Instruct: p.Instruct, RefText: p.RefText, Seed: p.Seed,
		SpeedMultiplier: p.SpeedMultiplier, CloneModel: p.CloneModel, DesignModel: p.DesignModel, CreatedAt: p.CreatedAt,
		AudioURL: "/api/voices/custom-presets/" + p.ID + "/audio",
	}
	if refErr != nil {
		dto.RefError = refErr.Error()
	}
	return dto
}

func (s *Server) handleListCustomVoicePresets(w http.ResponseWriter, r *http.Request) {
	presets, err := s.Store.ListVoicePresets()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	groups, err := s.Store.VoicePresetGroups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]customVoicePresetDTO, len(presets))
	for i, p := range presets {
		out[i] = voicePresetDTO(p, nil)
		out[i].GroupLabel = groups[p.ID]
	}
	writeJSON(w, http.StatusOK, out)
}

// DefaultCloneModel re-exports voices.DefaultCloneModel for existing call
// sites in this package.
const DefaultCloneModel = voices.DefaultCloneModel

// ensureVoiceRef returns presetID's reference clip path, rendering it
// through the job queue (jobs.Manager.RunVoiceProvision) if it doesn't
// exist yet, instead of calling voicerefs.EnsureFile directly. The
// existence check itself stays outside the queue (a plain os.Stat, the
// overwhelmingly common case for anything already rendered - previewing a
// preset, serving book narration) so it never waits on a poolDesign slot;
// only an actual VoiceDesign render does. That's what keeps this from ever
// spawning its own worker-side model instance uncoordinated with every
// other kind of voice work already contending for that same single slot -
// see internal/jobs.KindVoiceProvision's own doc comment. name is the Jobs
// dashboard label.
//
// jobs.Manager.generate's own KindVoiceClone case now uses this exact same
// pattern directly (its own os.Stat fast path, then a nested
// RunVoiceProvision call on a miss) despite running inside an already-
// dispatched poolGeneration task - safe specifically because
// KindVoiceProvision lives in its own poolDesign now, a genuinely different,
// independently-managed pool from poolGeneration, so that nested call
// blocks on a pool it isn't itself occupying (see generate's own doc
// comment for the full reasoning, including the deadlock-shaped bug that
// skipping the fast path would reintroduce). Do not, however, call this
// (or nest RunVoiceProvision at all) from provisionCharacterVoice/
// regenerateCharacterVoice when themselves invoked via RunVoiceProvision/
// EnqueueVoiceProvision - those already run *inside* a dispatched
// poolDesign task, and poolDesign's own single slot means a second nested
// call there really would deadlock waiting on itself, the same shape this
// comment used to warn about more broadly - call voicerefs.EnsureFile/
// Regenerate directly there instead, exactly as those call sites already
// do.
func (s *Server) ensureVoiceRef(ctx context.Context, presetID, name, instruct string, seed int, refText string, speedMultiplier float64, cloneModel, designModel string) (string, error) {
	path := audiopath.VoicePresetRefFile(s.DataDir, presetID)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	return s.Jobs.RunVoiceProvision(ctx, "", presetID, "generate", name, func(ctx context.Context, attempt int) (string, error) {
		return s.renderWithSeedBump(presetID, seed, attempt, func(effectiveSeed int) (string, error) {
			return voicerefs.EnsureFile(ctx, s.TTS, s.DataDir, presetID, instruct, effectiveSeed, refText, speedMultiplier, cloneModel, designModel)
		})
	})
}

// regenerateVoiceRef is ensureVoiceRef's always-re-render counterpart,
// routing voicerefs.Regenerate through the same RunVoiceProvision queue
// call ("regenerate" mode) instead of calling it directly - same
// model-instance-contention reasoning as ensureVoiceRef, minus the
// os.Stat fast path since a regenerate always needs a fresh render
// regardless of what's already on disk.
func (s *Server) regenerateVoiceRef(ctx context.Context, presetID, name, instruct string, seed int, refText string, speedMultiplier float64, cloneModel, designModel string) (string, error) {
	return s.Jobs.RunVoiceProvision(ctx, "", presetID, "regenerate", name, func(ctx context.Context, attempt int) (string, error) {
		return s.renderWithSeedBump(presetID, seed, attempt, func(effectiveSeed int) (string, error) {
			return voicerefs.Regenerate(ctx, s.TTS, s.DataDir, presetID, instruct, effectiveSeed, refText, speedMultiplier, cloneModel, designModel)
		})
	})
}

// renderWithSeedBump perturbs seed by attempt before calling render, and -
// on a successful render from a genuine retry (attempt > 0) - persists the
// bumped seed back onto presetID's own row via UpdateVoicePresetSeed.
// VoiceDesign is fully deterministic on (instruct, seed, refText,
// language) (see findDerivationSource's own doc comment), so a naive retry
// with an unchanged seed after a numerically-unstable crash ("Qwen3
// sampler has no finite logits", a real, repeatedly-observed audio.cpp
// failure mode) is guaranteed to reproduce the exact same crash rather
// than actually getting a second try. attempt 0 (the common case) always
// renders with the preset's own unmodified seed, so nothing changes for a
// normal, non-retried render. Persisting a successful bumped seed matters
// because the preset's *stored* seed is otherwise the source of truth a
// later "Regenerate"/edit-save-unchanged reads from - without this, that
// next call would silently revert to the original, crash-triggering seed
// and fail again from attempt 0 every time.
func (s *Server) renderWithSeedBump(presetID string, seed, attempt int, render func(effectiveSeed int) (string, error)) (string, error) {
	effectiveSeed := seed + attempt
	result, err := render(effectiveSeed)
	if err != nil {
		return result, err
	}
	if attempt > 0 {
		if uerr := s.Store.UpdateVoicePresetSeed(presetID, effectiveSeed); uerr != nil {
			log.Printf("httpapi: persist bumped seed for preset %s: %v", presetID, uerr)
		}
	}
	return result, nil
}

type customVoicePresetRequest struct {
	Name            string   `json:"name"`
	Instruct        string   `json:"instruct"`
	RefText         string   `json:"refText"`
	Seed            *int     `json:"seed"`            // nil on create = server picks a random one
	SpeedMultiplier *float64 `json:"speedMultiplier"` // nil = 1.0 (no change)
	CloneModel      *string  `json:"cloneModel"`      // nil on create = DefaultCloneModel; nil on update = unchanged
	// DesignModel: nil on create = voices.DefaultDesignModel (always
	// breeze_tts for a brand-new voice, regardless of whether its recipe
	// happens to match an existing preset's own - see voices.
	// DefaultDesignModel's own doc comment); nil on update = unchanged.
	DesignModel *string `json:"designModel"`
}

func (s *Server) handleCreateCustomVoicePreset(w http.ResponseWriter, r *http.Request) {
	var req customVoicePresetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || req.Instruct == "" || req.RefText == "" {
		writeError(w, http.StatusBadRequest, "name, instruct, and refText are required")
		return
	}
	seed := req.Seed
	var seedVal int
	if seed != nil {
		seedVal = *seed
	} else {
		seedVal = store.RandomSeed()
	}
	speedVal := 1.0
	if req.SpeedMultiplier != nil {
		speedVal = *req.SpeedMultiplier
	}
	cloneModel := DefaultCloneModel
	if req.CloneModel != nil && *req.CloneModel != "" {
		cloneModel = *req.CloneModel
	}
	designModel := voices.DefaultDesignModel
	if req.DesignModel != nil && *req.DesignModel != "" {
		designModel = *req.DesignModel
	}

	p, err := s.Store.CreateVoicePreset(req.Name, req.Instruct, req.RefText, seedVal, speedVal, cloneModel, designModel)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var refErr error
	if cached, ok := voicerefs.LookupCachedDesign(s.DataDir, voicerefs.DesignConfigHash(p.Instruct, p.Seed, p.RefText, "Auto", p.DesignModel)); ok {
		// A "test this design" preview (POST /api/voices/design-test)
		// already rendered exactly this instruct/seed/refText/designModel
		// at natural pace - re-stretch it to this preset's own speed
		// instead of paying for a second VoiceDesign render. The common
		// case right after previewing a brand-new voice and saving it
		// unchanged.
		_, refErr = voicerefs.DeriveFromCached(s.DataDir, p.ID, cached, speedVal)
	} else if sourceID, sourceSpeed, ok := s.findDerivationSource(p.Instruct, p.RefText, p.Seed, p.DesignModel, p.ID); ok {
		// Same instruct/seed/ref_text/designModel as an existing,
		// already-rendered preset - VoiceDesign is deterministic on that
		// tuple, so re-stretching the source's clip gives the same result
		// a full re-render would, without paying for one.
		_, refErr = voicerefs.DeriveFromExisting(r.Context(), s.DataDir, p.ID, sourceID, speedVal/sourceSpeed)
	} else {
		_, refErr = s.ensureVoiceRef(r.Context(), p.ID, p.Name, p.Instruct, p.Seed, p.RefText, p.SpeedMultiplier, p.CloneModel, p.DesignModel)
	}
	writeJSON(w, http.StatusCreated, voicePresetDTO(p, refErr))
}

// findDerivationSource looks for an existing preset (built-in or custom,
// other than excludeID) with the exact same instruct/refText/seed/
// designModel as a preset being created, and an already-rendered reference
// clip on disk. cloneModel is deliberately NOT part of the match:
// VoiceDesign rendering (what produces the reference clip) never depends on
// which clone_model a preset uses for per-paragraph cloning afterward -
// only instruct/seed/refText/language/designModel do (see internal/
// audioworker's Design). designModel IS part of the match, unlike
// cloneModel - two presets with identical instruct/seed/refText but
// different design engines render audibly different clips, so deriving one
// from the other would silently serve the wrong engine's sound; this is
// also why a brand-new preset's own designModel is always set explicitly
// (voices.DefaultDesignModel) rather than left blank, so it can never
// accidentally match a legacy/built-in source that happens to resolve to
// the same engine today but isn't recorded as such. When found, the caller
// can derive the new preset's clip by re-stretching the source's instead of
// re-rendering via VoiceDesign - see voicerefs.DeriveFromExisting.
func (s *Server) findDerivationSource(instruct, refText string, seed int, designModel, excludeID string) (sourceID string, sourceSpeed float64, ok bool) {
	hasRef := func(id string) bool {
		_, err := os.Stat(audiopath.VoicePresetRefFile(s.DataDir, id))
		return err == nil
	}

	for _, p := range voices.Presets {
		if p.ID != excludeID && p.Instruct == instruct && p.RefText == refText && p.Seed == seed && p.DesignModel == designModel && hasRef(p.ID) {
			return p.ID, p.SpeedMultiplier, true
		}
	}
	if customs, err := s.Store.ListVoicePresets(); err == nil {
		for _, p := range customs {
			if p.ID != excludeID && p.Instruct == instruct && p.RefText == refText && p.Seed == seed && p.DesignModel == designModel && hasRef(p.ID) {
				return p.ID, p.SpeedMultiplier, true
			}
		}
	}
	return "", 0, false
}

func (s *Server) handleUpdateCustomVoicePreset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing, err := s.Store.GetVoicePreset(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "voice preset not found")
		return
	}

	var req customVoicePresetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" || req.Instruct == "" || req.RefText == "" {
		writeError(w, http.StatusBadRequest, "name, instruct, and refText are required")
		return
	}
	seedVal := existing.Seed
	if req.Seed != nil {
		seedVal = *req.Seed
	}
	speedVal := existing.SpeedMultiplier
	if req.SpeedMultiplier != nil {
		speedVal = *req.SpeedMultiplier
	}
	cloneModelVal := existing.CloneModel
	if req.CloneModel != nil && *req.CloneModel != "" {
		cloneModelVal = *req.CloneModel
	}
	designModelVal := existing.DesignModel
	if req.DesignModel != nil && *req.DesignModel != "" {
		designModelVal = *req.DesignModel
	}

	if err := s.Store.UpdateVoicePreset(id, req.Name, req.Instruct, req.RefText, seedVal, speedVal, cloneModelVal, designModelVal); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p := store.VoicePreset{
		ID: id, Name: req.Name, Instruct: req.Instruct, RefText: req.RefText, Seed: seedVal,
		SpeedMultiplier: speedVal, CloneModel: cloneModelVal, DesignModel: designModelVal, CreatedAt: existing.CreatedAt,
	}

	// instruct/refText/seed/speedMultiplier/cloneModel/designModel all feed
	// the reference clip, so any of them changing means it has to be
	// re-rendered - EnsureFile alone would just keep serving the stale one
	// since a file's already there. Same "regenerate" mode/dedup key as
	// handleRegenerateCustomVoicePreset, so an edit-save and an explicit
	// "Regenerate" click for the same preset correctly join into one queued
	// render instead of racing.
	_, refErr := s.regenerateVoiceRef(r.Context(), p.ID, p.Name, p.Instruct, p.Seed, p.RefText, p.SpeedMultiplier, p.CloneModel, p.DesignModel)
	writeJSON(w, http.StatusOK, voicePresetDTO(p, refErr))
}

// handleRegenerateCustomVoicePreset force re-renders id's reference clip
// via VoiceDesign from its currently-saved recipe, without touching any of
// the preset's saved fields - the "Regenerate" button on the Voices page,
// for retrying after a RefError (tts-service was briefly down, e.g.) or
// just to pick up a fresh render, without going through Edit -> Save
// unchanged to get the same effect (see handleUpdateCustomVoicePreset,
// which always regenerates too, but only as a side effect of saving).
//
// Routed through jobs.Manager.RunVoiceProvision (a KindVoiceProvision
// task, dedup key "regenerate:"+p.ID) rather than calling voicerefs.
// Regenerate directly, so this shows up on the Jobs dashboard like any
// other voice work and queues behind - never races - a characterization
// task for the same identity (see taskBlockedBySpeakerCharacterization);
// still blocks this handler's own response the same way the direct call
// used to, so the response shape (voicePresetDTO, synchronous refErr)
// is unchanged for the frontend.
//
// A successful re-render invalidates every already-generated paragraph
// that was cloned from the old clip: the recipe (instruct/seed/refText)
// is unchanged by this action, so paragraph_audio's own voice_id (a hash
// of presetID+instruct+language, not of the clip's actual audio content -
// see store.VoiceID) never changes and wouldn't otherwise orphan anything,
// even though the underlying reference audio genuinely did just change -
// see invalidateAudioForVoicePreset.
func (s *Server) handleRegenerateCustomVoicePreset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := s.Store.GetVoicePreset(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "voice preset not found")
		return
	}

	_, refErr := s.Jobs.RunVoiceProvision(r.Context(), "", p.ID, "regenerate", p.Name, func(ctx context.Context, attempt int) (string, error) {
		return s.renderWithSeedBump(p.ID, p.Seed, attempt, func(effectiveSeed int) (string, error) {
			_, err := voicerefs.Regenerate(ctx, s.TTS, s.DataDir, p.ID, p.Instruct, effectiveSeed, p.RefText, p.SpeedMultiplier, p.CloneModel, p.DesignModel)
			return p.ID, err
		})
	})
	if refErr == nil {
		s.invalidateAudioForVoicePreset(*p)
	}
	writeJSON(w, http.StatusOK, voicePresetDTO(*p, refErr))
}

// invalidateAudioForVoicePreset deletes every already-generated paragraph
// (across every book, not just one - a custom preset can be shared by any
// number of books/characters) whose audio was actually cloned from
// preset's own reference clip, now that a fresh one exists - see
// handleRegenerateCustomVoicePreset's own doc comment for why this can't
// rely on paragraph_audio's voice_id changing on its own the way a
// characterization's instruct rewrite does (see recharacterizeAndInvalidate).
//
// Checks two possible voice_id candidates per book, because
// narration.Resolver resolves the "instruct" hash input differently
// depending on how a paragraph reaches this preset: BookVoice (a book
// whose own VoicePresetID is preset.ID) uses that book's own saved
// VoiceInstruct copy, while CharacterVoice (any character, in any book,
// assigned this preset) uses preset.Instruct directly - the two are
// normally equal (kept in sync when a preset is selected) but aren't
// guaranteed to be, so both are checked rather than assumed identical.
// Every other book/language combination simply won't match anything and
// costs one cheap no-op query - fine at this app's own library scale (see
// backend/CLAUDE.md's "single-reader app" framing elsewhere).
//
// Best-effort: logged, not surfaced as a request failure, since the
// regenerate itself (the caller's actual job) already succeeded by the
// time this runs; worst case some now-stale audio lingers reachable
// until manually cleared, same tradeoff recharacterizeAndInvalidate's own
// version of this already accepts.
func (s *Server) invalidateAudioForVoicePreset(preset store.VoicePreset) {
	books, err := s.Store.ListBooks()
	if err != nil {
		log.Printf("httpapi: could not list books to invalidate voice %q: %v", preset.Name, err)
		return
	}
	for _, b := range books {
		voiceIDs := map[string]bool{
			store.VoiceID(preset.ID, preset.Instruct, b.VoiceLanguage): true,
		}
		if b.VoicePresetID == preset.ID {
			voiceIDs[store.VoiceID(preset.ID, b.VoiceInstruct, b.VoiceLanguage)] = true
		}
		for voiceID := range voiceIDs {
			refs, derr := s.Store.DeleteParagraphAudioByVoiceID(b.ID, voiceID)
			if derr != nil {
				log.Printf("httpapi: could not invalidate audio for voice %q in book %s: %v", preset.Name, b.ID, derr)
				continue
			}
			for _, ref := range refs {
				if rerr := os.Remove(s.paragraphAudioPath(ref.BookID, ref.ChapterID, ref.VoiceID, ref.Idx)); rerr != nil && !os.IsNotExist(rerr) {
					log.Printf("httpapi: remove stale audio file for voice %q: %v", preset.Name, rerr)
				}
			}
		}
	}
}

func (s *Server) handleDeleteCustomVoicePreset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// No worker-side state to evict any more (see internal/audioworker's
	// docstring: it holds no per-preset state, only per-clone_model loaded
	// model instances) - just the on-disk reference clip and the DB row.
	if err := voicerefs.Delete(s.DataDir, id); err != nil {
		writeLoggedWarning(r, "delete cached reference clip for %s: %v", id, err)
	}
	if err := s.Store.DeleteVoicePreset(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type testVoiceRequest struct {
	Text string `json:"text"`
	// CloneModel, when set, overrides the preset's saved cloning model -
	// lets the editor preview the model currently selected in the form
	// (which may not be saved yet) instead of always hearing the preset's
	// persisted one.
	CloneModel string `json:"cloneModel"`
}

// testVoice synthesizes text with the given voice recipe and writes the
// resulting audio/wav straight to the response - shared by the built-in
// and custom preset test endpoints, which differ only in where
// instruct/seed/refText/speedMultiplier/cloneModel/designModel come from.
func (s *Server) testVoice(w http.ResponseWriter, r *http.Request, presetID, name, instruct string, seed int, refText string, speedMultiplier float64, cloneModel, designModel string) {
	var req testVoiceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	if req.CloneModel != "" {
		cloneModel = req.CloneModel
	}

	refPath, err := s.ensureVoiceRef(r.Context(), presetID, name, instruct, seed, refText, speedMultiplier, cloneModel, designModel)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ttsworker unavailable: "+err.Error())
		return
	}
	refAudio, err := os.ReadFile(refPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	audio, err := s.TTS.Generate(r.Context(), text, cloneModel, refAudio, refText, "Auto", "", "")
	if err != nil {
		writeError(w, http.StatusBadGateway, "ttsworker unavailable: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Write(audio)
}

type testVoiceDesignRequest struct {
	Instruct string `json:"instruct"`
	// Seed nil = tts-service picks one at random (fine for a quick
	// "surprise me" listen, but that render can't be reused on save - see
	// handleCreateCustomVoicePreset - since nothing here knows what seed
	// it landed on). The editor pins a seed for a brand-new voice up
	// front specifically so Test and Save always agree on one.
	Seed *int   `json:"seed"`
	Text string `json:"text"`
	// DesignModel previews a specific VoiceDesign engine before saving -
	// "" defers to the worker's own process-wide default, same as an
	// unset preset field.
	DesignModel string `json:"designModel"`
}

// handleTestVoiceDesign previews a voice instruction directly via the
// worker's stateless VoiceDesign task (no preset_id at all, unlike
// handleTestCustomVoicePreset/handleTestPreset, which both require one) -
// the only way to hear an instruct before it's been saved as a preset.
// When Seed is given, the render is also cached by its exact (instruct,
// seed, text, language) recipe (see voicerefs.DesignConfigHash) so that
// creating the preset right after testing it, unchanged, reuses this exact
// clip instead of paying for a second render.
//
// Runs through jobs.Manager.RunVoiceDesignPreview (a real
// KindVoiceDesignPreview task in poolGeneration) rather than calling
// s.TTS.Design directly - this used to bypass the job queue entirely, so a
// "Test" click competed for the ttsworker process uncoordinated with (and
// outside the concurrency limit protecting) actual paragraph generation.
// Still blocks and returns the WAV bytes inline, same as before - see
// RunVoiceDesignPreview's own doc comment for why that shape is kept.
func (s *Server) handleTestVoiceDesign(w http.ResponseWriter, r *http.Request) {
	var req testVoiceDesignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	instruct := strings.TrimSpace(req.Instruct)
	if instruct == "" {
		writeError(w, http.StatusBadRequest, "instruct is required")
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}

	var seed64 *int64
	if req.Seed != nil {
		v := int64(*req.Seed)
		seed64 = &v
	}
	audio, err := s.Jobs.RunVoiceDesignPreview(r.Context(), instruct, func(ctx context.Context) ([]byte, error) {
		return s.TTS.Design(ctx, text, instruct, "Auto", req.DesignModel, seed64)
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, "ttsworker unavailable: "+err.Error())
		return
	}
	if req.Seed != nil {
		voicerefs.CacheDesignRender(s.DataDir, voicerefs.DesignConfigHash(instruct, *req.Seed, text, "Auto", req.DesignModel), audio)
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Write(audio)
}

// handleTestCustomVoicePreset synthesizes arbitrary text with an existing
// custom preset's actual saved voice (its real clone, same path as book
// narration - not a one-off design-path guess), so the editor can hear how
// it sounds on something other than the fixed reference line before using
// it in a book.
func (s *Server) handleTestCustomVoicePreset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	preset, err := s.Store.GetVoicePreset(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if preset == nil {
		writeError(w, http.StatusNotFound, "voice preset not found")
		return
	}
	s.testVoice(w, r, preset.ID, preset.Name, preset.Instruct, preset.Seed, preset.RefText, preset.SpeedMultiplier, preset.CloneModel, preset.DesignModel)
}

// handleTestPreset is the built-in-preset equivalent of
// handleTestCustomVoicePreset, so the same "hear it on other text" preview
// works for curated presets too, not just custom ones.
func (s *Server) handleTestPreset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	preset, ok := voices.PresetsByID[id]
	if !ok {
		writeError(w, http.StatusNotFound, "preset not found")
		return
	}
	s.testVoice(w, r, preset.ID, preset.Name, preset.Instruct, preset.Seed, preset.RefText, preset.SpeedMultiplier, DefaultCloneModel, preset.DesignModel)
}

// handleRegeneratePreset is the built-in-preset equivalent of
// handleRegenerateCustomVoicePreset - force re-renders id's reference clip
// via VoiceDesign from its fixed, compiled-in recipe (built-ins are never
// stored in the DB - see internal/voices' own doc comment - but their
// rendered reference clip is cached on disk under their own id exactly
// like a custom preset's, via the same internal/voicerefs). Useful to
// retry after tts-service was briefly down when a preset's clip was first
// needed, without waiting for the next arbitrary request that happens to
// call EnsureFile on it.
func (s *Server) handleRegeneratePreset(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	preset, ok := voices.PresetsByID[id]
	if !ok {
		writeError(w, http.StatusNotFound, "preset not found")
		return
	}
	_, refErr := s.regenerateVoiceRef(r.Context(), preset.ID, preset.Name, preset.Instruct, preset.Seed, preset.RefText, preset.SpeedMultiplier, DefaultCloneModel, preset.DesignModel)
	dto := voicePresetDTOOut{
		ID: preset.ID, Name: preset.Name, Instruct: preset.Instruct, Seed: preset.Seed, RefText: preset.RefText,
		SpeedMultiplier: preset.SpeedMultiplier, CloneModel: DefaultCloneModel, DesignModel: preset.DesignModel,
		AudioURL: "/api/voices/presets/" + preset.ID + "/audio",
	}
	if refErr != nil {
		dto.RefError = refErr.Error()
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleGetPresetAudio serves a built-in preset's reference clip, so the
// user can preview what a curated narrator actually sounds like before
// picking it. Created on first request if the worker hasn't rendered one
// yet (e.g. this preset has never been used).
func (s *Server) handleGetPresetAudio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	preset, ok := voices.PresetsByID[id]
	if !ok {
		writeError(w, http.StatusNotFound, "preset not found")
		return
	}
	path, err := s.ensureVoiceRef(r.Context(), id, preset.Name, preset.Instruct, preset.Seed, preset.RefText, preset.SpeedMultiplier, DefaultCloneModel, preset.DesignModel)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ttsworker unavailable: "+err.Error())
		return
	}
	http.ServeFile(w, r, path)
}

// handleGetCustomVoiceAudio serves a custom preset's reference clip -
// what the user will actually hear as their narrator, previewable the
// same way as a built-in one. Created on first request if it's missing
// (e.g. the worker was unavailable when this preset was created/edited).
func (s *Server) handleGetCustomVoiceAudio(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	preset, err := s.Store.GetVoicePreset(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if preset == nil {
		writeError(w, http.StatusNotFound, "voice preset not found")
		return
	}
	path, err := s.ensureVoiceRef(r.Context(), id, preset.Name, preset.Instruct, preset.Seed, preset.RefText, preset.SpeedMultiplier, preset.CloneModel, preset.DesignModel)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ttsworker unavailable: "+err.Error())
		return
	}
	http.ServeFile(w, r, path)
}
