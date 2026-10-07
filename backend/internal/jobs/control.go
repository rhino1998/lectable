package jobs

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/sched"
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
		PresetID: t.presetID, Instruct: t.instruct,
		Emotion: taskEmotion(t), Chars: taskChars(t),
	}
}

// Snapshot reports every task for the Jobs dashboard: inFlight is what's
// running (sorted by tier, then book and position), queued everything else
// in the scheduler's own dispatch order - the order they will actually be
// considered in, each at its effective tier. Preprocessing phases and bulk
// groups are rows like any other.
func (m *Manager) Snapshot() (inFlight, queued []QueueTask) {
	for _, info := range m.sched.Snapshot() {
		var qt QueueTask
		switch t := info.Task.(type) {
		case *task:
			qt = toQueueTask(t, info.Priority)
			qt.Attempt = info.Attempt
		case *pipelineTask:
			qt = toPipelineQueueTask(t, info.Priority)
		default:
			continue
		}
		if info.State == sched.Running {
			inFlight = append(inFlight, qt)
		} else {
			queued = append(queued, qt)
		}
	}
	sort.SliceStable(inFlight, func(i, j int) bool {
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
	return inFlight, queued
}

// deliverFailure sends err to every caller blocked on t's result. Every
// waiter channel is buffered for its one send, so this never blocks. For a
// task that never ran (task.Ended); one that did delivers its own outcome
// from handleResult.
func deliverFailure(t *task, err error) {
	t.m.mu.Lock()
	llm, provision, preview, text := t.llmWaiters, t.provisionWaiters, t.previewWaiters, t.previewTextWaiters
	sfx, music := t.sfxGenWaiters, t.musicGenWaiters
	t.m.mu.Unlock()
	for _, w := range llm {
		w <- llmOutcome{err: err}
	}
	for _, w := range provision {
		w <- provisionOutcome{err: err}
	}
	for _, w := range preview {
		w <- previewOutcome{err: err}
	}
	for _, w := range text {
		w <- previewTextOutcome{err: err}
	}
	for _, w := range sfx {
		w <- err
	}
	for _, w := range music {
		w <- err
	}
}

// Cancel cancels one task or group by its QueueTask.ID - httpapi's DELETE
// /api/jobs/{id}, the Jobs dashboard's per-row "Cancel" button. A queued
// task is removed (store state untouched, as if it had never been
// enqueued) and its waiters get a canceled outcome; a running one has its
// context cancelled and ends through its normal result handling. Whatever
// depended on it fails, and whatever only it wanted is cancelled with it
// (see internal/sched). Returns false if id names nothing queued or
// running.
func (m *Manager) Cancel(id string) bool {
	return m.sched.Cancel(id)
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
	ofChapter := func(c sched.Task) bool {
		t, ok := c.(*task)
		return ok && t.chapterID == chapterID
	}
	for {
		m.sched.CancelWhere(ofChapter)
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

// CancelAll cancels every queued and in-flight task and group at once -
// httpapi's DELETE /api/jobs, the Jobs dashboard's "Cancel all" button.
// Returns how many were acted on; an in-flight one still has to notice its
// cancelled context.
func (m *Manager) CancelAll() int {
	return m.sched.CancelWhere(func(sched.Task) bool { return true })
}

// CancelBook cancels every queued and in-flight task and group working on
// bookID - its preprocessing run, bulk groups, and each per-chapter or
// per-character task - httpapi's DELETE /api/books/{id}/jobs, the library
// card's "Cancel jobs" button. Returns how many were acted on; an
// in-flight one still has to notice its cancelled context.
func (m *Manager) CancelBook(bookID string) int {
	return m.sched.CancelWhere(func(c sched.Task) bool {
		switch t := c.(type) {
		case *task:
			return t.bookID == bookID
		case *pipelineTask:
			return t.bookID == bookID
		}
		return false
	})
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

// PromoteTier bumps task or group id to newTier, provided that's strictly
// more urgent than its current effective tier - httpapi's PUT
// /api/jobs/{id}/tier, the Jobs dashboard's per-row priority menu. A bump
// is one more request for the task (sched.Scheduler.Bump), so whatever it
// depends on, and whatever a bumped group has asked for, becomes that
// urgent with it. Returns ErrInvalidTier for a newTier that isn't one of
// the four tiers, ErrTaskNotFound if id names nothing, or
// ErrTierNotMoreUrgent if it's already at least that urgent.
func (m *Manager) PromoteTier(id string, newTier int) error {
	if newTier != TierUrgent && newTier != TierLookahead && newTier != TierNormal && newTier != TierBackground {
		return ErrInvalidTier
	}
	current, ok := m.sched.Priority(id)
	if !ok {
		return ErrTaskNotFound
	}
	if newTier >= current {
		return ErrTierNotMoreUrgent
	}
	if !m.sched.Bump(id, newTier) {
		return ErrTaskNotFound
	}
	if c, _, ok := m.sched.Get(id); ok {
		if pt, ok := c.(*pipelineTask); ok {
			if f := pt.children(); f != nil {
				m.cascadePromotion(pt.bookID, *f, newTier)
			}
		}
	}
	return nil
}

// cascadePromotion carries a group's promotion to the tasks it fanned out
// that it didn't adopt (pushed with no group context - paragraph audio; an
// adopted task follows its group by itself): every queued or running task
// f matches in the group's book gets the same tier.
func (m *Manager) cascadePromotion(bookID string, f ChildFilter, newTier int) {
	for _, key := range m.taskKeys(func(t *task, _ sched.State) bool { return t.bookID == bookID && f.matches(t) }) {
		if p, ok := m.sched.Priority(key); ok && newTier < p {
			m.sched.Raise(sourcePromote, key, newTier)
		}
	}
}
