package taskqueue

import "testing"

// fakeTask is a minimal, business-logic-free Task used only by this
// package's own tests - it stands in for whatever a real caller's task
// type would be (internal/jobs' own paragraph/attribution/etc. tasks, or
// something entirely unrelated), to prove the scheduling machinery in
// this file works generically rather than only against jobs' own kinds.
type fakeTask struct {
	key   string
	pool  PoolKey
	tier  int
	order int // used by lessByOrder below - lower dispatches first among ties
}

func (t *fakeTask) Key() string   { return t.key }
func (t *fakeTask) Pool() PoolKey { return t.pool }
func (t *fakeTask) Tier() int     { return t.tier }
func (t *fakeTask) Less(other Task) bool {
	o, ok := other.(*fakeTask)
	if !ok {
		return false
	}
	return t.order < o.order
}
func (t *fakeTask) Promote(newTier int) {
	if newTier < t.tier {
		t.tier = newTier
	}
}

func newTask(key string, pool PoolKey, tier int) *fakeTask {
	return &fakeTask{key: key, pool: pool, tier: tier}
}

func mustPop(t *testing.T, q *Queue) Task {
	t.Helper()
	task, ok := q.Pop()
	if !ok {
		t.Fatalf("Pop: expected a task, got none")
	}
	return task
}

func expectNoPop(t *testing.T, q *Queue) {
	t.Helper()
	if task, ok := q.Pop(); ok {
		t.Fatalf("Pop: expected nothing dispatchable, got %q", task.Key())
	}
}

func TestPushDedup(t *testing.T) {
	q := NewQueue(nil)
	if !q.Push(newTask("a", "p", 0)) {
		t.Fatalf("first push of a fresh key should succeed")
	}
	if q.Push(newTask("a", "p", 0)) {
		t.Fatalf("pushing an already-queued key a second time should report false")
	}
	if q.Len() != 1 {
		t.Fatalf("expected exactly one queued task, got %d", q.Len())
	}

	task := mustPop(t, q)
	if q.Push(newTask("a", "p", 0)) {
		t.Fatalf("pushing an already-in-flight key should also report false")
	}
	q.Finish(task.Key())
	if !q.Push(newTask("a", "p", 0)) {
		t.Fatalf("pushing a key again after Finish should succeed")
	}
}

// TestTierOrderingAcrossPools proves priority is tier-first across every
// pool, not decided per-pool: a lower-tier (more urgent) task in an
// entirely different, unregistered pool dispatches before a higher-tier
// one, with no pool yet active.
func TestTierOrderingAcrossPools(t *testing.T) {
	q := NewQueue(nil)
	q.Push(newTask("background", "pool-a", 5))
	q.Push(newTask("urgent", "pool-b", -1))

	got := mustPop(t, q)
	if got.Key() != "urgent" {
		t.Fatalf("expected the lower-tier task from a different pool to win, got %q", got.Key())
	}
}

// TestDefaultCapacityIsOne proves an unregistered pool defaults to a
// single in-flight slot, and TestRegisteredCapacity proves an explicitly
// configured one is honored - "pre-defined pool sizes by key, otherwise
// default to 1".
func TestDefaultCapacityIsOne(t *testing.T) {
	q := NewQueue(nil)
	q.Push(newTask("a", "solo", 0))
	q.Push(newTask("b", "solo", 0))

	mustPop(t, q) // claims the one default slot
	expectNoPop(t, q)
}

func TestRegisteredCapacity(t *testing.T) {
	q := NewQueue(nil)
	q.RegisterPool("wide", PoolConfig{Capacity: 3})
	for _, k := range []string{"a", "b", "c", "d"} {
		q.Push(newTask(k, "wide", 0))
	}

	dispatched := map[string]bool{}
	for i := 0; i < 3; i++ {
		dispatched[mustPop(t, q).Key()] = true
	}
	if len(dispatched) != 3 {
		t.Fatalf("expected 3 concurrently dispatched tasks, got %d", len(dispatched))
	}
	expectNoPop(t, q) // 4th task blocked - pool already at its registered capacity
}

