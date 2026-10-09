package sched

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// fake is a task whose Run blocks until the test releases it.
type fake struct {
	key      string
	ctx      string
	pos      int
	capacity int // max running tasks sharing ctx, 0 = unlimited
	prepare  func() ([]Need, error)

	mu       sync.Mutex
	release  chan error
	runs     int
	ended    bool
	endErr   error
	attempts []int
}

func newFake(key, ctx string) *fake { return &fake{key: key, ctx: ctx, release: make(chan error)} }

func (f *fake) Key() string             { return f.key }
func (f *fake) Context() string         { return f.ctx }
func (f *fake) Less(o Task, _ int) bool { return f.pos < o.(*fake).pos }
func (f *fake) CanStart(running []Task) bool {
	if f.capacity == 0 {
		return true
	}
	n := 0
	for _, r := range running {
		if r.Context() == f.ctx {
			n++
		}
	}
	return n < f.capacity
}
func (f *fake) Prepare() ([]Need, error) {
	if f.prepare == nil {
		return nil, nil
	}
	return f.prepare()
}
func (f *fake) Run(ctx context.Context, attempt int) error {
	f.mu.Lock()
	f.runs++
	f.attempts = append(f.attempts, attempt)
	f.mu.Unlock()
	select {
	case err := <-f.release:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (f *fake) Ended(err error) {
	f.mu.Lock()
	f.ended, f.endErr = true, err
	f.mu.Unlock()
}

// result waits for Ended, which the scheduler delivers outside its lock.
func (f *fake) result() (bool, error) {
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		ended, err := f.ended, f.endErr
		f.mu.Unlock()
		if ended || time.Now().After(deadline) {
			return ended, err
		}
		time.Sleep(time.Millisecond)
	}
}

func need(f *fake) Need { return Need{Key: f.key, New: func() Task { return f }} }

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func state(s *Scheduler, f *fake) string {
	if _, st, ok := s.Get(f.key); ok {
		return st.String()
	}
	return "gone"
}

func wantState(t *testing.T, s *Scheduler, f *fake, want string) {
	t.Helper()
	eventually(t, f.key+" to be "+want, func() bool { return state(s, f) == want })
}

// finish lets f's current run return err and waits for the scheduler to
// have handled it.
func finish(t *testing.T, s *Scheduler, f *fake, err error) {
	t.Helper()
	f.mu.Lock()
	runs := f.runs
	f.mu.Unlock()
	f.release <- err
	eventually(t, f.key+" to leave its run", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.ended || f.runs > runs {
			return true
		}
		_, st, ok := s.Get(f.key)
		return !ok || st != Running
	})
}

func newSched(t *testing.T, cfg Config) *Scheduler {
	t.Helper()
	s := New(context.Background(), cfg)
	t.Cleanup(s.Close)
	return s
}

func priority(t *testing.T, s *Scheduler, f *fake) int {
	t.Helper()
	p, ok := s.Priority(f.key)
	if !ok {
		t.Fatalf("%s is not in the graph", f.key)
	}
	return p
}

func TestPriorityIsDerivedAndDrops(t *testing.T) {
	s := newSched(t, Config{})
	hold := newFake("hold", "gpu")
	hold.capacity = 1
	dep := newFake("dep", "gpu")
	dep.capacity = 1
	a := newFake("a", "gpu")
	a.prepare = func() ([]Need, error) { return []Need{need(dep)}, nil }

	s.Request("x", 0, hold) // occupies the context so nothing else starts
	s.Request("bulk", 5, dep)
	s.Request("reader", 1, a)

	if got := priority(t, s, dep); got != 1 {
		t.Fatalf("dep priority = %d, want its dependent's 1", got)
	}
	s.Withdraw("reader", "a")
	if state(s, a) != "gone" {
		t.Fatalf("a should be cancelled once nobody wants it")
	}
	if got := priority(t, s, dep); got != 5 {
		t.Fatalf("dep priority = %d, want to drop back to its own 5", got)
	}
	if ended, err := a.result(); !ended || !errors.Is(err, ErrCanceled) {
		t.Fatalf("a ended=%v err=%v, want ErrCanceled", ended, err)
	}
}

