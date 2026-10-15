package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/sched"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/ttsworker"
	"github.com/rhino1998/lectable/backend/internal/voicefx"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
	"github.com/rhino1998/lectable/backend/internal/voices"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// startTask dispatches t into a just-freed worker slot without waiting for
// the result, branching on poolFor(t.kind): the two LLM kinds run
// t.runLLM directly (see startLLMTask), everything else marks the
// paragraph generating and fires its generation call (see
// startGenerationTask).
func (m *Manager) startTask(ctx context.Context, t *task) chan taskResult {
	if poolFor(t.kind) == poolLLM {
		return m.startLLMTask(ctx, t)
	}
	return m.startGenerationTask(ctx, t)
}

func (m *Manager) startLLMTask(ctx context.Context, t *task) chan taskResult {
	ch := make(chan taskResult, 1)
	// KindLLMPreview has no backing chapter/character at all - none of the
	// attribution-specific runLLM/requeue machinery below applies, just run
	// the caller-supplied raw prompt call directly (see startGenerationTask's
	// own early KindVoiceDesignPreview/KindSFXPreview branches for the same
	// shape on the generation side).
	if t.kind == KindLLMPreview {
		go func() {
			text, err := t.runPreviewText(ctx)
			ch <- taskResult{text: text, err: err}
		}()
		return ch
	}
	go func() {
		attributed, requeue, err := t.runLLM(ctx)
		ch <- taskResult{attributed: attributed, requeue: requeue, err: err}
	}()
	return ch
}

func (m *Manager) startGenerationTask(ctx context.Context, t *task) chan taskResult {
	ch := make(chan taskResult, 1)
	// KindVoiceDesignPreview has no backing paragraph at all - none of the
	// store bookkeeping below applies (see handleResult's own early
	// branch for this kind), just run the caller-supplied render directly.
	if t.kind == KindVoiceDesignPreview {
		go func() {
			audio, err := t.runPreview(ctx)
			ch <- taskResult{audio: audio, err: err}
		}()
		return ch
	}
	// KindVoiceProvision likewise has no backing paragraph - see
	// EnqueueVoiceProvision's own doc comment.
	if t.kind == KindVoiceProvision || t.kind == KindRecapNarration || t.kind == KindVoiceCheck {
		go func() {
			presetID, err := t.runProvision(ctx, t.attempt)
			ch <- taskResult{presetID: presetID, err: err}
		}()
		return ch
	}
	// KindListenPreview is KindLLMPreview's text-result shape, outside
	// poolLLM.
	if t.kind == KindListenPreview {
		go func() {
			text, err := t.runPreviewText(ctx)
			ch <- taskResult{text: text, err: err}
		}()
		return ch
	}
	// KindSFXPreview has no backing paragraph either - same shape as
	// KindVoiceDesignPreview above, just under its own kind/pool identity.
	if t.kind == KindSFXPreview {
		go func() {
			audio, err := t.runPreview(ctx)
			ch <- taskResult{audio: audio, err: err}
		}()
		return ch
	}
	// KindSFXGeneration does carry a backing paragraph (for dedup and the
	// Jobs dashboard - see EnqueueSFXGeneration), but none of the
	// narration-specific store bookkeeping below applies to it: the
	// caller-supplied closure does its own persistence entirely (see
	// runSFXGen's own doc comment).
	if t.kind == KindSFXGeneration {
		go func() {
			err := t.runSFXGen(ctx, t.attempt)
			ch <- taskResult{err: err}
		}()
		return ch
	}
	// KindMusicGeneration is runSFXGen's own exact sibling shape, just
	// under its own runMusicGen field - see EnqueueMusicGeneration's own
	// doc comment.
	if t.kind == KindMusicGeneration || t.kind == KindMusicLiveGeneration {
		go func() {
			if t.kind == KindMusicLiveGeneration {
				// Queue the next region now, while this one generates,
				// so it dispatches the moment this finishes.
				m.continueLiveMusic(t)
			}
			err := t.runMusicGen(ctx, t.attempt)
			ch <- taskResult{err: err}
		}()
		return ch
	}
	// KindLengthEstimate has no backing paragraph either - t.paragraph.ID
	// is "" (see EnqueueLengthEstimate), so SetParagraphGenerating/publish
	// below would write/broadcast nothing meaningful (SetParagraphGenerating
	// would just match zero rows, and publishing a ParagraphUpdate at
	// t.chapterIdx/t.paragraph.Idx - both 0 - would misleadingly look like
	// that chapter's real paragraph 0 started generating). generate()
	// itself still runs exactly as it would for a real KindVoiceClone task.
	if t.kind == KindLengthEstimate {
		go func() {
			audio, _, err := m.generate(ctx, t)
			ch <- taskResult{audio: audio.WAV, err: err}
		}()
		return ch
	}
	// For a scare-quote merge group (see pushResolvedTask), mark/publish
	// every member as generating, not just the anchor t.paragraph - all of
	// them are about to be produced by the same single TTS call, and
	// leaving the others looking "pending" until the merged result splits
	// would be a visibly stale/misleading status in the meantime.
	generating := []store.Paragraph{t.paragraph}
	if len(t.mergeParagraphs) > 1 {
		generating = t.mergeParagraphs
	}
	for _, p := range generating {
		if err := m.store.SetParagraphGenerating(p.ID, t.voiceID); err != nil {
			log.Printf("jobs: mark generating: %v", err)
		}
	}
	go func() {
		audio, words, err := m.generate(ctx, t)
		ch <- taskResult{audio: audio.WAV, latents: audio.Latents, words: words, err: err}
	}()
	return ch
}

