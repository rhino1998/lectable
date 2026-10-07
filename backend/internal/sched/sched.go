// Package sched is the job scheduler: one graph of tasks, each wanted by
// named requesters and by the tasks that need it, dispatched in one total
// order under one admission rule. See docs/scheduler-design.md for the
// rules and why they are what they are.
//
// A task's effective priority is derived from who wants it (never stored),
// a task nobody wants any more is cancelled, and a task is evaluated
// (Task.Prepare) when it is added and again each time everything it was
// waiting for has ended - never polled in between.
package sched

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// NoPriority is the effective priority of a task nothing wants (only seen
// while it winds down).
const NoPriority = int(^uint(0) >> 1)

// BumpSource is the request source Bump and Unbump use.
const BumpSource = "bump"

var (
	// ErrYield, returned from Task.Run, sends the task back through Prepare
	// without counting an attempt: it stopped early to let something more
	// urgent run (see Scheduler.ShouldYield) and keeps its own progress.
	ErrYield = errors.New("sched: task yielded")
	// ErrCanceled is how a task ends when it is cancelled by hand, or when
	// nothing wants it any more.
	ErrCanceled = errors.New("sched: task canceled")
)

// DependencyError is how a task ends when a dependency failed or was
// cancelled.
type DependencyError struct {
	Key string // the dependency
	Err error
}

func (e *DependencyError) Error() string { return fmt.Sprintf("dependency %s: %v", e.Key, e.Err) }
func (e *DependencyError) Unwrap() error { return e.Err }

// Need is one task another task waits for.
type Need struct {
	Key string
	// New builds the task when Key isn't in the graph. nil means "only if
	// it is already there".
	New func() Task
	// Member marks a group's member rather than a dependency: its failure
	// doesn't fail the task that asked for it.
	Member bool
}

// Task is one unit of work. Every method except Run is called with the
// scheduler's lock held: keep them cheap, and never call back into the
// Scheduler from them.
type Task interface {
	// Key identifies the task; one task per key is in the graph at a time.
	Key() string
	// Context names what must be resident for the task to run (a model),
	// or "" for none. Only one context has running tasks at a time.
	Context() string
	// CanStart reports whether the task may start beside running, which are
	// all in its own context or need none.
	CanStart(running []Task) bool
	// Less orders the task before other when both have effective priority
	// priority.
	Less(other Task, priority int) bool
	// Prepare reports what the task still needs before it can run. Called
	// when the task is added and again each time its outstanding needs have
	// all ended; a need that already ended for this task is ignored. An
	// error fails the task.
	Prepare() ([]Need, error)
	// Run does the work. attempt starts at 0.
	Run(ctx context.Context, attempt int) error
}

// Retryer lets a task decide which failures are worth another attempt.
// Without it every failure but a cancelled context is.
type Retryer interface{ Retryable(err error) bool }

// Ender is told, once and outside the scheduler's lock, how the task ended:
// nil, its last error, ErrCanceled, or a *DependencyError.
type Ender interface{ Ended(err error) }

// Merger is given a second task requested under its own key, to take over
// whatever of it is newer. running reports whether Run is under way.
type Merger interface {
	Merge(newer Task, running bool)
}

// State is where a task is.
type State int

const (
	Waiting State = iota // has needs that haven't ended
	Ready                // could run
	Running
)

func (s State) String() string {
	return [...]string{"waiting", "ready", "running"}[s]
}

// Info is one task as Snapshot reports it.
type Info struct {
	Task     Task
	State    State
	Priority int // effective
	Attempt  int
	Bumped   bool
}

// Config tunes a Scheduler. The zero value works.
type Config struct {
	// MaxAttempts is how many times a failing task runs. Default 3.
	MaxAttempts int
	// Dwell is how long after a context switch only work at DwellExempt or
	// more urgent may force another. 0 disables it.
	Dwell time.Duration
	// DwellExempt is the least urgent priority that ignores Dwell.
	DwellExempt int
	// OnSwitch is called, outside the lock, when the current context
	// changes.
	OnSwitch func(from, to string)
	// OnChange is called, outside the lock, after anything Snapshot reports
	// may have changed.
	OnChange func()
	// Manual stops the scheduler from running tasks itself: the caller
	// takes each task to start from Pop and reports it with Finish. For
	// tests that step dispatch by hand.
	Manual bool
	// Now replaces time.Now, for tests.
	Now func() time.Time
}

