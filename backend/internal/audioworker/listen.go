package audioworker

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
)

// listenDefaultMaxTokens caps an answer when the caller names no limit:
// the questions asked of a clip want a sentence, and audio.cpp's own
// default (2048) lets a rambling answer run for seconds.
const listenDefaultMaxTokens = 120

// Listen answers question about samples (mono or interleaved float32 at
// sampleRate/channels) in text, on the "listener" auxEngine - Audio
// Flamingo, an audio-language model that hears the voice itself (who is
// speaking, how) where Transcribe only returns the words. It is a heavy
// model (~9 GB at Q8), so unlike the transcriber it takes the residency
// gate and evicts every other model family first. Decoding is greedy
// (audio.cpp's default), so the same clip and question give the same
// answer.
func (w *Worker) Listen(samples []float32, sampleRate, channels int, question string, maxTokens int) (string, error) {
	w.touch()

	eng := w.auxEngine("listener")
	if err := w.getAux(eng); err != nil {
		return "", err
	}
	defer w.releaseAux(eng)

	session, err := w.checkoutAuxSession(eng)
	if err != nil {
		return "", err
	}

	if maxTokens <= 0 {
		maxTokens = listenDefaultMaxTokens
	}
	request := audiocpp.NewRequest()
	defer request.Close()
	request.SetAudio(samples, sampleRate, channels)
	request.SetOption("instruct", question)
	request.SetOption("max_tokens", strconv.Itoa(maxTokens))
	if err := request.Err(); err != nil {
		eng.avail <- session
		return "", fmt.Errorf("build request: %w", err)
	}

	result, err := session.Run(request)
	eng.avail <- session
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
	return strings.TrimSpace(text.Text), nil
}
