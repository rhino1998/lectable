package jobs

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/ttsworker/ttsworkertest"
)

// TestPipelinePhasesRunInDependencyOrder is EnqueuePipeline's own core
// contract for its three actually-chained phases: characterization never
// starts before attribution (same book) has finished, and voice
// provisioning never starts before characterization has - see
// pipelineResolver's own doc comment. Direction carries no such
// dependency at all (see TestDirectionPhaseRunsWithoutWaitingOnEarlierPhases
// below for that half of the contract), so it's deliberately left out of
// the ordering assertion here - its position in order is unconstrained.
func TestPipelinePhasesRunInDependencyOrder(t *testing.T) {
	mgr := newTestManager()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)

	var mu sync.Mutex
	var order []int
	phase := func(i int) PipelinePhaseFunc {
		return func(context.Context, func() int) error {
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			return nil
		}
	}

	if err := mgr.EnqueuePipeline("book-1", [pipelinePhaseCount]PipelinePhaseFunc{
		phase(pipelinePhaseAttribution), phase(pipelinePhaseCharacterization), phase(pipelinePhaseVoiceProvision), phase(pipelinePhaseDirection), phase(pipelinePhaseMusic),
	}); err != nil {
		t.Fatalf("EnqueuePipeline: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 5
	})

	mu.Lock()
	defer mu.Unlock()
	pos := make(map[int]int, len(order))
	for i, phaseIdx := range order {
		pos[phaseIdx] = i
	}
	if pos[pipelinePhaseAttribution] >= pos[pipelinePhaseCharacterization] {
		t.Fatalf("expected attribution to run before characterization, got order %v", order)
	}
	if pos[pipelinePhaseCharacterization] >= pos[pipelinePhaseVoiceProvision] {
		t.Fatalf("expected characterization to run before voice provisioning, got order %v", order)
	}
}

// TestDirectionPhaseRunsWithoutWaitingOnEarlierPhases confirms direction
// tagging dispatches immediately rather than waiting for attribution/
// characterization/voice-provisioning to clear first - the actual point of
// giving it no dependency in pipelineResolver, since it needs nothing
// those three produce (see httpapi.directChapter/preprocessDirectionPhase's
// own doc comments). Blocks attribution on a channel the test controls and
// asserts direction has already finished before attribution is ever
// allowed to proceed - if direction were still chained behind it, this
// would hang until the test's own timeout.
func TestDirectionPhaseRunsWithoutWaitingOnEarlierPhases(t *testing.T) {
	mgr := newTestManager()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)

	release := make(chan struct{})
	var directionDone atomic.Bool
	attribution := func(context.Context, func() int) error {
		<-release
		return nil
	}
	direction := func(context.Context, func() int) error {
		directionDone.Store(true)
		return nil
	}

	if err := mgr.EnqueuePipeline("book-1", [pipelinePhaseCount]PipelinePhaseFunc{
		pipelinePhaseAttribution:      attribution,
		pipelinePhaseCharacterization: func(context.Context, func() int) error { return nil },
		pipelinePhaseVoiceProvision:   func(context.Context, func() int) error { return nil },
		pipelinePhaseDirection:        direction,
		pipelinePhaseMusic:            func(context.Context, func() int) error { return nil },
	}); err != nil {
		t.Fatalf("EnqueuePipeline: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool { return directionDone.Load() })
	close(release)
}

// TestEnqueuePipelineRejectsConcurrentRunForSameBook mirrors the old
// Server.pipelineRunning-based 409 dedup: a second EnqueuePipeline call
// for a book that already has one in progress is rejected outright, not
// silently interleaved with the first.
func TestEnqueuePipelineRejectsConcurrentRunForSameBook(t *testing.T) {
	mgr := newTestManager()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)

	block := make(chan struct{})
	first := func(context.Context, func() int) error {
		<-block
		return nil
	}
	noop := func(context.Context, func() int) error { return nil }

	if err := mgr.EnqueuePipeline("book-1", [pipelinePhaseCount]PipelinePhaseFunc{first, noop, noop, noop, noop}); err != nil {
		t.Fatalf("first EnqueuePipeline: %v", err)
	}
	waitFor(t, time.Second, func() bool { return mgr.IsPipelineRunning("book-1") })

	if err := mgr.EnqueuePipeline("book-1", [pipelinePhaseCount]PipelinePhaseFunc{noop, noop, noop, noop, noop}); err != ErrPipelineAlreadyRunning {
		t.Fatalf("expected ErrPipelineAlreadyRunning, got %v", err)
	}

	close(block)
	waitFor(t, time.Second, func() bool { return !mgr.IsPipelineRunning("book-1") })
}

