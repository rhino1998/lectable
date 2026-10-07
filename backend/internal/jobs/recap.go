package jobs

import (
	"context"
	"fmt"

	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// RecapFunc writes one book's "previously on" recap - a KindRecap task's
// work (httpapi.writeRecap). DirectionFunc's shape; the count and requeue
// are unused.
type RecapFunc func(ctx context.Context) (int, func(), error)

// RunRecap runs fn as bookID's KindRecap task (poolLLM) and blocks until it
// ends - RunSummary's shape, keyed by book. For the recap group
// (httpapi.enqueueRecap), which passes its own tier.
func (m *Manager) RunRecap(ctx context.Context, bookID string, tier int, fn RecapFunc) error {
	_, err := m.runLLMTaskDirect(ctx, KindRecap, tier, bookID, "", 0, bookID, "Recap", fn)
	return err
}

// RunRecapNarration runs fn - speak bookID's recap and save the clip - as
// a KindRecapNarration task and blocks until it ends. It is a task of its
// own, in poolGeneration, because the clone model can't be resident beside
// the LLM that wrote the recap; fn calls NarrateText for the audio.
func (m *Manager) RunRecapNarration(ctx context.Context, bookID string, tier int, fn func(ctx context.Context, attempt int) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resultCh := make(chan provisionOutcome, 1)
	m.pushTask(&task{
		parent: parentOf(ctx),
		kind:   KindRecapNarration,
		tier:   tier,
		bookID: bookID,
		llmKey: bookID,
		label:  "Recap",
		runProvision: func(ctx context.Context, attempt int) (string, error) {
			return "", fn(ctx, attempt)
		},
		provisionWaiters: []chan provisionOutcome{resultCh},
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case out := <-resultCh:
		return out.err
	}
}

// NarrateText speaks text in voice, as a paragraph of bookID's narration
// would be - the book's pronunciation rules, pause tags and the
// completeness check with its retries included - and returns the WAV with
// its words' timings (nil when the clip couldn't be aligned, or for a
// designed voice, which isn't). The clip is the caller's: nothing is stored
// or shared. Only for use inside a poolGeneration task (RunRecapNarration's
// fn), which is what holds the clone model's place in the worker.
func (m *Manager) NarrateText(ctx context.Context, bookID string, voice narration.ResolvedVoice, text string) ([]byte, []ttsproto.Word, error) {
	kind := KindVoiceClone
	if voice.PresetID == "" {
		kind = KindVoiceDesign
	}
	audio, words, err := m.generate(ctx, &task{
		m:               m,
		kind:            kind,
		bookID:          bookID,
		voiceID:         voice.VoiceID(),
		paragraph:       store.Paragraph{Text: text},
		instruct:        voice.Instruct,
		presetID:        voice.PresetID,
		language:        voice.Language,
		refText:         voice.RefText,
		speedMultiplier: voice.SpeedMultiplier,
		seed:            voice.Seed,
		cloneModel:      voice.CloneModel,
		designModel:     voice.DesignModel,
		cloneInstruct:   voice.CloneInstruct,
	})
	if err != nil {
		return nil, nil, err
	}
	if len(audio.WAV) == 0 {
		return nil, nil, fmt.Errorf("no audio generated")
	}
	return audio.WAV, words, nil
}