// TestOnePoolAtATime proves the core mutual-exclusion rule: while pool A
// still has an in-flight task, pool B never dispatches, even when B's own
// queued work would otherwise clearly outrank whatever's left in A.
func TestOnePoolAtATime(t *testing.T) {
	q := NewQueue(nil)
	q.Push(newTask("a1", "pool-a", 0))
	a1 := mustPop(t, q) // pool-a becomes active

	q.Push(newTask("b-urgent", "pool-b", -5))
	expectNoPop(t, q) // pool-a hasn't drained yet, even though pool-b is far more urgent

	q.Finish(a1.Key()) // pool-a fully drains
	got := mustPop(t, q)
	if got.Key() != "b-urgent" {
		t.Fatalf("expected pool-b's task once pool-a drained, got %q", got.Key())
	}
}

// TestGracefulDrainNeverCancels proves that once pool A is asked to
// "drain" in favor of pool B, it isn't force-cancelled - a task A already
// has in flight keeps running (this package has no cancellation
// mechanism at all; a caller wanting one tracks it one layer up), and A
// simply stops being handed any *more* new work in the meantime.
func TestGracefulDrainNeverCancels(t *testing.T) {
	q := NewQueue(nil)
	q.Push(newTask("a1", "pool-a", 0))
	a1 := mustPop(t, q) // pool-a becomes active, at its (default) capacity of 1

	q.Push(newTask("a3", "pool-a", 0)) // more pool-a work arrives...
	q.Push(newTask("b-urgent", "pool-b", -5))

	// a1 is still in flight regardless of what Pop does next - nothing
	// here ever reaches in to cancel it; this package has no
	// cancellation mechanism for already-dispatched work at all.
	if !q.InFlight(a1.Key()) {
		t.Fatalf("a1 should still be in flight - this package never force-cancels")
	}
	expectNoPop(t, q) // can't start a3 (pool-a already full) or b-urgent (pool-a still active and not drained)

	q.Finish(a1.Key()) // pool-a fully drains
	got := mustPop(t, q)
	if got.Key() != "b-urgent" {
		t.Fatalf("expected pool-b once pool-a drained and something outranked it (not a3, pool-a's own queued work), got %q", got.Key())
	}
}

// TestMinimizeTransitions proves that when a currently-active pool's next
// candidate ties in priority with another pool's, the queue keeps
// dispatching from the active pool rather than switching - "a bunch of
// non-interdependent background tasks should try to minimize pool
// transitions".
func TestMinimizeTransitions(t *testing.T) {
	q := NewQueue(nil)
	var activations, deactivations []string
	q.RegisterPool("pool-a", PoolConfig{
		OnActivate:   func() { activations = append(activations, "a") },
		OnDeactivate: func() { deactivations = append(deactivations, "a") },
	})
	q.RegisterPool("pool-b", PoolConfig{
		OnActivate:   func() { activations = append(activations, "b") },
		OnDeactivate: func() { deactivations = append(deactivations, "b") },
	})

	q.Push(newTask("a1", "pool-a", 1))
	first := mustPop(t, q) // pool-a becomes active
	if first.Key() != "a1" {
		t.Fatalf("expected a1 first, got %q", first.Key())
	}
	if len(activations) != 1 || activations[0] != "a" {
		t.Fatalf("expected exactly one activation (a), got %v", activations)
	}
	q.Finish(first.Key())

	// Same tier as a2 would be - pool-b's task ties, doesn't outrank.
	q.Push(newTask("a2", "pool-a", 1))
	q.Push(newTask("b1", "pool-b", 1))

	got := mustPop(t, q)
	if got.Key() != "a2" {
		t.Fatalf("expected to keep dispatching from the already-active pool on a tie, got %q", got.Key())
	}
	if len(activations) != 1 {
		t.Fatalf("a tied candidate elsewhere should not have triggered another activation, got %v", activations)
	}
	if len(deactivations) != 0 {
		t.Fatalf("expected no deactivation while staying in the same pool, got %v", deactivations)
	}

	q.Finish(got.Key())
	// Now pool-a has nothing left - it must actually switch, firing hooks.
	got2 := mustPop(t, q)
	if got2.Key() != "b1" {
		t.Fatalf("expected pool-b once pool-a genuinely ran out of work, got %q", got2.Key())
	}
	if len(deactivations) != 1 || deactivations[0] != "a" {
		t.Fatalf("expected pool-a to deactivate exactly once, got %v", deactivations)
	}
	if len(activations) != 2 || activations[1] != "b" {
		t.Fatalf("expected pool-b to activate exactly once, got %v", activations)
	}
}

