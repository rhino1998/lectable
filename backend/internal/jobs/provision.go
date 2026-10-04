package jobs

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// previewSeq gives every RunVoiceDesignPreview call its own dedup key
// (see task.dedupKey's KindVoiceDesignPreview case) - a preview has no
// natural shared identity like a chapter or character id to dedup
// against, so each one always queues as new rather than risking two
// unrelated "Test" clicks joining into one task and getting each other's
// audio back.
var previewSeq int64

// RunVoiceDesignPreview runs fn (rendering one caller-supplied instruct/
// text/seed/language via the TTS worker - see httpapi.handleTestVoiceDesign,
// the only caller) as a real KindVoiceDesignPreview task in poolGeneration,
// the same slot pool and concurrency limit (maxInFlight) real paragraph
// generation draws from - rather than calling the TTS worker directly,
// uncoordinated with everything else competing for it, the way this used
// to. Still blocks the caller until the render finishes, same shape as
// RunCharacterization: see EnqueueAttribution's own doc comment for why
// fire-and-forget doesn't fit here either - a single preview render is
// exactly the "one request, holding the connection open while the reader
// waits to actually hear it is fine" case that doc comment describes, not
// the unbounded-bulk-work case that made attribution stop blocking.
func (m *Manager) RunVoiceDesignPreview(ctx context.Context, instruct string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resultCh := make(chan previewOutcome, 1)
	seq := atomic.AddInt64(&previewSeq, 1)
	m.pushTask(&task{
		parent: parentOf(ctx),
		kind:   KindVoiceDesignPreview,
		tier:   TierUrgent,
		llmKey: fmt.Sprintf("%d", seq),
		// Doubles as httpapi's job-queue dashboard identifier for this
		// kind (see QueueTask.Label's own doc comment) and as what makes
		// two concurrent previews distinguishable there at all -
		// JobsPage's own taskKey relies on (kind, bookId, chapterIdx,
		// paragraphIdx, label) being unique per task, and every one of
		// those besides label is a shared zero value for this kind.
		label: fmt.Sprintf("preview #%d", seq),
		// Purely for the dashboard's own "Voice" column (see
		// toQueueTask/QueueTask.Instruct) - not read by runPreview itself,
		// which already has this and everything else it needs baked into
		// its own closure.
		instruct:       instruct,
		runPreview:     fn,
		previewWaiters: []chan previewOutcome{resultCh},
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-resultCh:
		return out.audio, out.err
	}
}

// EnqueueVoiceProvision queues fn (one character's voice provisioning or
// forced regeneration - see httpapi.provisionCharacterVoice/
// regenerateCharacterVoice, the only callers) as a KindVoiceProvision task -
// fire-and-forget, like EnqueueAttribution/EnqueueCharacterization, and for
// the same reason: SpeakersPage's "Generate/Regenerate all voices" can
// queue a whole roster at once, and blocking a request per character hits
// the same browser connection-cap/page-reload problems EnqueueAttribution's
// own doc comment describes. TierBackground - same reasoning as
// defaultAttributionTier/EnqueueCharacterization's own tier choice: bulk
// background work like this shouldn't preempt more urgent poolGeneration
// work (an actual paragraph the reader is waiting to hear).
//
// mode ("generate" or "regenerate" - httpapi's only two callers) becomes
// part of llmKey/dedupKey (see task.dedupKey's KindVoiceProvision case) so
// a generate call and a regenerate call for the same character are tracked
// as distinct tasks - joining them would be wrong, since one wants "skip
// if already provisioned" and the other wants "always re-render." label is
// the character's name, for the Jobs dashboard - this kind has no
// chapter/paragraph of its own to show, same reason
// KindSpeakerCharacterization carries one.
func (m *Manager) EnqueueVoiceProvision(bookID, characterID, mode, label string, fn func(ctx context.Context, attempt int) (string, error)) {
	m.pushTask(&task{
		kind:         KindVoiceProvision,
		tier:         TierBackground,
		bookID:       bookID,
		llmKey:       mode + ":" + characterID,
		label:        label,
		runProvision: fn,
	})
}

// RunVoiceProvision is EnqueueVoiceProvision's blocking sibling -
// provisionMissingCharacterVoices (the lazy, automatic-during-reading
// path) uses this instead so it can use the freshly (or already)
// provisioned presetID immediately once this returns, the same way
// RunVoiceDesignPreview blocks that method's own caller for its render.
// Same TierUrgent/dedup-join shape as RunCharacterization/
// RunVoiceDesignPreview: a lazy request for a character already being
// provisioned - by this same lazy path racing itself, or an explicit
// "Generate voice" click sharing the same "generate" mode dedup key (see
// EnqueueVoiceProvision/task.dedupKey) - joins that existing task's
// waiters instead of starting a second one (see pushTask's own doc
// comment), rather than being dropped the way a duplicate
// EnqueueVoiceProvision push would be. Unlike EnqueueVoiceProvision's
// fire-and-forget callers, routing the lazy path through here (instead of
// calling the provisioner function directly, which is what this
// replaced) is what makes lazy provisioning show up on the Jobs
// dashboard at all - see provisionWaiters' own doc comment.
func (m *Manager) RunVoiceProvision(ctx context.Context, bookID, characterID, mode, label string, fn func(ctx context.Context, attempt int) (string, error)) (string, error) {
	return m.RunVoiceProvisionAt(ctx, TierUrgent, bookID, characterID, mode, label, fn)
}

// RunVoiceProvisionAt is RunVoiceProvision at a caller-chosen tier - see
// RunCharacterizationAt.
func (m *Manager) RunVoiceProvisionAt(ctx context.Context, tier int, bookID, characterID, mode, label string, fn func(ctx context.Context, attempt int) (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	resultCh := make(chan provisionOutcome, 1)
	m.pushTask(&task{
		parent:           parentOf(ctx),
		kind:             KindVoiceProvision,
		tier:             tier,
		bookID:           bookID,
		llmKey:           mode + ":" + characterID,
		label:            label,
		runProvision:     fn,
		provisionWaiters: []chan provisionOutcome{resultCh},
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case out := <-resultCh:
		return out.presetID, out.err
	}
}

// EnqueueSFXGeneration queues fn (render this paragraph's own Stable Audio
// SFX clip and persist it - see httpapi.handleGenerateParagraphSFX, the only
// caller) as a KindSFXGeneration task in poolSFX - fire-and-forget, same
// shape as EnqueueVoiceProvision: the reader's "Generate" click in the
// SFX panel gets an immediate 202 rather than holding the HTTP request
// open for the render itself (a multi-step diffusion call, materially
// slower than an ordinary clone). TierUrgent, not TierBackground: unlike
// EnqueueVoiceProvision's own bulk "regenerate every character" callers,
// this is always one reader clicking one paragraph's own "Generate" button
// right now, the same urgency EnqueueParagraphRegenerate already assumes
// for the analogous narration action.
func (m *Manager) EnqueueSFXGeneration(bookID, chapterID string, chapterIdx int, paragraph store.Paragraph, fn func(ctx context.Context, attempt int) error) {
	m.pushTask(&task{
		kind:       KindSFXGeneration,
		tier:       TierUrgent,
		bookID:     bookID,
		chapterID:  chapterID,
		chapterIdx: chapterIdx,
		paragraph:  paragraph,
		runSFXGen:  fn,
	})
}

// sfxPreviewSeq is previewSeq's own counterpart for KindSFXPreview - see
// its own doc comment for why a preview always needs a fresh, unique
// dedup key rather than sharing one with anything else.
var sfxPreviewSeq int64

// RunSFXPreview runs fn (a standalone render with no backing paragraph,
// from any of this app's generation-only engines - ACE-Step, Stable Audio
// (Music/SFX/Medium) - see httpapi.handleGenerateSFX, the only
// caller) as a real KindSFXPreview task in poolSFX, blocking the caller
// until it finishes - RunVoiceDesignPreview's own exact shape (see its
// doc comment for why blocking, not fire-and-forget, is the right shape
// for a single "test this prompt" click). engine is purely a label for
// the Jobs dashboard (e.g. "ace_step", "stable_audio_sfx") - this package never
// branches on it; which engine actually runs is entirely up to fn.
func (m *Manager) RunSFXPreview(ctx context.Context, engine, prompt string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resultCh := make(chan previewOutcome, 1)
	seq := atomic.AddInt64(&sfxPreviewSeq, 1)
	m.pushTask(&task{
		parent:         parentOf(ctx),
		kind:           KindSFXPreview,
		tier:           TierUrgent,
		llmKey:         fmt.Sprintf("%d", seq),
		label:          fmt.Sprintf("%s preview #%d", engine, seq),
		instruct:       prompt,
		runPreview:     fn,
		previewWaiters: []chan previewOutcome{resultCh},
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-resultCh:
		return out.audio, out.err
	}
}

// llmPreviewSeq is sfxPreviewSeq's own counterpart for KindLLMPreview.
var llmPreviewSeq int64

// RunLLMPreview runs fn (a standalone raw system+user prompt call against
// this app's own embedded speaker-attribution GGUF model - see
// httpapi.handleTestLLM, the only caller) as a real KindLLMPreview task in
// poolLLM, blocking the caller until it finishes - RunSFXPreview's own
// exact shape (see its doc comment for why blocking is the right shape
// for a single "test this prompt" click), just carrying a text result
// instead of audio bytes. userPrompt is purely a Jobs-dashboard label
// (mirrors RunSFXPreview's own prompt param) - this package never
// inspects it.
func (m *Manager) RunLLMPreview(ctx context.Context, userPrompt string, fn func(ctx context.Context) (string, error)) (string, error) {
	return m.RunLLMPreviewAt(ctx, TierUrgent, fmt.Sprintf("llm preview #%d", atomic.AddInt64(&llmPreviewSeq, 1)), userPrompt, fn)
}

// RunLLMPreviewAt is RunLLMPreview at a caller-chosen tier and label - for
// a diagnostic that makes many LLM calls (httpapi.handlePreviewAttribution
// runs a whole chapter's attribution), which must still go through poolLLM
// rather than calling the worker directly: outside the queue its LLM calls
// overlapped TTS generation in the worker and crashed it
// (GGML_ASSERT(stat == cudaSuccess) in audio.cpp, 2026-09-29), but at
// TierUrgent it would stall a reader's lookahead for minutes.
func (m *Manager) RunLLMPreviewAt(ctx context.Context, tier int, label, userPrompt string, fn func(ctx context.Context) (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	resultCh := make(chan previewTextOutcome, 1)
	seq := atomic.AddInt64(&llmPreviewSeq, 1)
	m.pushTask(&task{
		parent:             parentOf(ctx),
		kind:               KindLLMPreview,
		tier:               tier,
		llmKey:             fmt.Sprintf("%d", seq),
		label:              label,
		instruct:           userPrompt,
		runPreviewText:     fn,
		previewTextWaiters: []chan previewTextOutcome{resultCh},
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case out := <-resultCh:
		return out.text, out.err
	}
}
