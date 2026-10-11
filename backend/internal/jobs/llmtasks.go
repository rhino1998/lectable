package jobs

import (
	"context"

	"github.com/rhino1998/lectable/backend/internal/sched"
)

// runLLMTask is RunCharacterization's blocking implementation (attribution
// used to share this too - see EnqueueAttribution's own doc comment for
// why it no longer does, and RunAttribution's for the one caller that now
// legitimately can) - see RunCharacterization's own doc comment for the
// caller-facing behavior (join-in-place on a duplicate call,
// ctx-cancellation-safe). tier is the caller's choice, not always
// TierUrgent (RunCharacterization's own callers all want TierUrgent, but
// runLLMTaskDirect's callers - see its own doc comment - share this same
// push-and-wait shape at other tiers). llmKey/label become task.llmKey/
// task.label - see task's own doc comment for what each kind uses them
// for.
func (m *Manager) runLLMTask(ctx context.Context, kind Kind, tier int, bookID, chapterID string, chapterIdx int, llmKey, label string, fn func(ctx context.Context) (int, error)) (int, error) {
	resultCh := make(chan llmOutcome, 1)
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		parent:     parentOf(ctx),
		kind:       kind,
		tier:       tier,
		chapterIdx: chapterIdx,
		bookID:     bookID,
		chapterID:  chapterID,
		llmKey:     llmKey,
		label:      label,
		// Adapts fn (a CharacterizationFunc's own (int, error) shape,
		// see RunCharacterization) to task.runLLM's shared signature -
		// characterization never pauses/requeues mid-call the way
		// attribution can (see AttributionFunc's own doc comment), so
		// requeue is always nil here.
		runLLM: func(ctx context.Context) (int, func(), error) {
			attributed, err := fn(ctx)
			return attributed, nil, err
		},
		llmWaiters:  []chan llmOutcome{resultCh},
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case out := <-resultCh:
		return out.attributed, out.err
	}
}

// runLLMTaskDirect is runLLMTask's counterpart for AttributionFunc/
// DirectionFunc, whose own (int, func(), error) shape (pause/resume via a
// non-nil requeue) already matches task.runLLM's field type exactly - see
// EnqueueAttribution/EnqueueDirection's own comments to that effect -
// unlike CharacterizationFunc, which runLLMTask itself adapts from a
// simpler, never-pauses (int, error) shape. Used by RunAttribution/
// RunDirection, the blocking siblings EnqueueAttribution/EnqueueDirection
// don't otherwise have - see RunAttribution's own doc comment for why a
// blocking form is safe again now, for the one caller that needs it.
//
// If fn pauses partway (non-nil requeue), the partial outcome is still
// delivered here as soon as handleResult processes it - same as any other
// llmWaiter - while the actual continuation runs later, fully detached,
// via EnqueueAttribution/EnqueueDirection's own fire-and-forget requeue
// closure (see attributeChapter/directChapter). This call does not, and
// cannot, wait for that continuation too: it's a fresh, independently
// dispatched task with no channel of its own back to here. A caller that
// cares about the *whole* chapter finishing, not just this one dispatch,
// has to accept that a paused-and-resumed chapter may report done before
// its continuation actually lands - see RunAttribution's own doc comment.
func (m *Manager) runLLMTaskDirect(ctx context.Context, kind Kind, tier int, bookID, chapterID string, chapterIdx int, llmKey, label string, fn func(ctx context.Context) (int, func(), error)) (int, error) {
	resultCh := make(chan llmOutcome, 1)
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		parent:      parentOf(ctx),
		kind:        kind,
		tier:        tier,
		chapterIdx:  chapterIdx,
		bookID:      bookID,
		chapterID:   chapterID,
		llmKey:      llmKey,
		label:       label,
		runLLM:      fn,
		llmWaiters:  []chan llmOutcome{resultCh},
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case out := <-resultCh:
		return out.attributed, out.err
	}
}

// defaultAttributionTier is EnqueueAttribution's own tier - TierBackground,
// not TierUrgent: unlike EnqueueParagraphRegenerate (a single paragraph the
// reader is waiting to actually hear right now), attribution never blocks
// anything the reader is looking at - SpeakersPage's "Attribute all" alone
// can enqueue a whole book's worth of chapters at once, and there's no
// reason that bulk background work should preempt a more urgent poolLLM
// task (RunCharacterization, still TierUrgent) queued behind it.
const defaultAttributionTier = TierBackground

