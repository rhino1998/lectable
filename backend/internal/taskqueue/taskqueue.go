// Package taskqueue is a generic priority/dependency scheduler, extracted
// so a pool of work slots and a "check whether this is already done before
// creating a new job for it" dependency mechanism aren't things every kind
// of background job (paragraph audio, LLM attribution, whatever comes
// next) has to hand-roll or copy-paste for itself - see internal/jobs,
// this package's one real caller, for what that copy-pasting used to look
// like (three near-identical "look for a queued/in-flight match, else
// check persisted state, else lazily create one" functions, one per
// dependency kind) and this package's own tests for a minimal instantiation
// with synthetic task types that have nothing to do with jobs' own
// business logic at all.
//
// Dispatch order across the whole queue, regardless of pool, is:
//
//  1. Tier (Task.Tier - lower first), after "priority promotion": a task
//     that something else currently queued depends on is promoted - via
//     Task.Promote, a real, permanent mutation, not a value recomputed
//     fresh (and so free to drift back down) on every call - to the most
//     urgent tier among everything (transitively) depending on it, so a
//     low-priority blocker of high-priority work is never left to starve
//     behind unrelated same-tier work (see Resolver, computeState).
//  2. Within an otherwise-tied tier, a task that currently has other
//     queued work depending on it dispatches before one that doesn't -
//     finishing a blocker sooner is what actually lets its own dependents
//     become eligible sooner.
//  3. Still tied, and only ever between two tasks sharing the same Pool:
//     Task.Less - each Task implementation's own type-specific ordering
//     (e.g. series/chapter/paragraph position) - never applied across
//     different pools, which have no shared notion of what "closer" means.
//  4. A stable, always-defined last resort: insertion order.
//
// Separately from that ordering, only one PoolKey may have any task
// actually dispatched ("active") at a time - see Queue's own doc comment
// on why, and PoolConfig.OnActivate/OnDeactivate for observing the
// transitions that constraint produces. This is a real, load-bearing
// constraint this package enforces structurally (not merely an ordering
// preference #1-4 above could be mistaken for) - see the "one pool at a
// time" section on Queue.
package taskqueue

import (
	"sort"
	"sync"
)

// PoolKey identifies which worker pool a Task dispatches through - just a
// comparable value a caller picks (a short string is easiest), not a
// closed, hand-maintained enum a new kind of work has to be wired into via
// a growing switch statement. An unregistered key still works (see
// Queue.RegisterPool) - it defaults to a single-slot pool with no
// transition hooks, so a caller (or a test - see this package's own) can
// introduce a brand new pool just by having some Task report it, with no
// separate registration step required unless it wants non-default
// capacity or hooks.
type PoolKey string

// PoolConfig configures one PoolKey - see Queue.RegisterPool.
type PoolConfig struct {
	// Capacity is how many of this pool's own tasks may be dispatched
	// ("in flight") at once. <= 0 (including an unregistered pool's own
	// implicit zero value) defaults to 1 - see Queue.capacity.
	Capacity int

	// OnActivate, if set, runs synchronously, with Queue's own lock held
	// (keep it fast and non-reentrant into Queue - see Queue.Pop's own
	// doc comment), the moment this pool becomes the queue's single
	// active pool coming from some other pool (or from no pool at all) -
	// never merely because this pool dispatches another one of its own
	// tasks while it was already active (see "one pool at a time" below
	// and Queue's own "minimize transitions" tie-break for why that
	// distinction is exactly the point: a real load/unload cost - e.g.
	// swapping which model is resident - should only actually happen on
	// a genuine transition, not every single dispatch).
	OnActivate func()

	// OnDeactivate mirrors OnActivate: runs once this pool was the active
	// one, every one of its own in-flight tasks has finished (Finish
	// brought this pool's own count to zero), and some *different* pool
	// is about to become active in its place - never merely because this
	// pool is momentarily out of eligible work while it's still going to
	// be the one that keeps going (see betterCandidate/Pop's own
	// "graceful drain" - an in-flight task already dispatched is never
	// force-cancelled to hurry a transition along).
	OnDeactivate func()
}