type node struct {
	task     Task
	seq      int64
	state    State
	requests map[string]int
	wantedBy map[*node]bool // value: wanted as a member
	needs    map[*node]bool // unfinished; value: member
	adopted  map[*node]bool // asked for while running - see Adopt
	ended    map[string]bool
	attempt  int
	cancel   context.CancelFunc
	canceled bool // asked to stop while running
	doomed   bool // picked by a CancelWhere under way
	gone     bool

	prio    int
	prioGen uint64
}

// Scheduler runs tasks. Create one with New.
type Scheduler struct {
	cfg Config

	mu    sync.Mutex
	nodes map[string]*node
	seq   int64
	gen   uint64
	gate  func(Task) bool

	cur        string // the resident context
	since      time.Time
	switchPrio int
	timer      *time.Timer

	callbacks  []func()
	closed     bool
	baseCtx    context.Context
	baseCancel context.CancelFunc
}

// New returns an empty Scheduler; tasks run under ctx.
func New(ctx context.Context, cfg Config) *Scheduler {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	base, cancel := context.WithCancel(ctx)
	return &Scheduler{cfg: cfg, nodes: map[string]*node{}, baseCtx: base, baseCancel: cancel}
}

// Close cancels every running task and stops admitting new ones.
func (s *Scheduler) Close() {
	s.mu.Lock()
	s.closed = true
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()
	s.baseCancel()
}

// do runs fn under the lock, re-runs admission, and then delivers whatever
// callbacks that produced.
func (s *Scheduler) do(fn func()) {
	s.mu.Lock()
	fn()
	s.admit()
	cbs := s.callbacks
	s.callbacks = nil
	onChange := s.cfg.OnChange
	s.mu.Unlock()
	for _, cb := range cbs {
		cb()
	}
	if onChange != nil {
		onChange()
	}
}

// Request asks for t on behalf of source at priority (lower is more
// urgent), adding it to the graph if its key isn't there. It reports
// whether the task was added; if not, the existing task gains the request
// (a source asking twice keeps its more urgent priority) and, when it
// implements Merger, is offered t.
func (s *Scheduler) Request(source string, priority int, t Task) (added bool) {
	s.do(func() { added = s.request(source, priority, t) })
	return added
}

func (s *Scheduler) request(source string, priority int, t Task) bool {
	if n, ok := s.nodes[t.Key()]; ok {
		if old, has := n.requests[source]; !has || priority < old {
			n.requests[source] = priority
		}
		s.merge(n, t)
		return false
	}
	n := s.add(t)
	n.requests[source] = priority
	s.prepare(n)
	return true
}

func (s *Scheduler) merge(n *node, t Task) {
	if m, ok := n.task.(Merger); ok && n.task != t {
		m.Merge(t, n.state == Running)
	}
}

// Adopt asks for t on behalf of parent's running task, as a member: t takes
// on the parent's priority while the parent runs, and is cancelled with it
// unless something else wants it. ok is false, and nothing happens, when
// parent isn't a running task.
func (s *Scheduler) Adopt(parent string, t Task) (added, ok bool) {
	s.do(func() {
		p, found := s.nodes[parent]
		if !found || p.state != Running || p.canceled {
			return
		}
		ok = true
		n, exists := s.nodes[t.Key()]
		if !exists {
			n = s.add(t)
			added = true
		} else {
			s.merge(n, t)
		}
		n.wantedBy[p] = true
		if p.adopted == nil {
			p.adopted = map[*node]bool{}
		}
		p.adopted[n] = true
		if added {
			s.prepare(n)
		}
	})
	return added, ok
}