// generate resolves t into audio, dispatching on t.kind (set once at
// creation - see pushResolvedTask): KindVoiceClone clones t.presetID's
// stable reference clip via voicerefs.EnsureFile; KindVoiceDesign is a fully
// custom instruct with no preset backing it at all, rendered straight via
// VoiceDesign instead, mirroring the old tts-service /synthesize's own
// no-clone-prompt fallback.
//
// EnsureFile still has its own cache-miss fallback that renders the clip via
// ttsworker.Manager.Design (and this call runs *inside* an
// already-dispatched poolGeneration task), but by the time a KindVoiceClone
// task actually reaches this code that fallback is dead code in practice:
// referenceClipDependency makes "the preset's reference clip exists" a
// tracked task dependency (see Manager.needs), so Pop never dispatches
// a KindVoiceClone task whose clip is missing in the first place - it
// instead dispatches (and, via priority promotion, prioritizes) a
// KindVoiceProvision task to render the clip first, and only dispatches the
// clone task once that dependency clears. That's what closes the deadlock
// an earlier version of this hit: that version routed the cache-miss
// ensure-ref-clip step through a second, nested jobs.Manager.
// RunVoiceProvision call, which was safe for a TierBackground outer
// KindVoiceClone task but a real, confirmed-live deadlock for a
// TierUrgent/TierLookahead one - the outer task could never be canceled to
// let its own nested child dispatch (this package never force-cancels
// in-flight work at all - see the scheduler's own "one pool at a time"
// doc comment), and the child could never dispatch while the outer task
// kept poolGeneration active, a standoff only an explicit manual Cancel
// could break. The dependency system fixes this structurally instead of
// serializing around it: the clone task simply never occupies a pool slot
// until its dependency is resolved, so there's nothing left to deadlock.
// mergedGenerationText joins a scare-quote merge group's own members'
// ResolveGenerationText into the single string one TTS call renders for
// the whole group - see generate/handleMergedResult. Plain single-space
// joining, not a byte-exact reconstruction of the original source
// paragraph epub.splitQuoteSegments split apart (trailing/leading
// whitespace was already trimmed off each segment at import time) - fine
// here, since the goal is one naturally-flowing sentence for the TTS
// engine to render, not recovering the original markup.
func mergedGenerationText(members []store.Paragraph, cloneModel string, lexicon []pronounce.LexiconEntry) string {
	texts := make([]string, len(members))
	for i, p := range members {
		texts[i] = p.ResolveGenerationText(cloneModel, lexicon)
	}
	return strings.Join(texts, " ")
}

// unvoicedSeconds is how long an ellipsis-only line's silence lasts.
const unvoicedSeconds = 0.8

// unvoicedSampleRate matches the narration models' own output.
const unvoicedSampleRate = 24000

// unvoicedTask reports whether t renders one ellipsis-only paragraph
// (store.Paragraph.Unvoiced): its clip is silence, made here with no
// model, reference clip or pass behind it.
func unvoicedTask(t *task) bool {
	if t.kind != KindVoiceClone && t.kind != KindVoiceDesign {
		return false
	}
	return len(t.mergeParagraphs) <= 1 && t.paragraph.Unvoiced()
}

// silentClip is an unvoiced paragraph's audio.
func silentClip() ([]byte, error) {
	return wav.Encode(make([]float32, int(unvoicedSeconds*unvoicedSampleRate)), unvoicedSampleRate, 1)
}

func (m *Manager) generate(ctx context.Context, t *task) (ttsworker.Audio, []ttsproto.Word, error) {
	audio, words, err := m.generateClean(ctx, t)
	if err != nil {
		return audio, words, err
	}
	audio, err = withVoiceEffect(audio, t.effect)
	return audio, words, err
}

// withVoiceEffect puts a generated clip through effect (voicefx.Apply).
// The clip's length is kept, so its word timings stay true; its latents
// are dropped, since they no longer decode to these samples. A clip with
// no audio (a line found on a shared clip) and "" come back as given.
func withVoiceEffect(audio ttsworker.Audio, effect voicefx.VoiceEffect) (ttsworker.Audio, error) {
	if voicefx.Tag(effect) == "" || len(audio.WAV) == 0 {
		return audio, nil
	}
	processed, err := voicefx.Apply(effect, audio.WAV)
	if err != nil {
		return ttsworker.Audio{}, fmt.Errorf("voice effect %s: %w", effect, err)
	}
	return ttsworker.Audio{WAV: processed}, nil
}