// Task is the minimal contract a caller's own task type must satisfy to be
// scheduled by Queue. Deliberately free of any notion of what the work
// actually is (audio, an LLM call, anything else) - see Resolver for how a
// caller plugs in its own dependency semantics on top of this.
type Task interface {
	// Key uniquely identifies this task - see Queue.Push.
	Key() string
	// Pool reports which PoolKey this task dispatches through.
	Pool() PoolKey
	// Tier reports this task's own intrinsic priority - lower sorts
	// first. See the package doc comment for exactly where this fits
	// among Queue's other ordering rules (priority promotion first,
	// this second, Less third).
	Tier() int
	// Less breaks a tie against other once Tier and dependency-promotion
	// are already equal. Queue only ever calls this between two tasks
	// that share a Pool (see the package doc comment) - a Task
	// implementation is free to assume that and type-assert other to its
	// own concrete type without checking. Return false if there's no
	// real preference either way; Queue still applies a stable,
	// insertion-order tiebreak on top so dispatch order stays
	// deterministic regardless.
	Less(other Task) bool
	// Promote is called whenever priority promotion (see the package doc
	// comment) determines this task should be at least as urgent as
	// newTier - the implementation should update whatever backing state
	// Tier() reads so it reflects newTier from then on, permanently: once
	// promoted, a task stays promoted, even if whatever depended on it
	// later finishes or disappears - the same "don't risk
	// under-prioritizing it a second time" reasoning that makes Queue
	// apply this as a real, stored mutation rather than a value
	// recomputed fresh (and so able to drift back down) on every call.
	// Queue only ever calls this with a newTier at least as urgent
	// (numerically ≤) as Tier() already reports at the time of the call;
	// an implementation that already stores its own tier as a plain
	// field can just do `if newTier < t.tier { t.tier = newTier }`.
	// Always called with Queue's own lock already held (from inside
	// Pop/Snapshot) - never call this yourself from outside; see
	// WithTask if some other caller needs to safely mutate a task it
	// looked up.
	Promote(newTier int)
}

// LockedQueue is the narrow handle a Resolver receives - Queue's own
// Push/Find, but safe to call from inside a Resolver call specifically
// because both already operate under the very same lock Queue holds while
// invoking Resolver in the first place; Resolver must never call Queue's
// own exported (self-locking) methods directly, which would deadlock
// against that already-held lock.
type LockedQueue struct {
	q *Queue
	// readOnly disables Push (see its own doc comment) - set only by
	// Snapshot's own read-only computeState pass, never by Pop's.
	readOnly bool
	// memo backs Memo - scoped to this one LockedQueue, i.e. one
	// computeState pass, never shared across passes.
	memo map[any]any
}

// Memo returns fn's result for key, calling fn only the first time key is
// seen during this one resolve pass (a single computeState call - every
// Resolver call within one Pop/Snapshot/AnyEligibleQueued shares it, and
// the next pass starts empty). A Resolver runs once per queued task per
// pass, so any lookup it makes that depends on something coarser than the
// task itself (its book, its chapter, a disk check keyed by preset) would
// otherwise repeat once per task - a real, measured stall: several
// hundred queued paragraphs each re-querying the same book/chapter rows
// made a single Pop take over a second. Values are only ever as fresh as
// the start of the pass, which is fine: the whole pass runs under Queue's
// own lock, and the next Pop recomputes from scratch anyway.
func (lq *LockedQueue) Memo(key any, fn func() any) any {
	if v, ok := lq.memo[key]; ok {
		return v
	}
	if lq.memo == nil {
		lq.memo = make(map[any]any)
	}
	v := fn()
	lq.memo[key] = v
	return v
}

// Get returns the task (queued or in flight) whose Key is exactly key - an
// O(1) lookup, unlike Find's linear scan, for a Resolver that already
// knows the exact key of the dependency it's looking for.
func (lq *LockedQueue) Get(key string) (Task, bool) {
	if e, ok := lq.q.queued[key]; ok {
		return e.task, true
	}
	if t, ok := lq.q.inFlight[key]; ok {
		return t, true
	}
	return nil, false
}

// Push adds t if its Key isn't already queued or in flight, reporting
// whether it was actually added - the same dedup Queue.Push itself
// enforces. A Resolver calls this to lazily create a dependency task the
// first time it discovers one should exist but doesn't yet; a second,
// concurrent discovery of the same not-yet-resolved dependency (two
// different tasks each needing the same missing prerequisite) naturally
// finds it already present instead of double-pushing.
//
// Always reports false, without adding anything, on the read-only
// LockedQueue Snapshot's own computeState pass hands to Resolver - a
// caller merely reading the queue to display it should never, by that
// read alone, cause new work to be scheduled (see Snapshot's own doc
// comment). A Resolver written against this type's own documented
// contract already treats a false return the same way it treats "not
// found yet" - it just reports no dependency rather than one it can't
// actually create right now, which is exactly the right answer for a
// read-only pass: nothing here is lying about state, it's just
// declining to mutate it.
func (lq *LockedQueue) Push(t Task) bool {
	if lq.readOnly {
		return false
	}
	return lq.q.pushLocked(t)
}