// Join makes the task already at key a member of parent's running task, as
// Adopt would a new one - for a task the parent asked for by other means.
// Whatever else wants it still does. ok is false, and nothing happens,
// when parent isn't a running task or key names none.
func (s *Scheduler) Join(parent, key string) (ok bool) {
	s.do(func() {
		p, found := s.nodes[parent]
		n, exists := s.nodes[key]
		if !found || !exists || p == n || p.state != Running || p.canceled {
			return
		}
		ok = true
		n.wantedBy[p] = true
		if p.adopted == nil {
			p.adopted = map[*node]bool{}
		}
		p.adopted[n] = true
	})
	return ok
}

// Wanted is one task a source asks for.
type Wanted struct {
	Priority int
	Task     Task
}

// Replace makes wanted the whole set of tasks source asks for: new ones
// are requested, and its requests for anything else are withdrawn.
func (s *Scheduler) Replace(source string, wanted []Wanted) {
	s.do(func() {
		keep := make(map[string]bool, len(wanted))
		for _, w := range wanted {
			keep[w.Task.Key()] = true
			if !s.request(source, w.Priority, w.Task) {
				s.nodes[w.Task.Key()].requests[source] = w.Priority
			}
		}
		for _, n := range s.snapshotNodes() {
			if _, has := n.requests[source]; has && !keep[n.task.Key()] {
				s.withdraw(n, source)
			}
		}
	})
}

// Retain withdraws source's requests for every task keep rejects.
func (s *Scheduler) Retain(source string, keep func(key string) bool) {
	s.do(func() {
		for _, n := range s.snapshotNodes() {
			if _, has := n.requests[source]; has && !n.gone && !keep(n.task.Key()) {
				s.withdraw(n, source)
			}
		}
	})
}

// Withdraw removes source's request for key, reporting whether there was
// one. A task left with no request and nothing needing it is cancelled.
func (s *Scheduler) Withdraw(source, key string) (found bool) {
	s.do(func() {
		n, ok := s.nodes[key]
		if !ok {
			return
		}
		if _, found = n.requests[source]; found {
			s.withdraw(n, source)
		}
	})
	return found
}

func (s *Scheduler) withdraw(n *node, source string) {
	delete(n.requests, source)
	s.dropIfUnwanted(n)
}

// Raise adds source's request for the task already at key, at priority,
// replacing any it had. It reports whether key names a task.
func (s *Scheduler) Raise(source, key string, priority int) (found bool) {
	s.do(func() {
		if n, ok := s.nodes[key]; ok {
			n.requests[source] = priority
			found = true
		}
	})
	return found
}

// Bump adds a personal request for key at priority; Unbump removes it.
// Both report whether key names a task.
func (s *Scheduler) Bump(key string, priority int) bool {
	return s.Raise(BumpSource, key, priority)
}

func (s *Scheduler) Unbump(key string) bool { return s.Withdraw(BumpSource, key) }

// Canceled reports whether key's running task has been asked to stop.
func (s *Scheduler) Canceled(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[key]
	return ok && n.canceled
}

// Cancel ends key's task with ErrCanceled, failing whatever depends on it.
// A running task is asked to stop and ends once Run returns.
func (s *Scheduler) Cancel(key string) (found bool) {
	s.do(func() {
		n, ok := s.nodes[key]
		if ok {
			s.cancelNode(n)
		}
		found = ok
	})
	return found
}

// CancelWhere cancels every task match accepts, reporting how many. Each
// ends with ErrCanceled, including one that depends on another of them.
func (s *Scheduler) CancelWhere(match func(Task) bool) (count int) {
	s.do(func() {
		var doomed []*node
		for _, n := range s.nodes {
			if match(n.task) {
				n.doomed = true
				doomed = append(doomed, n)
			}
		}
		for _, n := range doomed {
			s.cancelNode(n)
		}
		count = len(doomed)
	})
	return count
}

// SetGate limits admission to tasks gate accepts (nil: all). Running tasks
// are unaffected.
func (s *Scheduler) SetGate(gate func(Task) bool) {
	s.do(func() { s.gate = gate })
}

// SetManual turns Manual on or off; turning it off starts whatever may run.
func (s *Scheduler) SetManual(manual bool) {
	s.do(func() { s.cfg.Manual = manual })
}