// generateClean is generate before the voice's effect.
func (m *Manager) generateClean(ctx context.Context, t *task) (ttsworker.Audio, []ttsproto.Word, error) {
	if unvoicedTask(t) {
		t.clipKey, t.clipHit = "", false
		if t.shared {
			t.clipKey = m.taskClipKey(t, nil, "")
			if m.claimSharedClip(ctx, t); t.clipHit {
				return ttsworker.Audio{}, []ttsproto.Word{}, nil
			}
		}
		clip, err := silentClip()
		// Empty, not nil, word timings: there is nothing to align.
		return ttsworker.Audio{WAV: clip}, []ttsproto.Word{}, err
	}
	if t.kind == KindVoiceDesign {
		var seed *int64
		// 0 means no seed anchor (see httpapi.resolveVoiceSeed) - leave nil
		// so the worker picks one at random rather than literally seeding
		// with 0.
		if t.seed != 0 {
			v := int64(t.seed)
			seed = &v
		}
		// "" clone model: design engines take no Higgs pause tags, but
		// pronunciation fixes and lexicon rules apply like any narration.
		lexicon := m.snapshotLexicon(t)
		text := t.paragraph.ResolveGenerationText("", lexicon)
		if len(t.mergeParagraphs) > 1 {
			text = mergedGenerationText(t.mergeParagraphs, "", lexicon)
		}
		t.clipKey, t.clipHit = "", false
		if t.shared {
			t.clipKey = m.taskClipKey(t, lexicon, "")
			if m.claimSharedClip(ctx, t); t.clipHit {
				return ttsworker.Audio{}, nil, nil
			}
		}
		audio, err := m.tts.Design(ctx, text, t.instruct, t.language, t.designModel, seed)
		return ttsworker.Audio{WAV: audio}, nil, err
	}

	cloneModel := t.cloneModel
	if cloneModel == "" {
		cloneModel = voices.DefaultCloneModel
	}
	refPath, err := voicerefs.EnsureFile(ctx, m.tts, m.dataDir, t.presetID, t.instruct, t.seed, t.refText, t.speedMultiplier, t.designModel)
	if err != nil {
		return ttsworker.Audio{}, nil, fmt.Errorf("ensure reference clip: %w", err)
	}
	refText := t.refText
	emotion, variantPath, variantLine, ok := m.emotionVariant(t)
	if ok {
		refPath, refText = variantPath, variantLine
	}
	refAudio, err := os.ReadFile(refPath)
	if err != nil {
		return ttsworker.Audio{}, nil, fmt.Errorf("read reference clip: %w", err)
	}
	// t.paragraph.ResolveGenerationText applies pronunciation/emphasis
	// substitutions (and, for Higgs, pause tags) to t.paragraph.Text, or
	// returns it unchanged if there are none (the common case) - only the
	// text actually sent for cloning. Deliberately NOT applied to t.paragraph.Text itself:
	// alignParagraph (see its own call site) uses that field directly for
	// forced alignment/word-timing, which must stay the plain spoken words -
	// an inserted tag token there would show up as a spurious "word" with no
	// corresponding audio.
	//
	// For a scare-quote merge group (t.mergeParagraphs, len > 1), the same
	// treatment is applied to every member and joined into one string - one
	// TTS call renders the whole group together, later split back into each
	// member's own audio file by handleMergedResult.
	//
	// alignText is what the completeness check (generateCloneChecked)
	// aligns against - the same text each result path would otherwise
	// align itself (alignParagraph/handleMergedResult), so the check's own
	// word timings are reused rather than aligning twice. A length
	// estimate's throwaway sample is never saved, so it skips the check.
	lexicon := m.snapshotLexicon(t)
	text := t.paragraph.ResolveGenerationText(cloneModel, lexicon)
	alignText := t.paragraph.Text
	if len(t.mergeParagraphs) > 1 {
		text = mergedGenerationText(t.mergeParagraphs, cloneModel, lexicon)
		alignText = text
	}
	if t.kind == KindLengthEstimate {
		alignText = ""
	}
	t.clipKey, t.clipHit = "", false
	if t.shared {
		t.clipKey = m.taskClipKey(t, lexicon, emotion)
		if m.claimSharedClip(ctx, t); t.clipHit {
			return ttsworker.Audio{}, nil, nil
		}
	}
	return m.generateCloneChecked(ctx, cloneModel, refAudio, refText, t.language, text, t.cloneInstruct, alignText)
}

// emotionVariant returns the emotion-variant reference clip (its emotion,
// and the line it speaks, its transcript) t should clone from instead of its preset's
// base clip, if t's paragraph currently has an effective emotion whose
// variant has been rendered (see variantClipDependency) - re-read from the
// store rather than trusted from t.paragraph, since a manual override can
// land between enqueue and dispatch. ok is false for a neutral line, a
// scare-quote merge group, or a variant that failed to render (the base
// clip is used then).
func (m *Manager) emotionVariant(t *task) (emotion, path, refLine string, ok bool) {
	if t.presetID == "" || len(t.mergeParagraphs) > 1 || voices.Directed(t.cloneModel) {
		return "", "", "", false
	}
	p, err := m.store.GetParagraph(t.paragraph.ID)
	if err != nil || p == nil {
		return "", "", "", false
	}
	e, known := emotions.Get(p.EffectiveEmotion())
	if !known || !voicerefs.VariantExists(m.dataDir, t.presetID, e.ID) {
		return "", "", "", false
	}
	return e.ID, audiopath.VoicePresetVariantFile(m.dataDir, t.presetID, e.ID), e.RefLine, true
}