// Find returns the first task (queued or in flight) matching pred.
func (lq *LockedQueue) Find(pred func(Task) bool) (Task, bool) { return lq.q.findLocked(pred) }

// FindInFlight is Find's own narrower sibling - in-flight tasks only, never
// a merely-queued one. Needed for a Resolver that wants "depend on whatever
// of this kind is already running" without also matching a same-kind task
// that's simply queued alongside t: two such tasks would each find the
// other as a "match" under Find, deadlocking both (t depends on the other
// depends on t) - a real failure mode a same-kind mutual-exclusion
// dependency runs into that an explicit-relationship dependency
// (attributionOrderDependency-style, where t only ever looks for one
// specific earlier task) does not. Restricting the search to already-
// in-flight tasks breaks that symmetry: of any group of same-kind tasks
// sitting in the queue together, none of them find each other yet (none
// is in flight), so the underlying queue's own ordering picks one to
// dispatch first; every other one then finds that one in flight and
// correctly waits, without ever depending on a peer that is itself
// waiting on it.
func (lq *LockedQueue) FindInFlight(pred func(Task) bool) (Task, bool) {
	return lq.q.findInFlightLocked(pred)
}

// Resolver reports every task t currently depends on - other tasks that
// must finish before t may dispatch, and whose own urgency t's should be
// promoted onto in the meantime (see the package doc comment's "priority
// promotion" rule). Returning an empty slice means t has nothing currently
// blocking it - the overwhelmingly common case.
//
// This is where the actual "is the real, persisted thing this dependency
// represents already done" precheck belongs - Resolver has no built-in
// opinion on what "done" means for any given dependency; it's expected to
// do that check itself, and only lazily create and push
// (LockedQueue.Push) a fresh dependency task when that check says no and
// nothing already queued/in-flight covers it either (Find first, same as
// Queue.Pop's own dedup discipline). Making this the one place that
// precheck has to happen - rather than something every dependency kind
// has to separately remember to hand-roll, the way internal/jobs' old
// characterizationBlockerFor/speechDirectionBlockerFor/
// referenceClipBlockerFor each did before this package existed - is
// exactly what stops a caller from accidentally recreating a "finished"
// job's task forever (the bug this package was built to make structurally
// harder to reintroduce).
//
// Called by Queue with its own internal lock already held, once per task
// per Pop/computeState pass - keep it cheap. A lookup shared by many
// tasks (their book, their chapter) belongs behind LockedQueue.Memo, and
// a search for a dependency with a known key behind LockedQueue.Get
// rather than Find: with hundreds of tasks queued, anything per-task that
// isn't O(1) runs on every single Pop.
type Resolver func(lq *LockedQueue, t Task) []Task

// entry is one queued (not yet dispatched) task plus its own insertion
// sequence number - Queue's always-defined, deterministic last-resort
// tiebreak (see betterCandidate) when nothing else distinguishes two
// candidates.
type entry struct {
	task Task
	seq  int64
}

// Queue is a priority/dependency scheduler over an arbitrary set of
// PoolKeys - see the package doc comment for the full ordering rules and
// PoolConfig for per-pool capacity/transition hooks.
//
// One pool at a time: across every registered (or merely-referenced)
// pool, at most one may have any task actually dispatched ("in flight") at
// once - Pop never returns a task from pool B while pool A still has one
// of its own in flight, even if B's own candidate would otherwise
// outrank A's. This mirrors internal/jobs' own real constraint (every
// pool ultimately dispatches against one shared, VRAM-limited worker
// process that can't usefully run two model families at once - see
// backend/CLAUDE.md's "ttsworker / audioworker" section) generalized into
// this package rather than reimplemented per caller: Queue enforces it
// unconditionally rather than making it a property some pools opt into
// and others don't, since a caller with only one real pool (or with pools
// that never actually contend for the same underlying resource) simply
// never notices the difference. A pool already active is still free to
// keep dispatching more of its own work up to its own Capacity while it
// remains the active one; the constraint only ever stops a *different*
// pool from starting.
//
// Graceful, not forced: Queue never cancels an already-dispatched task to
// hurry a transition along. Once some other pool's best candidate
// genuinely outranks the active pool's own (see betterCandidate), the
// active pool simply stops being handed any *more* new work (Pop returns
// nothing for it) so it drains to zero on its own as its existing
// in-flight tasks finish naturally - the transition (and its
// OnDeactivate/OnActivate hooks) actually happens on the very next Pop
// call after that drain completes.
//
// Minimize transitions: when nothing outranks the active pool's own next
// candidate (including a plain tie), Queue keeps dispatching from it
// rather than switching - a burst of same-tier, non-interdependent
// background work across several pools should mostly finish out
// whichever pool it's already in rather than thrashing back and forth
// (and firing OnActivate/OnDeactivate) for no ordering benefit at all.
type Queue struct {
	mu sync.Mutex

	resolver Resolver

	poolConfigs map[PoolKey]PoolConfig

	queued   map[string]*entry
	inFlight map[string]Task

	inFlightCount map[PoolKey]int

	hasActive  bool
	activePool PoolKey

	seq int64
}

