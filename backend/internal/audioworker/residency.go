package audioworker

import "sync"

// residencyGate makes "only one heavy model kind resident at a time" an
// actual guarantee instead of best-effort VRAM hygiene. getCloneModel/
// getDesignModel/getAux each evict every other kind before loading their
// own weights, but eviction has to skip anything still in flight (closing
// a model mid-Run is a use-after-free - see loadedClone.inFlight), and
// during chapter generation the Higgs pool is essentially *always* in
// flight. So a Stable Audio request (music/ambience) or a design request
// arriving mid-chapter used to load its own multi-GB weights right on top
// of a running Higgs session - confirmed live: Higgs, Stable Audio and a
// Breeze clone model all resident at once, maxing VRAM and silently
// killing the worker.
//
// A caller acquires the gate for its kind before touching any model and
// releases it once finished (the same span as the model's own inFlight
// reservation). Any number of same-kind callers share it; a caller of a
// different kind waits until every current holder has released, at which
// point its own get* call's eviction step finds nothing in flight and
// genuinely frees the previous kind's VRAM before loading. Waiters are
// admitted strictly in arrival order - a new same-kind arrival queues
// behind an already-waiting different-kind caller rather than joining the
// active group - otherwise jobs' steady stream of concurrent Generate
// calls would starve a single music/design request indefinitely.
//
// Kinds (see the gateKind* helpers): every clone model shares one kind -
// co-residency among clone models is governed separately by
// MaxExtraClones' LRU cap, as before - while each design engine and each
// aux engine is its own kind, matching evictOtherDesignEngines/
// unloadOtherAux's "one resident at a time" rule. The aligner is small
// and deliberately persistent (see UnloadCloneModels) so it isn't gated.
type residencyGate struct {
	mu      sync.Mutex
	active  string // kind currently holding the gate; meaningful only while count > 0
	count   int
	waiters []*gateWaiter
}

type gateWaiter struct {
	kind  string
	ready chan struct{}
}

const gateKindClone = "clone"

func gateKindDesign(engineID string) string { return "design:" + engineID }
func gateKindAux(key string) string         { return "aux:" + key }

// acquire blocks until kind may hold the gate.
func (g *residencyGate) acquire(kind string) {
	g.mu.Lock()
	if len(g.waiters) == 0 && (g.count == 0 || g.active == kind) {
		g.active = kind
		g.count++
		g.mu.Unlock()
		return
	}
	wt := &gateWaiter{kind: kind, ready: make(chan struct{})}
	g.waiters = append(g.waiters, wt)
	g.admitLocked()
	g.mu.Unlock()
	<-wt.ready
}

// release drops one hold acquired by acquire.
func (g *residencyGate) release() {
	g.mu.Lock()
	g.count--
	g.admitLocked()
	g.mu.Unlock()
}

// admitLocked admits waiters from the front of the queue for as long as
// each one's kind can hold the gate - so a run of same-kind waiters at the
// front is admitted together, but never past a different-kind waiter.
func (g *residencyGate) admitLocked() {
	for len(g.waiters) > 0 {
		wt := g.waiters[0]
		if g.count > 0 && g.active != wt.kind {
			return
		}
		g.waiters = g.waiters[1:]
		g.active = wt.kind
		g.count++
		close(wt.ready)
	}
}