func (m *Manager) finishResult(t *task, result taskResult) (final bool, err error) {
	logTaskTiming(t, result.err)
	if t.kind != KindVoiceClone && t.kind != KindVoiceDesign {
		ran := t.ranBefore + time.Since(t.startedAt)
		switch {
		case result.requeue != nil:
			t.ranBefore = ran
		case result.err == nil:
			m.durations.record(t.kind, ran)
		}
	}
	if result.err == nil && !t.clipHit {
		// An unparseable clip just counts for no audio (wav.Duration's 0).
		audio, _ := wav.Duration(result.audio)
		m.throughput.record(time.Now(), taskChars(t), audio)
	}
	m.maybeRecoverWorker(result.err)
	if t.kind == KindLengthEstimate {
		// Best-effort, throwaway: no waiters, no paragraph, nothing to
		// retry for - a failed or implausible sample just leaves
		// Book.EstimateSecPerChar at its previous value (0, on a book's
		// first upload), and estimateTotalSeconds's own fallback tiering
		// already handles that.
		if result.err != nil {
			log.Printf("jobs: length estimate for book %s: %v", t.bookID, result.err)
			return true, result.err
		}
		dur, err := wav.Duration(result.audio)
		if err != nil || dur <= 0 || t.estimateChars == 0 {
			log.Printf("jobs: length estimate for book %s: parse wav duration: %v", t.bookID, err)
			return true, result.err
		}
		if err := m.store.SetLengthEstimate(t.bookID, dur.Seconds()/float64(t.estimateChars)); err != nil {
			log.Printf("jobs: length estimate for book %s: save: %v", t.bookID, err)
		}
		return true, result.err
	}
	if t.kind == KindVoiceDesignPreview {
		// A genuine (non-preempted) failure gets retried via the queue
		// itself - see maxTaskAttempts/the scheduler's retry - before ever
		// delivering to previewWaiters: RunVoiceDesignPreview's caller is
		// blocked on exactly one outcome, so retrying has to happen before
		// that single delivery, not race it.
		if retryEligible(t, result.err) {
			log.Printf("jobs: voice design preview failed (attempt %d/%d), retrying via queue: %v", t.attempt+1, maxTaskAttempts, result.err)
			return false, result.err
		}
		// Same delivery pattern as the poolLLM branch below, just carrying
		// audio bytes (previewWaiters/previewOutcome) instead of an
		// attributed count (llmWaiters/llmOutcome) - see RunVoiceDesignPreview.
		m.mu.Lock()
		waiters := t.previewWaiters
		m.mu.Unlock()
		outcome := previewOutcome{audio: result.audio, err: result.err}
		for _, waiter := range waiters {
			waiter <- outcome
		}
		if result.err != nil {
			log.Printf("jobs: voice design preview failed: %v", result.err)
		}
		return true, result.err
	}
	if t.kind == KindSFXPreview {
		// Same retry-before-delivery reasoning as the design-preview
		// branch above - RunSFXPreview's caller is blocked on exactly one
		// outcome.
		if retryEligible(t, result.err) {
			log.Printf("jobs: %s failed (attempt %d/%d), retrying via queue: %v", t.kind, t.attempt+1, maxTaskAttempts, result.err)
			return false, result.err
		}
		m.mu.Lock()
		waiters := t.previewWaiters
		m.mu.Unlock()
		outcome := previewOutcome{audio: result.audio, err: result.err}
		for _, waiter := range waiters {
			waiter <- outcome
		}
		if result.err != nil {
			log.Printf("jobs: %s failed: %v", t.kind, result.err)
		}
		return true, result.err
	}
	if t.kind == KindSFXGeneration {
		if retryEligible(t, result.err) {
			log.Printf("jobs: sfx generation task %s failed (attempt %d/%d), retrying via queue: %v", t.dedupKey(), t.attempt+1, maxTaskAttempts, result.err)
			return false, result.err
		}
		m.mu.Lock()
		waiters := t.sfxGenWaiters
		m.mu.Unlock()
		for _, waiter := range waiters {
			waiter <- result.err
		}
		if result.err != nil {
			log.Printf("jobs: sfx generation task %s failed: %v", t.dedupKey(), result.err)
		}
		return true, result.err
	}
	if t.kind == KindMusicGeneration || t.kind == KindMusicLiveGeneration {
		if retryEligible(t, result.err) {
			log.Printf("jobs: music generation task %s failed (attempt %d/%d), retrying via queue: %v", t.dedupKey(), t.attempt+1, maxTaskAttempts, result.err)
			return false, result.err
		}
		m.mu.Lock()
		waiters := t.musicGenWaiters
		m.mu.Unlock()
		for _, waiter := range waiters {
			waiter <- result.err
		}
		if result.err != nil {
			log.Printf("jobs: music generation task %s failed: %v", t.dedupKey(), result.err)
		}
		// Whether this batch just succeeded or permanently failed, every
		// region it reached is no longer AudioPending/AudioGenerating
		// (generateMusicRegion itself sets Ready/Error per region as it
		// goes, before this ever runs) - advance to whatever comes after
		// them in this chapter now, the same way a paragraph finishing does
		// (saveParagraphAudio/handleMergedResult).
		m.MaybeAdvanceChapterMusic(t.bookID, t.chapterID)
		return true, result.err
	}
	if t.kind == KindVoiceProvision || t.kind == KindRecapNarration || t.kind == KindVoiceCheck {
		if retryEligible(t, result.err) {
			log.Printf("jobs: %s task %s failed (attempt %d/%d), retrying via queue: %v", t.kind, t.dedupKey(), t.attempt+1, maxTaskAttempts, result.err)
			return false, result.err
		}
		// Deliver to any RunVoiceProvision callers currently waiting
		// (usually none - EnqueueVoiceProvision's own fire-and-forget
		// callers never populate this - see provisionWaiters' own doc
		// comment) - buffered sends, same no-block-on-a-stopped-caller
		// reasoning as the preview/LLM branches above.
		m.mu.Lock()
		waiters := t.provisionWaiters
		m.mu.Unlock()
		outcome := provisionOutcome{presetID: result.presetID, err: result.err}
		for _, waiter := range waiters {
			waiter <- outcome
		}
		if result.err != nil {
			log.Printf("jobs: %s task %s failed: %v", t.kind, t.dedupKey(), result.err)
		}
		return true, result.err
	}
	if t.kind == KindLLMPreview || t.kind == KindListenPreview {
		// Same retry-before-delivery reasoning as the preview/provision
		// branches above - RunLLMPreview's caller is blocked on exactly one
		// outcome.
		if retryEligible(t, result.err) {
			log.Printf("jobs: llm preview failed (attempt %d/%d), retrying via queue: %v", t.attempt+1, maxTaskAttempts, result.err)
			return false, result.err
		}
		m.mu.Lock()
		waiters := t.previewTextWaiters
		m.mu.Unlock()
		outcome := previewTextOutcome{text: result.text, err: result.err}
		for _, waiter := range waiters {
			waiter <- outcome
		}
		if result.err != nil {
			log.Printf("jobs: llm preview failed: %v", result.err)
		}
		return true, result.err
	}
	if poolFor(t.kind) == poolLLM {
		t.attributed += result.attributed
		if result.requeue != nil {
			// Paused partway: yield, and pick the remainder up on the next
			// run (see task.Run).
			t.resume = result.requeue
			return false, sched.ErrYield
		}
		if retryEligible(t, result.err) {
			log.Printf("jobs: %s task %s failed (attempt %d/%d), retrying via queue: %v", t.kind, t.dedupKey(), t.attempt+1, maxTaskAttempts, result.err)
			return false, result.err
		}
		m.mu.Lock()
		waiters := t.llmWaiters
		m.mu.Unlock()
		outcome := llmOutcome{attributed: t.attributed, err: result.err}
		for _, waiter := range waiters {
			waiter <- outcome
		}
		if result.err != nil {
			log.Printf("jobs: %s task %s failed permanently after %d attempt(s): %v", t.kind, t.dedupKey(), t.attempt+1, result.err)
		}
		return true, result.err
	}
	if result.err != nil {
		// A merged group's own combined generation call failing (e.g. the
		// worker chokes on the longer combined text) falls back straight to
		// generating every member independently, exactly today's ordinary
		// path, rather than retrying the same merged call - merging is
		// purely an audio-quality optimization, never something a
		// paragraph's own audio correctness should depend on succeeding.
		if len(t.mergeParagraphs) > 1 {
			log.Printf("jobs: merged generation for chapter %s failed (%d paragraphs), falling back to independent generation: %v", t.chapterID, len(t.mergeParagraphs), result.err)
			m.generateIndependently(t)
			return true, nil
		}
		if retryEligible(t, result.err) {
			log.Printf("jobs: synthesize paragraph %s failed (attempt %d/%d), retrying via queue: %v", t.paragraph.ID, t.attempt+1, maxTaskAttempts, result.err)
			return false, result.err
		}
		m.failParagraph(t, t.paragraph, result.err)
		return true, result.err
	}
	// A render that comes back after shutdown isn't saved: the store may
	// already be closed, and a clip without its row is just a stray file.
	if m.stopped() {
		return true, context.Canceled
	}
	if fresh, stale := m.refreshStaleGeneration(t); stale {
		log.Printf("jobs: paragraph %s generation text changed while generating, discarding and requeueing", t.paragraph.ID)
		t.paragraph, t.mergeParagraphs = fresh.paragraph, fresh.mergeParagraphs
		t.genLexicon, t.genLexiconSet = nil, false
		return false, sched.ErrYield
	}
	if t.clipHit {
		// The clip generate found may be gone or replaced by now.
		if !m.useSharedClip(t) {
			return false, sched.ErrYield
		}
		return true, nil
	}
	if len(t.mergeParagraphs) > 1 {
		m.handleMergedResult(t, result.audio, result.latents, result.words)
		return true, nil
	}
	m.saveParagraphAudio(t, t.paragraph, result.audio, result.latents, result.words)
	return true, nil
}