// NewQueue returns an empty Queue. resolver may be nil (equivalent to a
// Resolver that always reports no dependencies) for a caller with no
// dependency semantics of its own at all.
func NewQueue(resolver Resolver) *Queue {
	if resolver == nil {
		resolver = func(*LockedQueue, Task) []Task { return nil }
	}
	return &Queue{
		resolver:      resolver,
		poolConfigs:   make(map[PoolKey]PoolConfig),
		queued:        make(map[string]*entry),
		inFlight:      make(map[string]Task),
		inFlightCount: make(map[PoolKey]int),
	}
}

// RegisterPool sets pool's capacity/transition hooks - see PoolConfig.
// Safe to call at any time, including after tasks are already
// queued/in-flight for pool (it only affects future capacity/transition
// checks). A pool never explicitly registered still works, defaulting to
// PoolConfig{Capacity: 1} with no hooks - see Queue's own doc comment.
func (q *Queue) RegisterPool(pool PoolKey, cfg PoolConfig) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.poolConfigs[pool] = cfg
}

func (q *Queue) capacity(pool PoolKey) int {
	if cfg, ok := q.poolConfigs[pool]; ok && cfg.Capacity > 0 {
		return cfg.Capacity
	}
	return 1
}

// Push queues t, reporting whether it was actually added - false if
// t.Key() already names a task this queue currently has queued or in
// flight. A true duplicate push is a caller error to report, not merge -
// any "a second request for the same work should join the first one's
// waiters" behavior is business-specific (internal/jobs' own dedup layer
// does this one level up) and out of scope for this package.
func (q *Queue) Push(t Task) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pushLocked(t)
}

func (q *Queue) pushLocked(t Task) bool {
	key := t.Key()
	if _, ok := q.queued[key]; ok {
		return false
	}
	if _, ok := q.inFlight[key]; ok {
		return false
	}
	q.seq++
	q.queued[key] = &entry{task: t, seq: q.seq}
	return true
}

// Find returns the first task (queued or in flight) matching pred.
func (q *Queue) Find(pred func(Task) bool) (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.findLocked(pred)
}

// WithEachTask is WithTask's own multi-match sibling: it calls fn, still
// under Queue's own lock, on every task (queued or in flight) matching
// pred, rather than stopping at the first - for a caller like a pipeline
// phase promotion that needs to reach every one of a book's own several
// real per-chapter tasks at once, not just one identified by an exact
// Key(). Returns how many matched (0 if none, fn never called). Same
// locking rationale as WithTask - a match's mutation happens while still
// holding the lock that guards Queue's own concurrent Tier/Pool/Less reads.
func (q *Queue) WithEachTask(pred func(Task) bool, fn func(Task)) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, e := range q.queued {
		if pred(e.task) {
			fn(e.task)
			n++
		}
	}
	for _, t := range q.inFlight {
		if pred(t) {
			fn(t)
			n++
		}
	}
	return n
}

// WithTask finds the first task (queued or in flight) matching pred and,
// while still holding Queue's own lock, calls fn on it - the safe way for
// a caller to mutate a task's own state after looking it up, rather than
// mutating it after a plain Find already returned and released the lock:
// that gap would race against Queue's own internal reads of that same
// state (Tier/Pool/Less, read from inside Pop/Snapshot under this same
// lock) running concurrently on another goroutine. Reports whether a
// match was found (fn is never called otherwise).
func (q *Queue) WithTask(pred func(Task) bool, fn func(Task)) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.findLocked(pred)
	if !ok {
		return false
	}
	fn(t)
	return true
}