func TestCancelCascadesOnlyToUnwanted(t *testing.T) {
	s := newSched(t, Config{})
	hold := newFake("hold", "gpu")
	hold.capacity = 1
	shared, only, own := newFake("shared", "gpu"), newFake("only", "gpu"), newFake("own", "gpu")
	for _, f := range []*fake{shared, only, own} {
		f.capacity = 1
	}
	a, b := newFake("a", "gpu"), newFake("b", "gpu")
	a.prepare = func() ([]Need, error) { return []Need{need(shared), need(only), need(own)}, nil }
	b.prepare = func() ([]Need, error) { return []Need{need(shared)}, nil }

	s.Request("x", 0, hold)
	s.Request("x", 5, own) // individually queued
	s.Request("x", 5, a)
	s.Request("x", 5, b)

	s.Cancel("a")
	for f, want := range map[*fake]string{a: "gone", only: "gone", shared: "ready", own: "ready", b: "waiting"} {
		if got := state(s, f); got != want {
			t.Errorf("%s is %s, want %s", f.key, got, want)
		}
	}
}

func TestDependencyFailureFailsDependentsButNotGroups(t *testing.T) {
	s := newSched(t, Config{MaxAttempts: 1})
	bad, good := newFake("bad", ""), newFake("good", "")
	dependent := newFake("dependent", "")
	dependent.prepare = func() ([]Need, error) { return []Need{need(bad)}, nil }
	group := newFake("group", "")
	group.prepare = func() ([]Need, error) {
		return []Need{{Key: "bad", Member: true}, {Key: "good", New: func() Task { return good }, Member: true}}, nil
	}
	s.Request("x", 1, dependent)
	s.Request("x", 1, group)

	boom := errors.New("boom")
	finish(t, s, bad, boom)
	var depErr *DependencyError
	if ended, err := dependent.result(); !ended || !errors.As(err, &depErr) || depErr.Key != "bad" || !errors.Is(err, boom) {
		t.Fatalf("dependent ended=%v err=%v, want a DependencyError on bad", ended, err)
	}
	wantState(t, s, group, "waiting")
	finish(t, s, good, nil)
	wantState(t, s, group, "running")
	finish(t, s, group, nil)
	if ended, err := group.result(); !ended || err != nil {
		t.Fatalf("group ended=%v err=%v, want a clean finish", ended, err)
	}
}

func TestPrepareRunsAgainOnceNeedsEnd(t *testing.T) {
	s := newSched(t, Config{})
	phase, m1, m2 := newFake("phase", ""), newFake("m1", ""), newFake("m2", "")
	phaseDone := false
	group := newFake("group", "")
	group.prepare = func() ([]Need, error) {
		if !phaseDone {
			return []Need{need(phase)}, nil
		}
		// Everything ever asked for is named again; ended needs are ignored.
		return []Need{need(phase), {Key: "m1", New: func() Task { return m1 }, Member: true}, {Key: "m2", New: func() Task { return m2 }, Member: true}}, nil
	}
	s.Request("x", 1, group)
	if state(s, m1) != "gone" {
		t.Fatalf("members must not exist before the phase ends")
	}
	phaseDone = true
	finish(t, s, phase, nil)
	wantState(t, s, m1, "running")
	wantState(t, s, m2, "running")
	finish(t, s, m1, nil)
	finish(t, s, m2, errors.New("member failed"))
	finish(t, s, m2, errors.New("member failed"))
	finish(t, s, m2, errors.New("member failed"))
	wantState(t, s, group, "running")
}

