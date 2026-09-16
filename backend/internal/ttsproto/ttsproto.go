// Package ttsproto is the wire format shared between internal/ttsworker
// (the client/supervisor side, imported by cmd/server) and internal/
// audioworker (the actual worker implementation, imported only by
// cmd/ttsworker). It has no dependency on audiocpp-go and must stay that
// way: it's what lets both sides of the process boundary share one set of
// request/response types without cmd/server ever transitively importing
// audiocpp-go (see backend/CLAUDE.md's "never link libaudiocpp.so into
// cmd/server" invariant).
package ttsproto

// GenerateRequest is POST /generate's body: clone cloneModel's voice from a
// caller-supplied reference clip (raw WAV bytes, base64) plus a paragraph
// of text. The worker holds no per-preset state - the caller (backend) owns
// and persists reference clips on its own disk, and resends the relevant
// one on every call; audio.cpp's own session-level cache (keyed by hash of
// reference audio + text) transparently dedupes repeated pairs.
type GenerateRequest struct {
	Text           string `json:"text"`
	CloneModel     string `json:"cloneModel"`
	RefAudioBase64 string `json:"refAudioBase64"`
	RefText        string `json:"refText"`
	Language       string `json:"language"`
	// Instruct is a clone-time style instruction sent alongside the
	// reference clip - only honored by a family with its own instructOption
	// (breeze_tts today - see audioworker.cloneFamily.instructOption's own
	// doc comment), silently ignored by every other family. "" (most
	// callers) is today's unchanged behavior.
	Instruct string `json:"instruct,omitempty"`
	// GuidanceScale, when non-empty, overrides the family's own configured
	// instruct-time guidance_scale for this one call - see
	// audioworker.GenerateRequest.GuidanceScale's own doc comment. Ignored
	// entirely unless Instruct is also set.
	GuidanceScale string `json:"guidanceScale,omitempty"`
}

// DesignRequest is POST /design's body: render a fresh reference clip via
// VoiceDesign, at natural pace (unstretched - speed adjustment is the
// caller's job, see internal/wsola).
type DesignRequest struct {
	RefText  string `json:"refText"`
	Instruct string `json:"instruct"`
	Seed     *int64 `json:"seed,omitempty"`
	Language string `json:"language"`
	// DesignModel selects which VoiceDesign engine renders this clip (see
	// audioworker's designEngines - "qwen3_tts"/"breeze_tts") - ""
	// defers to the worker's own process-wide default.
	DesignModel string `json:"designModel,omitempty"`
}

// MusicRequest is POST /music's body: render an ACE-Step music clip from
// Prompt (plus optional Lyrics), via ACE-Step's "text2music" route only -
// see audioworker.Worker.Music's own doc comment for why this app never
// drives its other, source-audio-conditioned routes. Every field but
// Prompt left at its zero value defers to audio.cpp's own built-in
// default (duration -1/auto, num_inference_steps 8, guidance_scale 1.0,
// seed random - see docs/models/ace_step.md in the audio.cpp checkout).
type MusicRequest struct {
	Prompt            string  `json:"prompt"`
	Lyrics            string  `json:"lyrics,omitempty"`
	NegativePrompt    string  `json:"negativePrompt,omitempty"`
	DurationSeconds   float64 `json:"durationSeconds,omitempty"`
	NumInferenceSteps int     `json:"numInferenceSteps,omitempty"`
	GuidanceScale     float64 `json:"guidanceScale,omitempty"`
	Seed              *int64  `json:"seed,omitempty"`
}