// handleResult settles a dispatched task's result and reports it to the
// scheduler: nil when the work is done (or was handled another way), the
// error when it failed, sched.ErrYield when the task should run again
// without that counting as a failure. A failure the scheduler will retry
// (retryEligible - the same test task.Retryable gives it) isn't settled:
// waiters and the store only hear of the last attempt.
func (m *Manager) handleResult(t *task, result taskResult) error {
	final, err := m.finishResult(t, result)
	if t.claimed != "" {
		m.clips.release(t.claimed)
		t.claimed = ""
	}
	if final {
		m.mu.Lock()
		t.settled = true
		m.mu.Unlock()
		if err != nil {
			err = &settledError{err}
		}
	}
	return err
}

// settledError marks a failure handleResult has already settled, so the
// scheduler doesn't retry it (task.Retryable).
type settledError struct{ error }

func (e *settledError) Unwrap() error { return e.error }

// refreshStaleGeneration re-reads t's paragraph(s) and reports whether any
// one's generation text changed after t snapshotted it - e.g. a
// pronunciation re-run landing while the clip was rendering, whose audio
// invalidation (DeleteParagraphAudioForIdxs) this result would otherwise
// silently undo by saving audio of the old text. When stale, fresh is a
// copy of t carrying the current paragraphs, to render again (not counted
// as a retry attempt - nothing failed). A paragraph that can't be re-read
// is treated as unchanged.
func (m *Manager) refreshStaleGeneration(t *task) (fresh *task, stale bool) {
	cloneModel := t.cloneModel
	if cloneModel == "" {
		cloneModel = voices.DefaultCloneModel
	}
	lexicon := m.lexiconFor(t.bookID)
	rendered := lexicon
	if t.genLexiconSet {
		rendered = t.genLexicon
	}
	current := func(p store.Paragraph) store.Paragraph {
		cur, err := m.store.GetParagraph(p.ID)
		if err != nil || cur == nil {
			return p
		}
		if cur.ResolveGenerationText(cloneModel, lexicon) != p.ResolveGenerationText(cloneModel, rendered) {
			stale = true
		}
		return *cur
	}
	cp := *t
	cp.genLexicon, cp.genLexiconSet = nil, false
	cp.paragraph = current(t.paragraph)
	if len(t.mergeParagraphs) > 1 {
		cp.mergeParagraphs = make([]store.Paragraph, len(t.mergeParagraphs))
		for i, p := range t.mergeParagraphs {
			cp.mergeParagraphs[i] = current(p)
		}
	}
	if !stale {
		return nil, false
	}
	return &cp, true
}