// WithQueuedTask is WithTask's own narrower sibling: it only matches
// against q.queued, never q.inFlight. Needed for any mutation that isn't
// safe to make while a task is actually dispatched - an in-flight task's
// own goroutine (jobs.Manager.generate) reads its work-defining fields
// (voice/instruct/preset/etc.) with no lock of its own, on the assumption
// nothing mutates them after dispatch; WithTask's own callers that only
// touch bookkeeping fields already safe to adjust mid-flight (tier,
// chapterIdx, waiter lists) can keep matching in-flight tasks too, but a
// caller refreshing work-defining fields on a dedup merge must use this
// instead, or risk racing that read.
func (q *Queue) WithQueuedTask(pred func(Task) bool, fn func(Task)) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range q.queued {
		if pred(e.task) {
			fn(e.task)
			return true
		}
	}
	return false
}

func (q *Queue) findLocked(pred func(Task) bool) (Task, bool) {
	for _, e := range q.queued {
		if pred(e.task) {
			return e.task, true
		}
	}
	for _, t := range q.inFlight {
		if pred(t) {
			return t, true
		}
	}
	return nil, false
}

// findInFlightLocked is findLocked's own in-flight-only half - see
// LockedQueue.FindInFlight's own doc comment for why a Resolver needs this
// distinct from Find.
func (q *Queue) findInFlightLocked(pred func(Task) bool) (Task, bool) {
	for _, t := range q.inFlight {
		if pred(t) {
			return t, true
		}
	}
	return nil, false
}

// Cancel removes and returns the queued (not in-flight) task matching
// pred, if any - an in-flight task can't be pulled back off its slot this
// way (there is no dispatched work for this package to cancel - a caller
// tracking real cancellable work, like internal/jobs' own context.
// CancelFunc, does that one layer up).
func (q *Queue) Cancel(pred func(Task) bool) (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for key, e := range q.queued {
		if pred(e.task) {
			delete(q.queued, key)
			return e.task, true
		}
	}
	return nil, false
}

// Drain removes and returns every currently queued (not in-flight) task,
// leaving the queue empty of queued work - a caller's own bulk-cancel
// operation (an in-flight task can't be pulled back off its slot this
// way - see Cancel's own doc comment).
func (q *Queue) Drain() []Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Task, 0, len(q.queued))
	for _, e := range q.queued {
		out = append(out, e.task)
	}
	q.queued = make(map[string]*entry)
	return out
}

// InFlight reports whether key names a task currently dispatched.
func (q *Queue) InFlight(key string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.inFlight[key]
	return ok
}

// InFlightTasks returns every currently in-flight task, no particular
// order - a pure lookup with no dependency resolution triggered, unlike
// Snapshot's own queued half (see its own doc comment).
func (q *Queue) InFlightTasks() []Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]Task, 0, len(q.inFlight))
	for _, t := range q.inFlight {
		out = append(out, t)
	}
	return out
}

// AnyEligibleQueued reports whether any currently queued task matching
// pred is also actually dispatchable right now - no unresolved dependency
// of its own (see computeState) - unlike AnyQueued, which counts a
// matching task regardless of whether anything could ever dispatch it in
// its current state.
//
// This distinction is the whole point for a caller like
// jobs.Manager.HasHigherPriorityWork: "is there real work waiting that
// yielding this pool would actually unblock" is a fundamentally different
// question from "does some queued task merely carry an urgent tier" - a
// task that's urgent on paper but blocked on, say, this exact pool's own
// in-flight work finishing first would never actually get to run just
// because this pool paused, so counting it as a reason to pause is
// self-defeating: it can cause a task to pause-and-immediately-resume
// every single batch, forever, for no benefit at all (confirmed live: a
// chapter's own direction-tagging task pausing every batch because that
// same chapter's own paragraphs - blocked on that very task - were
// sitting in the queue at TierUrgent).
//
// Read-only, like Snapshot - never creates a new dependency task as a
// side effect (computeState's own create=false).
func (q *Queue) AnyEligibleQueued(pred func(Task) bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	states := q.computeState(false)
	for key, e := range q.queued {
		if !pred(e.task) {
			continue
		}
		if st := states[key]; st == nil || len(st.deps) == 0 {
			return true
		}
	}
	return false
}

// AnyQueued reports whether any currently queued (not in-flight) task
// matches pred - a pure lookup, no dependency resolution triggered (see
// Find, which also checks in-flight and is used where that matters, and
// AnyEligibleQueued, which also checks whether a match could actually
// dispatch right now).
func (q *Queue) AnyQueued(pred func(Task) bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, e := range q.queued {
		if pred(e.task) {
			return true
		}
	}
	return false
}