// EnqueueAttribution queues fn as a KindSpeakerAttribution task - sharing
// the same priority heap as voice-clone/design generation, but dispatched
// into poolLLM's own dedicated slot(s) rather than poolGeneration's (see
// Kind's doc comment and maxAttributionInFlight) - and returns immediately
// without waiting for it to run, same fire-and-forget shape as
// EnqueueChapter. If a chapter is already queued or in flight for
// attribution (e.g. two "attribute this chapter" clicks racing, or
// SpeakersPage's "Attribute all" re-enqueuing an already-queued chapter),
// pushTask's own dedup silently joins it rather than starting a second run
// - fine here since nothing is waiting on a result to join anyway. Always
// queued at defaultAttributionTier (see its own doc comment).
//
// Deliberately not the caller's HTTP request context, or blocking on one at
// all - an earlier version (RunAttribution, since reintroduced below for a
// different kind of caller - see its own doc comment) took ctx and blocked
// the calling goroutine until the task finished; the task itself never
// actually depended on that ctx (it ran via the scheduler's own
// long-lived context regardless - see worker/startTask), but the *HTTP
// handler* did block on it for as long as attribution took, which for a
// several-hundred-paragraph chapter can be minutes. SpeakersPage's
// "Attribute all" fires one such request per unattributed chapter in a
// book at once - for a 100-chapter book that meant up to 99 requests
// sitting open at once behind poolLLM's single slot, for as long as the
// whole book took to attribute; browsers also cap concurrent connections
// per origin (typically 6 for HTTP/1.1), so most of those requests
// wouldn't even reach the server until an earlier one's connection freed
// up. A page reload while that backlog was still draining aborted every
// not-yet-sent request outright - since those had never even reached
// pushTask, that attribution never happened at all, not just paused.
// Progress is observable via GET /api/jobs (already reports queued/
// in-flight speaker_attribution tasks per chapter) and by refetching the
// book's speaker table once a chapter's task clears - not from this call's
// own return, which carries no result.
func (m *Manager) EnqueueAttribution(bookID, chapterID string, chapterIdx int, fn AttributionFunc) {
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:        KindSpeakerAttribution,
		tier:        defaultAttributionTier,
		chapterIdx:  chapterIdx,
		bookID:      bookID,
		chapterID:   chapterID,
		llmKey:      chapterID,
		runLLM:      fn, // AttributionFunc's underlying type already matches runLLM's field type exactly
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
}

// RunAttribution is EnqueueAttribution's blocking sibling - unlike that
// function's own doc comment above (explaining why an earlier blocking
// version was removed), blocking here is safe again because the intended
// caller, httpapi.runBookPipeline, is itself a long-lived background
// goroutine the server started, not an HTTP handler with a browser
// connection behind it: nothing times out or gets dropped on a page reload
// if this takes minutes, and there's exactly one such goroutine per
// in-progress pipeline run, not one per chapter the way SpeakersPage's
// "Attribute all" fires. Same join-on-duplicate and requeue-on-pause
// behavior as EnqueueAttribution (see runLLMTaskDirect's own doc comment
// for what blocking here does and doesn't wait for when fn pauses partway
// through). tier, unlike EnqueueAttribution's own hardcoded
// defaultAttributionTier, is caller-supplied - httpapi's pipeline phase
// passes its own pipelineTask's live Tier() at dispatch time, so a
// preprocessing run already promoted (or promoted later - see
// Manager.PromoteTier's own cascade) actually dispatches its real
// per-chapter work at that tier instead of silently staying at
// TierBackground regardless of how urgent the phase itself now claims to
// be.
func (m *Manager) RunAttribution(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, fn AttributionFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindSpeakerAttribution, tier, bookID, chapterID, chapterIdx, chapterID, "", fn)
}

// defaultReattributionTier mirrors defaultAttributionTier's own reasoning:
// "Auto Split" never blocks anything the reader is looking at right now,
// so this bulk background work shouldn't preempt a more urgent poolLLM
// task (RunCharacterization, still TierUrgent) queued behind it.
const defaultReattributionTier = TierBackground