func TestRetriesKeepDependentsAndYieldIsFree(t *testing.T) {
	s := newSched(t, Config{})
	dep := newFake("dep", "")
	top := newFake("top", "")
	top.prepare = func() ([]Need, error) { return []Need{need(dep)}, nil }
	s.Request("x", 1, top)

	finish(t, s, dep, ErrYield)
	finish(t, s, dep, errors.New("one"))
	finish(t, s, dep, errors.New("two"))
	wantState(t, s, top, "waiting")
	finish(t, s, dep, nil)
	wantState(t, s, top, "running")
	if !slices.Equal(dep.attempts, []int{0, 0, 1, 2}) {
		t.Fatalf("attempts = %v, want [0 0 1 2]", dep.attempts)
	}
}

func TestTieStaysUntilEmptyThenSwitches(t *testing.T) {
	var mu sync.Mutex
	var switches []string
	s := newSched(t, Config{OnSwitch: func(from, to string) {
		mu.Lock()
		switches = append(switches, from+">"+to)
		mu.Unlock()
	}})
	t1, t2 := newFake("t1", "tts"), newFake("t2", "tts")
	l1 := newFake("l1", "llm")
	for i, f := range []*fake{t1, l1, t2} {
		f.capacity, f.pos = 1, i
	}
	s.Request("x", 2, t1)
	s.Request("x", 2, l1) // sorts before t2 by position, but it's the other context
	s.Request("x", 2, t2)

	wantState(t, s, t1, "running")
	finish(t, s, t1, nil)
	wantState(t, s, t2, "running")
	if state(s, l1) != "ready" {
		t.Fatalf("l1 is %s; a tie must not take the context", state(s, l1))
	}
	finish(t, s, t2, nil)
	wantState(t, s, l1, "running")
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(switches, []string{">tts", "tts>llm"}) {
		t.Fatalf("switches = %v", switches)
	}
}

func TestMoreUrgentDrainsThenSwitches(t *testing.T) {
	s := newSched(t, Config{})
	t1, t2 := newFake("t1", "tts"), newFake("t2", "tts")
	t1.capacity, t2.capacity = 1, 1
	l1 := newFake("l1", "llm")
	free := newFake("free", "")
	s.Request("x", 3, t1)
	s.Request("x", 3, t2)
	wantState(t, s, t1, "running")

	s.Request("x", 1, l1)
	s.Request("x", 3, free)
	wantState(t, s, free, "running") // needs no context: unaffected
	if !s.ShouldYield("t1") {
		t.Fatalf("t1 should be asked to yield to the more urgent l1")
	}
	finish(t, s, t1, ErrYield)
	wantState(t, s, l1, "running")
	if state(s, t1) != "ready" || state(s, t2) != "ready" {
		t.Fatalf("tts work should wait: t1 %s, t2 %s", state(s, t1), state(s, t2))
	}
	finish(t, s, l1, nil)
	wantState(t, s, t1, "running")
}

func TestDwellHoldsAllButExemptWork(t *testing.T) {
	now := time.Unix(0, 0)
	s := newSched(t, Config{Dwell: time.Minute, DwellExempt: 0, Now: func() time.Time { return now }})
	bg1, bg2 := newFake("bg1", "tts"), newFake("bg2", "tts")
	bg1.capacity, bg2.capacity = 1, 1
	look := newFake("look", "llm")
	live := newFake("live", "llm")

	s.Request("x", 3, bg1)
	s.Request("x", 3, bg2)
	s.Request("x", 1, look)
	finish(t, s, bg1, nil)
	wantState(t, s, bg2, "running") // dwell: lookahead may not take the context yet
	if s.ShouldYield("bg2") {
		t.Fatalf("no yield while the dwell holds the context")
	}

	now = now.Add(2 * time.Minute)
	s.Kick()
	if !s.ShouldYield("bg2") {
		t.Fatalf("after the dwell, bg2 should yield to lookahead work")
	}
	finish(t, s, bg2, ErrYield)
	wantState(t, s, look, "running")

	// Live work ignores the dwell.
	bg3 := newFake("bg3", "tts")
	s.Request("x", 0, bg3)
	s.Request("x", 0, live)
	finish(t, s, look, nil)
	wantState(t, s, live, "running") // tie at 0 stays in llm
	finish(t, s, live, nil)
	wantState(t, s, bg3, "running") // llm ran out of the class it switched for
}

