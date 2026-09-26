package audioworker

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

func writeError(w http.ResponseWriter, status int, err error) {
	log.Printf("request failed: %v", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ttsproto.ErrorResponse{Detail: err.Error()})
}

func writeWav(w http.ResponseWriter, samples []float32, sampleRate, channels int) {
	data, err := encodeWav(samples, sampleRate, channels)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// HandleGenerate serves POST /generate: clone cloneModel's voice from a
// caller-supplied reference clip, given a paragraph of text.
func (w *Worker) HandleGenerate(rw http.ResponseWriter, r *http.Request) {
	var req ttsproto.GenerateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	// A noReference family (see families.go's own doc comment) ignores
	// RefSamples/RefText entirely, so a caller sending an empty
	// RefAudioBase64 for one (voicerefs.Regenerate does, for exactly this
	// reason) is expected, not an error - skip WAV-decoding a payload
	// that's never going to be used rather than failing on an empty/
	// invalid WAV before Worker.Generate ever gets a chance to ignore it.
	clip := &wavClip{}
	if req.RefAudioBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(req.RefAudioBase64)
		if err != nil {
			writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid refAudioBase64: %w", err))
			return
		}
		clip, err = decodeWav(raw)
		if err != nil {
			writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid reference wav: %w", err))
			return
		}
	}

	res, err := w.GenerateFull(GenerateRequest{
		Text:           req.Text,
		CloneModel:     req.CloneModel,
		RefSamples:     clip.Samples,
		RefSampleRate:  clip.SampleRate,
		RefChannels:    clip.Channels,
		RefText:        req.RefText,
		Language:       req.Language,
		Instruct:       req.Instruct,
		GuidanceScale:  req.GuidanceScale,
		Temperature:    req.Temperature,
		TextChunkSize:  req.TextChunkSize,
		ReturnLatents:  req.ReturnLatents,
		ReferenceCodes: req.ReferenceCodes,
	})
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}
	if !req.ReturnLatents {
		writeWav(rw, res.Audio.Samples, res.Audio.SampleRate, res.Audio.Channels)
		return
	}
	data, err := encodeWav(res.Audio.Samples, res.Audio.SampleRate, res.Audio.Channels)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}
	writeJSON(rw, ttsproto.GenerateResponse{WAV: data, Latents: res.Latents, ReferenceCodes: res.ReferenceCodes})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// HandleDesign serves POST /design.
func (w *Worker) HandleDesign(rw http.ResponseWriter, r *http.Request) {
	var req ttsproto.DesignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}

	audio, err := w.Design(req.RefText, req.Instruct, req.Language, req.DesignModel, req.Seed, req.GuidanceScale, req.Temperature)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}
	writeWav(rw, audio.Samples, audio.SampleRate, audio.Channels)
}

// HandleMusic serves POST /music: render an ACE-Step music clip.
func (w *Worker) HandleMusic(rw http.ResponseWriter, r *http.Request) {
	var req ttsproto.MusicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}

	audio, err := w.Music(req)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}
	writeWav(rw, audio.Samples, audio.SampleRate, audio.Channels)
}

// HandleStableAudioMusic serves POST /stable-audio-music: render a Stable
// Audio 3 clip via the Small/Music checkpoint.
func (w *Worker) HandleStableAudioMusic(rw http.ResponseWriter, r *http.Request) {
	var req ttsproto.StableAudioRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}

	audio, err := w.StableAudioMusic(req)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}
	writeWav(rw, audio.Samples, audio.SampleRate, audio.Channels)
}

// HandleStableAudioSFX serves POST /stable-audio-sfx: render a Stable
// Audio 3 clip via the Small/SFX checkpoint - HandleStableAudioMusic's own
// counterpart, same request shape.
func (w *Worker) HandleStableAudioSFX(rw http.ResponseWriter, r *http.Request) {
	var req ttsproto.StableAudioRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}

	audio, err := w.StableAudioSFX(req)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}
	writeWav(rw, audio.Samples, audio.SampleRate, audio.Channels)
}

