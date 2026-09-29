package jobs

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/taskqueue"
)

// QueueTask is one task as seen from outside the package (httpapi's
// job-queue dashboard) - just the fields needed to identify and display
// it, not the internal dispatch bookkeeping (refText/seed/etc.) task
// itself carries. ParagraphIdx/PresetID/Instruct are meaningless (zero
// value) for the two LLM kinds, which aren't paragraph-scoped - httpapi's
// DTO layer keys its display on Kind. ChapterIdx/ChapterID are also
// meaningless (zero value) for KindSpeakerCharacterization specifically,
// which is character- rather than chapter-scoped; Label carries a
// human-readable identifier for that case instead (see task.label's own
// doc comment) - "" for every other kind, which already has a chapter/
// paragraph to show. PresetID/Instruct are already resolved onto task at
// enqueue time (see pushResolvedTask), so exposing them here is free - no
// extra store lookup needed, unlike book/chapter titles which httpapi
// resolves separately.
type QueueTask struct {
	// ID is t.dedupKey() - already a stable, unique-per-task identifier
	// used internally for dedup (see task.dedupKey's own doc comment), so
	// exposing it verbatim is the natural handle for Cancel/CancelAll and
	// for a frontend to key a dashboard row on, rather than reconstructing
	// an ad hoc composite key from the other, display-oriented fields.
	ID           string
	Kind         Kind
	Label        string
	BookID       string
	ChapterID    string
	ChapterIdx   int
	ParagraphIdx int
	Tier         int    // TierUrgent (-1), TierLookahead (0), TierNormal (1), or TierBackground (2)
	PresetID     string // "" if this voice is a pure custom instruct with no preset backing it
	Instruct     string
	// Attempt counts genuine failures this exact unit of work has already
	// suffered (see task.attempt's own doc comment) - 0 for a task on its
	// first ever dispatch. Deliberately NOT bumped by a cooperative pause
	// (AttributionFunc/DirectionFunc requeuing the rest of a chapter
	// between batches - see taskResult.requeue) - only by a genuine retry
	// after failure (see retryEligible/maxTaskAttempts) - so a reader
	// watching this stay at 0 across many requeues of the same long
	// chapter can tell that's normal batch-by-batch progress, not a stuck
	// retry loop.
	Attempt int
	// Emotion is the emotion id whose voice variant this generation clones
	// from ("" for neutral, or anything that isn't a single emotional line
	// - see taskEmotion), so the dashboard can show "Fritz (Sad)".
	Emotion string
	// Chars is how much paragraph text a narration task renders (see
	// taskChars) - 0 for every other kind. With Manager.Throughput, what
	// the Jobs views estimate time left from.
	Chars int
}

// taskEmotion is the emotion t's line generates in, for display: the
// paragraph's effective emotion as of enqueue, under the same conditions
// variantClipDependency/emotionVariant apply one (a preset-backed voice,
// not a scare-quote merge group). A manual override after enqueue
// re-enqueues the line, so this rarely lags the store.
func taskEmotion(t *task) string {
	if t.kind != KindVoiceClone && t.kind != KindVoiceDesign {
		return ""
	}
	if t.presetID == "" || len(t.mergeParagraphs) > 1 {
		return ""
	}
	if e := t.paragraph.EffectiveEmotion(); e != emotions.Neutral {
		return e
	}
	return ""
}

func toQueueTask(t *task, tier int) QueueTask {
	paragraphIdx := t.paragraph.Idx
	if t.kind == KindMusicLiveGeneration {
		// One region per task - report where that region starts.
		paragraphIdx = t.musicRegionStart
	}
	return QueueTask{
		ID: t.dedupKey(), Kind: t.kind, Label: t.label, BookID: t.bookID, ChapterID: t.chapterID,
		ChapterIdx: t.chapterIdx, ParagraphIdx: paragraphIdx, Tier: tier,
		PresetID: t.presetID, Instruct: t.instruct, Attempt: t.attempt,
		Emotion: taskEmotion(t), Chars: taskChars(t),
	}
}