// EnqueueReattribution queues fn as a KindSpeakerReattribution task -
// EnqueueAttribution's sibling for the "Auto Split" action (see
// httpapi.handleReattributeSpeaker, the one caller, which enqueues one of
// these per chapter containing a paragraph currently credited to the
// speaker being eliminated), sharing its fire-and-forget shape, poolLLM
// slot(s), and join-on-duplicate dedup - see EnqueueAttribution's own doc
// comment for the full "why fire-and-forget, not blocking" rationale,
// which applies identically here (a whole book's worth of chapters can be
// queued by one Auto Split click, the same "don't hold dozens of HTTP
// requests open behind poolLLM's one slot" concern).
//
// speakerKey namespaces the dedup key alongside chapterID (see
// task.dedupKey's KindSpeakerReattribution case): a character's own ID, or
// the literal "unknown" sentinel for a run targeting Unknown-attributed
// paragraphs (Unknown has no characters-table row of its own to key off -
// see store.Paragraph.Speaker's own doc comment). Without this, two
// different speakers' own reattribution runs against the same chapter
// would collide into one task under EnqueueAttribution's own bare
// chapterID-only key, which is safe there only because a chapter never has
// more than one attribution run meaningfully in flight at once. label is
// the speaker's own display name, for the Jobs dashboard (see
// QueueTask.Label).
func (m *Manager) EnqueueReattribution(bookID, chapterID string, chapterIdx int, speakerKey, label string, fn ReattributionFunc) {
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:        KindSpeakerReattribution,
		tier:        defaultReattributionTier,
		chapterIdx:  chapterIdx,
		bookID:      bookID,
		chapterID:   chapterID,
		llmKey:      chapterID + "|" + speakerKey,
		label:       label,
		runLLM:      fn,
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
}

// RunReattribution is EnqueueReattribution's blocking sibling,
// RunAttribution's exact counterpart for "Auto Split" - used by the
// per-chapter fan-out inside an Auto Split bulk group (the one caller), a
// background pipeline task rather than an HTTP handler, so blocking is
// safe for the same reason RunAttribution's own doc comment gives. Same
// (chapter, speaker)-scoped dedup key as EnqueueReattribution, so a
// paused chapter's own fire-and-forget continuation (see
// httpapi.reattributeChapterSpeaker) joins/collides with it exactly as
// before. tier is caller-supplied (the wrapping task's own live tier -
// see PipelinePhaseFunc).
func (m *Manager) RunReattribution(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, speakerKey, label string, fn ReattributionFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindSpeakerReattribution, tier, bookID, chapterID, chapterIdx, chapterID+"|"+speakerKey, label, fn)
}

// EnqueueDirection queues fn as a KindSpeechDirection task - EnqueueAttribution's
// exact fire-and-forget shape (same dedup-by-chapter, same poolLLM
// dispatch, same defaultAttributionTier), for speech-direction tagging
// instead of speaker attribution. See EnqueueAttribution's own doc comment
// for the full reasoning (why fire-and-forget, why TierBackground, how
// progress is observed via GET /api/jobs) - it applies here unchanged.
func (m *Manager) EnqueueDirection(bookID, chapterID string, chapterIdx int, fn DirectionFunc) {
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:        KindSpeechDirection,
		tier:        defaultAttributionTier,
		chapterIdx:  chapterIdx,
		bookID:      bookID,
		chapterID:   chapterID,
		llmKey:      chapterID,
		runLLM:      fn, // DirectionFunc's underlying type already matches runLLM's field type exactly
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
}

// RunDirection is EnqueueDirection's blocking sibling, RunAttribution's
// exact counterpart for speech-direction tagging - see RunAttribution's
// own doc comment for why blocking is safe for its one caller
// (httpapi.runBookPipeline) despite EnqueueAttribution/EnqueueDirection's
// own fire-and-forget shape existing specifically to avoid that for an
// HTTP handler, and for why tier is caller-supplied rather than
// defaultAttributionTier.
func (m *Manager) RunDirection(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, fn DirectionFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindSpeechDirection, tier, bookID, chapterID, chapterIdx, chapterID, "", fn)
}

// EnqueueAmbience queues fn as a KindAmbience task -
// EnqueueDirection's exact fire-and-forget shape (same dedup-by-chapter,
// same poolLLM dispatch, same defaultAttributionTier), for background-
// music tone-region scoring instead of speech-direction tagging. See
// EnqueueAttribution's own doc comment for the full reasoning behind that
// shape - it applies here unchanged.
func (m *Manager) EnqueueAmbience(bookID, chapterID string, chapterIdx int, fn AmbienceFunc) {
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:        KindAmbience,
		tier:        defaultAttributionTier,
		chapterIdx:  chapterIdx,
		bookID:      bookID,
		chapterID:   chapterID,
		llmKey:      chapterID,
		runLLM:      fn, // AmbienceFunc's underlying type already matches runLLM's field type exactly
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
}