// failParagraph marks one paragraph AudioError and publishes it - shared by
// the ordinary single-paragraph failure path and generateIndependently's
// own per-paragraph fallback loop.
func (m *Manager) failParagraph(t *task, paragraph store.Paragraph, err error) {
	log.Printf("jobs: synthesize paragraph %s: %v", paragraph.ID, err)
	_ = m.store.SetParagraphError(paragraph.ID, t.voiceID, err.Error())
}

// saveParagraphAudio persists one paragraph's own finished clip - file on
// disk, ready status/duration, published update, detached word-alignment -
// shared by the ordinary single-paragraph success path, handleMergedResult
// (once per split-out clip), and generateIndependently's own fallback loop,
// so every way a paragraph's audio can end up on disk goes through exactly
// one write/publish/error-handling path. words, when non-nil, are audio's
// own already-computed word timings (generateCloneChecked's), saved
// directly instead of running alignParagraph.
func (m *Manager) saveParagraphAudio(t *task, paragraph store.Paragraph, audio []byte, lat *ttsproto.Latents, words []ttsproto.Word) {
	outPath := audiopath.ParagraphFile(m.dataDir, t.bookID, t.chapterID, t.voiceID, paragraph.Idx)
	dur, err := wav.Duration(audio)
	if err != nil {
		log.Printf("jobs: parse wav duration for %s: %v", paragraph.ID, err)
	}
	saved := false
	if t.clipKey != "" && paragraph.ID == t.paragraph.ID && len(t.mergeParagraphs) <= 1 {
		// "[]" until this clip's own alignment lands (below).
		part := clipPart{duration: dur.Seconds(), wordTimings: "[]"}
		if words != nil {
			if wordsJSON, err := json.Marshal(words); err == nil {
				part.wordTimings = string(wordsJSON)
			}
		}
		var reused bool
		if saved, reused = m.saveSharedClip(t, []store.Paragraph{paragraph}, audio, lat, []clipPart{part}); reused {
			m.MaybeAdvanceChapterMusic(t.bookID, t.chapterID)
			return
		}
	}
	if !saved {
		// Only when the shared store couldn't take it.
		err := audiopath.EnsureDir(outPath)
		if err == nil {
			err = audiopath.WriteClipLatents(outPath, audio, lat)
		}
		if err != nil {
			log.Printf("jobs: write audio file: %v", err)
			_ = m.store.SetParagraphError(paragraph.ID, t.voiceID, "failed to save audio: "+err.Error())
			return
		}
		if err := m.store.SetParagraphReady(paragraph.ID, t.voiceID, dur.Seconds()); err != nil {
			log.Printf("jobs: mark ready: %v", err)
		}
		if words != nil {
			m.saveWordTimings(t, paragraph, words)
		}
	}
	// This paragraph's own narration may be the last piece some chapter
	// tone region needed before it can start generating - see
	// MaybeAdvanceChapterMusic's own doc comment. Cheap no-op for the
	// overwhelming common case (MusicEnabled false).
	m.MaybeAdvanceChapterMusic(t.bookID, t.chapterID)

	// Word-level alignment is a separate, best-effort step against
	// tts-service's own generation lock (see its POST /align) - it runs
	// detached rather than inline here so a slow/failed alignment never
	// delays audio becoming playable or blocks this worker goroutine from
	// picking up the next result. Re-aligns paragraph.Text directly against
	// this specific clip regardless of whether it came from a lone
	// generation call or a split-out slice of a merged one - a merged
	// clip's own split-out piece is by this point just an ordinary,
	// independent wav file on disk, so there's no merged-alignment offset
	// bookkeeping to carry through here.
	if words != nil {
		return
	}
	go m.alignParagraph(t, paragraph, audio, dur.Seconds())
}

// alignParagraph fetches word-level timing for a just-generated paragraph
// and, if successful, saves it and pushes a follow-up ParagraphUpdate
// carrying the same full ready-state (status/duration/URL) plus Words -
// resending the unchanged fields rather than an alignment-only patch, so
// the frontend's straightforward field-overwrite merge (useBookUpdates.ts)
// can't accidentally blank them out. Failure is logged and otherwise
// silent: the paragraph stays playable, just without real word timing -
// ParagraphText.tsx falls back to its character-position estimate.
func (m *Manager) alignParagraph(t *task, paragraph store.Paragraph, audio []byte, durationSeconds float64) {
	words, err := m.alignWithSplit(m.ctx, paragraph.Text, audio, t.language)
	if err != nil {
		log.Printf("jobs: align paragraph %s: %v", paragraph.ID, err)
		return
	}
	m.saveWordTimings(t, paragraph, words)
}

