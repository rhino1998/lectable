package audioworker

import (
	"encoding/base64"
	"fmt"
	"strconv"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
	"github.com/rhino1998/lectable/backend/internal/latents"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// stableAudio renders a Stable Audio 3 clip from req.Prompt on engineKey's
// own auxEngine - "stable_audio_music" (Small/Music - see
// stableAudioMusicModelPath's own doc comment), "stable_audio_sfx"
// (Small/SFX - see stableAudioSFXModelPath's own doc comment), or
// "stable_audio_medium" (the larger Medium package - see
// stableAudioMediumModelPath's own doc comment): all three are the same
// audio.cpp family/task and request shape, just three different
// checkpoints (see docs/models/stable_audio.md's own "same user-facing
// controls" framing for Medium, and "intended for SFX prompts instead of
// music prompts" for Small/SFX), so this one function backs all three of
// Worker's own exported entry points rather than duplicating it three
// times. Plain text-to-audio when req.InitAudioBase64 == "" (every SFX/
// one-off preview caller today); otherwise takes whichever of Stable
// Audio's two source-audio-conditioned paths req itself selects
// (req.InpaintMaskEndSeconds > 0 for inpaint_audio, else init_audio) -
// see ttsproto.StableAudioRequest's own doc comment for the full
// difference and why internal/musicgen specifically needs inpaint_audio
// for continuation. Every field but Prompt left at its zero value defers
// to audio.cpp's own built-in default - see ttsproto.StableAudioRequest's
// own doc comment.
func (w *Worker) stableAudio(engineKey string, req ttsproto.StableAudioRequest) (*audiocpp.AudioBuffer, error) {
	audio, _, err := w.stableAudioFull(engineKey, req)
	return audio, err
}

// stableAudioFull is stableAudio plus req.ReturnLatents' latents (the whole
// generated window) and req.InitLatents in place of init/inpaint audio.
func (w *Worker) stableAudioFull(engineKey string, req ttsproto.StableAudioRequest) (*audiocpp.AudioBuffer, *ttsproto.Latents, error) {
	w.touch()

	eng := w.auxEngine(engineKey)
	if err := w.getAux(eng); err != nil {
		return nil, nil, err
	}
	defer w.releaseAux(eng)

	session, err := w.checkoutAuxSession(eng)
	if err != nil {
		return nil, nil, err
	}

	request := audiocpp.NewRequest()
	defer request.Close()
	request.SetText(req.Prompt, "")
	if req.InitLatents != nil && req.InitAudioBase64 != "" {
		eng.avail <- session
		return nil, nil, fmt.Errorf("initLatents and initAudioBase64 are mutually exclusive")
	}
	if req.InitLatents != nil {
		if got, want := req.InitLatents.Meta["codec_model"], modelIdentity(eng.modelPath); !sameModel(got, want) {
			eng.avail <- session
			return nil, nil, fmt.Errorf("%s (latents from %q, loaded %q)", ttsproto.ErrLatentsModelMismatch, got, want)
		}
		lat, err := latentsFromWire(req.InitLatents)
		if err == nil {
			err = request.AddLatents(lat)
		}
		if err != nil {
			eng.avail <- session
			return nil, nil, fmt.Errorf("invalid initLatents: %w", err)
		}
		setAudioInputKind(request, req)
	}
	if req.InitAudioBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(req.InitAudioBase64)
		if err != nil {
			eng.avail <- session
			return nil, nil, fmt.Errorf("invalid initAudioBase64: %w", err)
		}
		clip, err := decodeWav(raw)
		if err != nil {
			eng.avail <- session
			return nil, nil, fmt.Errorf("invalid init-audio wav: %w", err)
		}
		request.SetAudio(clip.Samples, clip.SampleRate, clip.Channels)
		setAudioInputKind(request, req)
	}
	if req.ReturnLatents {
		request.SetOption("return_latents", "true")
	}
	if req.NegativePrompt != "" {
		request.SetOption("negative_prompt", req.NegativePrompt)
	}
	if req.DurationSeconds > 0 {
		request.SetOption("duration_seconds", strconv.FormatFloat(req.DurationSeconds, 'f', -1, 64))
	}
	if req.NumInferenceSteps > 0 {
		request.SetOption("num_inference_steps", strconv.Itoa(req.NumInferenceSteps))
	}
	if req.GuidanceScale > 0 {
		request.SetOption("guidance_scale", strconv.FormatFloat(req.GuidanceScale, 'f', -1, 64))
	}
	if req.Seed != nil {
		request.SetOption("seed", strconv.FormatInt(*req.Seed, 10))
	}
	if err := request.Err(); err != nil {
		eng.avail <- session
		return nil, nil, fmt.Errorf("build request: %w", err)
	}

	result, err := session.Run(request)
	if err != nil {
		eng.avail <- session
		return nil, nil, fmt.Errorf("run: %w", err)
	}
	eng.avail <- session
	defer result.Close()

	audio, err := result.Audio()
	if err != nil {
		return nil, nil, fmt.Errorf("read audio: %w", err)
	}
	if audio == nil {
		return nil, nil, fmt.Errorf("stable_audio produced no audio")
	}
	if !req.ReturnLatents {
		return audio, nil, nil
	}
	lats, err := result.Latents()
	if err != nil {
		return nil, nil, fmt.Errorf("read latents: %w", err)
	}
	if len(lats) != 1 {
		return nil, nil, fmt.Errorf("stable_audio returned %d latents, want 1", len(lats))
	}
	return audio, wireFromLatents(lats[0], modelIdentity(eng.modelPath)), nil
}