// RunAmbience is EnqueueAmbience's blocking sibling, RunAttribution's
// exact counterpart for background-music tone-region scoring - see
// RunAttribution's own doc comment for why blocking is safe for its
// pipeline-phase caller (httpapi.preprocessAmbiencePhase) despite
// EnqueueAmbience's own fire-and-forget shape existing specifically to
// avoid that for an HTTP handler, and for why tier is caller-supplied
// rather than defaultAttributionTier.
func (m *Manager) RunAmbience(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, fn AmbienceFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindAmbience, tier, bookID, chapterID, chapterIdx, chapterID, "", fn)
}

// EnqueueScareQuote queues fn as a KindScareQuote task - EnqueueDirection's
// exact fire-and-forget shape (dedup-by-chapter, poolLLM,
// defaultAttributionTier). See EnqueueAttribution's own doc comment for the
// reasoning behind that shape.
func (m *Manager) EnqueueScareQuote(bookID, chapterID string, chapterIdx int, fn ScareQuoteFunc) {
	m.enqueueChapterLLM(KindScareQuote, bookID, chapterID, chapterIdx, fn)
}

// RunScareQuote is EnqueueScareQuote's blocking sibling - RunDirection's
// exact counterpart, for httpapi.preprocessScareQuotePhase.
func (m *Manager) RunScareQuote(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, fn ScareQuoteFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindScareQuote, tier, bookID, chapterID, chapterIdx, chapterID, "", fn)
}

// EnqueueSummary queues fn as a KindSummary task - EnqueueScareQuote's
// shape.
func (m *Manager) EnqueueSummary(bookID, chapterID string, chapterIdx int, fn SummaryFunc) {
	m.enqueueChapterLLM(KindSummary, bookID, chapterID, chapterIdx, fn)
}

// RunSummary is EnqueueSummary's blocking sibling, for the preprocess
// phase and the bulk action.
func (m *Manager) RunSummary(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, fn SummaryFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindSummary, tier, bookID, chapterID, chapterIdx, chapterID, "", fn)
}

// RunEmbedding runs fn as a KindEmbedding task and waits for it - RunSummary's
// shape, for the preprocess phase.
func (m *Manager) RunEmbedding(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, fn EmbeddingFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindEmbedding, tier, bookID, chapterID, chapterIdx, chapterID, "", fn)
}

// EnqueuePronunciation queues fn as a KindPronunciation task - same shape
// as EnqueueScareQuote.
func (m *Manager) EnqueuePronunciation(bookID, chapterID string, chapterIdx int, fn PronunciationFunc) {
	m.enqueueChapterLLM(KindPronunciation, bookID, chapterID, chapterIdx, fn)
}

// RunPronunciation is EnqueuePronunciation's blocking sibling, for
// httpapi.preprocessPronunciationPhase.
func (m *Manager) RunPronunciation(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, fn PronunciationFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindPronunciation, tier, bookID, chapterID, chapterIdx, chapterID, "", fn)
}

// enqueueChapterLLM is EnqueueScareQuote/EnqueuePronunciation's shared
// fire-and-forget push - a chapter-keyed poolLLM task at
// defaultAttributionTier.
func (m *Manager) enqueueChapterLLM(kind Kind, bookID, chapterID string, chapterIdx int, fn func(ctx context.Context) (int, func(), error)) {
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:        kind,
		tier:        defaultAttributionTier,
		chapterIdx:  chapterIdx,
		bookID:      bookID,
		chapterID:   chapterID,
		llmKey:      chapterID,
		runLLM:      fn,
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
}

// HasHigherPriorityWork reports whether *any* pool currently has a task
// queued (not counting whatever's already in flight - a task already
// dispatched into poolLLM's own slot is necessarily the caller itself) at a
// tier more urgent than TierBackground, AND actually dispatchable right
// now (no unresolved dependency of its own - see the scheduler.
// AnyEligibleQueued). AttributionFunc/DirectionFunc implementations check
// this between batches to decide whether to pause a long-running
// background run and requeue the rest, rather than make a just-arrived,
// more urgent task wait out however many batches remain in this one
// first - see EnqueueAttribution/defaultAttributionTier and
// httpapi.attributeChapter/directChapter, its callers.
//
// The "actually dispatchable" half matters concretely, not just in
// theory: a chapter's own in-flight KindSpeechDirection task must never
// count that exact same chapter's own paragraphs (TierUrgent/
// TierLookahead, but blocked on this very task via
// speechDirectionDependency) as a reason to pause - yielding would
// accomplish nothing (poolGeneration's own top candidate would still be
// blocked the instant it tried to dispatch), so this used to just pause
// after every single batch, requeue, and get immediately redispatched
// again since nothing else outranked it - a confirmed-live, pointless
// pause/resume churn on every batch of every chapter with its own
// lookahead work queued, not a genuine yield to anything.
//
// Checks every TTS-family pool, not just poolLLM: an in-flight
// attribution/characterization/direction-tagging task should yield just as
// readily for a reader's own genuinely-eligible urgent paragraph
// (TierUrgent/TierLookahead KindVoiceClone/KindVoiceDesign) or an urgent
// voice-provisioning task (poolDesign) as for another urgent poolLLM task,
// since fill's own mutual exclusion means neither can even start until
// this one actually stops. A TTS-family task never needs an equivalent of
// its own: it isn't a long-running loop that can check this between
// batches the way an attribution/direction run is, so its only lever for
// this same contention is the passive one - the scheduler's own
// "graceful drain" simply refusing to backfill a pool with more
// lower-priority work while another pool's own more urgent (and eligible)
// work waits on it to drain.
func (m *Manager) HasHigherPriorityWork() bool {
	for _, key := range m.taskKeys(func(t *task, st sched.State) bool { return st == sched.Running && poolFor(t.kind) == poolLLM }) {
		if m.sched.ShouldYield(key) {
			return true
		}
	}
	return false
}

