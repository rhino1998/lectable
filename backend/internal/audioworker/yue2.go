package audioworker

import (
	"fmt"
	"math"
	"strconv"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// yue2TokensPerSecond is YuE2's semantic frame rate: one codec token per
// 40ms of audio.
const yue2TokensPerSecond = 25

// yue2DefaultSeconds caps a request that names no duration. audio.cpp's
// own cap is 9000 tokens (six minutes), far more than a test render wants.
const yue2DefaultSeconds = 30

// yue2DefaultMinTokens is audio.cpp's semantic_min_tokens default, which
// must not exceed the cap.
const yue2DefaultMinTokens = 200

// YuE2 renders a YuE2 clip in req.Style on the "yue2" auxEngine -
// instrumental when req.Lyrics is empty. See ttsproto.YuE2Request for how
// its fields map onto a model with no duration option.
func (w *Worker) YuE2(req ttsproto.YuE2Request) (*audiocpp.AudioBuffer, error) {
	w.touch()

	eng := w.auxEngine("yue2")
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
	// No lyrics at all means instrumental.
	if req.Lyrics != "" {
		request.SetOption("lyrics", req.Lyrics)
	}
	request.SetOption("style", req.Style)
	if req.COT != "" {
		request.SetOption("cot", req.COT)
	}
	seconds := req.DurationSeconds
	if seconds <= 0 {
		seconds = yue2DefaultSeconds
	}
	maxTokens := max(1, int(math.Round(seconds*yue2TokensPerSecond)))
	request.SetOption("semantic_max_tokens", strconv.Itoa(maxTokens))
	if maxTokens < yue2DefaultMinTokens {
		request.SetOption("semantic_min_tokens", strconv.Itoa(maxTokens))
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
	eng.avail <- session
	if err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	defer result.Close()

	audio, err := result.Audio()
	if err != nil {
		return nil, fmt.Errorf("read audio: %w", err)
	}
	if audio == nil {
		return nil, fmt.Errorf("yue2 produced no audio")
	}
	return audio, nil
}