// HandleStableAudioMedium serves POST /stable-audio-medium: render a
// Stable Audio 3 clip via the larger Medium checkpoint -
// HandleStableAudioMusic's own counterpart, same request shape.
func (w *Worker) HandleStableAudioMedium(rw http.ResponseWriter, r *http.Request) {
	var req ttsproto.StableAudioRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}

	audio, lat, err := w.StableAudioMediumFull(req)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}
	if !req.ReturnLatents {
		writeWav(rw, audio.Samples, audio.SampleRate, audio.Channels)
		return
	}
	data, err := encodeWav(audio.Samples, audio.SampleRate, audio.Channels)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}
	writeJSON(rw, ttsproto.StableAudioResponse{WAV: data, Latents: lat})
}

// HandleCodecDecode serves POST /codec-decode: WAV for the request's
// latents - see Worker.CodecDecode.
func (w *Worker) HandleCodecDecode(rw http.ResponseWriter, r *http.Request) {
	var req ttsproto.CodecDecodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	audio, err := w.CodecDecode(req)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}
	writeWav(rw, audio.Samples, audio.SampleRate, audio.Channels)
}

// HandleCodecEncode serves POST /codec-encode: a ttsproto.Latents (JSON)
// for the request's audio - see Worker.CodecEncode.
func (w *Worker) HandleCodecEncode(rw http.ResponseWriter, r *http.Request) {
	var req ttsproto.CodecEncodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	lat, err := w.CodecEncode(req)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}
	writeJSON(rw, lat)
}

// HandleTranscribe serves POST /transcribe.
func (w *Worker) HandleTranscribe(rw http.ResponseWriter, r *http.Request) {
	var req ttsproto.TranscribeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	raw, err := base64.StdEncoding.DecodeString(req.AudioBase64)
	if err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid audioBase64: %w", err))
		return
	}
	clip, err := decodeWav(raw)
	if err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid wav: %w", err))
		return
	}

	text, err := w.Transcribe(clip.Samples, clip.SampleRate, clip.Channels)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(ttsproto.TranscribeResponse{Text: text})
}

// HandleAlign serves POST /align.
func (w *Worker) HandleAlign(rw http.ResponseWriter, r *http.Request) {
	var req ttsproto.AlignRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	raw, err := base64.StdEncoding.DecodeString(req.AudioBase64)
	if err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid audioBase64: %w", err))
		return
	}
	clip, err := decodeWav(raw)
	if err != nil {
		writeError(rw, http.StatusBadRequest, fmt.Errorf("invalid wav: %w", err))
		return
	}

	words, err := w.Align(req.Text, clip.Samples, clip.SampleRate, clip.Channels, req.Language)
	if err != nil {
		writeError(rw, http.StatusInternalServerError, err)
		return
	}
	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(ttsproto.AlignResponse{Words: words})
}

// HandleUnload serves POST /unload: frees every currently-loaded clone
// model right now (see Worker.UnloadAll's own doc comment) rather than
// waiting for Config.IdleUnloadAfter - called by cmd/server (via
// internal/ttsworker.Manager.UnloadTTS) on a genuine pool-family switch
// away from TTS work, or when the reader pauses the whole job queue - see
// backend/CLAUDE.md's "ttsworker / audioworker / llmworker" section.
func (w *Worker) HandleUnload(rw http.ResponseWriter, r *http.Request) {
	w.UnloadAll()
	rw.WriteHeader(http.StatusNoContent)
}

// HandleHealth returns a GET /health handler. llmLoaded reports whether
// ttsworker's own internal/llmworker.Worker currently has its model
// resident - audioworker has no reference to that Worker itself (a
// different, sibling package, both wired together only by cmd/ttsworker's
// own main), so the caller passes its current value in on every request
// (see cmd/ttsworker/main.go's own /health handler) rather than this
// package importing llmworker just to ask.
func (w *Worker) HandleHealth(llmLoaded bool) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(ttsproto.HealthResponse{
			Status:            "ok",
			DefaultCloneModel: w.defaultCloneModel,
			CloneModelsLoaded: w.loadedCloneModelIDs(),
			LLMModelLoaded:    llmLoaded,
		})
	}
}
