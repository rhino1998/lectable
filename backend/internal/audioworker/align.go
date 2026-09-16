package audioworker

import (
	"fmt"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// alignerSampleRate: audio.cpp's qwen3_forced_aligner (like its qwen3_asr
// sibling) reports word_timestamps' start/end as sample counts in this
// model's own fixed internal processing rate - NOT the sample rate of
// whatever audio was actually passed in. This is deliberate framework-wide
// behavior, confirmed empirically (not assumed) in the original Python
// implementation: feeding the same real silence duration at two different
// input sample rates returned the exact same raw sample counts both times.
// Get this wrong and every reported timestamp is off by a double-digit
// percentage - carry this constant over verbatim, don't re-derive it.
const alignerSampleRate = 16000

// alignSession lazily loads the forced-aligner Model/Session once and reuses
// it for the worker's whole lifetime - mirrors alignment.py's module-level
// _session singleton.
func (w *Worker) alignSession() (*audiocpp.Session, error) {
	w.alignMu.Lock()
	defer w.alignMu.Unlock()

	if w.alignSessionH != nil || w.alignErr != nil {
		return w.alignSessionH, w.alignErr
	}

	model, err := w.loadModel(alignerModelPath, audiocpp.ModelConfig{FamilyHint: "qwen3_forced_aligner"}, nil)
	if err != nil {
		w.alignErr = fmt.Errorf("load aligner model: %w", err)
		return nil, w.alignErr
	}
	session, err := model.Session("align", "offline", audiocpp.BackendConfig{Backend: backendName, Threads: sessionThreads}, nil)
	if err != nil {
		model.Close()
		w.alignErr = fmt.Errorf("create aligner session: %w", err)
		return nil, w.alignErr
	}
	w.alignModel = model
	w.alignSessionH = session
	return w.alignSessionH, nil
}

// Align returns one Word per word in text, located within samples (mono
// float32 at sampleRate/channels) - mirrors alignment.py's align_words.
// Standalone alignment doesn't chunk long audio; callers pass one
// paragraph's clip at a time.
func (w *Worker) Align(text string, samples []float32, sampleRate, channels int, language string) ([]ttsproto.Word, error) {
	w.touch()
	session, err := w.alignSession()
	if err != nil {
		return nil, err
	}

	request := audiocpp.NewRequest()
	defer request.Close()
	request.SetText(text, languageOption(language)).SetAudio(samples, sampleRate, channels)
	if err := request.Err(); err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	w.singleSessionRunMu.Lock()
	result, err := session.Run(request)
	w.singleSessionRunMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	defer result.Close()

	words, err := result.Words()
	if err != nil {
		return nil, fmt.Errorf("read words: %w", err)
	}
	out := make([]ttsproto.Word, len(words))
	for i, wd := range words {
		out[i] = ttsproto.Word{
			Text:  wd.Text,
			Start: float64(wd.StartSample) / alignerSampleRate,
			End:   float64(wd.EndSample) / alignerSampleRate,
		}
	}
	return out, nil
}