// setAudioInputKind applies req's init/inpaint choice to an audio or
// latents input.
func setAudioInputKind(request *audiocpp.Request, req ttsproto.StableAudioRequest) {
	if req.InpaintMaskEndSeconds > 0 {
		request.SetOption("audio_input_kind", "inpaint_audio")
		request.SetOption("inpaint_mask_start_seconds", strconv.FormatFloat(req.InpaintMaskStartSeconds, 'f', -1, 64))
		request.SetOption("inpaint_mask_end_seconds", strconv.FormatFloat(req.InpaintMaskEndSeconds, 'f', -1, 64))
		return
	}
	request.SetOption("audio_input_kind", "init_audio")
	if req.InitNoiseLevel > 0 {
		request.SetOption("init_noise_level", strconv.FormatFloat(req.InitNoiseLevel, 'f', -1, 64))
	}
}

// StableAudioMusic renders via the Small/Music checkpoint - see
// stableAudio's own doc comment.
func (w *Worker) StableAudioMusic(req ttsproto.StableAudioRequest) (*audiocpp.AudioBuffer, error) {
	return w.stableAudio("stable_audio_music", req)
}

// StableAudioSFX renders via the Small/SFX checkpoint - see stableAudio's
// own doc comment.
func (w *Worker) StableAudioSFX(req ttsproto.StableAudioRequest) (*audiocpp.AudioBuffer, error) {
	return w.stableAudio("stable_audio_sfx", req)
}

// StableAudioMedium renders via the larger Medium checkpoint - see
// stableAudio's own doc comment.
func (w *Worker) StableAudioMedium(req ttsproto.StableAudioRequest) (*audiocpp.AudioBuffer, error) {
	return w.stableAudio("stable_audio_medium", req)
}

// StableAudioMediumFull is StableAudioMedium with latents in/out - see
// stableAudioFull.
func (w *Worker) StableAudioMediumFull(req ttsproto.StableAudioRequest) (*audiocpp.AudioBuffer, *ttsproto.Latents, error) {
	return w.stableAudioFull("stable_audio_medium", req)
}