// TestStrictlyBetterElsewhereTransitionsImmediately proves the mirror
// case of TestMinimizeTransitions: a genuinely more urgent task in
// another pool DOES trigger a transition (once the active pool drains),
// firing both hooks exactly once.
func TestStrictlyBetterElsewhereTransitionsImmediately(t *testing.T) {
	q := NewQueue(nil)
	var events []string
	q.RegisterPool("pool-a", PoolConfig{OnDeactivate: func() { events = append(events, "deactivate-a") }})
	q.RegisterPool("pool-b", PoolConfig{OnActivate: func() { events = append(events, "activate-b") }})

	q.Push(newTask("a1", "pool-a", 5))
	a1 := mustPop(t, q)

	q.Push(newTask("b1", "pool-b", -5)) // strictly more urgent
	expectNoPop(t, q)                   // pool-a hasn't drained

	q.Finish(a1.Key())
	got := mustPop(t, q)
	if got.Key() != "b1" {
		t.Fatalf("expected b1, got %q", got.Key())
	}
	if len(events) != 2 || events[0] != "deactivate-a" || events[1] != "activate-b" {
		t.Fatalf("expected [deactivate-a activate-b] in order, got %v", events)
	}
}

// TestDependencyBlocksDispatch proves a task with an unresolved dependency
// (per Resolver) is never dispatched while that dependency is still
// outstanding, queued or in flight, even though it would otherwise be the
// clear top candidate.
func TestDependencyBlocksDispatch(t *testing.T) {
	blocker := newTask("blocker", "pool", 0)
	dependent := newTask("dependent", "pool", -10) // far more urgent on paper

	resolver := func(lq *LockedQueue, t Task) []Task {
		if t.Key() == "dependent" {
			if _, ok := lq.Find(func(c Task) bool { return c.Key() == "blocker" }); ok {
				return []Task{blocker}
			}
		}
		return nil
	}
	q := NewQueue(resolver)
	q.Push(blocker)
	q.Push(dependent)

	got := mustPop(t, q)
	if got.Key() != "blocker" {
		t.Fatalf("expected the blocker to dispatch first (dependent is blocked), got %q", got.Key())
	}
	expectNoPop(t, q) // dependent still blocked - blocker is now in flight, not finished

	q.Finish(blocker.Key())
	got2 := mustPop(t, q)
	if got2.Key() != "dependent" {
		t.Fatalf("expected dependent once its blocker finished, got %q", got2.Key())
	}
}

// TestPriorityPromotion proves a low-tier task that something urgent
// depends on inherits that urgency (dispatches ahead of unrelated,
// nominally-same-tier work), rather than starving behind it purely
// because its own declared Tier is low.
func TestPriorityPromotion(t *testing.T) {
	blocker := newTask("blocker", "pool", 10)     // background, on paper
	dependent := newTask("dependent", "pool", -1) // urgent
	unrelated := newTask("unrelated", "pool", 10) // same background tier as blocker, nothing depends on it

	resolver := func(lq *LockedQueue, t Task) []Task {
		if t.Key() == "dependent" {
			return []Task{blocker}
		}
		return nil
	}
	q := NewQueue(resolver)
	q.RegisterPool("pool", PoolConfig{Capacity: 1})
	q.Push(unrelated)
	q.Push(blocker)
	q.Push(dependent)

	got := mustPop(t, q)
	if got.Key() != "blocker" {
		t.Fatalf("expected the promoted blocker to dispatch before same-tier unrelated work, got %q", got.Key())
	}
}