// Kick re-runs admission - after a clock change in tests, or when
// something CanStart or the gate reads has changed.
func (s *Scheduler) Kick() { s.do(func() {}) }

// Get returns key's task and state.
func (s *Scheduler) Get(key string) (Task, State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[key]
	if !ok {
		return nil, 0, false
	}
	return n.task, n.state, true
}

// Each calls fn on every task, under the lock.
func (s *Scheduler) Each(fn func(Task, State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.nodes {
		fn(n.task, n.state)
	}
}

// Priority reports key's effective priority.
func (s *Scheduler) Priority(key string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[key]
	if !ok {
		return 0, false
	}
	s.gen++
	return s.priority(n), true
}

// Snapshot lists every task in dispatch order: the order admission walks
// the ready ones in.
func (s *Scheduler) Snapshot() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	nodes := s.snapshotNodes()
	s.sortNodes(nodes)
	out := make([]Info, len(nodes))
	for i, n := range nodes {
		_, bumped := n.requests[BumpSource]
		out[i] = Info{Task: n.task, State: n.state, Priority: s.priority(n), Attempt: n.attempt, Bumped: bumped}
	}
	return out
}

// ShouldYield reports whether key's running task should stop at its next
// convenient point and return ErrYield: a strictly more urgent ready task
// is blocked by it, in its own context or by a context switch waiting for
// that context to drain.
func (s *Scheduler) ShouldYield(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[key]
	if !ok || n.state != Running {
		return false
	}
	v := s.evaluate(true)
	p := s.priority(n)
	ctx := n.task.Context()
	if bp, ok := v.blockedIn[ctx]; ok && bp < p {
		return true
	}
	return ctx != "" && v.draining && v.drainPrio < p
}

func (s *Scheduler) snapshotNodes() []*node {
	out := make([]*node, 0, len(s.nodes))
	for _, n := range s.nodes {
		out = append(out, n)
	}
	return out
}

func (s *Scheduler) add(t Task) *node {
	s.seq++
	n := &node{
		task:     t,
		seq:      s.seq,
		requests: map[string]int{},
		wantedBy: map[*node]bool{},
		needs:    map[*node]bool{},
		ended:    map[string]bool{},
	}
	s.nodes[t.Key()] = n
	return n
}

// prepare asks n what it needs and links it up; with nothing outstanding
// n becomes ready.
func (s *Scheduler) prepare(n *node) {
	n.state = Waiting
	needs, err := n.task.Prepare()
	if err != nil {
		s.end(n, err)
		return
	}
	for _, need := range needs {
		if n.gone {
			return
		}
		if n.ended[need.Key] || need.Key == n.task.Key() {
			continue
		}
		d, ok := s.nodes[need.Key]
		fresh := false
		if !ok {
			if need.New == nil {
				continue
			}
			d = s.add(need.New())
			fresh = true
		}
		if _, linked := n.needs[d]; linked {
			continue
		}
		if s.reaches(d, n) {
			s.end(n, fmt.Errorf("sched: %s and %s need each other", n.task.Key(), need.Key))
			return
		}
		n.needs[d] = need.Member
		d.wantedBy[n] = need.Member
		if fresh {
			s.prepare(d) // may end d, and with it n
		}
	}
	if !n.gone && len(n.needs) == 0 {
		n.state = Ready
	}
}

// reaches reports whether from needs to, directly or not.
func (s *Scheduler) reaches(from, to *node) bool {
	if from == to {
		return true
	}
	for d := range from.needs {
		if s.reaches(d, to) {
			return true
		}
	}
	return false
}

func (s *Scheduler) cancelNode(n *node) {
	if n.gone {
		return
	}
	if n.state == Running {
		n.canceled = true
		if n.cancel != nil {
			n.cancel()
		}
		return
	}
	s.end(n, ErrCanceled)
}

func (s *Scheduler) dropIfUnwanted(n *node) {
	if !n.gone && len(n.requests) == 0 && len(n.wantedBy) == 0 {
		s.cancelNode(n)
	}
}