// CodecEncode encodes audio with a family's standalone codec (audio.cpp's
// "codec" task) - "stable_audio_medium": the SAME autoencoder of the Medium
// checkpoint, loaded on its own (no DiT/T5), e.g. to turn legacy music-seed
// WAVs into the latents StableAudioRequest.InitLatents takes.
func (w *Worker) CodecEncode(req ttsproto.CodecEncodeRequest) (*ttsproto.Latents, error) {
	w.touch()
	if req.Family != "stable_audio_medium" {
		return nil, fmt.Errorf("no codec for %q", req.Family)
	}
	raw, err := base64.StdEncoding.DecodeString(req.AudioBase64)
	if err != nil {
		return nil, fmt.Errorf("invalid audioBase64: %w", err)
	}
	clip, err := decodeWav(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid wav: %w", err)
	}
	eng := w.auxEngine("stable_audio_medium_codec")
	if err := w.getAux(eng); err != nil {
		return nil, err
	}
	defer w.releaseAux(eng)
	session, err := w.checkoutAuxSession(eng)
	if err != nil {
		return nil, err
	}
	request := audiocpp.NewRequest()
	defer request.Close()
	request.SetAudio(clip.Samples, clip.SampleRate, clip.Channels)
	if err := request.Err(); err != nil {
		eng.avail <- session
		return nil, fmt.Errorf("build request: %w", err)
	}
	result, err := session.Run(request)
	eng.avail <- session
	if err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	defer result.Close()
	lats, err := result.Latents()
	if err != nil {
		return nil, fmt.Errorf("read latents: %w", err)
	}
	if len(lats) != 1 {
		return nil, fmt.Errorf("codec returned %d latents, want 1", len(lats))
	}
	return wireFromLatents(lats[0], modelIdentity(eng.modelPath)), nil
}

// codecEngines maps a CodecDecodeRequest family to its codec-only aux
// engine.
var codecEngines = map[string]string{
	"stable_audio_medium": "stable_audio_medium_codec",
	"higgs_audio_tts":     "higgs_codec",
	"pocket_tts":          "pocket_codec",
}

// CodecDecode decodes latents or codes with a family's standalone codec
// ("stable_audio_medium", "higgs_audio_tts", "pocket_tts"), honoring their
// meta (decode_*, chunk_frames) - so what a generation returned decodes to
// that generation's audio (exactly, on the same backend). Rejects input
// from another checkpoint.
func (w *Worker) CodecDecode(req ttsproto.CodecDecodeRequest) (*audiocpp.AudioBuffer, error) {
	w.touch()
	key, ok := codecEngines[req.Family]
	if !ok {
		return nil, fmt.Errorf("no codec for %q", req.Family)
	}
	eng := w.auxEngine(key)
	if req.Latents == nil {
		return nil, fmt.Errorf("no latents")
	}
	if got, want := req.Latents.Meta["codec_model"], modelIdentity(eng.modelPath); !sameModel(got, want) {
		return nil, fmt.Errorf("%s (latents from %q, loaded %q)", ttsproto.ErrLatentsModelMismatch, got, want)
	}
	request := audiocpp.NewRequest()
	defer request.Close()
	switch req.Latents.Kind {
	case latents.KindCodes:
		codes, err := codesFromWire(req.Latents)
		if err == nil {
			err = request.AddCodes(codes, "codes")
		}
		if err != nil {
			return nil, err
		}
	default:
		lat, err := latentsFromWire(req.Latents)
		if err == nil {
			err = request.AddLatents(lat)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := w.getAux(eng); err != nil {
		return nil, err
	}
	defer w.releaseAux(eng)
	session, err := w.checkoutAuxSession(eng)
	if err != nil {
		return nil, err
	}
	result, err := session.Run(request)
	eng.avail <- session
	if err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	defer result.Close()
	audio, err := result.Audio()
	if err != nil {
		return nil, fmt.Errorf("read audio: %w", err)
	}
	return audio, nil
}