// TestCancelPipelineCancelsInFlightAndRemovesQueuedPhases checks
// CancelPipeline's two-case shape: it cancels the currently in-flight
// phase's own context (so a well-behaved phase - the only kind httpapi
// ever supplies - returns promptly) and removes every later *chained*
// phase still sitting in pipelineQueue, so neither characterization nor
// voice provisioning ever runs. Direction is deliberately left out of this
// assertion: unlike those two, it carries no dependency on attribution at
// all (see pipelineResolver), so it can legitimately have already
// dispatched and finished before CancelPipeline is even called - this test
// is only about the phases that are actually still chained behind
// attribution.
func TestCancelPipelineCancelsInFlightAndRemovesQueuedPhases(t *testing.T) {
	mgr := newTestManager()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)

	started := make(chan struct{})
	var laterRan atomic.Bool
	first := func(ctx context.Context, _ func() int) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	later := func(context.Context, func() int) error {
		laterRan.Store(true)
		return nil
	}
	noop := func(context.Context, func() int) error { return nil }

	if err := mgr.EnqueuePipeline("book-1", [pipelinePhaseCount]PipelinePhaseFunc{
		pipelinePhaseAttribution:      first,
		pipelinePhaseCharacterization: later,
		pipelinePhaseVoiceProvision:   later,
		pipelinePhaseDirection:        noop,
		pipelinePhaseMusic:            noop,
	}); err != nil {
		t.Fatalf("EnqueuePipeline: %v", err)
	}
	<-started

	mgr.CancelPipeline("book-1")

	waitFor(t, time.Second, func() bool { return !mgr.IsPipelineRunning("book-1") })
	// Give a wrongly-dispatched later phase a real chance to run before
	// concluding it never did.
	time.Sleep(50 * time.Millisecond)
	if laterRan.Load() {
		t.Fatalf("expected the chained characterization/voice-provisioning phases to never run after CancelPipeline")
	}
}

// TestPipelineRunsConcurrentlyWithGenerationPool is the whole point of
// giving preprocessing phases their own taskqueue.Queue (pipelineQueue)
// instead of one more PoolKey on m.queue: a phase blocked mid-run must
// never stall ordinary poolGeneration dispatch the way it would if
// poolPipeline shared m.queue's own "one pool active at a time" mutual
// exclusion with poolGeneration/poolDesign/poolLLM (see poolPipeline's own
// doc comment).
func TestPipelineRunsConcurrentlyWithGenerationPool(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)

	book, chapterID := createBookAndChapter(t, s, "", 0, "First paragraph.", "Second paragraph.")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)

	block := make(chan struct{})
	blockedPhase := func(context.Context, func() int) error {
		<-block
		return nil
	}
	noop := func(context.Context, func() int) error { return nil }
	if err := mgr.EnqueuePipeline(book.ID, [pipelinePhaseCount]PipelinePhaseFunc{blockedPhase, noop, noop, noop, noop}); err != nil {
		t.Fatalf("EnqueuePipeline: %v", err)
	}
	waitFor(t, time.Second, func() bool { return mgr.IsPipelineRunning(book.ID) })

	mgr.EnqueueChapter(book.ID, chapterID, 0)

	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	// Would never finish while the pipeline's own first phase is still
	// blocked above if poolPipeline wrongly shared m.queue's own mutual
	// exclusion - it lives on a wholly separate Queue instead, so ordinary
	// generation proceeds regardless of the still-in-flight pipeline phase.
	waitFor(t, 2*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, bookVoice.VoiceID()) })

	if !mgr.IsPipelineRunning(book.ID) {
		t.Fatalf("expected the pipeline's first phase to still be blocked")
	}
	close(block)
	waitFor(t, time.Second, func() bool { return !mgr.IsPipelineRunning(book.ID) })
}