// Snapshot reports the queue's current state for a job-queue dashboard:
// InFlight is whatever's actually dispatched to a worker slot right now,
// Queued is everything still waiting, ordered by taskqueue.Queue's own
// Snapshot (see its own doc comment for exactly what "ordered" means here
// and the approximation it admits to - popTask's real dispatch behavior
// also depends on which pool is active, each pool's own capacity, and the
// graceful-drain/minimize-transitions rules around switching pools, none
// of which a single sorted list can fully express). InFlight has no
// "what's next" ordering to reflect at all (every entry there is already
// concurrently active, not waiting in line), so it keeps the simple
// readable grouping.
//
// Also merges in pipelineQueue's own current state (toPipelineQueueTask) -
// a book's four preprocessing phases (see EnqueuePipeline) dispatch through
// a wholly separate Queue from m.queue (see poolPipeline's own doc
// comment), but they're still real, cancelable units of work a reader
// should be able to see and cancel from the same Jobs dashboard, not a
// second, invisible tracking mechanism the way the old bare-goroutine
// runBookPipeline was. pipelineQueue's own queued ordering isn't merged
// into qs's - the two Queues have no shared notion of relative priority -
// so a phase row's position in Queued is just "after every m.queue row",
// not evidence of real dispatch order between the two; InFlight's own
// explicit re-sort below still applies uniformly to both, since a phase
// with no ChapterIdx/ParagraphIdx of its own (both zero) sorts predictably
// to the front of its book's own group.
func (m *Manager) Snapshot() (inFlight, queued []QueueTask) {
	qs, ifs := m.queue.Snapshot()
	pqs, pifs := m.pipelineQueue.Snapshot()

	for _, e := range ifs {
		inFlight = append(inFlight, toQueueTask(e.Task.(*task), e.Tier))
	}
	for _, e := range pifs {
		inFlight = append(inFlight, toPipelineQueueTask(e.Task.(*pipelineTask), e.Tier))
	}
	sort.Slice(inFlight, func(i, j int) bool {
		a, b := inFlight[i], inFlight[j]
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		if a.BookID != b.BookID {
			return a.BookID < b.BookID
		}
		if a.ChapterIdx != b.ChapterIdx {
			return a.ChapterIdx < b.ChapterIdx
		}
		return a.ParagraphIdx < b.ParagraphIdx
	})

	for _, e := range qs {
		queued = append(queued, toQueueTask(e.Task.(*task), e.Tier))
	}
	for _, e := range pqs {
		queued = append(queued, toPipelineQueueTask(e.Task.(*pipelineTask), e.Tier))
	}
	return inFlight, queued
}

// deliverCanceled sends a context.Canceled outcome to every one of t's own
// waiters (llmWaiters/previewWaiters - a no-op for any kind that doesn't
// use them, e.g. KindVoiceClone/KindVoiceDesign/KindSpeakerAttribution/
// KindVoiceProvision, all of which are fire-and-forget) - the queued-task
// half of Cancel/CancelAll's own cancellation delivery. Every channel here
// is buffered (size 1, one send ever - see RunCharacterization/
// RunVoiceDesignPreview), so these sends never block. Only ever called for
// a task pulled straight out of the heap (never dispatched, so
// handleResult will never run for it and can't double-deliver) - an
// in-flight task's cancellation instead flows through the normal
// handleResult path once t.cancel's context is noticed (see task.cancel's
// own doc comment), which already knows how to deliver to these same
// waiters for any other failure.
func deliverCanceled(t *task) {
	for _, w := range t.llmWaiters {
		w <- llmOutcome{err: context.Canceled}
	}
	for _, w := range t.previewWaiters {
		w <- previewOutcome{err: context.Canceled}
	}
	for _, w := range t.previewTextWaiters {
		w <- previewTextOutcome{err: context.Canceled}
	}
}

// Cancel cancels one task by its QueueTask.ID (== task.dedupKey(), or a
// pipelineTask's own Key() - see cancelPipelineTask) - httpapi's DELETE
// /api/jobs/{id}, the Jobs dashboard's per-row "Cancel" button. A
// still-queued task is removed from the heap outright (it never ran at
// all - store state is untouched, same as if it had never been
// enqueued) and its own waiters, if any, are delivered a canceled outcome
// directly (see deliverCanceled). An in-flight task can't be un-dispatched
// - a worker slot is already running it - so this instead cancels its own
// dispatch context (t.cancel) and lets the task's normal completion path
// (handleResult) discover that and clean up exactly as it would for any
// other failure; see task.cancel's own doc comment for why that isn't
// always instant. Falls back to cancelPipelineTask if id doesn't match
// anything in m.queue - a dashboard row's ID alone doesn't say which
// Queue it came from, so this just tries both; the two id spaces can never
// collide (see pipelineKey's own "pipeline:" prefix vs. task.dedupKey's
// plain paragraph/llmKey-based ones). Returns false if id matches nothing
// queued or in flight in either Queue (already finished, or never
// existed).
func (m *Manager) Cancel(id string) bool {
	if t, ok := m.queue.Cancel(func(c taskqueue.Task) bool { return c.Key() == id }); ok {
		m.dropCanceledQueued(t.(*task))
		return true
	}
	// Not queued any more (Cancel above only removes queued work) - if id
	// still names anything at all, it must be in flight instead.
	if found, ok := m.queue.Find(func(c taskqueue.Task) bool { return c.Key() == id }); ok {
		if bt := found.(*task); bt.cancel != nil {
			bt.cancel()
		}
		return true
	}
	return m.cancelPipelineTask(id)
}