// Len reports how many tasks are currently queued (not in flight).
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queued)
}

// Finish marks the in-flight task named key as done, freeing its pool's
// slot. A no-op if key doesn't name a currently in-flight task (already
// finished, or never dispatched at all). Whether/when this actually
// triggers a pool transition (and its OnDeactivate/OnActivate hooks) is
// decided lazily, on the next Pop call, not here - see Queue's own
// "graceful drain" section.
func (q *Queue) Finish(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	t, ok := q.inFlight[key]
	if !ok {
		return
	}
	delete(q.inFlight, key)
	q.inFlightCount[t.Pool()]--
}

// taskState is computeState's own per-task scratch result: task itself
// (so promotion below has something to call Promote on), deps (this
// task's current, unresolved dependencies - non-empty means blocked, not
// dispatchable at all right now), effTier (Tier after priority
// promotion), and hasQueuedDependents (whether some other still-queued
// task currently has this one among its own deps) - see the package doc
// comment's ordering rules #1/#2.
type taskState struct {
	task                Task
	deps                []Task
	effTier             int
	hasQueuedDependents bool
}

// computeState resolves dependencies and priority promotion fresh, for
// every currently queued task (plus whatever a Resolver call lazily
// creates along the way, when create is true - see Resolver's own doc
// comment), and commits that promotion by calling Task.Promote on every
// task whose computed effTier actually improves on what Tier() already
// reports - a real, permanent mutation, not a value only ever held in the
// map this returns. Recomputed fresh on every Pop/Snapshot call rather
// than incrementally maintained: an earlier, mutation-based version of
// this same idea (internal/jobs' own effectiveTier/propagateDependencies)
// accumulated real staleness bugs from updating it in place at scattered
// call sites that didn't agree on when to run; always recomputing the
// whole (small, per this package's own expected scale) dependency graph
// from scratch, and only then applying the result as one single
// consistent mutation, avoids that class of bug while still giving
// promotion the "sticks permanently, visible on the task itself" property
// a caller like a dashboard needs (see Snapshot, which would otherwise
// have to track effective tier as a second, separate value from Tier()
// itself, and did for a while - a task moving from queued to in-flight
// showed its priority visibly *drop* purely as a display artifact of
// those two values disagreeing, confirmed live).
//
// create is false only for Snapshot's own read-only pass (see its doc
// comment): Resolver still runs exactly the same, but its LockedQueue
// can't actually push anything, so this can never itself bring a new
// dependency task into existence. It can still discover - and, same as
// always, commit - a promotion among tasks that already exist: mutating
// an existing task's own priority is not "scheduling new work" in the
// sense Snapshot's own contract cares about, so this part of computeState
// runs the same regardless of create. Pop always passes true: dispatch
// has to be able to act on a dependency that doesn't exist yet by
// creating it, which is the whole point of Resolver in the first place.
//
// Must be called with q.mu already held.
func (q *Queue) computeState(create bool) map[string]*taskState {
	states := make(map[string]*taskState, len(q.queued))
	lq := &LockedQueue{q: q, readOnly: !create}

	var worklist []Task
	for _, e := range q.queued {
		worklist = append(worklist, e.task)
	}
	for len(worklist) > 0 {
		t := worklist[len(worklist)-1]
		worklist = worklist[:len(worklist)-1]
		key := t.Key()
		if _, seen := states[key]; seen {
			continue
		}
		if _, inFlight := q.inFlight[key]; inFlight {
			// A dependency that's already dispatched is a pure sink for
			// this pass: it can't be reprioritized in the sense of
			// changing dispatch order (it's already running), but it can
			// still be promoted below - see this function's own doc
			// comment on why that matters for display - and it has no
			// deps of its own worth resolving again here - popTask
			// already verified that once, when it was itself dispatched.
			states[key] = &taskState{task: t, effTier: t.Tier()}
			continue
		}
		if _, queued := q.queued[key]; !queued {
			// Resolver returned a task this queue has never actually
			// been given (via Push, including from inside Resolver
			// itself) - a caller bug. Treat it as an unconditionally
			// unresolved dependency (so whatever named it as a
			// dependency simply never dispatches) rather than panicking
			// or silently ignoring it - safe, and detectable from the
			// caller side by that task never making progress.
			states[key] = &taskState{task: t, deps: []Task{t}, effTier: t.Tier()}
			continue
		}
		deps := q.resolver(lq, t)
		states[key] = &taskState{task: t, deps: deps, effTier: t.Tier()}
		worklist = append(worklist, deps...)
	}

	// Relax effective tiers: a task's own tier is a lower bound, further
	// lowered (never raised) to match anything that depends on it,
	// transitively. Bounded by len(states) passes - even a Resolver that
	// (against its own contract) produced a cycle just settles at that
	// cycle's shared minimum after one trip around rather than looping
	// forever, the same guarantee this replaces relied on.
	for pass := 0; pass < len(states)+1; pass++ {
		changed := false
		for _, st := range states {
			for _, dep := range st.deps {
				if dst, ok := states[dep.Key()]; ok && st.effTier < dst.effTier {
					dst.effTier = st.effTier
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}

	for _, st := range states {
		for _, dep := range st.deps {
			if dst, ok := states[dep.Key()]; ok {
				dst.hasQueuedDependents = true
			}
		}
	}

	// Commit: this is the one place Promote is ever called - see the
	// package doc comment's "priority promotion" section and Task.
	// Promote's own doc comment for why this is a real, permanent
	// mutation rather than a value kept only in the map just built.
	for _, st := range states {
		if st.effTier < st.task.Tier() {
			st.task.Promote(st.effTier)
		}
	}

	return states
}

// candidate is one pool's own best still-queued, dispatchable-right-now
// task, as scored by betterCandidate.
type candidate struct {
	task                Task
	effTier             int
	hasQueuedDependents bool
	seq                 int64
}

// better reports whether a should be preferred over b - see the package
// doc comment for the full rule (tier, then dependents, then same-pool
// Less, then insertion order).
func better(a, b *candidate) bool {
	if a.effTier != b.effTier {
		return a.effTier < b.effTier
	}
	if a.hasQueuedDependents != b.hasQueuedDependents {
		return a.hasQueuedDependents
	}
	if a.task.Pool() == b.task.Pool() {
		if a.task.Less(b.task) {
			return true
		}
		if b.task.Less(a.task) {
			return false
		}
	}
	return a.seq < b.seq
}

// Pop dispatches and returns the single best task to run next, or reports
// false if nothing is currently both queued and eligible (every
// dependency clear, its pool under capacity, and - see Queue's own "one
// pool at a time" section - either no other pool is currently active, or
// this task's pool already is). The returned task is moved from queued to
// in-flight; call Finish(task.Key()) once it's actually done.
func (q *Queue) Pop() (Task, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	states := q.computeState(true)

	bestByPool := make(map[PoolKey]*candidate)
	for key, e := range q.queued {
		st := states[key]
		if st == nil || len(st.deps) > 0 {
			continue // blocked on an unresolved dependency
		}
		pool := e.task.Pool()
		if q.inFlightCount[pool] >= q.capacity(pool) {
			continue // no free slot in this pool right now
		}
		cand := &candidate{task: e.task, effTier: st.effTier, hasQueuedDependents: st.hasQueuedDependents, seq: e.seq}
		if cur, ok := bestByPool[pool]; !ok || better(cand, cur) {
			bestByPool[pool] = cand
		}
	}
	if len(bestByPool) == 0 {
		return nil, false
	}

	dispatch := func(pool PoolKey, cand *candidate) Task {
		delete(q.queued, cand.task.Key())
		q.inFlight[cand.task.Key()] = cand.task
		q.inFlightCount[pool]++
		return cand.task
	}

	if q.hasActive {
		activeBest := bestByPool[q.activePool] // nil if the active pool has nothing eligible left

		var elsewherePool PoolKey
		var elsewhereBest *candidate
		for pool, cand := range bestByPool {
			if pool == q.activePool {
				continue
			}
			if activeBest != nil && !better(cand, activeBest) {
				continue // doesn't outrank staying in the active pool
			}
			if elsewhereBest == nil || better(cand, elsewhereBest) {
				elsewherePool, elsewhereBest = pool, cand
			}
		}

		if elsewhereBest != nil {
			if q.inFlightCount[q.activePool] > 0 {
				// Something elsewhere genuinely outranks continuing the
				// active pool, but it hasn't drained yet - refuse to
				// admit more work into it (even work that's still
				// eligible) so it actually finishes draining, rather
				// than force-cancelling anything already dispatched.
				return nil, false
			}
			q.transition(elsewherePool)
			return dispatch(elsewherePool, elsewhereBest), true
		}
		if activeBest != nil {
			return dispatch(q.activePool, activeBest), true
		}
		return nil, false
	}

	var firstPool PoolKey
	var first *candidate
	for pool, cand := range bestByPool {
		if first == nil || better(cand, first) {
			firstPool, first = pool, cand
		}
	}
	q.transition(firstPool)
	return dispatch(firstPool, first), true
}

// QueuedSnapshot is one queued task as reported by Snapshot, paired with
// its own current effective (post-promotion) tier - see Snapshot's own
// doc comment.
type QueuedSnapshot struct {
	Task Task
	Tier int
}

// Snapshot reports every currently queued task - ordered the same way
// Pop itself ranks candidates (effective tier after priority promotion,
// then has-queued-dependents, then - only within a shared pool, see
// Task.Less's own doc comment - each Task's own type-specific ordering,
// then insertion order), each paired with that same effective tier - and
// every currently in-flight task (no particular order - every entry there
// is already concurrently active, not waiting in line), each paired with
// the effective tier it actually dispatched at (frozen at that moment,
// via inFlightTier - it can't be reprioritized any further now, and
// re-deriving it dynamically would make an already-dispatched task's
// reported tier flicker for no reason, or worse, appear to *drop* the
// instant a promoted task moves from queued to in-flight, purely as a
// display artifact - confirmed live). A caller's own read-only dashboard.
//
// Promotion is real here, not approximated: this runs the exact same
// Resolver-driven computeState pass Pop does, over every currently queued
// task, so a task blocking something urgent genuinely sorts (and reports
// its Tier) as if it already had that urgency - a caller watching this
// should actually see a blocking dependency's priority rise the moment
// something urgent starts depending on it, not just once it happens to be
// dispatched. The one thing withheld from Resolver here is the power to
// create a brand new dependency task (computeState's own create=false) -
// a caller reading the queue to display it should never, by that read
// alone, cause new work to be scheduled; a dependency that doesn't exist
// yet simply doesn't show up as one here; the moment it's actually needed,
// Pop still creates it exactly as always.
//
// Still not a literal dispatch simulation: it doesn't reflect which pool
// is currently active, each pool's own capacity, or the graceful-drain/
// minimize-transitions rules around switching pools - none of which a
// single flat ordering of every pool's tasks together can express. That
// caveat is unavoidable for this kind of read-only listing; the ordering
// itself, and the Tier each task is reported at, are not approximations.
func (q *Queue) Snapshot() (queued []QueuedSnapshot, inFlight []QueuedSnapshot) {
	q.mu.Lock()
	defer q.mu.Unlock()

	states := q.computeState(false)
	entries := make([]*entry, 0, len(q.queued))
	for _, e := range q.queued {
		entries = append(entries, e)
	}
	cand := func(e *entry) *candidate {
		st := states[e.task.Key()]
		return &candidate{task: e.task, effTier: st.effTier, hasQueuedDependents: st.hasQueuedDependents, seq: e.seq}
	}
	sort.SliceStable(entries, func(i, j int) bool { return better(cand(entries[i]), cand(entries[j])) })

	queued = make([]QueuedSnapshot, len(entries))
	for i, e := range entries {
		// e.task.Tier() now, not states[...].effTier: computeState just
		// committed this task's own promotion for real (see its own doc
		// comment) - Tier() is the single source of truth from here on,
		// for both halves of this listing alike.
		queued[i] = QueuedSnapshot{Task: e.task, Tier: e.task.Tier()}
	}
	for _, t := range q.inFlight {
		// Whatever this task was promoted to (permanently, possibly by
		// an earlier Pop or Snapshot call, not necessarily this one) -
		// never a separately-tracked value that could disagree with
		// Tier() and show a spurious "downgrade" the instant a task
		// moves from queued to in-flight, confirmed live under the
		// previous, non-mutating design.
		inFlight = append(inFlight, QueuedSnapshot{Task: t, Tier: t.Tier()})
	}
	return queued, inFlight
}

// transition makes to the active pool, firing the outgoing pool's
// OnDeactivate and the incoming pool's OnActivate - unless to is already
// the active pool, in which case this is a no-op (no hooks fire for a
// pool simply continuing to dispatch more of its own work - see Queue's
// own "minimize transitions" doc comment). Must be called with q.mu
// already held.
func (q *Queue) transition(to PoolKey) {
	if q.hasActive && q.activePool == to {
		return
	}
	if q.hasActive {
		if cfg, ok := q.poolConfigs[q.activePool]; ok && cfg.OnDeactivate != nil {
			cfg.OnDeactivate()
		}
	}
	if cfg, ok := q.poolConfigs[to]; ok && cfg.OnActivate != nil {
		cfg.OnActivate()
	}
	q.activePool = to
	q.hasActive = true
}
