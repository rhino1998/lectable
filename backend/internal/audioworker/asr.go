package audioworker

import (
	"fmt"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
)

// transcriberSession lazily loads the ASR Model/Session once and reuses it
// for the worker's whole lifetime - align.go's alignSession counterpart,
// with the same "persistent, shared, only freed by UnloadAll" lifetime and
// for the same reasons (see UnloadCloneModels' doc comment): it runs once
// per generated paragraph, as part of jobs' completeness check.
func (w *Worker) transcriberSession() (*audiocpp.Session, error) {
	w.asrMu.Lock()
	defer w.asrMu.Unlock()

	if w.asrSessionH != nil || w.asrErr != nil {
		return w.asrSessionH, w.asrErr
	}

	model, err := w.loadModel(transcriberModelPath, audiocpp.ModelConfig{FamilyHint: "parakeet_tdt"}, nil)
	if err != nil {
		w.asrErr = fmt.Errorf("load transcriber model: %w", err)
		return nil, w.asrErr
	}
	session, err := model.Session("asr", "offline", audiocpp.BackendConfig{Backend: backendName, Threads: sessionThreads}, nil)
	if err != nil {
		model.Close()
		w.asrErr = fmt.Errorf("create transcriber session: %w", err)
		return nil, w.asrErr
	}
	w.asrModel = model
	w.asrSessionH = session
	return w.asrSessionH, nil
}

// Transcribe returns what's actually spoken in samples (mono float32 at
// sampleRate/channels) - free ASR, not forced against any expected text,
// so unlike Align it can reveal words a generation added (a repeated
// phrase, a leaked reference line) as well as ones it dropped. Parakeet
// TDT is multilingual and detects the language itself, so no language
// option is passed.
func (w *Worker) Transcribe(samples []float32, sampleRate, channels int) (string, error) {
	w.touch()
	session, err := w.transcriberSession()
	if err != nil {
		return "", err
	}

	request := audiocpp.NewRequest()
	defer request.Close()
	request.SetAudio(samples, sampleRate, channels)
	if err := request.Err(); err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}

	w.asrRunMu.Lock()
	result, err := session.Run(request)
	w.asrRunMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("run: %w", err)
	}
	defer result.Close()

	text, err := result.Text()
	if err != nil {
		return "", fmt.Errorf("read text: %w", err)
	}
	if text == nil {
		return "", nil
	}
	return text.Text, nil
}

func (w *Worker) unloadTranscriber() {
	w.asrMu.Lock()
	if w.asrSessionH != nil {
		w.asrSessionH.Close()
	}
	if w.asrModel != nil {
		w.asrModel.Close()
	}
	w.asrSessionH = nil
	w.asrModel = nil
	w.asrErr = nil
	w.asrMu.Unlock()
}