// TestHasQueuedDependentsTiebreak proves the #2 ordering rule directly: at
// an equal (non-promoted) tier, a task something else still queued
// depends on dispatches before one nothing depends on.
func TestHasQueuedDependentsTiebreak(t *testing.T) {
	blocker := newTask("blocker", "pool", 5)
	dependent := newTask("dependent", "pool", 5) // same tier as blocker - no promotion needed to demonstrate this rule
	leaf := newTask("leaf", "pool", 5)

	resolver := func(lq *LockedQueue, t Task) []Task {
		if t.Key() == "dependent" {
			return []Task{blocker}
		}
		return nil
	}
	q := NewQueue(resolver)
	q.RegisterPool("pool", PoolConfig{Capacity: 1})
	// Push leaf first so a naive FIFO tiebreak would pick it over blocker
	// - this test only passes if the has-queued-dependents rule actually
	// takes priority over plain insertion order.
	q.Push(leaf)
	q.Push(blocker)
	q.Push(dependent)

	got := mustPop(t, q)
	if got.Key() != "blocker" {
		t.Fatalf("expected blocker (has a queued dependent) before leaf (has none), got %q", got.Key())
	}
}

// TestLazyDependencyCreationChecksDoneFirst is the generic version of the
// actual bug this package exists to make structurally harder to write:
// a Resolver that lazily creates a fresh dependency task must consult
// real "is this already done" state first, or it recreates a finished
// dependency's task forever. This models that shape with a synthetic,
// business-logic-free "job artifact" (just a bool) instead of jobs'
// real chapter-state enum.
func TestLazyDependencyCreationChecksDoneFirst(t *testing.T) {
	done := false
	created := 0

	resolver := func(lq *LockedQueue, t Task) []Task {
		if t.Key() != "dependent" {
			return nil
		}
		if existing, ok := lq.Find(func(c Task) bool { return c.Key() == "prereq" }); ok {
			return []Task{existing}
		}
		if done {
			return nil // the real precheck: already done, nothing to (re)create
		}
		created++
		prereq := newTask("prereq", "pool", 0)
		lq.Push(prereq)
		return []Task{prereq}
	}

	q := NewQueue(resolver)
	q.Push(newTask("dependent", "pool", 0))

	// First pass: no prereq exists yet and it isn't done, so the resolver
	// creates exactly one.
	got := mustPop(t, q)
	if got.Key() != "prereq" {
		t.Fatalf("expected the lazily-created prereq to dispatch first, got %q", got.Key())
	}
	if created != 1 {
		t.Fatalf("expected exactly one prereq creation, got %d", created)
	}
	done = true
	q.Finish(got.Key())

	// Second pass: prereq is gone (finished) and done is now true - the
	// resolver must NOT recreate it, or "dependent" would spin forever.
	got2 := mustPop(t, q)
	if got2.Key() != "dependent" {
		t.Fatalf("expected dependent to dispatch now that its prereq is done, got %q", got2.Key())
	}
	if created != 1 {
		t.Fatalf("expected no additional prereq creation once done - got %d total creations", created)
	}
}

// TestArbitraryPoolKeyNeedsNoRegistration proves a Task can simply report
// a brand new PoolKey the queue has never seen before and have it work,
// with no prior registration step at all (default capacity 1, no hooks) -
// "tasks should be able to just define a pool key".
func TestArbitraryPoolKeyNeedsNoRegistration(t *testing.T) {
	q := NewQueue(nil)
	q.Push(newTask("only", PoolKey("brand-new-pool-nobody-registered"), 0))
	got := mustPop(t, q)
	if got.Key() != "only" {
		t.Fatalf("expected the task from the unregistered pool to dispatch, got %q", got.Key())
	}
}

// TestSameTierLessTiebreak proves rule #3: once tier, promotion, and the
// has-dependents check are all tied, Task.Less (a type-specific
// comparator - here a plain "order" field, standing in for something like
// series/chapter position) decides.
func TestSameTierLessTiebreak(t *testing.T) {
	q := NewQueue(nil)
	q.RegisterPool("pool", PoolConfig{Capacity: 1})
	second := &fakeTask{key: "second", pool: "pool", tier: 0, order: 2}
	first := &fakeTask{key: "first", pool: "pool", tier: 0, order: 1}
	// Pushed out of "order" value order, to prove Less (not insertion
	// order) is what actually decides here.
	q.Push(second)
	q.Push(first)

	got := mustPop(t, q)
	if got.Key() != "first" {
		t.Fatalf("expected Less's own ordering (order field) to win, got %q", got.Key())
	}
}

