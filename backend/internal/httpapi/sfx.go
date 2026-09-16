package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// generateSFXRequest is POST /api/sfx/generate's body - the standalone
// test surface for every generation-only "gen task" engine this app
// wires in (ACE-Step, Stable Audio Music/SFX/Medium), one shared request
// shape covering the union of what any of them accept. Engine selects
// which one actually runs; every field below that engine doesn't use is
// simply ignored (e.g. Lyrics only means anything for "ace_step"). See
// audioworker.Worker.Music/StableAudioMusic/StableAudioSFX/
// StableAudioMedium's own doc comments for which knobs each engine
// actually reads and what each defaults to when left zero.
type generateSFXRequest struct {
	// Engine is "ace_step", "stable_audio_music" (the default),
	// "stable_audio_sfx", or "stable_audio_medium".
	Engine            string  `json:"engine,omitempty"`
	Prompt            string  `json:"prompt"`
	Lyrics            string  `json:"lyrics,omitempty"`
	NegativePrompt    string  `json:"negativePrompt,omitempty"`
	DurationSeconds   float64 `json:"durationSeconds,omitempty"`
	NumInferenceSteps int     `json:"numInferenceSteps,omitempty"`
	GuidanceScale     float64 `json:"guidanceScale,omitempty"`
	Seed              *int64  `json:"seed,omitempty"`
}

// handleGenerateSFX is the standalone SFX/music test page's own backend
// surface (see frontend SFXPage.tsx) - handleTestVoiceDesign's own
// counterpart for this app's generation-only engines: stateless, not
// attached to any book/chapter/paragraph, returns raw WAV bytes rather
// than persisting anything. A paragraph wanting to keep a generated
// sound-effect clip uses the paragraph-scoped POST .../generate-sfx
// endpoint instead (handleGenerateParagraphSFX, chapters.go) - this one is
// purely for iterating on a prompt/knob/engine combination before (or
// without) attaching one to a real paragraph.
//
// Dispatched through jobs.Manager.RunSFXPreview (KindSFXPreview, poolSFX)
// rather than calling s.TTS directly - same "handled as a pooled job"
// reasoning handleGenerateParagraphSFX's own doc comment gives: this way a
// standalone test render properly queues/shares poolSFX's own concurrency
// limit and TTS/LLM-pool mutual exclusion with every other real
// generation task, instead of racing the worker uncoordinated. One
// endpoint for every engine, not one per engine: RunSFXPreview doesn't
// care which of them a given call's own closure actually invokes.
func (s *Server) handleGenerateSFX(w http.ResponseWriter, r *http.Request) {
	var req generateSFXRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt is required")
		return
	}
	if s.TTS == nil {
		writeError(w, http.StatusServiceUnavailable, "tts worker not configured")
		return
	}

	engine := strings.TrimSpace(req.Engine)
	if engine == "" {
		engine = "stable_audio_music"
	}

	var run func(ctx context.Context) ([]byte, error)
	switch engine {
	case "ace_step":
		run = func(ctx context.Context) ([]byte, error) {
			return s.TTS.Music(ctx, ttsproto.MusicRequest{
				Prompt:            prompt,
				Lyrics:            strings.TrimSpace(req.Lyrics),
				NegativePrompt:    strings.TrimSpace(req.NegativePrompt),
				DurationSeconds:   req.DurationSeconds,
				NumInferenceSteps: req.NumInferenceSteps,
				GuidanceScale:     req.GuidanceScale,
				Seed:              req.Seed,
			})
		}
	case "stable_audio_music":
		run = func(ctx context.Context) ([]byte, error) {
			return s.TTS.StableAudioMusic(ctx, ttsproto.StableAudioRequest{
				Prompt:            prompt,
				NegativePrompt:    strings.TrimSpace(req.NegativePrompt),
				DurationSeconds:   req.DurationSeconds,
				NumInferenceSteps: req.NumInferenceSteps,
				GuidanceScale:     req.GuidanceScale,
				Seed:              req.Seed,
			})
		}
	case "stable_audio_sfx":
		run = func(ctx context.Context) ([]byte, error) {
			return s.TTS.StableAudioSFX(ctx, ttsproto.StableAudioRequest{
				Prompt:            prompt,
				NegativePrompt:    strings.TrimSpace(req.NegativePrompt),
				DurationSeconds:   req.DurationSeconds,
				NumInferenceSteps: req.NumInferenceSteps,
				GuidanceScale:     req.GuidanceScale,
				Seed:              req.Seed,
			})
		}
	case "stable_audio_medium":
		run = func(ctx context.Context) ([]byte, error) {
			return s.TTS.StableAudioMedium(ctx, ttsproto.StableAudioRequest{
				Prompt:            prompt,
				NegativePrompt:    strings.TrimSpace(req.NegativePrompt),
				DurationSeconds:   req.DurationSeconds,
				NumInferenceSteps: req.NumInferenceSteps,
				GuidanceScale:     req.GuidanceScale,
				Seed:              req.Seed,
			})
		}
	default:
		writeError(w, http.StatusBadRequest, "unknown engine "+engine)
		return
	}

	wavBytes, err := s.Jobs.RunSFXPreview(r.Context(), engine, prompt, run)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ttsworker unavailable: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.Write(wavBytes)
}