func TestSnapshotOrderIsDispatchOrder(t *testing.T) {
	s := newSched(t, Config{})
	s.SetGate(func(Task) bool { return false })
	var all []*fake
	for _, spec := range []struct {
		key  string
		ctx  string
		prio int
	}{{"bg-llm", "llm", 3}, {"bg-tts-2", "tts", 3}, {"look-llm", "llm", 1}, {"bg-tts-1", "tts", 3}, {"live-tts", "tts", 0}} {
		f := newFake(spec.key, spec.ctx)
		f.capacity = 1
		f.pos = map[string]int{"bg-tts-1": 1, "bg-tts-2": 2}[spec.key]
		all = append(all, f)
		s.Request("x", spec.prio, f)
	}
	var listed []string
	for _, info := range s.Snapshot() {
		listed = append(listed, info.Task.Key())
	}

	s.SetGate(nil)
	var ran []string
	for range all {
		var running *fake
		eventually(t, "a task to run", func() bool {
			for _, f := range all {
				if state(s, f) == "running" {
					running = f
					return true
				}
			}
			return false
		})
		ran = append(ran, running.key)
		finish(t, s, running, nil)
	}
	// live-tts makes tts current, so within a class tts sorts first from
	// then on; before anything ran there was no current context.
	want := []string{"live-tts", "look-llm", "bg-llm", "bg-tts-1", "bg-tts-2"}
	if !slices.Equal(ran, want) {
		t.Fatalf("ran %v, want %v (listed %v)", ran, want, listed)
	}
	if listed[0] != "live-tts" || listed[1] != "look-llm" {
		t.Fatalf("listed %v, want the same class order", listed)
	}
}

func TestBumpAndUnbump(t *testing.T) {
	s := newSched(t, Config{})
	s.SetGate(func(Task) bool { return false })
	f := newFake("f", "")
	s.Request("bulk", 3, f)
	s.Bump("f", 1)
	if got := priority(t, s, f); got != 1 {
		t.Fatalf("bumped priority = %d, want 1", got)
	}
	s.Unbump("f")
	if got := priority(t, s, f); got != 3 {
		t.Fatalf("priority after unbump = %d, want 3", got)
	}
}

func TestReplaceWithdrawsWhatSourceNoLongerWants(t *testing.T) {
	s := newSched(t, Config{})
	s.SetGate(func(Task) bool { return false })
	p1, p2, p3 := newFake("p1", "tts"), newFake("p2", "tts"), newFake("p3", "tts")
	s.Request("bulk", 3, p2)
	s.Replace("reader", []Wanted{{1, p1}, {1, p2}})
	s.Replace("reader", []Wanted{{1, p3}})
	if state(s, p1) != "gone" || state(s, p3) != "ready" {
		t.Fatalf("p1 %s, p3 %s", state(s, p1), state(s, p3))
	}
	if got := priority(t, s, p2); got != 3 {
		t.Fatalf("p2 priority = %d, want the bulk request's 3", got)
	}
}

func TestCancelRunningEndsWhenRunReturns(t *testing.T) {
	s := newSched(t, Config{})
	f := newFake("f", "")
	s.Request("x", 1, f)
	wantState(t, s, f, "running")
	s.Cancel("f")
	wantState(t, s, f, "gone")
	if ended, err := f.result(); !ended || !errors.Is(err, ErrCanceled) {
		t.Fatalf("ended=%v err=%v", ended, err)
	}
}