// TestLessNeverCompapresAcrossPools proves Task.Less is only ever invoked
// between two tasks sharing a Pool - a Less implementation that panics on
// an unexpected concrete type would otherwise blow up the moment two
// different pools' tasks tied on tier.
func TestLessNeverComparesAcrossPools(t *testing.T) {
	type otherTask struct{ *fakeTask }
	panicky := &fakeTask{key: "a", pool: "pool-a", tier: 0}
	other := &otherTask{&fakeTask{key: "b", pool: "pool-b", tier: 0}}

	q := NewQueue(nil)
	q.Push(panicky)
	q.Push(Task(other))

	// Whichever dispatches first, this must not panic - Less would only
	// ever be reached if both compared tasks shared a pool, which they
	// don't. Finish the first before popping the second - only one pool
	// may be active at a time (see TestOnePoolAtATime).
	first := mustPop(t, q)
	q.Finish(first.Key())
	_ = mustPop(t, q)
}

// TestFinishNoopForUnknownKey proves Finish is safe to call for a key
// that was never dispatched (or already finished) - no panic, no effect.
func TestFinishNoopForUnknownKey(t *testing.T) {
	q := NewQueue(nil)
	q.Finish("never-existed")
}

// TestCancelRemovesQueuedTask proves Cancel removes a still-queued task
// so it never dispatches, without disturbing anything else.
func TestCancelRemovesQueuedTask(t *testing.T) {
	q := NewQueue(nil)
	q.Push(newTask("keep", "pool", 0))
	q.Push(newTask("drop", "pool", 0))

	cancelled, ok := q.Cancel(func(t Task) bool { return t.Key() == "drop" })
	if !ok || cancelled.Key() != "drop" {
		t.Fatalf("expected to cancel 'drop', got %v ok=%v", cancelled, ok)
	}
	if q.Len() != 1 {
		t.Fatalf("expected exactly one task left queued, got %d", q.Len())
	}
	got := mustPop(t, q)
	if got.Key() != "keep" {
		t.Fatalf("expected 'keep' to still be dispatchable, got %q", got.Key())
	}
}

// TestSnapshotReflectsRealPromotion is the regression test for a real
// live bug: Snapshot must report a blocked task's own *promoted* tier,
// not its raw one - a dashboard watching this should see a blocker's
// priority actually rise the moment something urgent starts depending on
// it, not just once it's dispatched.
func TestSnapshotReflectsRealPromotion(t *testing.T) {
	blocker := newTask("blocker", "pool", 10)     // background, on paper
	dependent := newTask("dependent", "pool", -1) // urgent

	resolver := func(lq *LockedQueue, t Task) []Task {
		if t.Key() == "dependent" {
			return []Task{blocker}
		}
		return nil
	}
	q := NewQueue(resolver)
	q.Push(blocker)
	q.Push(dependent)

	queued, _ := q.Snapshot()
	if len(queued) != 2 {
		t.Fatalf("expected 2 queued entries, got %d", len(queued))
	}
	if queued[0].Task.Key() != "blocker" || queued[0].Tier != -1 {
		t.Fatalf("expected the blocker to sort first, reporting its promoted tier (-1), got %+v", queued[0])
	}

	// Once actually dispatched, it must keep reporting that same promoted
	// tier in the in-flight half - not drop back to its own raw tier
	// (10) purely because it moved from queued to in-flight. Confirmed
	// live: without this, a dashboard watching this transition sees a
	// blocked task's priority appear to *downgrade* the instant it
	// starts running, which is backwards - it dispatched now precisely
	// because it was promoted.
	dispatched, ok := mustPop(t, q).(*fakeTask)
	if !ok || dispatched.key != "blocker" {
		t.Fatalf("expected to dispatch the blocker, got %v", dispatched)
	}
	_, inFlight := q.Snapshot()
	if len(inFlight) != 1 || inFlight[0].Task.Key() != "blocker" || inFlight[0].Tier != -1 {
		t.Fatalf("expected the in-flight blocker to still report its dispatch-time tier (-1), got %+v", inFlight)
	}
}

