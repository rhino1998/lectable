package jobs

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/rhino1998/lectable/backend/internal/sched"
)

// sourceDirect is the request source of everything asked for outright (a
// button, an API call, a lookahead). A task with a request is never
// cancelled just because the group or dependent that also wanted it went
// away - see internal/sched.
const sourceDirect = "direct"

// sourcePromote is the request source of a promotion that isn't a person's
// own bump of that row: a group's promotion reaching its children, or live
// music pulling its chapter's scoring forward.
const sourcePromote = "promote"

type parentKey struct{}

// withParent marks ctx as belonging to the running group task key, so the
// tasks pushed under it are adopted by that group (Manager.pushTask).
func withParent(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, parentKey{}, key)
}

func parentOf(ctx context.Context) string {
	key, _ := ctx.Value(parentKey{}).(string)
	return key
}

// Key, Context, CanStart, Less, Prepare and Run make task a sched.Task.
func (t *task) Key() string { return t.dedupKey() }

// Context is the task's pool: the scheduler runs one pool at a time, since
// they share ttsworker's one VRAM budget (see kindPool).
func (t *task) Context() string { return string(poolFor(t.kind)) }

// CanStart holds each pool to its capacity, speaker attribution to one
// task at a time whatever the LLM pool's capacity (two chapters attributed
// side by side would each miss the characters the other is discovering),
// and a chapter's emotion or pronunciation pass to one run at a time.
func (t *task) CanStart(running []sched.Task) bool {
	pool := poolFor(t.kind)
	inPool := 0
	for _, r := range running {
		rt, ok := r.(*task)
		if !ok {
			continue
		}
		if poolFor(rt.kind) == pool {
			inPool++
		}
		if t.kind == KindSpeakerAttribution && rt.kind == KindSpeakerAttribution {
			return false
		}
		// A chapter's pass and its window runs write the same paragraphs.
		if (t.kind == KindSpeechDirection || t.kind == KindPronunciation) && rt.kind == t.kind && rt.chapterID == t.chapterID {
			return false
		}
	}
	return inPool < poolCapacity[pool]
}

func (t *task) Less(other sched.Task, priority int) bool {
	o, ok := other.(*task)
	if !ok {
		return false // a group row: no order between the two
	}
	return lessTask(t, o, priority)
}

func (t *task) Prepare() ([]sched.Need, error) { return t.m.needs(t), nil }

// Run dispatches t and handles its result. A paused LLM pass (its run
// returned a requeue callback) yields rather than ending: the next Run
// calls the callback, which pushes the remainder under this same key, and
// pushTask hands that remainder's work to t instead of queueing it.
func (t *task) Run(ctx context.Context, attempt int) error {
	m := t.m
	t.attempt = attempt
	t.startedAt = time.Now()
	if p, ok := m.sched.Priority(t.Key()); ok && p != sched.NoPriority {
		t.tier = p
	}
	if resume := t.resume; resume != nil {
		t.resume = nil
		if !m.resumeInto(t, resume) {
			// The callback queued nothing under this key: there is no
			// remainder to run.
			return m.handleResult(t, taskResult{})
		}
	}
	return m.handleResult(t, <-m.startTask(ctx, t))
}

// Retryable matches retryEligible, which handleResult uses to decide
// whether a failure is final.
func (t *task) Retryable(err error) bool {
	var settled *settledError
	return !errors.As(err, &settled) && retryEligible(t, err)
}

// Ended is the scheduler's last word on t. A task that ran has already
// settled its own outcome; one that never did (cancelled while queued, or
// failed with a dependency) is settled here.
func (t *task) Ended(err error) {
	m := t.m
	m.mu.Lock()
	m.chapterPending[t.chapterID]--
	if m.chapterPending[t.chapterID] <= 0 {
		delete(m.chapterPending, t.chapterID)
	}
	settled := t.settled
	m.mu.Unlock()
	if settled || err == nil {
		return
	}
	var depErr *sched.DependencyError
	if errors.As(err, &depErr) {
		log.Printf("jobs: %s task %s not run: %v", t.kind, t.dedupKey(), err)
	}
	if depErr != nil && (t.kind == KindVoiceClone || t.kind == KindVoiceDesign) {
		paragraphs := t.mergeParagraphs
		if len(paragraphs) == 0 {
			paragraphs = append(paragraphs, t.paragraph)
		}
		for _, p := range paragraphs {
			m.failParagraph(t, p, err)
		}
		return
	}
	if errors.Is(err, sched.ErrCanceled) {
		err = context.Canceled
	}
	deliverFailure(t, err)
}

// Merge folds a second push of t's key into t: its waiters always, a more
// urgent chapter position, and - while t hasn't started - the voice it
// would render under, which may have been reassigned since t was queued.
// An in-flight render reads those fields unlocked, so they're left alone;
// the result is then found stale against the current voice and re-queued.
func (t *task) Merge(newer sched.Task, running bool) {
	n, ok := newer.(*task)
	if !ok {
		return
	}
	t.m.mu.Lock()
	t.llmWaiters = append(t.llmWaiters, n.llmWaiters...)
	t.provisionWaiters = append(t.provisionWaiters, n.provisionWaiters...)
	t.previewWaiters = append(t.previewWaiters, n.previewWaiters...)
	t.previewTextWaiters = append(t.previewTextWaiters, n.previewTextWaiters...)
	t.m.mu.Unlock()
	if running {
		return
	}
	if n.tier < t.tier {
		t.tier = n.tier
		t.chapterIdx = n.chapterIdx
	}
	if t.kind == KindVoiceClone || t.kind == KindVoiceDesign {
		t.kind = n.kind
		t.voiceID = n.voiceID
		t.instruct = n.instruct
		t.presetID = n.presetID
		t.language = n.language
		t.refText = n.refText
		t.speedMultiplier = n.speedMultiplier
		t.seed = n.seed
		t.cloneModel = n.cloneModel
		t.designModel = n.designModel
		t.cloneInstruct = n.cloneInstruct
	}
}
