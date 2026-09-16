package audioworker

import (
	"fmt"
	"strconv"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// Music renders an ACE-Step music clip from req.Prompt (plus optional
// lyrics), on the "ace_step" auxEngine's own pooled, warm "gen" session.
// Deliberately scoped to ACE-Step's "text2music" route only: this app has
// no source audio track to feed the other routes (complete/lego/extract/
// cover/repaint all take/require one - see docs/models/ace_step.md in the
// audio.cpp checkout), and "which route" is itself a UI/product decision
// well beyond plumbing the engine in at all. Every field but Prompt left
// at its zero value defers to audio.cpp's own built-in default - see
// ttsproto.MusicRequest's own doc comment.
func (w *Worker) Music(req ttsproto.MusicRequest) (*audiocpp.AudioBuffer, error) {
	w.touch()

	eng := w.auxEngine("ace_step")
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
	request.SetOption("route", "text2music")
	if req.Lyrics != "" {
		request.SetOption("lyrics", req.Lyrics)
	}
	if req.DurationSeconds > 0 {
		request.SetOption("duration_seconds", strconv.FormatFloat(req.DurationSeconds, 'f', -1, 64))
	}
	if req.NegativePrompt != "" {
		request.SetOption("negative_prompt", req.NegativePrompt)
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
		return nil, fmt.Errorf("ace_step produced no audio")
	}
	return audio, nil
}