// saveWordTimings persists one paragraph's own word timings.
func (m *Manager) saveWordTimings(t *task, paragraph store.Paragraph, words []ttsproto.Word) {
	wordsJSON, err := json.Marshal(words)
	if err != nil {
		log.Printf("jobs: marshal word timings for %s: %v", paragraph.ID, err)
		return
	}
	if err := m.store.SetParagraphWordTimings(paragraph.ID, t.voiceID, string(wordsJSON)); err != nil {
		log.Printf("jobs: save word timings for %s: %v", paragraph.ID, err)
		return
	}
}

// handleMergedResult saves mergedAudio - one shared clip generate already
// rendered for the whole scare-quote merge group in a single TTS call (see
// generate/mergedGenerationText) - as the ONE physical audio file for the
// group, written under the anchor paragraph's own path
// (t.mergeParagraphs[0]). Every other member gets no file of its own at
// all: Store.SetParagraphReadyPointer records where its own content starts
// within the anchor's file (in seconds, from one shared forced-alignment
// call over the whole merged text/audio - see alignMergedAudio), and
// httpapi resolves its audioUrl straight to the anchor's, so a scare quote
// (see store.Paragraph.ScareQuote) plays in the same breath as the
// narration around it with the underlying recording never touched, cut, or
// re-encoded at all.
//
// This deliberately replaces an earlier version that decoded, sliced, and
// re-encoded a separate file per member at each alignment-derived
// boundary - a real, observed source of audible artifacts: forced-
// alignment word timing is precise enough for karaoke-highlighting (being
// off by a few tens of milliseconds doesn't matter there) but not for
// actually splicing audio, where the same slop produces an audible click
// or clips real speech. Pointing every member at one untouched recording
// instead has no cut to get slightly wrong in the first place.
//
// Falls back to generateIndependently - today's ordinary one-call-per-
// paragraph path, each member with its own real file and its own ordinary
// alignment - if alignment fails, or if the aligner's own word count
// doesn't match mergedGenerationText's own simple whitespace tokenization
// closely enough to locate every member's own boundary with confidence:
// merging is an audio-quality optimization, never something a paragraph's
// own audio correctness should depend on.
func (m *Manager) handleMergedResult(t *task, mergedAudio []byte, mergedLatents *ttsproto.Latents, precomputed []ttsproto.Word) {
	cloneModel := t.cloneModel
	if cloneModel == "" {
		cloneModel = voices.DefaultCloneModel
	}
	lexicon := m.lexiconFor(t.bookID)
	texts := make([]string, len(t.mergeParagraphs))
	for i, p := range t.mergeParagraphs {
		texts[i] = p.ResolveGenerationText(cloneModel, lexicon)
	}
	mergedText := strings.Join(texts, " ")

	words, ok := m.alignMergedAudio(t, texts, mergedText, mergedAudio, precomputed)
	if !ok {
		log.Printf("jobs: could not align merged clip for chapter %s (%d paragraphs); regenerating independently", t.chapterID, len(t.mergeParagraphs))
		m.generateIndependently(t)
		return
	}

	totalSeconds := 0.0
	if dur, err := wav.Duration(mergedAudio); err != nil {
		log.Printf("jobs: parse wav duration for merged clip (chapter %s): %v", t.chapterID, err)
	} else {
		totalSeconds = dur.Seconds()
	}

	parts := make([]clipPart, len(t.mergeParagraphs))
	wordIdx := 0
	for i, p := range t.mergeParagraphs {
		n := len(strings.Fields(texts[i]))
		memberWords := words[wordIdx : wordIdx+n]
		wordIdx += n

		// The anchor owns everything from the very top of the file
		// (offset 0), even if the first spoken word starts a fraction of a
		// second in (leading silence) - every other member starts exactly
		// where its own first word does.
		start := 0.0
		if i > 0 && len(memberWords) > 0 {
			start = memberWords[0].Start
		}
		end := totalSeconds
		if i < len(t.mergeParagraphs)-1 {
			if next := words[wordIdx:]; len(next) > 0 {
				end = next[0].Start
			}
		}

		wordsJSON, err := json.Marshal(memberWords)
		if err != nil {
			log.Printf("jobs: marshal word timings for %s: %v", p.ID, err)
			wordsJSON = []byte("[]")
		}
		parts[i] = clipPart{pointerSeconds: start, duration: end - start, wordTimings: string(wordsJSON)}
	}

	saved := false
	if t.clipKey != "" {
		saved, _ = m.saveSharedClip(t, t.mergeParagraphs, mergedAudio, mergedLatents, parts)
	}
	if !saved {
		anchor := t.mergeParagraphs[0]
		outPath := audiopath.ParagraphFile(m.dataDir, t.bookID, t.chapterID, t.voiceID, anchor.Idx)
		err := audiopath.EnsureDir(outPath)
		if err == nil {
			err = audiopath.WriteClipLatents(outPath, mergedAudio, mergedLatents)
		}
		if err != nil {
			log.Printf("jobs: write merged audio file: %v", err)
			for _, p := range t.mergeParagraphs {
				m.failParagraph(t, p, fmt.Errorf("failed to save audio: %w", err))
			}
			return
		}
		m.applyClipParts(t.mergeParagraphs, t.voiceID, "", parts)
	}
	// Same reasoning as saveParagraphAudio's own identical call - any of
	// this group's members finishing may be the last piece some chapter
	// tone region needed.
	m.MaybeAdvanceChapterMusic(t.bookID, t.chapterID)
}