// RunCharacterization queues fn as a KindSpeakerCharacterization task -
// EnqueueAttribution's sibling for the other LLM kind, sharing its poolLLM
// slot(s) and join-on-duplicate behavior, but (unlike EnqueueAttribution)
// still blocks the caller until a slot dispatches it and fn returns,
// returning ctx.Err() if ctx is cancelled first without leaking the
// still-queued or in-flight task itself (resultCh is buffered, so
// handleResult's send into it once fn actually finishes never blocks on a
// caller who stopped listening) - see CharacterizationFunc's own doc
// comment for why blocking is fine here even though it stopped being fine
// for attribution. Differences from EnqueueAttribution are otherwise just
// shape: keyed by characterID rather than a chapter (a character isn't
// chapter/paragraph-scoped), no chapterIdx, and fn reports only an error,
// not a count. label is a human-readable identifier (the character's
// name) for the Jobs dashboard, which otherwise has no chapter/paragraph
// to show for this kind - see QueueTask.Label.
func (m *Manager) RunCharacterization(ctx context.Context, bookID, characterID, label string, fn CharacterizationFunc) error {
	return m.RunCharacterizationAt(ctx, TierUrgent, bookID, characterID, label, fn)
}

// RunCharacterizationAt is RunCharacterization at a caller-chosen tier - for
// a bulk group's own per-character fan-out (httpapi.handleBulkAction), which
// dispatches at the group's own live tier rather than TierUrgent.
func (m *Manager) RunCharacterizationAt(ctx context.Context, tier int, bookID, characterID, label string, fn CharacterizationFunc) error {
	_, err := m.runLLMTask(ctx, KindSpeakerCharacterization, tier, bookID, "", 0, characterID, label, func(ctx context.Context) (int, error) {
		return 0, fn(ctx)
	})
	return err
}

// EnqueueCharacterization queues fn as a KindSpeakerCharacterization task -
// RunCharacterization's fire-and-forget sibling, for a caller that wants to
// queue many characters' worth of work in one request rather than holding
// an HTTP connection open per character (see EnqueueAttribution's own doc
// comment for the full rationale, which applies identically here: a
// "Recharacterize all" firing one blocking request per character hits the
// same browser per-origin connection cap, and a page reload before every
// request even reached pushTask would silently drop whichever characters
// hadn't been dispatched yet - not just fail to report their result).
// TierBackground, not RunCharacterization's TierUrgent - same reasoning as
// defaultAttributionTier: bulk background work like this shouldn't preempt
// a reader's own explicit single "Regenerate" click (still TierUrgent via
// RunCharacterization) queued behind it. Same llmKey-based dedup as
// RunCharacterization - a duplicate call for a character already queued or
// in flight silently joins it via pushTask, fine here since nothing is
// waiting on a result to join anyway (no llmWaiters is set, unlike
// RunCharacterization's own resultCh). Progress is observable via GET
// /api/jobs and by refetching the book's speaker table once each
// character's task clears, same as EnqueueAttribution.
func (m *Manager) EnqueueCharacterization(bookID, characterID, label string, fn CharacterizationFunc) {
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:        KindSpeakerCharacterization,
		tier:        TierBackground,
		bookID:      bookID,
		llmKey:      characterID,
		label:       label,
		runLLM:      func(ctx context.Context) (int, func(), error) { return 0, nil, fn(ctx) },
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
}
