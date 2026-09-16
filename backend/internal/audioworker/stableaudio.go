package audioworker

import (
	"encoding/base64"
	"fmt"
	"strconv"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
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
	w.touch()

	eng := w.auxEngine(engineKey)
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
	request.SetText(req.Prompt, "")
	if req.InitAudioBase64 != "" {
		raw, err := base64.StdEncoding.DecodeString(req.InitAudioBase64)
		if err != nil {
			eng.avail <- session
			return nil, fmt.Errorf("invalid initAudioBase64: %w", err)
		}
		clip, err := decodeWav(raw)
		if err != nil {
			eng.avail <- session
			return nil, fmt.Errorf("invalid init-audio wav: %w", err)
		}
		request.SetAudio(clip.Samples, clip.SampleRate, clip.Channels)
		if req.InpaintMaskEndSeconds > 0 {
			request.SetOption("audio_input_kind", "inpaint_audio")
			request.SetOption("inpaint_mask_start_seconds", strconv.FormatFloat(req.InpaintMaskStartSeconds, 'f', -1, 64))
			request.SetOption("inpaint_mask_end_seconds", strconv.FormatFloat(req.InpaintMaskEndSeconds, 'f', -1, 64))
		} else {
			request.SetOption("audio_input_kind", "init_audio")
			if req.InitNoiseLevel > 0 {
				request.SetOption("init_noise_level", strconv.FormatFloat(req.InitNoiseLevel, 'f', -1, 64))
			}
		}
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
		return nil, fmt.Errorf("build request: %w", err)
	}

	result, err := session.Run(request)
	if err != nil {
		eng.avail <- session
		return nil, fmt.Errorf("run: %w", err)
	}
	eng.avail <- session
	defer result.Close()

	audio, err := result.Audio()
	if err != nil {
		return nil, fmt.Errorf("read audio: %w", err)
	}
	if audio == nil {
		return nil, fmt.Errorf("stable_audio produced no audio")
	}
	return audio, nil
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