func TestMutualNeedFails(t *testing.T) {
	s := newSched(t, Config{})
	a, b := newFake("a", ""), newFake("b", "")
	a.prepare = func() ([]Need, error) { return []Need{need(b)}, nil }
	b.prepare = func() ([]Need, error) { return []Need{need(a)}, nil }
	s.Request("x", 1, a)
	if state(s, a) != "gone" || state(s, b) != "gone" {
		t.Fatalf("a %s, b %s; a cycle must fail, not hang", state(s, a), state(s, b))
	}
}

func TestAdoptedTasksFollowAndEndWithTheirParent(t *testing.T) {
	s := newSched(t, Config{})
	hold := newFake("hold", "gpu")
	hold.capacity = 1
	s.Request("x", 0, hold)

	group := newFake("group", "")
	s.Request("bulk", 3, group)
	wantState(t, s, group, "running")
	child, kept := newFake("child", "gpu"), newFake("kept", "gpu")
	child.capacity, kept.capacity = 1, 1
	if _, ok := s.Adopt("group", child); !ok {
		t.Fatalf("a running task should be able to adopt")
	}
	s.Adopt("group", kept)
	s.Request("person", 3, kept)

	for _, info := range s.Snapshot() {
		if want := map[string]int{"group": 2}[info.Task.Key()]; len(info.Members) != want {
			t.Fatalf("%s reports %d members, want %d", info.Task.Key(), len(info.Members), want)
		}
	}

	s.Bump("group", 1)
	if got := priority(t, s, child); got != 1 {
		t.Fatalf("child priority = %d, want the bumped group's 1", got)
	}
	s.Cancel("group")
	wantState(t, s, child, "gone")
	if state(s, kept) != "ready" {
		t.Fatalf("kept is %s; a task someone else asked for must survive", state(s, kept))
	}
}

func TestJoinedTaskFollowsItsParentAndSurvivesItsCancel(t *testing.T) {
	s := newSched(t, Config{})
	hold := newFake("hold", "gpu")
	hold.capacity = 1
	s.Request("x", 0, hold)

	group := newFake("group", "")
	s.Request("bulk", 3, group)
	wantState(t, s, group, "running")
	child := newFake("child", "gpu")
	child.capacity = 1
	s.Request("direct", 3, child)
	if s.Join("group", "missing") {
		t.Fatalf("joined a task that isn't there")
	}
	if !s.Join("group", "child") {
		t.Fatalf("a running task should be able to take a queued one as a member")
	}

	s.Bump("group", 1)
	if got := priority(t, s, child); got != 1 {
		t.Fatalf("child priority = %d, want the bumped group's 1", got)
	}
	s.Cancel("group")
	if state(s, child) != "ready" {
		t.Fatalf("child is %s; its own request must keep it", state(s, child))
	}
}

func TestManualPopAndFinish(t *testing.T) {
	s := newSched(t, Config{Manual: true})
	dep, top := newFake("dep", "a"), newFake("top", "b")
	top.prepare = func() ([]Need, error) { return []Need{need(dep)}, nil }
	s.Request("x", 1, top)
	got, ok := s.Pop()
	if !ok || got.Key() != "dep" {
		t.Fatalf("popped %v, want dep", got)
	}
	if _, ok := s.Pop(); ok {
		t.Fatalf("top must wait for dep")
	}
	s.Finish("dep", nil)
	if got, ok := s.Pop(); !ok || got.Key() != "top" {
		t.Fatalf("popped %v, want top", got)
	}
}

func TestRetainWithdrawsTheRest(t *testing.T) {
	s := newSched(t, Config{Manual: true})
	a, b := newFake("a", ""), newFake("b", "")
	s.Request("reader", 1, a)
	s.Request("reader", 1, b)
	s.Request("bulk", 3, b)
	s.Retain("reader", func(string) bool { return false })
	if state(s, a) != "gone" || priority(t, s, b) != 3 {
		t.Fatalf("a %s, b priority %d; want a gone and b back at 3", state(s, a), priority(t, s, b))
	}
}