// CancelChapter cancels every queued task for chapterID and every in-flight
// one's context, then waits (until ctx is done) for the in-flight ones to
// actually finish - httpapi's single-chapter re-import, which is about to
// replace the chapter's paragraphs and can't have a task still writing
// results keyed by the old paragraphs' idx into the new ones. Keeps
// cancelling while it waits, since a still-running whole-book bulk group
// can push fresh tasks for this chapter in the meantime. Book-level
// pipeline tasks themselves are left alone: whatever they enqueue after
// the replace reads the new content. Returns false if ctx ran out with
// work for chapterID still in flight.
func (m *Manager) CancelChapter(ctx context.Context, chapterID string) bool {
	ofChapter := func(c taskqueue.Task) bool { return c.(*task).chapterID == chapterID }
	for {
		for {
			t, ok := m.queue.Cancel(ofChapter)
			if !ok {
				break
			}
			m.dropCanceledQueued(t.(*task))
		}
		for _, t := range m.queue.InFlightTasks() {
			if bt := t.(*task); bt.chapterID == chapterID && bt.cancel != nil {
				bt.cancel()
			}
		}
		if !m.IsGenerating(chapterID) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// dropCanceledQueued does the bookkeeping for bt, a task just removed from
// m.queue while still queued (taskqueue.Queue.Cancel): chapterPending,
// a change notification, and delivering a canceled outcome to any waiter.
func (m *Manager) dropCanceledQueued(bt *task) {
	m.mu.Lock()
	m.chapterPending[bt.chapterID]--
	if m.chapterPending[bt.chapterID] <= 0 {
		delete(m.chapterPending, bt.chapterID)
	}
	m.mu.Unlock()
	m.notifyChanged()
	deliverCanceled(bt)
}

// CancelAll cancels every queued and in-flight task at once, across both
// m.queue and pipelineQueue - httpapi's DELETE /api/jobs, the Jobs
// dashboard's "Cancel all" button. Exactly Cancel applied to every task
// currently in either queue/in flight, just batched into one pass per
// queue (one lock acquisition, one heap replacement each) rather than one
// Cancel call per id - functionally identical outcome, including the same
// in-flight cancellation caveats (see task.cancel's own doc comment).
// Returns how many tasks were acted on in total (queued removals plus
// in-flight cancellations requested, across both queues) - not a
// guarantee that many are done disappearing yet, since an in-flight
// task's own cancellation still runs its course through handleResult (or,
// for a pipeline phase, pipelineWorker's own completion path).
func (m *Manager) CancelAll() int {
	drained := m.queue.Drain()
	m.mu.Lock()
	for _, t := range drained {
		bt := t.(*task)
		m.chapterPending[bt.chapterID]--
		if m.chapterPending[bt.chapterID] <= 0 {
			delete(m.chapterPending, bt.chapterID)
		}
	}
	m.mu.Unlock()
	if len(drained) > 0 {
		m.notifyChanged()
	}

	inFlightCancels := make([]context.CancelFunc, 0, len(m.queue.InFlightTasks()))
	for _, t := range m.queue.InFlightTasks() {
		if bt := t.(*task); bt.cancel != nil {
			inFlightCancels = append(inFlightCancels, bt.cancel)
		}
	}

	for _, t := range drained {
		deliverCanceled(t.(*task))
	}
	for _, cancel := range inFlightCancels {
		cancel()
	}
	return len(drained) + len(inFlightCancels) + m.cancelAllPipelineTasks()
}

// ErrTaskNotFound is PromoteTier's own error when id doesn't name
// anything currently queued or in flight in either queue - httpapi maps
// this to 404, the same status Cancel's own false return does.
var ErrTaskNotFound = errors.New("jobs: task not found")

// ErrInvalidTier is PromoteTier's own error for a newTier that isn't one
// of TierUrgent/TierLookahead/TierBackground.
var ErrInvalidTier = errors.New("jobs: invalid tier")

// ErrTierNotMoreUrgent is PromoteTier's own error when newTier isn't
// strictly more urgent than the task's current tier - see PromoteTier's
// own doc comment for why this method only ever allows raising urgency,
// never lowering it.
var ErrTierNotMoreUrgent = errors.New("jobs: requested tier is not more urgent than the task's current tier")

// PromoteTier raises task id's own tier (queued or in-flight, an ordinary
// per-item task in m.queue or a preprocessing phase task in
// pipelineQueue - see EnqueuePipeline) to newTier, provided newTier is
// strictly more urgent than its current one - httpapi's PUT
// /api/jobs/{id}/tier, the Jobs dashboard's own per-row priority menu
// ("Bump to Urgent"/"Bump to Lookahead").
//
// Deliberately upgrade-only, never a general "set tier to anything":
// dependency-driven priority promotion (see taskqueue's own "priority
// promotion" section, and Task.Promote's own doc comment - "once
// promoted, a task stays promoted") already relies on a task's tier never
// dropping back down once something else has promoted it urgent on that
// task's behalf; a reader-triggered "set to anything" here could quietly
// violate that invariant for a task something else is still depending on
// being that urgent. Routed through taskqueue.Queue.WithTask (never
// Task.Promote directly - Promote's own doc comment: "never call this
// yourself from outside"), which finds id and holds the queue's lock
// across the read-then-maybe-write, so a concurrent Pop/Snapshot can
// never observe a half-updated tier.
//
// Tries m.queue first, then pipelineQueue - id's own prefix already
// disambiguates which one actually holds it (see pipelineKey), but this
// has no reason to duplicate that parsing when trying both in turn is
// just as cheap and one less thing to keep in sync if either key format
// ever changes. Returns ErrInvalidTier for a newTier that isn't one of
// TierUrgent/TierLookahead/TierNormal/TierBackground, ErrTaskNotFound if id matches
// nothing in either queue, or ErrTierNotMoreUrgent if it does but newTier
// isn't actually more urgent than what it already has.
func (m *Manager) PromoteTier(id string, newTier int) error {
	if newTier != TierUrgent && newTier != TierLookahead && newTier != TierNormal && newTier != TierBackground {
		return ErrInvalidTier
	}

	var found, applied bool
	var promoted *pipelineTask
	mutate := func(c taskqueue.Task) {
		found = true
		if newTier < c.Tier() {
			c.Promote(newTier)
			applied = true
			if pt, ok := c.(*pipelineTask); ok {
				promoted = pt
			}
		}
	}
	pred := func(c taskqueue.Task) bool { return c.Key() == id }

	if !m.queue.WithTask(pred, mutate) {
		m.pipelineQueue.WithTask(pred, mutate)
	}
	if !found {
		return ErrTaskNotFound
	}
	if !applied {
		return ErrTierNotMoreUrgent
	}
	if promoted != nil {
		if f := promoted.children(); f != nil {
			m.cascadePromotion(promoted.bookID, *f, newTier)
		}
	}
	m.notifyChanged()
	return nil
}

// cascadePromotion is PromoteTier's own follow-up for a promoted pipeline
// task (a preprocessing phase or a bulk group): bumping the pipeline task
// itself only reprioritizes poolPipeline's own bookkeeping, which barely
// matters (maxPipelineInFlight is generous, and pipelineTask.Less never
// distinguishes two of them) - the real work a reader wants sped up is
// whatever m.queue tasks its fan-out already dispatched, which stay at
// whatever tier they were dispatched at (see PipelinePhaseFunc's own doc
// comment on its tier parameter) without this. f (see pipelineTask.children)
// picks those children out; every match gets the same bump, including a
// paused chapter's detached continuation the pipeline task's own live
// tier() can't otherwise reach.
//
// A child's own blockers (e.g. a clone task's speechDirectionDependency)
// need no cascade here: taskqueue's dependency-tier propagation already
// relaxes a blocker's effective tier to match whatever depends on it.
func (m *Manager) cascadePromotion(bookID string, f ChildFilter, newTier int) {
	m.queue.WithEachTask(
		func(c taskqueue.Task) bool {
			ct, ok := c.(*task)
			return ok && ct.bookID == bookID && f.matches(ct)
		},
		func(c taskqueue.Task) {
			if newTier < c.Tier() {
				c.Promote(newTier)
			}
		},
	)
}