// TestSnapshotNeverCreatesDependencyTasks proves the other half of that
// same fix: Snapshot's own promotion pass must never actually bring a new
// dependency task into existence, even though it runs the very same
// Resolver a real Pop would - a caller reading the queue to display it
// should never, by that read alone, cause new work to be scheduled.
func TestSnapshotNeverCreatesDependencyTasks(t *testing.T) {
	created := 0
	resolver := func(lq *LockedQueue, t Task) []Task {
		if t.Key() != "dependent" {
			return nil
		}
		if existing, ok := lq.Find(func(c Task) bool { return c.Key() == "prereq" }); ok {
			return []Task{existing}
		}
		created++
		prereq := newTask("prereq", "pool", 0)
		lq.Push(prereq) // no-op here - Snapshot's own LockedQueue is read-only
		return []Task{prereq}
	}
	q := NewQueue(resolver)
	q.Push(newTask("dependent", "pool", 0))

	queued, _ := q.Snapshot()
	if created != 1 {
		t.Fatalf("expected the resolver to have run once, got %d", created)
	}
	if q.Len() != 1 {
		t.Fatalf("expected Snapshot's own read-only pass to create nothing, queue still has %d tasks", q.Len())
	}
	if len(queued) != 1 || queued[0].Task.Key() != "dependent" {
		t.Fatalf("expected only 'dependent' in the snapshot, got %+v", queued)
	}

	// A real Pop, by contrast, still creates it for real.
	got := mustPop(t, q)
	if got.Key() != "prereq" {
		t.Fatalf("expected Pop to actually create and dispatch the prereq, got %q", got.Key())
	}
}

// TestAnyEligibleQueuedIgnoresBlockedMatches is the regression test for a
// real live bug: a task that's blocked on some other, already-in-flight
// task asking "is there higher-priority work waiting" must never count
// as a reason for that in-flight task to pause - it can't actually be
// dispatched either way, so pausing for it accomplishes nothing and just
// churns (pause, requeue, immediately redispatch, forever). The blocker
// here is deliberately already in flight, not merely queued: a queued
// blocker would (correctly, since priority promotion is a real,
// permanent mutation now - see Task.Promote's own doc comment) itself
// get promoted to the dependent's own urgency and become genuinely
// eligible work in its own right, which is the right outcome, not the
// case this test is isolating.
func TestAnyEligibleQueuedIgnoresBlockedMatches(t *testing.T) {
	blocker := newTask("blocker", "pool-a", 5)      // itself merely background-tier
	dependent := newTask("dependent", "pool-b", -1) // urgent, but blocked

	resolver := func(lq *LockedQueue, t Task) []Task {
		if t.Key() != "dependent" {
			return nil
		}
		if existing, ok := lq.Find(func(c Task) bool { return c.Key() == "blocker" }); ok {
			return []Task{existing}
		}
		return nil // blocker already finished - nothing left to depend on
	}
	q := NewQueue(resolver)
	q.Push(blocker)
	if _, ok := q.Pop(); !ok {
		t.Fatalf("expected to dispatch the blocker")
	}
	q.Push(dependent)

	urgent := func(t Task) bool { return t.Tier() < 0 }
	if q.AnyEligibleQueued(urgent) {
		t.Fatalf("expected the blocked urgent task not to count as eligible work while its blocker is still in flight")
	}
	// AnyQueued, by contrast, doesn't know or care about blocking - it's
	// the plain check AnyEligibleQueued replaced for this exact purpose.
	if !q.AnyQueued(urgent) {
		t.Fatalf("expected AnyQueued to still report the urgent task by raw tier alone")
	}

	// Once the blocker actually finishes, the same urgent task becomes
	// genuinely eligible.
	q.Finish(blocker.Key())
	if !q.AnyEligibleQueued(urgent) {
		t.Fatalf("expected the now-unblocked urgent task to count as eligible work")
	}
}