// TestSnapshotIncludesInFlightPipelinePhase checks that a book's
// currently in-flight phase shows up in Manager.Snapshot's own InFlight
// list (what the Jobs dashboard renders) - the whole point of merging
// pipelineQueue into Snapshot, so a reader isn't limited to "preprocessing:
// yes/no" and can actually see which phase a book is on.
func TestSnapshotIncludesInFlightPipelinePhase(t *testing.T) {
	mgr := newTestManager()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)

	block := make(chan struct{})
	blocked := func(context.Context, func() int) error {
		<-block
		return nil
	}
	noop := func(context.Context, func() int) error { return nil }
	if err := mgr.EnqueuePipeline("book-1", [pipelinePhaseCount]PipelinePhaseFunc{blocked, noop, noop, noop, noop}); err != nil {
		t.Fatalf("EnqueuePipeline: %v", err)
	}
	defer close(block)

	var found QueueTask
	waitFor(t, time.Second, func() bool {
		inFlight, _ := mgr.Snapshot()
		for _, qt := range inFlight {
			if qt.Kind == pipelineKindNames[pipelinePhaseAttribution] && qt.BookID == "book-1" {
				found = qt
				return true
			}
		}
		return false
	})
	if found.ID != pipelineKey("book-1", pipelinePhaseAttribution) {
		t.Fatalf("expected ID %q, got %q", pipelineKey("book-1", pipelinePhaseAttribution), found.ID)
	}
	if found.Label != "Whole book" {
		t.Fatalf("expected Label %q, got %q", "Whole book", found.Label)
	}
}

// TestCancelByIDCancelsInFlightPipelinePhase checks Manager.Cancel's own
// fallback to cancelPipelineTask - the same DELETE /api/jobs/{id} a
// dashboard row's per-row "Cancel" button hits works for a pipeline-phase
// row's own ID too, not just an ordinary task's.
func TestCancelByIDCancelsInFlightPipelinePhase(t *testing.T) {
	mgr := newTestManager()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)

	started := make(chan struct{})
	first := func(ctx context.Context, _ func() int) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	noop := func(context.Context, func() int) error { return nil }
	if err := mgr.EnqueuePipeline("book-1", [pipelinePhaseCount]PipelinePhaseFunc{first, noop, noop, noop, noop}); err != nil {
		t.Fatalf("EnqueuePipeline: %v", err)
	}
	<-started

	id := pipelineKey("book-1", pipelinePhaseAttribution)
	if !mgr.Cancel(id) {
		t.Fatalf("expected Cancel(%q) to report true", id)
	}
	waitFor(t, time.Second, func() bool { return !mgr.IsPipelineRunning("book-1") })
}

// TestCancelAllCancelsPipelineTasksToo checks CancelAll's own pipeline-side
// half (cancelAllPipelineTasks) - the Jobs dashboard's "Cancel all" button
// now reaches pipeline rows too, not just ordinary ones.
func TestCancelAllCancelsPipelineTasksToo(t *testing.T) {
	mgr := newTestManager()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)

	started := make(chan struct{})
	first := func(ctx context.Context, _ func() int) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	noop := func(context.Context, func() int) error { return nil }
	if err := mgr.EnqueuePipeline("book-1", [pipelinePhaseCount]PipelinePhaseFunc{first, noop, noop, noop, noop}); err != nil {
		t.Fatalf("EnqueuePipeline: %v", err)
	}
	<-started

	n := mgr.CancelAll()
	if n == 0 {
		t.Fatalf("expected CancelAll to report at least one task acted on")
	}
	waitFor(t, time.Second, func() bool { return !mgr.IsPipelineRunning("book-1") })
}

// TestPromoteTierUpgradesQueuedPipelinePhase checks that PromoteTier (see
// manager_test.go's own TestPromoteTier* tests for the per-item-task
// half) works identically for a phase still queued in pipelineQueue,
// blocked on an earlier phase - the same fallback Cancel/CancelAll
// already use to reach a pipeline row by its own Key().
func TestPromoteTierUpgradesQueuedPipelinePhase(t *testing.T) {
	mgr := newTestManager()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)

	block := make(chan struct{})
	blocked := func(context.Context, func() int) error {
		<-block
		return nil
	}
	noop := func(context.Context, func() int) error { return nil }
	if err := mgr.EnqueuePipeline("book-1", [pipelinePhaseCount]PipelinePhaseFunc{blocked, noop, noop, noop, noop}); err != nil {
		t.Fatalf("EnqueuePipeline: %v", err)
	}
	defer close(block)

	id := pipelineKey("book-1", pipelinePhaseCharacterization)
	waitFor(t, time.Second, func() bool {
		_, queued := mgr.Snapshot()
		for _, qt := range queued {
			if qt.ID == id {
				return true
			}
		}
		return false
	})

	if err := mgr.PromoteTier(id, TierUrgent); err != nil {
		t.Fatalf("PromoteTier: %v", err)
	}

	_, queued := mgr.Snapshot()
	found := false
	for _, qt := range queued {
		if qt.ID == id {
			found = true
			if qt.Tier != TierUrgent {
				t.Fatalf("expected the promoted phase to report TierUrgent, got %d", qt.Tier)
			}
		}
	}
	if !found {
		t.Fatalf("expected phase %q to still be queued", id)
	}
}