// alignMergedAudio runs forced alignment once over the whole merged
// (mergedText, mergedAudio) - every member's own word timings and pointer
// boundary are sliced from this single result (handleMergedResult) rather
// than each being separately re-aligned against its own short text, which
// would be a genuine mismatch once that text no longer matches the shared,
// longer audio it's now part of.
//
// Returns ok=false - safe to treat exactly like a generation failure,
// never a partial/best-guess result - whenever the result can't be sliced
// per member with confidence: alignment itself failing, or the aligner
// returning a different number of words than strings.Fields(mergedText)
// (its own tokenization can legitimately diverge from a plain whitespace
// split - e.g. splitting a contraction, or dropping a stray punctuation-
// only token - and this is the one case with no graceful degradation,
// since a boundary word is genuinely unlocatable at that point).
func (m *Manager) alignMergedAudio(t *task, texts []string, mergedText string, mergedAudio []byte, precomputed []ttsproto.Word) ([]ttsproto.Word, bool) {
	words := precomputed
	if words == nil {
		var err error
		words, err = m.alignWithSplit(m.ctx, mergedText, mergedAudio, t.language)
		if err != nil {
			log.Printf("jobs: align merged clip for chapter %s: %v", t.chapterID, err)
			return nil, false
		}
	}
	wantWords := 0
	for _, txt := range texts {
		wantWords += len(strings.Fields(txt))
	}
	if wantWords == 0 || len(words) != wantWords {
		return nil, false
	}
	return words, true
}

// generateIndependently is handleMergedResult's own safety fallback (also
// used directly when the merged generation call itself fails) - renders
// and saves every member of t.mergeParagraphs one at a time, exactly the
// same generateClone/saveParagraphAudio path a non-merged task would use,
// so a merge/split failure degrades to "no merging happened" rather than
// ever leaving a paragraph without audio. Runs synchronously, inline in
// handleResult, rather than pushing fresh single-paragraph tasks back onto
// the queue: a requeue would need its own mechanism to guarantee it never
// re-attempts the same merge (scareQuoteMergeGroup is a pure function of
// stored paragraph state, which hasn't changed) - a real but rare extra
// slow step here (a handful of TTS calls) is a simpler, safer trade than
// that bookkeeping for a failure path this infrequent.
func (m *Manager) generateIndependently(t *task) {
	cloneModel := t.cloneModel
	if cloneModel == "" {
		cloneModel = voices.DefaultCloneModel
	}
	if t.kind == KindVoiceDesign {
		var seed *int64
		if t.seed != 0 {
			v := int64(t.seed)
			seed = &v
		}
		for _, p := range t.mergeParagraphs {
			audio, err := m.tts.Design(m.ctx, p.Text, t.instruct, t.language, t.designModel, seed)
			if err != nil {
				m.failParagraph(t, p, err)
				continue
			}
			if audio, err = voicefx.Apply(t.effect, audio); err != nil {
				m.failParagraph(t, p, err)
				continue
			}
			recipe := withEffect(designRecipe(t.designModel, t.instruct, t.seed, t.language, p.Text, p.Text), t.effect)
			m.saveIndependent(t, p, recipe.Key(m.clipTake(t.bookID, t.chapterID, p, nil, "")), audio, nil, nil)
		}
		return
	}
	refPath, err := voicerefs.EnsureFile(m.ctx, m.tts, m.dataDir, t.presetID, t.instruct, t.seed, t.refText, t.speedMultiplier, t.designModel)
	if err != nil {
		for _, p := range t.mergeParagraphs {
			m.failParagraph(t, p, fmt.Errorf("ensure reference clip: %w", err))
		}
		return
	}
	refAudio, err := os.ReadFile(refPath)
	if err != nil {
		for _, p := range t.mergeParagraphs {
			m.failParagraph(t, p, fmt.Errorf("read reference clip: %w", err))
		}
		return
	}
	lexicon := m.lexiconFor(t.bookID)
	for _, p := range t.mergeParagraphs {
		text := p.ResolveGenerationText(cloneModel, lexicon)
		audio, words, err := m.generateCloneChecked(m.ctx, cloneModel, refAudio, t.refText, t.language, text, t.cloneInstruct, p.Text)
		if err != nil {
			m.failParagraph(t, p, err)
			continue
		}
		if audio, err = withVoiceEffect(audio, t.effect); err != nil {
			m.failParagraph(t, p, err)
			continue
		}
		recipe := withEffect(m.clipRecipe(cloneModel, t.presetID, "", t.language, t.cloneInstruct, text, p.Text), t.effect)
		m.saveIndependent(t, p, recipe.Key(m.clipTake(t.bookID, t.chapterID, p, nil, cloneModel)), audio.WAV, audio.Latents, words)
	}
}

// saveIndependent saves the clip generateIndependently rendered for one
// member of t's merge group on its own, under key - that paragraph's own
// content key, as if it had never been part of a group.
func (m *Manager) saveIndependent(t *task, p store.Paragraph, key string, audio []byte, lat *ttsproto.Latents, words []ttsproto.Word) {
	single := *t
	single.paragraph, single.mergeParagraphs = p, nil
	single.clipKey, single.clipHit, single.claimed, single.fresh = key, false, "", false
	m.saveParagraphAudio(&single, p, audio, lat, words)
}