// end removes n from the graph: tells whatever wanted it, and cancels
// whatever only it wanted.
func (s *Scheduler) end(n *node, err error) {
	if n.gone {
		return
	}
	n.gone = true
	delete(s.nodes, n.task.Key())
	if e, ok := n.task.(Ender); ok {
		s.callbacks = append(s.callbacks, func() { e.Ended(err) })
	}

	for _, wanted := range []map[*node]bool{n.needs, n.adopted} {
		for d := range wanted {
			delete(d.wantedBy, n)
			s.dropIfUnwanted(d)
		}
	}
	n.needs, n.adopted = nil, nil

	key := n.task.Key()
	for w, member := range n.wantedBy {
		delete(w.adopted, n)
		if w.gone || w.state != Waiting {
			continue
		}
		delete(w.needs, n)
		w.ended[key] = true
		switch {
		case err != nil && !member && w.doomed:
			s.end(w, ErrCanceled)
		case err != nil && !member:
			s.end(w, &DependencyError{Key: key, Err: err})
		case len(w.needs) == 0:
			s.prepare(w)
		}
	}
	n.wantedBy = nil
}

// Finish reports that key's running task returned err. Only for a Manual
// scheduler.
func (s *Scheduler) Finish(key string, err error) {
	s.mu.Lock()
	n, ok := s.nodes[key]
	s.mu.Unlock()
	if ok && n.state == Running {
		s.finish(n, err)
	}
}

// finish handles Run returning.
func (s *Scheduler) finish(n *node, err error) {
	s.do(func() {
		if n.cancel != nil {
			n.cancel()
		}
		switch {
		case n.canceled:
			s.end(n, ErrCanceled)
		case err == nil:
			s.end(n, nil)
		case errors.Is(err, ErrYield):
			s.prepare(n)
		case n.attempt+1 < s.cfg.MaxAttempts && retryable(n.task, err):
			n.attempt++
			s.prepare(n)
		default:
			s.end(n, err)
		}
	})
}