// StableAudioRequest is POST /stable-audio's body: render a Stable Audio
// 3 clip from Prompt. Every field but Prompt left at its zero value defers
// to audio.cpp's own built-in default (duration 120s, num_inference_steps
// 8, guidance_scale 1.0, seed random - see docs/models/stable_audio.md in
// the audio.cpp checkout).
//
// InitAudioBase64 (raw WAV bytes, base64, the same shape RefAudioBase64
// already uses elsewhere in this file) opts into one of Stable Audio's two
// source-audio-conditioned paths, chosen by which of the two field groups
// below is actually set - never both:
//
//   - InitNoiseLevel > 0: audio_input_kind=init_audio, a whole-clip img2img-
//     style transformation (the model's own init_noise_level strength
//     parameter, 0..1, controls how much the output is allowed to deviate
//     from InitAudioBase64 - low values reproduce it closely, ~1.0 is
//     near-full reinterpretation). Not used by this app's own
//     internal/musicgen today - see InpaintMaskStartSeconds/
//     InpaintMaskEndSeconds below for why continuation specifically needed
//     something else - but kept as a real, working option for a future
//     caller that actually wants timbre/style transfer of a whole clip.
//   - InpaintMaskEndSeconds > 0: audio_input_kind=inpaint_audio, masking
//     exactly [InpaintMaskStartSeconds, InpaintMaskEndSeconds) of the
//     *requested* output timeline (not of InitAudioBase64's own length) for
//     the model to fill with new content, leaving everything outside that
//     range close to InitAudioBase64 - this is what internal/musicgen
//     actually uses for continuation-seeded chunks (see its own package
//     doc comment): setting InpaintMaskStartSeconds to InitAudioBase64's
//     own duration and InpaintMaskEndSeconds to DurationSeconds (the full
//     desired output length) is exactly Stable Audio 3's own documented
//     recipe for extending a clip past its current end ("For
//     continuations, set the mask start to the current end of your audio
//     and mask end to where you want the extension to finish" -
//     stable-audio-3's own prompting guide), which - unlike init_audio -
//     actually generates new material past the seed instead of
//     reproducing it and then trailing into silence once InitAudioBase64's
//     own content runs out (confirmed empirically against this box's real
//     GPU: init_audio at a low noise level reproduces the seed almost
//     verbatim for the seed's own duration, then outputs near-silence for
//     the rest of a longer DurationSeconds).
type StableAudioRequest struct {
	Prompt            string  `json:"prompt"`
	NegativePrompt    string  `json:"negativePrompt,omitempty"`
	DurationSeconds   float64 `json:"durationSeconds,omitempty"`
	NumInferenceSteps int     `json:"numInferenceSteps,omitempty"`
	GuidanceScale     float64 `json:"guidanceScale,omitempty"`
	Seed              *int64  `json:"seed,omitempty"`
	InitAudioBase64   string  `json:"initAudioBase64,omitempty"`
	InitNoiseLevel    float64 `json:"initNoiseLevel,omitempty"`
	// InpaintMaskStartSeconds/InpaintMaskEndSeconds - see the doc comment
	// above for the exact continuation recipe. Both are relative to the
	// requested *output* timeline (DurationSeconds), not to
	// InitAudioBase64's own length - audio.cpp's own inpaint_mask_start/
	// end_seconds request options.
	InpaintMaskStartSeconds float64 `json:"inpaintMaskStartSeconds,omitempty"`
	InpaintMaskEndSeconds   float64 `json:"inpaintMaskEndSeconds,omitempty"`
}

// AlignRequest is POST /align's body: forced word-level alignment of Text
// against AudioBase64 (a WAV clip, e.g. one already generated by
// /generate).
type AlignRequest struct {
	Text        string `json:"text"`
	AudioBase64 string `json:"audioBase64"`
	Language    string `json:"language"`
}

// Word is one forced-alignment result. No confidence field: audio.cpp
// hardcodes it to 0.0 in its own C++ source rather than computing anything,
// so it's not worth sending a field that's always the same meaningless
// placeholder.
type Word struct {
	Text  string  `json:"text"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

// AlignResponse is POST /align's response body.
type AlignResponse struct {
	Words []Word `json:"words"`
}

// LLMGenerateRequest is POST /llm/generate's body: one independent chat
// completion (system + user turn in, reply text out) against the worker's
// embedded GGUF model (internal/llmworker) - the wire-format counterpart of
// what speakerattr.Client.generate used to build and run in-process itself
// before the LLM moved into ttsworker alongside the TTS models (see
// backend/CLAUDE.md's "ttsworker / audioworker" section).
type LLMGenerateRequest struct {
	SystemPrompt string  `json:"systemPrompt"`
	UserPrompt   string  `json:"userPrompt"`
	Temp         float32 `json:"temp"`
	MaxTokens    int     `json:"maxTokens"`
}

// LLMGenerateResponse is POST /llm/generate's response body.
type LLMGenerateResponse struct {
	Text string `json:"text"`
}

// HealthResponse is GET /health's response body.
type HealthResponse struct {
	Status            string   `json:"status"`
	DefaultCloneModel string   `json:"defaultCloneModel"`
	CloneModelsLoaded []string `json:"cloneModelsLoaded"`
	// LLMModelLoaded reports whether internal/llmworker's own model is
	// currently resident - false either because it's never been used yet
	// or because it was idle-unloaded (see llmworker.Config.
	// IdleUnloadAfter); either way, the next POST /llm/generate call
	// reloads it lazily, same as CloneModelsLoaded omitting an entry that
	// simply hasn't been requested yet.
	LLMModelLoaded bool `json:"llmModelLoaded"`
}

// ErrorResponse is the body of any non-2xx response from the worker.
type ErrorResponse struct {
	Detail string `json:"detail"`
}
