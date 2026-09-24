package audioworker

import (
	"testing"
	"time"
)

// acquireAsync starts g.acquire(kind) in a goroutine and returns a channel
// closed once it returns.
func acquireAsync(g *residencyGate, kind string) chan struct{} {
	done := make(chan struct{})
	go func() {
		g.acquire(kind)
		close(done)
	}()
	return done
}

func requireBlocked(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s acquired, want blocked", what)
	case <-time.After(50 * time.Millisecond):
	}
}

func requireAcquired(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatalf("%s still blocked, want acquired", what)
	}
}

func TestResidencyGateSameKindShares(t *testing.T) {
	var g residencyGate
	g.acquire(gateKindClone("audiocpp-higgs-4b"))
	requireAcquired(t, acquireAsync(&g, gateKindClone("audiocpp-higgs-4b")), "second clone")
}

func TestResidencyGateOtherKindWaitsForDrain(t *testing.T) {
	var g residencyGate
	g.acquire(gateKindClone("audiocpp-higgs-4b"))
	g.acquire(gateKindClone("audiocpp-higgs-4b"))

	aux := acquireAsync(&g, gateKindAux("stable_audio_medium"))
	requireBlocked(t, aux, "aux while clone in flight")

	g.release()
	requireBlocked(t, aux, "aux with one clone still in flight")

	g.release()
	requireAcquired(t, aux, "aux after clones drained")
}

// A same-kind arrival must queue behind an already-waiting different-kind
// caller, or a steady stream of clone requests would starve it.
func TestResidencyGateNoStarvation(t *testing.T) {
	var g residencyGate
	g.acquire(gateKindClone("audiocpp-higgs-4b"))

	design := acquireAsync(&g, gateKindDesign("breeze_tts"))
	requireBlocked(t, design, "design while clone in flight")

	lateClone := acquireAsync(&g, gateKindClone("audiocpp-higgs-4b"))
	requireBlocked(t, lateClone, "clone arriving behind a waiting design")

	g.release()
	requireAcquired(t, design, "design after clone drained")
	requireBlocked(t, lateClone, "clone while design in flight")

	g.release()
	requireAcquired(t, lateClone, "clone after design released")
}

// Consecutive same-kind waiters at the front of the queue are admitted
// together once the gate frees up.
func TestResidencyGateAdmitsSameKindBatch(t *testing.T) {
	var g residencyGate
	g.acquire(gateKindAux("ace_step"))

	a := acquireAsync(&g, gateKindClone("audiocpp-higgs-4b"))
	requireBlocked(t, a, "first clone")
	b := acquireAsync(&g, gateKindClone("audiocpp-higgs-4b"))
	requireBlocked(t, b, "second clone")

	g.release()
	requireAcquired(t, a, "first clone")
	requireAcquired(t, b, "second clone")
}

// Different clone models are different kinds - only one is ever resident.
func TestResidencyGateCloneModelsExclusive(t *testing.T) {
	var g residencyGate
	g.acquire(gateKindClone("audiocpp-higgs-4b"))

	breeze := acquireAsync(&g, gateKindClone("audiocpp-breeze-tts"))
	requireBlocked(t, breeze, "breeze clone while higgs in flight")

	g.release()
	requireAcquired(t, breeze, "breeze clone after higgs drained")
}