func retryable(t Task, err error) bool {
	if r, ok := t.(Retryer); ok {
		return r.Retryable(err)
	}
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// priority is n's effective priority: the most urgent of its requests and
// of whatever needs it. Memoized per s.gen.
func (s *Scheduler) priority(n *node) int {
	if n.prioGen == s.gen {
		return n.prio
	}
	n.prioGen = s.gen
	n.prio = NoPriority // also what a cycle (never built, see prepare) would read
	p := NoPriority
	for _, rp := range n.requests {
		p = min(p, rp)
	}
	for w := range n.wantedBy {
		p = min(p, s.priority(w))
	}
	n.prio = p
	return p
}

func (s *Scheduler) inCurrent(n *node) bool {
	ctx := n.task.Context()
	return ctx == "" || ctx == s.cur
}

// sortNodes puts nodes in dispatch order. s.gen must be fresh.
func (s *Scheduler) sortNodes(nodes []*node) {
	sort.Slice(nodes, func(i, j int) bool {
		a, b := nodes[i], nodes[j]
		if pa, pb := s.priority(a), s.priority(b); pa != pb {
			return pa < pb
		}
		if ca, cb := s.inCurrent(a), s.inCurrent(b); ca != cb {
			return ca
		}
		if a.task.Less(b.task, s.priority(a)) {
			return true
		}
		if b.task.Less(a.task, s.priority(a)) {
			return false
		}
		return a.seq < b.seq
	})
}

// verdict is what one look at the ready tasks decided.
type verdict struct {
	start     *node  // the next task to start, if any
	switchTo  string // the context start needs made current ("" if it already is)
	blockedIn map[string]int
	draining  bool // a switch is waiting for the current context to empty
	drainPrio int
	dwellLeft time.Duration // > 0: the dwell refused a switch
}

// evaluate applies the admission rule - see the package doc - to find the
// single next task to start, without changing anything. With all set it
// keeps walking past that task, so blockedIn and draining describe every
// ready task (ShouldYield); otherwise it stops there.
func (s *Scheduler) evaluate(all bool) verdict {
	v := verdict{blockedIn: map[string]int{}}
	if s.closed {
		return v
	}
	s.gen++

	var ready []*node
	var running []Task
	ctxRunning := 0
	for _, n := range s.nodes {
		switch n.state {
		case Ready:
			if s.gate == nil || s.gate(n.task) {
				ready = append(ready, n)
			}
		case Running:
			running = append(running, n.task)
			if n.task.Context() != "" {
				ctxRunning++
			}
		}
	}
	s.sortNodes(ready)

	block := func(n *node) {
		ctx := n.task.Context()
		if _, ok := v.blockedIn[ctx]; !ok { // ready is sorted: the first is the most urgent
			v.blockedIn[ctx] = s.priority(n)
		}
	}
	pick := func(n *node, switchTo string) bool {
		if v.start == nil {
			v.start, v.switchTo = n, switchTo
		}
		return !all
	}

	decided := false // whether this look has ruled on leaving the current context
	for _, n := range ready {
		ctx := n.task.Context()
		if ctx == "" || ctx == s.cur || (s.cur == "" && ctxRunning == 0) {
			if ctx != "" && v.draining {
				continue
			}
			if !n.task.CanStart(running) {
				block(n)
				continue
			}
			switchTo := ""
			if ctx != s.cur {
				switchTo = ctx
			}
			if pick(n, switchTo) {
				return v
			}
			continue
		}
		if decided {
			continue
		}
		decided = true
		p := s.priority(n)
		if s.currentHasReady(ready, p) {
			continue
		}
		if left := s.dwellLeft(p); left > 0 {
			v.dwellLeft = left
			continue
		}
		if ctxRunning > 0 {
			v.draining, v.drainPrio = true, p
			continue
		}
		if !n.task.CanStart(running) {
			block(n)
			continue
		}
		if pick(n, ctx) {
			return v
		}
	}
	return v
}

// pop starts the next task evaluate picks, if any: the caller runs it.
func (s *Scheduler) pop() *node {
	v := s.evaluate(false)
	if v.dwellLeft > 0 && !s.cfg.Manual {
		if s.timer != nil {
			s.timer.Stop()
		}
		s.timer = time.AfterFunc(v.dwellLeft, s.Kick)
	}
	n := v.start
	if n == nil {
		return nil
	}
	if v.switchTo != "" {
		from := s.cur
		s.cur, s.since, s.switchPrio = v.switchTo, s.cfg.Now(), s.priority(n)
		if cb := s.cfg.OnSwitch; cb != nil {
			to := v.switchTo
			s.callbacks = append(s.callbacks, func() { cb(from, to) })
		}
	}
	n.state = Running
	return n
}

// admit starts every task that may start now. Called with the lock held
// after every change.
func (s *Scheduler) admit() {
	if s.cfg.Manual {
		return
	}
	for {
		n := s.pop()
		if n == nil {
			return
		}
		ctx, cancel := context.WithCancel(s.baseCtx)
		n.cancel = cancel
		attempt := n.attempt
		go func() { s.finish(n, n.task.Run(ctx, attempt)) }()
	}
}

// Pop starts the next task that may start and returns it, for the caller
// to run and report with Finish. Only for a Manual scheduler.
func (s *Scheduler) Pop() (t Task, ok bool) {
	s.do(func() {
		if n := s.pop(); n != nil {
			t, ok = n.task, true
		}
	})
	return t, ok
}

// currentHasReady reports whether the current context still has a ready
// task at priority p or more urgent - a tie keeps the context.
func (s *Scheduler) currentHasReady(ready []*node, p int) bool {
	for _, n := range ready {
		if n.task.Context() == s.cur && s.priority(n) <= p {
			return true
		}
	}
	return false
}

// dwellLeft is how much longer the dwell keeps a task at priority p from
// taking the context; 0 when it may.
func (s *Scheduler) dwellLeft(p int) time.Duration {
	if s.cfg.Dwell <= 0 || p <= s.cfg.DwellExempt {
		return 0
	}
	left := s.cfg.Dwell - s.cfg.Now().Sub(s.since)
	if left <= 0 {
		return 0
	}
	// The dwell only protects the work the last switch was made for.
	for _, n := range s.nodes {
		if n.state != Waiting && n.task.Context() == s.cur && s.priority(n) <= s.switchPrio {
			return left
		}
	}
	return 0
}
