package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/oggopus"
	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/taskqueue"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/ttsworker/ttsworkertest"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// newTestManager builds a Manager with a real taskqueue.Queue (registered
// with this package's own production pool capacities), but no store/tts -
// for tests that only exercise queue/dependency mechanics directly via
// hand-built *task values, the same lightweight, store-free shape every
// test in this file that constructs tasks by hand (rather than through
// the store-backed public API) has always used.
func newTestManager() *Manager {
	m := &Manager{
		chapterPending: map[string]int{},
		lookaheads:     map[string]*lookaheadRun{},
		wake:           make(chan struct{}, 1),
		changeSubs:     map[chan struct{}]struct{}{},
	}
	m.queue = taskqueue.NewQueue(m.resolveDependencies)
	m.queue.RegisterPool(poolGeneration, taskqueue.PoolConfig{Capacity: maxInFlight})
	m.queue.RegisterPool(poolLLM, taskqueue.PoolConfig{Capacity: maxAttributionInFlight})
	m.queue.RegisterPool(poolDesign, taskqueue.PoolConfig{Capacity: maxDesignInFlight})
	m.pipelineQueue = taskqueue.NewQueue(pipelineResolver)
	m.pipelineQueue.RegisterPool(poolPipeline, taskqueue.PoolConfig{Capacity: maxPipelineInFlight})
	m.pipelineWake = make(chan struct{}, 1)
	return m
}

// newTestManagerWithStore is newTestManager plus a real *store.DuckStore - for
// tests whose dependency resolvers need to look up real persisted state
// (a character's Summary, a chapter's State).
func newTestManagerWithStore(s *store.DuckStore) *Manager {
	m := newTestManager()
	// Through the cache, as in cmd/server - see store.Cached.
	m.store = store.NewCached(s)
	m.narration = narration.NewResolver(m.store)
	return m
}

// setQueue replaces mgr's whole queue with tasks, routing each through the
// real taskqueue.Queue.Push so its own bookkeeping stays consistent - the
// test-side equivalent of what used to be a raw heap assignment back when
// there was one to poke at directly.
func setQueue(mgr *Manager, tasks ...*task) {
	mgr.queue.Drain()
	for _, t := range tasks {
		mgr.queue.Push(t)
	}
}

// popAll drains every currently dispatchable task from mgr's queue,
// Finish-ing each one immediately (simulating instant completion) so the
// next one - including anything a dependency resolver lazily creates
// along the way - can dispatch in turn, until nothing is left. Returns
// the tasks in the order they dispatched.
func popAll(mgr *Manager) []*task {
	var out []*task
	for {
		tk, ok := mgr.queue.Pop()
		if !ok {
			return out
		}
		bt := tk.(*task)
		out = append(out, bt)
		mgr.queue.Finish(bt.dedupKey())
	}
}

func openTestStore(t *testing.T) *store.DuckStore {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "library.duckdb"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// createBookAndChapter makes a book (optionally in a series) with one
// chapter containing paragraphTexts, returning the book (already
// re-fetched) and its one chapter's id.
func createBookAndChapter(t *testing.T, s *store.DuckStore, seriesName string, seriesIndex float64, paragraphTexts ...string) (*store.Book, string) {
	t.Helper()
	blocks := make([]store.BlockInput, len(paragraphTexts))
	for i, text := range paragraphTexts {
		blocks[i] = store.BlockInput{Kind: store.BlockText, Text: text}
	}
	bookID, _, err := s.CreateBook("Book", "Author", "en", "", seriesName, seriesIndex, []store.ChapterInput{
		{Title: "Ch1", Blocks: blocks},
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	book, err := s.GetBook(bookID)
	if err != nil || book == nil {
		t.Fatalf("GetBook: %v", err)
	}
	chapters, err := s.ListChapterSummaries(bookID, "unused-voice-id", nil)
	if err != nil || len(chapters) != 1 {
		t.Fatalf("ListChapterSummaries: %v (chapters=%d)", err, len(chapters))
	}
	return book, chapters[0].ID
}

// createBookWithBlocks is createBookAndChapter's own counterpart for tests
// that need to control Inline/IsQuote directly (scare-quote merge group
// tests) rather than accepting whatever createBookAndChapter's plain-text
// blocks default to (both false).
func createBookWithBlocks(t *testing.T, s *store.DuckStore, blocks ...store.BlockInput) (*store.Book, string) {
	t.Helper()
	bookID, _, err := s.CreateBook("Book", "Author", "en", "", "", 0, []store.ChapterInput{
		{Title: "Ch1", Blocks: blocks},
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	book, err := s.GetBook(bookID)
	if err != nil || book == nil {
		t.Fatalf("GetBook: %v", err)
	}
	chapters, err := s.ListChapterSummaries(bookID, "unused-voice-id", nil)
	if err != nil || len(chapters) != 1 {
		t.Fatalf("ListChapterSummaries: %v (chapters=%d)", err, len(chapters))
	}
	return book, chapters[0].ID
}

// waitFor polls cond every 5ms until it returns true or timeout elapses,
// failing the test on timeout - this package's generation pipeline runs
// entirely against a fake in-process HTTP server and local disk, so a real
// completion should show up in well under a second; a generous timeout
// just guards against this hanging forever if something regresses.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s", timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func allParagraphsReady(t *testing.T, s *store.DuckStore, chapterID, voiceID string) bool {
	t.Helper()
	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	ids := make([]string, len(paragraphs))
	for i, p := range paragraphs {
		ids[i] = p.ID
	}
	states, err := s.ParagraphAudioStatuses(ids, voiceID)
	if err != nil {
		t.Fatalf("ParagraphAudioStatuses: %v", err)
	}
	for _, id := range ids {
		if states[id].Status != store.AudioReady {
			return false
		}
	}
	return true
}

func TestManagerGeneratesChapterEndToEnd(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)

	book, chapterID := createBookAndChapter(t, s, "", 0, "First paragraph.", "Second paragraph, a bit longer than the first.")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)

	mgr.EnqueueChapter(book.ID, chapterID, 0)

	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	voiceID := bookVoice.VoiceID()

	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, voiceID) })

	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	for _, p := range paragraphs {
		path := audiopath.ParagraphFile(dataDir, book.ID, chapterID, voiceID, p.Idx)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected audio file for paragraph %d at %s: %v", p.Idx, path, err)
		}
	}

	// The default book voice is a built-in preset (voices.DefaultPresetID),
	// which clones via a reference clip: expect exactly one Design call
	// (rendering the shared reference clip once, however many paragraphs
	// race through EnsureFile concurrently - see voicerefs.presetLocks)
	// plus one Generate call per paragraph.
	if got := len(fake.DesignCalls()); got != 1 {
		t.Fatalf("expected exactly 1 Design call (the shared reference clip), got %d", got)
	}
	if got := len(fake.GenerateCalls()); got != len(paragraphs) {
		t.Fatalf("expected %d Generate calls (one per paragraph), got %d", len(paragraphs), got)
	}

	// Word-level alignment runs detached after each paragraph goes ready
	// (see alignParagraph) - wait for it too and confirm it actually
	// persisted non-empty word timings. WordTimings lives per-voice in
	// paragraph_audio (see store.AudioState), not on the bare paragraphs
	// row GetParagraph returns - ParagraphAudioStatuses is the accessor
	// that actually carries it.
	ids := make([]string, len(paragraphs))
	for i, p := range paragraphs {
		ids[i] = p.ID
	}
	waitFor(t, 5*time.Second, func() bool {
		states, err := s.ParagraphAudioStatuses(ids, voiceID)
		if err != nil {
			t.Fatalf("ParagraphAudioStatuses: %v", err)
		}
		for _, id := range ids {
			if wt := states[id].WordTimings; wt == "" || wt == "[]" {
				return false
			}
		}
		return true
	})
}

func TestManagerFullyCustomInstructUsesDesignNotClone(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)

	book, chapterID := createBookAndChapter(t, s, "", 0, "Only paragraph.")
	if err := s.UpdateVoice(book.ID, "", "speak like a robot", "English", 0, book.CloneModel, store.CharacterVoiceModeNarrator, false, false); err != nil {
		t.Fatalf("UpdateVoice: %v", err)
	}
	book, err := s.GetBook(book.ID)
	if err != nil {
		t.Fatalf("GetBook: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)
	mgr.EnqueueChapter(book.ID, chapterID, 0)

	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	voiceID := bookVoice.VoiceID()
	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, voiceID) })

	if got := len(fake.GenerateCalls()); got != 0 {
		t.Fatalf("a fully custom instruct with no preset should never call Generate/clone, got %d calls", got)
	}
	if got := len(fake.DesignCalls()); got != 1 {
		t.Fatalf("expected exactly 1 Design call (one per paragraph, no shared reference clip), got %d", got)
	}
}

func TestManagerRegenerateParagraphReRenders(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)

	book, chapterID := createBookAndChapter(t, s, "", 0, "The only paragraph.")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)
	mgr.EnqueueChapter(book.ID, chapterID, 0)

	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	voiceID := bookVoice.VoiceID()
	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, voiceID) })

	callsBefore := len(fake.GenerateCalls())

	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	mgr.EnqueueParagraphRegenerate(book.ID, chapterID, 0, paragraphs[0])

	waitFor(t, 5*time.Second, func() bool { return len(fake.GenerateCalls()) > callsBefore })
	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, voiceID) })
}

// TestManagerUrgentColdCacheCloneDoesNotDeadlock is a real end-to-end
// regression test for the exact deadlock the reference-clip dependency
// mechanism (referenceClipBlockerFor/taskDependencies) exists to prevent:
// an urgent (TierUrgent/TierLookahead) KindVoiceClone task whose own preset
// has never been rendered before must still complete, not hang forever,
// even though this package never force-cancels in-flight work to make room
// for one (see urgentElsewhereQueued's own doc comment). An earlier version
// of this code nested a blocking, cross-pool RunVoiceProvision call
// directly inside generate() itself: the outer clone task occupied its own
// poolGeneration slot for as long as that call blocked, its nested child
// could never dispatch into poolDesign while poolGeneration stayed active
// (no two pools are ever active at once - see fill/tryFill), and the outer
// task had no way to be canceled to let it, a genuine, confirmed-live
// standoff only a manual Cancel could break. Now the clone task is simply
// never put in flight at
// all until its own reference-clip dependency (a real, independently-
// dispatched poolDesign task it merely depends on, never blocks inside)
// finishes - this is exactly what would have hung (until this test's own
// 5s waitFor timeout) under the old design.
func TestManagerUrgentColdCacheCloneDoesNotDeadlock(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)

	book, chapterID := createBookAndChapter(t, s, "", 0, "Only paragraph.")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)

	// EnqueueLookahead's own exact-position paragraph dispatches at
	// TierUrgent (see its own doc comment) - the book's default preset has
	// never been rendered in this fresh temp dir, so this is a genuine
	// cold-cache, urgent clone task that would have hung forever if it were
	// ever nested-blocked on its own dependency the old way.
	mgr.EnqueueLookahead(book.ID, 0, 0, 0)

	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	voiceID := bookVoice.VoiceID()

	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, voiceID) })
}

// --- task.Less ordering ---
//
// Tier ordering itself (lower tier always wins, regardless of pool) and
// cross-pool tie-breaking (has-dependents, then insertion order) are now
// internal/taskqueue's own responsibility, thoroughly covered by that
// package's tests against synthetic task types - see
// TestPopFavorsLowerTierAcrossPools/TestPopBreaksCrossPoolTiesByInsertionOrder
// below for the jobs-level version of that same guarantee. What's left to
// test here is task.Less's own kind-specific ordering, only ever invoked
// between two tasks sharing a pool.

func newLLMTask(bookID, chapterID string, chapterIdx int, tier int, seriesName string, seriesIndex float64) *task {
	return &task{
		kind: KindSpeakerAttribution, tier: tier, bookID: bookID, chapterID: chapterID, chapterIdx: chapterIdx,
		llmKey: chapterID, seriesName: seriesName, seriesIndex: seriesIndex,
	}
}

func newGenTask(bookID, chapterID string, chapterIdx, paragraphIdx int, tier int, seriesName string, seriesIndex float64) *task {
	return &task{
		kind: KindVoiceClone, tier: tier, bookID: bookID, chapterID: chapterID, chapterIdx: chapterIdx,
		paragraph: store.Paragraph{ID: chapterID + "-p", Idx: paragraphIdx},
		// seriesName/seriesIndex are never actually set on a real
		// generation task in production, but comparePosition applies
		// series ordering unconditionally, regardless of Kind (see its
		// own doc comment) - set here to prove that works for this kind
		// too, not just poolLLM's own pre-processing kinds.
		seriesName: seriesName, seriesIndex: seriesIndex,
	}
}

// sortedByLess sorts tasks by task.Less alone (no tier/dependents axis -
// those are taskqueue's job, not Less's - see this section's own doc
// comment), the same shape a caller invoking Less only ever does once
// every more general axis is already tied.
func sortedByLess(tasks []*task) []*task {
	sorted := append([]*task(nil), tasks...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Less(sorted[j]) })
	return sorted
}

func TestLessOrdersPreProcessingBySeriesIndexWithinSeries(t *testing.T) {
	// Two different series, each with two books out of SeriesIndex order
	// by bookID - if series ordering weren't applied, plain bookID
	// comparison would put "book-a-1" before "book-a-2" (matches, by
	// coincidence) but "book-b-1" before "book-b-2" in bookID order too;
	// pick bookIDs that would sort the *wrong* way under plain bookID
	// comparison to actually prove SeriesIndex, not bookID, drives the
	// order.
	seriesAFirst := newLLMTask("book-a-zzz-later-id", "ch-a1", 0, TierBackground, "Series A", 1)
	seriesASecond := newLLMTask("book-a-aaa-earlier-id", "ch-a2", 0, TierBackground, "Series A", 2)
	seriesBOnly := newLLMTask("book-b", "ch-b1", 0, TierBackground, "Series B", 1)

	got := sortedByLess([]*task{seriesASecond, seriesBOnly, seriesAFirst})

	// Within Series A, seriesAFirst (index 1) must come before seriesASecond
	// (index 2) despite sorting *after* it by bookID alone.
	posFirst, posSecond := -1, -1
	for i, tk := range got {
		if tk == seriesAFirst {
			posFirst = i
		}
		if tk == seriesASecond {
			posSecond = i
		}
	}
	if posFirst == -1 || posSecond == -1 {
		t.Fatalf("expected both Series A tasks in the sorted output")
	}
	if posFirst >= posSecond {
		t.Fatalf("expected the earlier SeriesIndex to sort first within a series, got order %v", taskLabels(got))
	}
}

func TestLessDoesNotOrderAcrossDifferentSeries(t *testing.T) {
	// No specific ordering is guaranteed *between* series (or a book with
	// no series at all) - just confirm this doesn't panic and produces a
	// stable, deterministic (if arbitrary) total order: sorting twice gives
	// the same result.
	tasks := []*task{
		newLLMTask("book-z", "ch-z", 0, TierBackground, "Series Z", 1),
		newLLMTask("book-a", "ch-a", 0, TierBackground, "Series A", 1),
		newLLMTask("book-standalone", "ch-s", 0, TierBackground, "", 0),
	}
	first := sortedByLess(tasks)
	second := sortedByLess(tasks)
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("expected a stable order across repeated sorts, got %v then %v", taskLabels(first), taskLabels(second))
		}
	}
}

// TestLessAppliesSeriesOrderingToGenerationTasksToo confirms series
// ordering is unconditional (see comparePosition's own doc comment), not
// limited to poolLLM's own pre-processing kinds: two same-series
// KindVoiceClone tasks must still sort by SeriesIndex, the same as the
// pre-processing test above, even though production code never actually
// populates these fields on a real generation task today.
func TestLessAppliesSeriesOrderingToGenerationTasksToo(t *testing.T) {
	earlierIndexLaterBookID := newGenTask("book-a-zzz", "ch1", 0, 0, TierBackground, "Series A", 1)
	laterIndexEarlierBookID := newGenTask("book-a-aaa", "ch2", 0, 0, TierBackground, "Series A", 2)

	got := sortedByLess([]*task{laterIndexEarlierBookID, earlierIndexLaterBookID})
	if got[0] != earlierIndexLaterBookID {
		t.Fatalf("expected the earlier SeriesIndex to sort first even for generation tasks, got order %v", taskLabels(got))
	}
}

// TestLessGroupsBackgroundClonesByReferenceWithinChapter covers
// compareClone: background clone tasks in one chapter run grouped by
// reference clip (preset + emotion variant) so the worker's resident
// reference-prefix KV is reused back to back, while lookahead tasks - and
// ordering across chapters - stay in reading order.
func TestLessGroupsBackgroundClonesByReferenceWithinChapter(t *testing.T) {
	clone := func(chapterIdx, idx, tier int, presetID, emotion string) *task {
		tk := newGenTask("book", fmt.Sprintf("ch%d", chapterIdx), chapterIdx, idx, tier, "", 0)
		tk.presetID = presetID
		tk.paragraph.IsQuote = emotion != ""
		tk.paragraph.Emotion = emotion
		return tk
	}
	labels := func(tasks []*task) []string {
		out := make([]string, len(tasks))
		for i, tk := range tasks {
			out[i] = fmt.Sprintf("%d/%d:%s", tk.chapterIdx, tk.paragraph.Idx, tk.refKey())
		}
		return out
	}

	background := []*task{
		clone(0, 0, TierBackground, "narrator", ""),
		clone(0, 1, TierBackground, "alice", ""),
		clone(0, 2, TierBackground, "narrator", ""),
		clone(0, 3, TierBackground, "alice", "angry"),
		clone(0, 4, TierBackground, "alice", ""),
		clone(1, 0, TierBackground, "alice", ""),
	}
	got := labels(sortedByLess(background))
	want := []string{
		"0/1:alice:", "0/4:alice:", "0/3:alice:angry",
		"0/0:narrator:", "0/2:narrator:",
		"1/0:alice:",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("background order = %v, want %v", got, want)
	}

	lookahead := []*task{
		clone(0, 2, TierLookahead, "narrator", ""),
		clone(0, 1, TierLookahead, "alice", ""),
		clone(0, 0, TierLookahead, "narrator", ""),
	}
	got = labels(sortedByLess(lookahead))
	want = []string{"0/0:narrator:", "0/1:alice:", "0/2:narrator:"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("lookahead order = %v, want %v", got, want)
	}
}

func taskLabels(tasks []*task) []string {
	out := make([]string, len(tasks))
	for i, tk := range tasks {
		out[i] = tk.bookID + "/" + tk.chapterID
	}
	return out
}

// --- Pop: tier-first ordering across pools ---

// TestPopFavorsLowerTierAcrossPools confirms Pop's own core property (see
// internal/taskqueue's own doc comment): it picks whichever pool holds
// the single lowest-tier queued task, entirely without regard to which
// pool each one belongs to - an urgent clone task must win against a
// background direction-tagging task even though direction-tagging is
// poolLLM and was pushed first.
func TestPopFavorsLowerTierAcrossPools(t *testing.T) {
	mgr := newTestManager()
	if _, ok := mgr.queue.Pop(); ok {
		t.Fatalf("expected nothing dispatchable from an empty queue")
	}

	direction := &task{kind: KindSpeechDirection, tier: TierBackground, bookID: "book-1", chapterID: "ch-1", llmKey: "ch-1"}
	urgentClone := &task{kind: KindVoiceClone, tier: TierUrgent, bookID: "book-2", chapterID: "ch-2", paragraph: store.Paragraph{ID: "p-1"}}
	setQueue(mgr, direction, urgentClone)

	got, ok := mgr.queue.Pop()
	if !ok || got.(*task).kind != KindVoiceClone {
		t.Fatalf("expected the urgent clone task to win over background direction-tagging regardless of pool, got %+v ok=%v", got, ok)
	}
}

// TestPopBreaksCrossPoolTiesByInsertionOrder documents a deliberate,
// intentional change from this package's own pre-taskqueue behavior:
// kindPriority (characterization before plain generation) only ever
// applies within one pool now (task.Less is never invoked across pools -
// see its own doc comment). Two same-tier tasks in *different* pools with
// nothing depending on either fall back to plain insertion order instead.
func TestPopBreaksCrossPoolTiesByInsertionOrder(t *testing.T) {
	mgr := newTestManager()
	background := &task{kind: KindVoiceClone, tier: TierBackground, bookID: "book-1", chapterID: "ch-1", paragraph: store.Paragraph{ID: "p-1"}}
	characterization := &task{kind: KindSpeakerCharacterization, tier: TierBackground, bookID: "book-1", llmKey: "char-1", label: "Alice"}
	setQueue(mgr, background, characterization)

	got, ok := mgr.queue.Pop()
	if !ok || got.(*task).kind != KindVoiceClone {
		t.Fatalf("expected whichever was pushed first to win a cross-pool tie, got %+v ok=%v", got, ok)
	}
}

// --- Snapshot ---

// TestSnapshotQueuedOrderMatchesDispatchOrder guards Snapshot's own tier-
// first ordering (see TestPopBreaksCrossPoolTiesByInsertionOrder above for
// why a same-tier cross-pool tie no longer favors characterization over
// plain generation the way it once did): both TierUrgent tasks must sort
// before the TierBackground one, and between the two TierUrgent tasks
// (different pools, no dependents on either), whichever was pushed first
// sorts first.
func TestSnapshotQueuedOrderMatchesDispatchOrder(t *testing.T) {
	mgr := newTestManager()

	urgentClone := &task{kind: KindVoiceClone, tier: TierUrgent, bookID: "book-z", chapterID: "ch-urgent", paragraph: store.Paragraph{ID: "p-1"}}
	characterization := &task{kind: KindSpeakerCharacterization, tier: TierUrgent, bookID: "book-m", llmKey: "char-1", label: "Alice"}
	background := &task{kind: KindVoiceClone, tier: TierBackground, bookID: "book-a", chapterID: "ch-bg", paragraph: store.Paragraph{ID: "p-2"}}
	setQueue(mgr, urgentClone, characterization, background)

	_, queued := mgr.Snapshot()
	if len(queued) != 3 {
		t.Fatalf("expected 3 queued tasks, got %d: %+v", len(queued), queued)
	}
	if queued[0].ID != urgentClone.dedupKey() {
		t.Fatalf("expected the first-pushed urgent task to sort first on a cross-pool tie, got %+v", queued[0])
	}
	if queued[1].ID != characterization.dedupKey() {
		t.Fatalf("expected the second urgent task next, got %+v", queued[1])
	}
	if queued[2].ID != background.dedupKey() {
		t.Fatalf("expected the background task last regardless of pool, got %+v", queued[2])
	}
}

// --- Dependency resolution (resolveDependencies) ---
//
// The three functions behind resolveDependencies -
// characterizationDependency/speechDirectionDependency/
// referenceClipDependency - are exercised here through the real
// taskqueue.Queue.Pop path (the only way to reach them at all - they take
// a *taskqueue.LockedQueue, which only that package can construct), not
// by calling them directly. This is deliberately the same path production
// code runs, and is what actually exercises the specific bug this whole
// mechanism exists to prevent: a dependency task getting silently
// recreated forever instead of ever recognizing its own real, persisted
// "already done" state.

// TestCharacterizationDependencyBlocksThenClearsOnceCharacterized is the
// core regression test for that bug, for characterizationDependency: an
// uncharacterized speaker gets a real KindSpeakerCharacterization task
// lazily created and dispatched ahead of the paragraph that needs it: the
// paragraph itself stays blocked while it's in flight, and once
// char.Summary is actually persisted (simulating the created task having
// really run), a fresh dependency check must find nothing left to do -
// not recreate another one.
func TestCharacterizationDependencyBlocksThenClearsOnceCharacterized(t *testing.T) {
	s := openTestStore(t)
	book, _ := createBookAndChapter(t, s, "", 0, "Alice said hello.")
	char, _, err := s.UpsertCharacter(store.SeriesScope(book), "Alice", false)
	if err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}

	characterizeCalls := 0
	mgr := newTestManagerWithStore(s)
	mgr.characterize = func(ctx context.Context, bookID string, c store.Character) error {
		characterizeCalls++
		return s.SetCharacterSummary(c.ID, "a warm, measured voice", "A short reference line.")
	}

	cloneForAlice := &task{kind: KindVoiceClone, bookID: book.ID, tier: TierBackground, paragraph: store.Paragraph{ID: "p-1", Speaker: "Alice"}}
	mgr.pushTask(cloneForAlice)

	got, ok := mgr.queue.Pop()
	if !ok {
		t.Fatalf("expected the lazily-created characterization task to dispatch")
	}
	blocker := got.(*task)
	if blocker.kind != KindSpeakerCharacterization || blocker.label != char.Name {
		t.Fatalf("expected a KindSpeakerCharacterization task for %q, got kind=%v label=%q", char.Name, blocker.kind, blocker.label)
	}
	if _, ok := mgr.queue.Pop(); ok {
		t.Fatalf("expected the clone to stay blocked while its characterization is in flight")
	}
	if characterizeCalls != 0 {
		t.Fatalf("expected characterize not to have actually run yet (task never dispatched via runLLM), got %d call(s)", characterizeCalls)
	}

	// Simulate the dispatched task actually finishing - persists
	// char.Summary via the fake characterize hook above.
	if _, _, err := blocker.runLLM(t.Context()); err != nil {
		t.Fatalf("runLLM: %v", err)
	}
	if characterizeCalls != 1 {
		t.Fatalf("expected characterize to have run exactly once, got %d", characterizeCalls)
	}
	mgr.queue.Finish(blocker.dedupKey())

	// Now that char.Summary is actually persisted, the clone must
	// dispatch directly - a fresh dependency check must NOT recreate
	// another characterization task.
	got2, ok := mgr.queue.Pop()
	if !ok || got2.(*task) != cloneForAlice {
		t.Fatalf("expected the clone to dispatch now that its speaker is already characterized, got %v ok=%v", got2, ok)
	}
	if characterizeCalls != 1 {
		t.Fatalf("expected characterize not to run again once already characterized, got %d call(s)", characterizeCalls)
	}
}

// TestCharacterizationDependencyJoinsAnAlreadyQueuedTask confirms a second,
// unrelated dependency lookup for the same still-uncharacterized speaker
// joins the same already-queued task rather than creating a duplicate.
func TestCharacterizationDependencyJoinsAnAlreadyQueuedTask(t *testing.T) {
	s := openTestStore(t)
	book, _ := createBookAndChapter(t, s, "", 0, "Alice said hello.", "Alice said hi again.")
	if _, _, err := s.UpsertCharacter(store.SeriesScope(book), "Alice", false); err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}

	mgr := newTestManagerWithStore(s)
	mgr.characterize = func(ctx context.Context, bookID string, c store.Character) error { return nil }

	first := &task{kind: KindVoiceClone, bookID: book.ID, tier: TierBackground, paragraph: store.Paragraph{ID: "p-1", Speaker: "Alice"}}
	second := &task{kind: KindVoiceClone, bookID: book.ID, tier: TierBackground, paragraph: store.Paragraph{ID: "p-2", Speaker: "Alice"}}
	mgr.pushTask(first)
	mgr.pushTask(second)

	got, ok := mgr.queue.Pop()
	if !ok || got.(*task).kind != KindSpeakerCharacterization {
		t.Fatalf("expected exactly one shared characterization task to dispatch, got %+v ok=%v", got, ok)
	}
	// Neither clone can dispatch while that one shared task is in flight -
	// if a duplicate had been created instead, one of these would
	// (wrongly) succeed here.
	if _, ok := mgr.queue.Pop(); ok {
		t.Fatalf("expected both clones to stay blocked on the one shared characterization task")
	}
}

// TestCharacterizationDependencyIgnoresNarratorAndUnrelatedSpeakers
// confirms Narrator (no character at all) and a speaker nobody has asked
// to characterize never gain a dependency.
func TestCharacterizationDependencyIgnoresNarratorAndUnrelatedSpeakers(t *testing.T) {
	mgr := newTestManager() // no store at all - these must never even look one up
	cloneForNarrator := &task{kind: KindVoiceClone, tier: TierBackground, paragraph: store.Paragraph{ID: "p-1", Speaker: "Narrator"}}
	cloneForBlank := &task{kind: KindVoiceClone, tier: TierBackground, paragraph: store.Paragraph{ID: "p-2", Speaker: ""}}
	setQueue(mgr, cloneForNarrator, cloneForBlank)

	dispatched := popAll(mgr)
	if len(dispatched) != 2 {
		t.Fatalf("expected both tasks to dispatch with no dependency at all, got %d: %+v", len(dispatched), dispatched)
	}
}

// twoChapterBook creates a book with two chapters (one paragraph each),
// returning the book id and both chapters, in order - attributionOrderDependency
// tests' own shared setup.
func twoChapterBook(t *testing.T, s *store.DuckStore) (bookID string, ch0, ch1 *store.Chapter) {
	t.Helper()
	bookID, _, err := s.CreateBook("Book", "Author", "en", "", "", 0, []store.ChapterInput{
		{Title: "Ch0", Blocks: []store.BlockInput{{Kind: store.BlockText, Text: "Alice said hello."}}},
		{Title: "Ch1", Blocks: []store.BlockInput{{Kind: store.BlockText, Text: "Bob said hi."}}},
	})
	if err != nil {
		t.Fatalf("CreateBook: %v", err)
	}
	ch0, err = s.GetChapterByIdx(bookID, 0)
	if err != nil || ch0 == nil {
		t.Fatalf("GetChapterByIdx(0): %v", err)
	}
	ch1, err = s.GetChapterByIdx(bookID, 1)
	if err != nil || ch1 == nil {
		t.Fatalf("GetChapterByIdx(1): %v", err)
	}
	return bookID, ch0, ch1
}

// TestAttributionOrderDependencyBlocksThenClearsOncePreviousChapterAttributed
// is TestCharacterizationDependencyBlocksThenClearsOnceCharacterized's own
// attributionOrderDependency counterpart: chapter 1's own KindSpeakerAttribution
// task gets chapter 0's own attribution lazily created and dispatched ahead
// of it, and once Passes.Attribution is actually persisted for chapter 0, a
// fresh dependency check must find nothing left to do - the exact ordering
// guarantee attributeChapter's own live "already known characters" read
// needs to stay correct across a book's own chapters (see
// attributionOrderDependency's own doc comment).
func TestAttributionOrderDependencyBlocksThenClearsOncePreviousChapterAttributed(t *testing.T) {
	s := openTestStore(t)
	bookID, ch0, ch1 := twoChapterBook(t, s)

	attributeCalls := 0
	mgr := newTestManagerWithStore(s)
	mgr.attribute = func(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
		attributeCalls++
		return 0, nil, s.SetChapterAttributed(ch.ID)
	}

	laterChapterTask := &task{kind: KindSpeakerAttribution, bookID: bookID, chapterID: ch1.ID, chapterIdx: 1, tier: TierBackground, llmKey: ch1.ID}
	mgr.pushTask(laterChapterTask)

	got, ok := mgr.queue.Pop()
	if !ok {
		t.Fatalf("expected the lazily-created chapter-0 attribution task to dispatch")
	}
	blocker := got.(*task)
	if blocker.kind != KindSpeakerAttribution || blocker.chapterID != ch0.ID {
		t.Fatalf("expected a KindSpeakerAttribution task for chapter 0, got kind=%v chapterID=%q", blocker.kind, blocker.chapterID)
	}
	if _, ok := mgr.queue.Pop(); ok {
		t.Fatalf("expected chapter 1's attribution to stay blocked while chapter 0's is in flight")
	}
	if attributeCalls != 0 {
		t.Fatalf("expected attribute not to have actually run yet (task never dispatched via runLLM), got %d call(s)", attributeCalls)
	}

	// Simulate the dispatched task actually finishing - persists
	// Passes.Attribution via the fake attribute hook above.
	if _, _, err := blocker.runLLM(t.Context()); err != nil {
		t.Fatalf("runLLM: %v", err)
	}
	if attributeCalls != 1 {
		t.Fatalf("expected attribute to have run exactly once, got %d", attributeCalls)
	}
	mgr.queue.Finish(blocker.dedupKey())

	// Now that chapter 0 is actually attributed, chapter 1 must dispatch
	// directly - a fresh dependency check must NOT recreate another
	// chapter-0 attribution task.
	got2, ok := mgr.queue.Pop()
	if !ok || got2.(*task) != laterChapterTask {
		t.Fatalf("expected chapter 1's attribution to dispatch now that chapter 0 is already attributed, got %v ok=%v", got2, ok)
	}
	if attributeCalls != 1 {
		t.Fatalf("expected attribute not to run again once chapter 0 is already attributed, got %d call(s)", attributeCalls)
	}
}

// TestScareQuoteDependencyBlocksAttributionAndDescription covers
// scareQuoteDependency: a chapter's attribution and description tasks both
// wait on one lazily-created KindScareQuote task for that chapter (shared,
// not one each), a failed run gets recreated rather than letting them
// through, and once Passes.ScareQuote is persisted both dispatch without
// another scare-quote run.
func TestScareQuoteDependencyBlocksAttributionAndDescription(t *testing.T) {
	s := openTestStore(t)
	book, chapterID := createBookAndChapter(t, s, "", 0, "Some narration.")

	calls := 0
	fail := true
	mgr := newTestManagerWithStore(s)
	mgr.scareQuote = func(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
		calls++
		if fail {
			return 0, nil, errors.New("sampled a bad response")
		}
		return 0, nil, s.SetChapterScareQuoted(ch.ID)
	}

	attrTask := &task{kind: KindSpeakerAttribution, bookID: book.ID, chapterID: chapterID, tier: TierBackground, llmKey: chapterID}
	descTask := &task{kind: KindDescription, bookID: book.ID, chapterID: chapterID, tier: TierBackground, llmKey: chapterID}
	mgr.pushTask(attrTask)
	mgr.pushTask(descTask)

	runBlocker := func() {
		t.Helper()
		got, ok := mgr.queue.Pop()
		if !ok {
			t.Fatalf("expected the lazily-created scare-quote task to dispatch")
		}
		blocker := got.(*task)
		if blocker.kind != KindScareQuote || blocker.chapterID != chapterID {
			t.Fatalf("expected a KindScareQuote task for chapter %q, got kind=%v chapterID=%q", chapterID, blocker.kind, blocker.chapterID)
		}
		if _, ok := mgr.queue.Pop(); ok {
			t.Fatalf("expected attribution/description to stay blocked while scare-quote tagging is in flight")
		}
		_, _, _ = blocker.runLLM(t.Context())
		mgr.queue.Finish(blocker.dedupKey())
	}

	runBlocker() // fails - Passes.ScareQuote stays unset
	fail = false
	runBlocker() // recreated, and succeeds this time
	if calls != 2 {
		t.Fatalf("expected exactly two scare-quote runs (one failed, one retried), got %d", calls)
	}

	dispatched := map[*task]bool{}
	for range 2 {
		got, ok := mgr.queue.Pop()
		if !ok {
			t.Fatalf("expected attribution and description to dispatch once scare quotes are tagged")
		}
		dispatched[got.(*task)] = true
	}
	if !dispatched[attrTask] || !dispatched[descTask] {
		t.Fatalf("expected both the attribution and description tasks to dispatch, got %v", dispatched)
	}
	if calls != 2 {
		t.Fatalf("expected no further scare-quote run once the chapter is tagged, got %d", calls)
	}
}

// TestAttributionOrderDependencyJoinsAnAlreadyQueuedTask covers the actual
// production shape (preprocessAttributionPhase/"Attribute all" pushing
// every not-yet-attributed chapter's own task at once, before any of them
// dispatches): chapter 1's task must join chapter 0's own already-queued
// task rather than creating a second, competing one for the same chapter.
func TestAttributionOrderDependencyJoinsAnAlreadyQueuedTask(t *testing.T) {
	s := openTestStore(t)
	bookID, ch0, ch1 := twoChapterBook(t, s)

	mgr := newTestManagerWithStore(s)
	mgr.attribute = func(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
		t.Fatalf("expected chapter 0's own already-queued task to be reused, not a lazily-created one calling m.attribute")
		return 0, nil, nil
	}

	earlierChapterTask := &task{kind: KindSpeakerAttribution, bookID: bookID, chapterID: ch0.ID, chapterIdx: 0, tier: TierBackground, llmKey: ch0.ID,
		runLLM: func(ctx context.Context) (int, func(), error) { return 0, nil, s.SetChapterAttributed(ch0.ID) },
	}
	laterChapterTask := &task{kind: KindSpeakerAttribution, bookID: bookID, chapterID: ch1.ID, chapterIdx: 1, tier: TierBackground, llmKey: ch1.ID}
	mgr.pushTask(earlierChapterTask)
	mgr.pushTask(laterChapterTask)

	got, ok := mgr.queue.Pop()
	if !ok || got.(*task) != earlierChapterTask {
		t.Fatalf("expected chapter 0's own real task to dispatch first, got %v ok=%v", got, ok)
	}
	if _, ok := mgr.queue.Pop(); ok {
		t.Fatalf("expected chapter 1's attribution to stay blocked while chapter 0's is in flight")
	}

	if _, _, err := earlierChapterTask.runLLM(t.Context()); err != nil {
		t.Fatalf("runLLM: %v", err)
	}
	mgr.queue.Finish(earlierChapterTask.dedupKey())

	got2, ok := mgr.queue.Pop()
	if !ok || got2.(*task) != laterChapterTask {
		t.Fatalf("expected chapter 1's attribution to dispatch once chapter 0's own task finishes, got %v ok=%v", got2, ok)
	}
}

// TestGlobalAttributionSlotDependencyBlocksAcrossDifferentBooks is
// globalAttributionSlotDependency's own reason to exist: two different
// books' own chapter-0 attribution tasks have no attributionOrderDependency
// relationship at all (chapterIdx 0 always skips it - see its own
// "prevIdx < 0" check), and maxAttributionInFlight (2 in this test, via
// newTestManager) would happily admit both into poolLLM at once - yet only
// one may actually dispatch, since both are KindSpeakerAttribution.
func TestGlobalAttributionSlotDependencyBlocksAcrossDifferentBooks(t *testing.T) {
	mgr := newTestManager()

	bookATask := &task{kind: KindSpeakerAttribution, bookID: "book-A", chapterID: "book-A-ch0", chapterIdx: 0, tier: TierBackground, llmKey: "book-A-ch0"}
	bookBTask := &task{kind: KindSpeakerAttribution, bookID: "book-B", chapterID: "book-B-ch0", chapterIdx: 0, tier: TierBackground, llmKey: "book-B-ch0"}
	mgr.pushTask(bookATask)
	mgr.pushTask(bookBTask)

	got, ok := mgr.queue.Pop()
	if !ok {
		t.Fatalf("expected one of the two books' attribution tasks to dispatch")
	}
	first := got.(*task)

	if _, ok := mgr.queue.Pop(); ok {
		t.Fatalf("expected the other book's attribution task to stay blocked while the first is in flight")
	}

	mgr.queue.Finish(first.dedupKey())

	got2, ok := mgr.queue.Pop()
	if !ok {
		t.Fatalf("expected the other book's attribution task to dispatch once the first finished")
	}
	if got2.(*task) == first {
		t.Fatalf("expected the second dispatch to be the OTHER book's task, got the same one again")
	}
}

// TestSpeechDirectionDependencyBlocksThenClearsOnceTagged is
// TestCharacterizationDependencyBlocksThenClearsOnceCharacterized's own
// speechDirectionDependency counterpart: an untagged chapter
// (ch.State != store.ChapterStateTagged) gets a real KindSpeechDirection
// task lazily created and dispatched ahead of the paragraph that depends
// on it, and once ChapterStateTagged is actually persisted, a fresh
// dependency check must find nothing left to do - the exact behavior the
// reported bug ("jobs keep requeuing the same direction tagging task
// instead of recognizing it's already done") required.
func TestSpeechDirectionDependencyBlocksThenClearsOnceTagged(t *testing.T) {
	s := openTestStore(t)
	book, chapterID := createBookAndChapter(t, s, "", 0, "Some narration.")
	// speechDirectionDependency is gated on book.SpeechDirection (off by
	// default - see store.Book.SpeechDirection's own doc comment), so this
	// test's whole premise (the dependency blocks until tagged) needs it
	// explicitly turned on.
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, "audiocpp-higgs-4b", book.CharacterVoiceMode, true, false); err != nil {
		t.Fatalf("UpdateVoice(SpeechDirection=true): %v", err)
	}

	directCalls := 0
	mgr := newTestManagerWithStore(s)
	mgr.narration = narration.NewResolver(s)
	mgr.direct = func(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
		directCalls++
		if err := s.SetChapterDirected(ch.ID); err != nil {
			return 0, nil, err
		}
		return 0, nil, nil
	}

	cloneTask := &task{kind: KindVoiceClone, bookID: book.ID, chapterID: chapterID, tier: TierBackground, paragraph: store.Paragraph{ID: "p-1"}}
	mgr.pushTask(cloneTask)

	got, ok := mgr.queue.Pop()
	if !ok {
		t.Fatalf("expected the lazily-created direction task to dispatch")
	}
	blocker := got.(*task)
	if blocker.kind != KindSpeechDirection || blocker.chapterID != chapterID {
		t.Fatalf("expected a KindSpeechDirection task for chapter %q, got kind=%v chapterID=%q", chapterID, blocker.kind, blocker.chapterID)
	}
	if _, ok := mgr.queue.Pop(); ok {
		t.Fatalf("expected the clone to stay blocked while direction-tagging is in flight")
	}

	if _, _, err := blocker.runLLM(t.Context()); err != nil {
		t.Fatalf("runLLM: %v", err)
	}
	mgr.queue.Finish(blocker.dedupKey())

	got2, ok := mgr.queue.Pop()
	if !ok || got2.(*task) != cloneTask {
		t.Fatalf("expected the clone to dispatch now that its chapter is already tagged, got %v ok=%v", got2, ok)
	}
	if directCalls != 1 {
		t.Fatalf("expected direct not to run again once the chapter is already tagged, got %d call(s)", directCalls)
	}
}

// TestSpeechDirectionDependencyAppliesToAnyCloneModel confirms emotion
// labeling (KindSpeechDirection) gates generation for a non-Higgs book
// too - emotion reaches TTS as a reference-clip variant, so it matters
// under every clone model - and that once the chapter is directed the
// dependency settles instead of recreating a task on every check.
func TestSpeechDirectionDependencyAppliesToAnyCloneModel(t *testing.T) {
	s := openTestStore(t)
	book, chapterID := createBookAndChapter(t, s, "", 0, "Some narration.")
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, "audiocpp-pocket-100m", book.CharacterVoiceMode, true, false); err != nil {
		t.Fatalf("UpdateVoice(SpeechDirection=true): %v", err)
	}

	directCalls, pronounceCalls := 0, 0
	mgr := newTestManagerWithStore(s)
	mgr.narration = narration.NewResolver(s)
	mgr.direct = func(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
		directCalls++
		return 0, nil, s.SetChapterDirected(ch.ID)
	}
	mgr.pronounce = func(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
		pronounceCalls++
		return 0, nil, s.SetChapterPronounced(ch.ID)
	}

	cloneTask := &task{kind: KindVoiceClone, bookID: book.ID, chapterID: chapterID, tier: TierBackground, paragraph: store.Paragraph{ID: "p-1"}}
	mgr.pushTask(cloneTask)

	// Both chapter passes block the clone; run each as it dispatches.
	seen := map[Kind]bool{}
	for range 2 {
		got, ok := mgr.queue.Pop()
		if !ok {
			t.Fatalf("expected a blocking chapter pass to dispatch")
		}
		blocker := got.(*task)
		if blocker == cloneTask {
			t.Fatalf("clone dispatched before its chapter was labeled and pronounced (seen %v)", seen)
		}
		if blocker.chapterID != chapterID {
			t.Fatalf("blocker for the wrong chapter: %q", blocker.chapterID)
		}
		seen[blocker.kind] = true
		if _, _, err := blocker.runLLM(t.Context()); err != nil {
			t.Fatalf("runLLM: %v", err)
		}
		mgr.queue.Finish(blocker.dedupKey())
	}
	if !seen[KindSpeechDirection] || !seen[KindPronunciation] {
		t.Fatalf("expected both a direction and a pronunciation task, got %v", seen)
	}

	got, ok := mgr.queue.Pop()
	if !ok || got.(*task) != cloneTask {
		t.Fatalf("expected the clone to dispatch once its chapter is labeled, got %v ok=%v", got, ok)
	}
	if directCalls != 1 || pronounceCalls != 1 {
		t.Fatalf("expected exactly one run of each pass, got direct=%d pronounce=%d", directCalls, pronounceCalls)
	}
}

// TestSpeechDirectionDependencyOnlyAppliesToGenerationKinds confirms
// resolveDependencies' own kind switch: only KindVoiceClone/KindVoiceDesign
// ever check speechDirectionDependency at all - a KindSpeakerAttribution
// task for the very same chapter must never be blocked by it.
func TestSpeechDirectionDependencyOnlyAppliesToGenerationKinds(t *testing.T) {
	mgr := newTestManager() // m.direct is nil - if this were ever consulted for attribution, it would need one
	attribution := &task{kind: KindSpeakerAttribution, tier: TierBackground, bookID: "book-1", chapterID: "ch-1", llmKey: "ch-1"}
	setQueue(mgr, attribution)

	got, ok := mgr.queue.Pop()
	if !ok || got.(*task) != attribution {
		t.Fatalf("expected the attribution task to dispatch with no direction dependency at all, got %+v ok=%v", got, ok)
	}
}

// TestHasHigherPriorityWorkIgnoresSelfBlockedGeneration is the regression
// test for a real live bug: a chapter's own in-flight KindSpeechDirection
// task must never see that exact chapter's own paragraphs (TierUrgent/
// TierLookahead, but blocked on this very task via
// speechDirectionDependency) as a reason to report HasHigherPriorityWork
// - yielding would accomplish nothing (poolGeneration's own top candidate
// would still be blocked the instant it tried to dispatch), and the old,
// blind "any queued task at a more urgent tier" check made this pause
// after literally every batch, get requeued, and immediately redispatch
// again since nothing else outranked it - a pointless, confirmed-live
// pause/resume churn.
func TestHasHigherPriorityWorkIgnoresSelfBlockedGeneration(t *testing.T) {
	s := openTestStore(t)
	book, chapterID := createBookAndChapter(t, s, "", 0, "Some narration.")
	// speechDirectionDependency is gated on book.SpeechDirection (off by
	// default), so this test's whole premise (dispatch blocks on an
	// untagged chapter) needs it explicitly turned on - see
	// TestSpeechDirectionDependencyBlocksThenClearsOnceTagged's own doc
	// comment for the same setup.
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, "audiocpp-higgs-4b", book.CharacterVoiceMode, true, false); err != nil {
		t.Fatalf("UpdateVoice(SpeechDirection=true): %v", err)
	}

	mgr := newTestManagerWithStore(s)
	mgr.narration = narration.NewResolver(s)
	mgr.direct = func(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
		return 0, nil, nil
	}

	// The chapter's own paragraph, urgent, blocked on this exact chapter's
	// not-yet-tagged state - this is what a reader jumping into an
	// untagged chapter looks like.
	urgentClone := &task{kind: KindVoiceClone, tier: TierUrgent, bookID: book.ID, chapterID: chapterID, paragraph: store.Paragraph{ID: "p-1"}}
	mgr.pushTask(urgentClone)

	got, ok := mgr.queue.Pop()
	if !ok || got.(*task).kind != KindSpeechDirection {
		t.Fatalf("expected the lazily-created direction task to dispatch first, got %+v ok=%v", got, ok)
	}
	if mgr.HasHigherPriorityWork() {
		t.Fatalf("expected no higher-priority work - the only urgent task queued is blocked on this exact dispatch")
	}

	// An unrelated, genuinely eligible urgent task elsewhere must still
	// count normally.
	unrelatedUrgent := &task{kind: KindVoiceClone, tier: TierUrgent, bookID: "book-2", chapterID: "ch-2", paragraph: store.Paragraph{ID: "p-2"}}
	mgr.pushTask(unrelatedUrgent)
	if !mgr.HasHigherPriorityWork() {
		t.Fatalf("expected the unrelated, unblocked urgent task to count as higher-priority work")
	}
}

// --- Priority promotion ---

// TestCharacterizationDependencyPromotesBlockerAheadOfBackgroundWork
// confirms priority promotion actually works through the real
// resolveDependencies path (the generic mechanism itself - fixpoint
// relaxation over the dependency graph - is exhaustively tested against
// synthetic task types in internal/taskqueue's own TestPriorityPromotion;
// this is the jobs-level wiring of it): a background characterization
// task, lazily created for an urgent paragraph, must dispatch ahead of
// unrelated background work already queued, not queue behind it just
// because its own declared tier is merely TierBackground.
func TestCharacterizationDependencyPromotesBlockerAheadOfBackgroundWork(t *testing.T) {
	s := openTestStore(t)
	book, _ := createBookAndChapter(t, s, "", 0, "Alice said hello.")
	if _, _, err := s.UpsertCharacter(store.SeriesScope(book), "Alice", false); err != nil {
		t.Fatalf("UpsertCharacter: %v", err)
	}

	mgr := newTestManagerWithStore(s)
	mgr.characterize = func(ctx context.Context, bookID string, c store.Character) error { return nil }

	otherBackgroundWork := &task{kind: KindSpeakerAttribution, tier: TierBackground, bookID: book.ID, chapterID: "ch-other", llmKey: "ch-other"}
	urgentCloneForAlice := &task{kind: KindVoiceClone, tier: TierUrgent, bookID: book.ID, paragraph: store.Paragraph{ID: "p-1", Speaker: "Alice"}}
	mgr.pushTask(otherBackgroundWork)
	mgr.pushTask(urgentCloneForAlice)

	got, ok := mgr.queue.Pop()
	if !ok {
		t.Fatalf("expected something dispatchable")
	}
	bt := got.(*task)
	if bt.kind != KindSpeakerCharacterization {
		t.Fatalf("expected the lazily-created characterization task to dispatch first (promoted ahead of unrelated background work), got kind=%v", bt.kind)
	}
}

// --- pushTask dedup merge ---

// queuedTasks reads back mgr's own queued (not in-flight) tasks via
// Snapshot - non-destructive, unlike popAll, so a test can inspect the
// result more than once.
func queuedTasks(mgr *Manager) []*task {
	qs, _ := mgr.queue.Snapshot()
	out := make([]*task, len(qs))
	for i, e := range qs {
		out[i] = e.Task.(*task)
	}
	return out
}

// TestPushTaskMergeRefreshesVoiceOnAStillQueuedTask is the regression test
// for a real bug: a paragraph queued once under one resolved voice (e.g.
// by lookahead, before speaker attribution ever touched it), still
// sitting queued (not yet dispatched - poolGeneration was full, say) when
// something re-pushes it after its effective voice changed (attribution
// reassigning it to a character, a character's own voice reassignment,
// book/MultiVoice changes), used to dedup-merge into the existing entry
// touching only tier/chapterIdx/waiters, silently keeping the *first*
// push's now-stale voice fields. The paragraph would then render under
// the wrong voice, paragraphsNeedingGeneration would immediately see it
// as not-ready under the *current* voice, and queue a second, correct
// render right behind it - from a reader's perspective, a paragraph that
// had just finished generating flipping back to pending and re-rendering.
// pushTask's merge branch must instead pick up the newer push's voice.
func TestPushTaskMergeRefreshesVoiceOnAStillQueuedTask(t *testing.T) {
	mgr := newTestManager()

	staleVoice := &task{
		kind: KindVoiceClone, tier: TierLookahead, bookID: "book-1", chapterID: "ch-1",
		paragraph: store.Paragraph{ID: "p-1"},
		voiceID:   "voice-narrator", instruct: "old instruct", presetID: "preset-narrator",
	}
	mgr.pushTask(staleVoice)

	freshVoice := &task{
		kind: KindVoiceClone, tier: TierLookahead, bookID: "book-1", chapterID: "ch-1",
		paragraph: store.Paragraph{ID: "p-1", Speaker: "Alice"},
		voiceID:   "voice-alice", instruct: "new instruct", presetID: "preset-alice",
	}
	mgr.pushTask(freshVoice)

	queued := queuedTasks(mgr)
	if len(queued) != 1 {
		t.Fatalf("expected the second push to dedup-merge into the first (one queued task), got %d", len(queued))
	}
	got := queued[0]
	if got.voiceID != "voice-alice" || got.presetID != "preset-alice" || got.instruct != "new instruct" {
		t.Fatalf("expected the merge to refresh voice fields to the freshly-resolved voice, got voiceID=%q presetID=%q instruct=%q",
			got.voiceID, got.presetID, got.instruct)
	}
}

// TestPushTaskMergeNeverMutatesVoiceOnAnInFlightTask confirms the fix
// above is deliberately scoped to still-queued tasks only: an in-flight
// task's own generate() goroutine reads these same fields with no lock of
// its own, on the assumption nothing mutates them after dispatch, so
// mutating them concurrently here would be a genuine data race, not just
// a logical staleness bug - and there's nothing to correct anyway, since
// a render already underway can't retroactively change which voice it
// asked the worker for.
func TestPushTaskMergeNeverMutatesVoiceOnAnInFlightTask(t *testing.T) {
	mgr := newTestManager()

	// presetID deliberately left empty - referenceClipDependency (see its
	// own doc comment) only special-cases that as "not a real resolved
	// clone task", which would otherwise insert a lazily-created
	// KindVoiceProvision dependency ahead of it and make Pop below return
	// that instead of this task.
	inFlight := &task{
		kind: KindVoiceClone, tier: TierLookahead, bookID: "book-1", chapterID: "ch-1",
		paragraph: store.Paragraph{ID: "p-1"},
		voiceID:   "voice-narrator", instruct: "old instruct",
	}
	mgr.queue.Push(inFlight)
	popped, ok := mgr.queue.Pop() // moves it from queued into in-flight
	if !ok || popped.(*task) != inFlight {
		t.Fatalf("expected to pop the just-pushed task, got %+v ok=%v", popped, ok)
	}

	freshVoice := &task{
		kind: KindVoiceClone, tier: TierLookahead, bookID: "book-1", chapterID: "ch-1",
		paragraph: store.Paragraph{ID: "p-1", Speaker: "Alice"},
		voiceID:   "voice-alice", instruct: "new instruct", presetID: "preset-alice",
	}
	mgr.pushTask(freshVoice)

	if inFlight.voiceID != "voice-narrator" || inFlight.instruct != "old instruct" || inFlight.presetID != "" {
		t.Fatalf("expected the in-flight task's voice fields to stay untouched, got voiceID=%q instruct=%q presetID=%q",
			inFlight.voiceID, inFlight.instruct, inFlight.presetID)
	}
}

// TestHandleResultRetriesPoolLLMFailureThenGivesUp confirms handleResult's
// poolLLM retry-vs-give-up behavior: a genuine failure (e.g. a
// persistently malformed model response) is retried via the queue up to
// maxTaskAttempts (carrying attempt forward), an explicit reader Cancel
// (context.Canceled) is never resurrected by that retry regardless of
// remaining budget, and a cooperative pause (result.requeue non-nil)
// always takes priority over the generic retry path since it already
// carries the narrower onlyUnattributed-style continuation, strictly
// better than a full re-run.
func TestHandleResultRetriesPoolLLMFailureThenGivesUp(t *testing.T) {
	newTask := func() *task {
		return &task{
			kind:      KindSpeakerAttribution,
			tier:      TierBackground,
			bookID:    "book-1",
			chapterID: "ch-2",
			llmKey:    "ch-2",
			runLLM:    func(ctx context.Context) (int, func(), error) { return 0, nil, nil },
		}
	}
	dispatch := func(mgr *Manager, t *task) {
		mgr.pushTask(t)
		got, ok := mgr.queue.Pop()
		if !ok || got.(*task) != t {
			panic("test setup: expected to dispatch the task just pushed")
		}
	}

	// A genuine failure is retried via the queue itself - see
	// maxTaskAttempts/retryEligible - carrying attempt forward so the budget
	// is actually spent down.
	mgr := newTestManager()
	t2 := newTask()
	dispatch(mgr, t2)
	mgr.handleResult(t2, taskResult{err: errors.New("no JSON array found in response"), requeue: nil})
	if got := queuedTasks(mgr); len(got) != 1 {
		t.Fatalf("expected the failed task to be retried via the queue, got %d queued tasks", len(got))
	} else if got[0].attempt != 1 {
		t.Fatalf("expected the retried task's attempt to be 1, got %d", got[0].attempt)
	}

	// Once attempt has already reached maxTaskAttempts, the same kind of
	// failure must NOT be retried again - budget exhausted, so it's a real
	// failure now (no waiters here to actually observe that, since
	// KindSpeakerAttribution never has any - EnqueueAttribution - but the
	// queue must stay empty either way).
	mgr = newTestManager()
	t2Exhausted := newTask()
	t2Exhausted.attempt = maxTaskAttempts - 1 // already made the last dispatch this budget allows
	dispatch(mgr, t2Exhausted)
	mgr.handleResult(t2Exhausted, taskResult{err: errors.New("no JSON array found in response"), requeue: nil})
	if got := queuedTasks(mgr); len(got) != 0 {
		t.Fatalf("expected no further retry once attempt budget is exhausted, got %d queued tasks", len(got))
	}

	// An explicit reader Cancel produces a context.Canceled error - must NOT
	// be resurrected via retry (retryEligible excludes any context.Canceled
	// outright - see its own doc comment for why).
	mgr = newTestManager()
	t3 := newTask()
	t3.chapterID, t3.llmKey = "ch-3", "ch-3"
	dispatch(mgr, t3)
	mgr.handleResult(t3, taskResult{err: context.Canceled, requeue: nil})
	if got := queuedTasks(mgr); len(got) != 0 {
		t.Fatalf("expected no requeue for an explicitly-canceled task, got %d queued tasks", len(got))
	}

	// A cooperative pause (result.requeue non-nil) is always honored, with
	// no additional full-rerun requeue on top of it.
	mgr = newTestManager()
	requeueCalled := false
	t4 := newTask()
	t4.chapterID, t4.llmKey = "ch-4", "ch-4"
	dispatch(mgr, t4)
	mgr.handleResult(t4, taskResult{err: nil, requeue: func() { requeueCalled = true }})
	if !requeueCalled {
		t.Fatalf("expected result.requeue to be called")
	}
	if got := queuedTasks(mgr); len(got) != 0 {
		t.Fatalf("expected no additional full-rerun requeue when result.requeue already handled it, got %d queued tasks", len(got))
	}
}

// TestHandleResultRetriesGenerationFailureThenGivesUp is
// TestHandleResultRetriesPoolLLMFailureThenGivesUp's own poolGeneration
// counterpart: a genuine paragraph-generation failure retries via the queue
// (carrying attempt forward, same requeueCopy mechanism) up to
// maxTaskAttempts, an explicit reader Cancel (context.Canceled) is never
// resurrected by that retry regardless of remaining budget, and once budget
// is exhausted the paragraph is finally marked AudioError instead of
// retried again.
func TestHandleResultRetriesGenerationFailureThenGivesUp(t *testing.T) {
	s := openTestStore(t)
	_, chapterID := createBookAndChapter(t, s, "", 0, "Only paragraph.")
	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil || len(paragraphs) != 1 {
		t.Fatalf("ListParagraphsRaw: %v (n=%d)", err, len(paragraphs))
	}
	paragraph := paragraphs[0]
	const voiceID = "voice-1"
	audioErrorFor := func() string {
		t.Helper()
		states, err := s.ParagraphAudioStatuses([]string{paragraph.ID}, voiceID)
		if err != nil {
			t.Fatalf("ParagraphAudioStatuses: %v", err)
		}
		return states[paragraph.ID].Error
	}
	dispatch := func(mgr *Manager, tk *task) {
		mgr.pushTask(tk)
		got, ok := mgr.queue.Pop()
		if !ok || got.(*task) != tk {
			panic("test setup: expected to dispatch the task just pushed")
		}
	}

	// A genuine failure with budget remaining is retried via the queue.
	mgr := newTestManagerWithStore(s)
	t1 := &task{kind: KindVoiceClone, paragraph: paragraph, voiceID: voiceID}
	dispatch(mgr, t1)
	mgr.handleResult(t1, taskResult{err: errors.New("ttsworker: connection reset")})
	if got := queuedTasks(mgr); len(got) != 1 {
		t.Fatalf("expected the failed paragraph to be retried via the queue, got %d queued tasks", len(got))
	} else if got[0].attempt != 1 {
		t.Fatalf("expected the retried task's attempt to be 1, got %d", got[0].attempt)
	}
	if got := audioErrorFor(); got != "" {
		t.Fatalf("expected no AudioError persisted while a retry is still pending, got %q", got)
	}

	// An explicit reader Cancel produces a context.Canceled error - must go
	// straight to AudioError, never be silently resurrected by the retry
	// path meant for genuine failures.
	mgr = newTestManagerWithStore(s)
	t2 := &task{kind: KindVoiceClone, paragraph: paragraph, voiceID: voiceID}
	dispatch(mgr, t2)
	mgr.handleResult(t2, taskResult{err: context.Canceled})
	if got := queuedTasks(mgr); len(got) != 0 {
		t.Fatalf("expected no retry for an explicitly-canceled paragraph, got %d queued tasks", len(got))
	}
	if got := audioErrorFor(); got == "" {
		t.Fatalf("expected an explicitly-canceled paragraph to be marked AudioError, not silently retried")
	}

	// Once attempt already sits at the last dispatch this budget allows,
	// the same kind of genuine failure must finally give up.
	mgr = newTestManagerWithStore(s)
	t3 := &task{kind: KindVoiceClone, paragraph: paragraph, voiceID: voiceID, attempt: maxTaskAttempts - 1}
	dispatch(mgr, t3)
	mgr.handleResult(t3, taskResult{err: errors.New("ttsworker: connection reset")})
	if got := queuedTasks(mgr); len(got) != 0 {
		t.Fatalf("expected no further retry once attempt budget is exhausted, got %d queued tasks", len(got))
	}
	if got := audioErrorFor(); got == "" {
		t.Fatalf("expected the paragraph to be marked AudioError once budget is exhausted")
	}
}

// TestHandleResultDiscardsStaleGenerationText covers a clip whose
// paragraph's generation text changed while it rendered (a pronunciation
// re-run landing mid-generation): the result must not be saved - that
// would undo the re-run's audio invalidation with audio of the old text -
// and the paragraph is requeued carrying its current text instead.
func TestHandleResultDiscardsStaleGenerationText(t *testing.T) {
	s := openTestStore(t)
	_, chapterID := createBookAndChapter(t, s, "", 0, "Free demesnes.")
	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil || len(paragraphs) != 1 {
		t.Fatalf("ListParagraphsRaw: %v (n=%d)", err, len(paragraphs))
	}
	mgr := newTestManagerWithStore(s)
	tk := &task{kind: KindVoiceClone, paragraph: paragraphs[0], voiceID: "voice-1"}
	mgr.pushTask(tk)
	if got, ok := mgr.queue.Pop(); !ok || got.(*task) != tk {
		t.Fatal("test setup: expected to dispatch the task just pushed")
	}

	fix := []pronounce.Substitution{{Offset: 5, Length: 8, Replacement: "domains"}}
	if err := mgr.store.SetParagraphPronunciation(chapterID, map[int][]pronounce.Substitution{0: fix}); err != nil {
		t.Fatalf("SetParagraphPronunciation: %v", err)
	}
	mgr.handleResult(tk, taskResult{audio: []byte("stale")})

	got := queuedTasks(mgr)
	if len(got) != 1 {
		t.Fatalf("expected the stale paragraph to be requeued, got %d queued tasks", len(got))
	}
	if text := got[0].paragraph.ResolveGenerationText(""); text != "Free domains." {
		t.Errorf("requeued generation text = %q, want %q", text, "Free domains.")
	}
	if got[0].attempt != 0 {
		t.Errorf("requeued attempt = %d, want 0 (a stale result isn't a failure)", got[0].attempt)
	}
	if st, err := s.GetParagraphAudioStatus(paragraphs[0].ID, "voice-1"); err == nil && st == store.AudioReady {
		t.Errorf("stale audio was saved as ready")
	}
}

// TestUrgentGenerationDispatchesDespitePoolLLMBacklog is a real end-to-end
// regression test (driven through Manager.Start's actual worker loop, not
// just handleResult in isolation the way the tests above check it) for a
// live starvation bug: a background KindSpeakerCharacterization backlog
// much bigger than poolLLM's own maxAttributionInFlight slots, combined
// with an urgent poolDesign voice-provisioning task needing the
// mutual-exclusion gate to close, would keep poolLLM occupied forever if
// it kept backfilling its own slots from that backlog instead of letting
// itself drain once poolDesign's own urgent work is queued and waiting -
// confirmed live in production: a 150-chapter "Attribute all" backlog
// starving an urgent voice-provisioning task indefinitely. Fixed
// generically now by internal/taskqueue.Queue's own graceful-drain rule
// (see its own doc comment) - this test would time out without it, since
// the backlog here is deliberately large enough that draining it *without*
// that rule (rather than just letting the one in-flight batch finish)
// would take far longer than this test's own assertion window.
//
// This package never force-cancels any in-flight work at all - each
// background task here finishes on its own after a short simulated delay,
// the same shape a real CharacterizeVoice call finishing normally would
// take, not something that needs to be canceled to make room.
func TestUrgentGenerationDispatchesDespitePoolLLMBacklog(t *testing.T) {
	mgr := newTestManager()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)

	// Deep enough that repeatedly backfilling poolLLM's own slots from this
	// backlog (rather than letting it drain to zero after the first batch)
	// would take much longer than the 1s dispatch timeout below to work
	// through: backlogSize/maxAttributionInFlight rounds of taskDelay each.
	// Built directly via pushTask, the same shape EnqueueCharacterization
	// itself pushes (kind/tier/llmKey/label/runLLM) - not through
	// EnqueueCharacterization itself, which also calls m.seriesFor(bookID)
	// (a real store.Store lookup this lightweight, store-free test setup
	// doesn't have, same as every other test in this file that constructs
	// *task values directly rather than going through the store-backed
	// public API).
	const backlogSize = maxAttributionInFlight * 50
	const taskDelay = 15 * time.Millisecond
	for i := 0; i < backlogSize; i++ {
		name := fmt.Sprintf("Char %d", i)
		mgr.pushTask(&task{
			kind:   KindSpeakerCharacterization,
			tier:   TierBackground,
			bookID: "book-1",
			llmKey: fmt.Sprintf("char-%d", i),
			label:  name,
			runLLM: func(ctx context.Context) (int, func(), error) {
				time.Sleep(taskDelay)
				return 0, nil, nil
			},
		})
	}

	// Give the backlog a real chance to actually fill every poolLLM slot
	// before the urgent generation task shows up - otherwise the race
	// this test exists to catch never has a chance to occur at all.
	time.Sleep(taskDelay / 2)

	// A result channel, not a direct t.Errorf/t.Fatalf call, since this
	// runs in its own goroutine: if the outer test has already returned
	// (e.g. the select below hit its own timeout and failed first), calling
	// a *testing.T method from a goroutine that outlives the test panics
	// the whole binary ("Log in goroutine after ... has completed").
	type provisionResult struct {
		presetID string
		err      error
	}
	resultCh := make(chan provisionResult, 1)
	go func() {
		presetID, err := mgr.RunVoiceProvision(ctx, "book-1", "char-urgent", "generate", "Urgent Character", func(ctx context.Context, attempt int) (string, error) {
			return "preset-1", nil
		})
		resultCh <- provisionResult{presetID: presetID, err: err}
	}()

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("RunVoiceProvision: %v", result.err)
		}
		if result.presetID != "preset-1" {
			t.Fatalf("RunVoiceProvision returned %q, want %q", result.presetID, "preset-1")
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatalf("urgent poolDesign task never dispatched - poolLLM kept refilling its own drained slots from the background backlog instead of letting the mutual-exclusion gate close")
	}
}

// --- Cancel / CancelAll ---

func TestCancelRemovesQueuedTask(t *testing.T) {
	s := openTestStore(t)
	fake := ttsworkertest.New(t)
	mgr := NewManager(s, fake.Manager(), t.TempDir())

	book, chapterID := createBookAndChapter(t, s, "", 0, "p1", "p2")
	mgr.enqueueChapter(t.Context(), book.ID, chapterID, 0, 0) // synchronous, no worker started to drain it

	// 2 paragraph clone tasks - the shared "ensure the book's own
	// (cold-cache) reference clip" dependency (see
	// referenceClipDependency) is only ever created lazily the first time
	// something actually tries to dispatch a task (Pop), which never
	// happens here since Start is never called - Snapshot itself is
	// deliberately side-effect-free (see its own doc comment) and never
	// triggers it either.
	inFlight, queued := mgr.Snapshot()
	if len(inFlight) != 0 || len(queued) != 2 {
		t.Fatalf("expected 2 queued paragraph tasks and 0 in flight before Start, got inFlight=%d queued=%d", len(inFlight), len(queued))
	}

	id := queued[0].ID
	if !mgr.Cancel(id) {
		t.Fatalf("expected Cancel to report success for a queued task")
	}
	if mgr.Cancel(id) {
		t.Fatalf("expected a second Cancel of the same id to report false")
	}

	_, queued = mgr.Snapshot()
	if len(queued) != 1 {
		t.Fatalf("expected 1 remaining queued task after Cancel, got %d", len(queued))
	}
}

func TestCancelAllClearsEverythingQueued(t *testing.T) {
	s := openTestStore(t)
	fake := ttsworkertest.New(t)
	mgr := NewManager(s, fake.Manager(), t.TempDir())

	book, chapterID := createBookAndChapter(t, s, "", 0, "p1", "p2", "p3")
	mgr.enqueueChapter(t.Context(), book.ID, chapterID, 0, 0)

	// 3 paragraph clone tasks - see TestCancelRemovesQueuedTask's own
	// comment on why no reference-clip dependency task exists yet.
	n := mgr.CancelAll()
	if n != 3 {
		t.Fatalf("expected CancelAll to report 3 tasks acted on, got %d", n)
	}
	inFlight, queued := mgr.Snapshot()
	if len(inFlight) != 0 || len(queued) != 0 {
		t.Fatalf("expected an empty queue after CancelAll, got inFlight=%d queued=%d", len(inFlight), len(queued))
	}
}

// --- PromoteTier ---

func TestPromoteTierUpgradesQueuedTask(t *testing.T) {
	mgr := newTestManager()
	t1 := newGenTask("book-1", "ch-1", 0, 0, TierBackground, "", 0)
	if !mgr.queue.Push(t1) {
		t.Fatalf("push failed")
	}

	if err := mgr.PromoteTier(t1.dedupKey(), TierUrgent); err != nil {
		t.Fatalf("PromoteTier: %v", err)
	}

	_, queued := mgr.Snapshot()
	if len(queued) != 1 || queued[0].Tier != TierUrgent {
		t.Fatalf("expected the promoted task to report TierUrgent, got %+v", queued)
	}
}

// TestPromoteTierRejectsDowngrade and TestPromoteTierRejectsSameTier check
// PromoteTier's own core contract - upgrade only, never a general
// reprioritize - see its own doc comment for why: a task something else
// has already promoted urgent must never quietly report a less urgent
// tier again.
func TestPromoteTierRejectsDowngrade(t *testing.T) {
	mgr := newTestManager()
	t1 := newGenTask("book-1", "ch-1", 0, 0, TierUrgent, "", 0)
	mgr.queue.Push(t1)

	if err := mgr.PromoteTier(t1.dedupKey(), TierBackground); !errors.Is(err, ErrTierNotMoreUrgent) {
		t.Fatalf("expected ErrTierNotMoreUrgent, got %v", err)
	}
	_, queued := mgr.Snapshot()
	if queued[0].Tier != TierUrgent {
		t.Fatalf("expected the task's own tier to stay TierUrgent, got %d", queued[0].Tier)
	}
}

func TestPromoteTierRejectsSameTier(t *testing.T) {
	mgr := newTestManager()
	t1 := newGenTask("book-1", "ch-1", 0, 0, TierLookahead, "", 0)
	mgr.queue.Push(t1)

	if err := mgr.PromoteTier(t1.dedupKey(), TierLookahead); !errors.Is(err, ErrTierNotMoreUrgent) {
		t.Fatalf("expected ErrTierNotMoreUrgent for a same-tier request, got %v", err)
	}
}

func TestPromoteTierRejectsInvalidTierValue(t *testing.T) {
	mgr := newTestManager()
	t1 := newGenTask("book-1", "ch-1", 0, 0, TierBackground, "", 0)
	mgr.queue.Push(t1)

	if err := mgr.PromoteTier(t1.dedupKey(), 42); !errors.Is(err, ErrInvalidTier) {
		t.Fatalf("expected ErrInvalidTier, got %v", err)
	}
}

func TestPromoteTierReturnsNotFoundForUnknownID(t *testing.T) {
	mgr := newTestManager()
	if err := mgr.PromoteTier("does-not-exist", TierUrgent); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("expected ErrTaskNotFound, got %v", err)
	}
}

// TestPauseStopsNewDispatchButNotInFlight confirms Pause's own contract:
// no *new* task is dispatched while paused (queued work just waits), but
// nothing already in flight is force-cancelled, and Resume lets dispatch
// continue exactly where it left off.
func TestPauseStopsNewDispatchButNotInFlight(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)

	book, chapterID := createBookAndChapter(t, s, "", 0, "First paragraph.", "Second paragraph.")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)

	if mgr.Paused() {
		t.Fatalf("expected a fresh Manager not to start paused")
	}
	mgr.Pause()
	if !mgr.Paused() {
		t.Fatalf("expected Paused() to report true right after Pause")
	}

	mgr.EnqueueChapter(book.ID, chapterID, 0)

	// Give the worker loop a real chance to (wrongly) dispatch something
	// if Pause didn't actually take effect - a generous margin given the
	// fake ttsworker resolves near-instantly, so any real dispatch would
	// already show up well within this window.
	time.Sleep(100 * time.Millisecond)
	inFlight, queued := mgr.Snapshot()
	// The chapter's own pipeline_generate_chapter row stays in flight
	// while it waits on its paragraphs (see waitForChapterAudio) - that's
	// bookkeeping on pipelineQueue, not dispatched paragraph work.
	generateRow := false
	for _, qt := range inFlight {
		if qt.Kind == "pipeline_generate_chapter" {
			generateRow = true
			continue
		}
		t.Fatalf("expected no paragraph work dispatched while paused, got %s in flight", qt.Kind)
	}
	if !generateRow {
		t.Fatalf("expected the chapter's generate row to stay in flight while its paragraphs wait")
	}
	if len(queued) == 0 {
		t.Fatalf("expected the enqueued paragraphs to still be sitting queued while paused")
	}

	mgr.Resume()
	if mgr.Paused() {
		t.Fatalf("expected Paused() to report false right after Resume")
	}

	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, bookVoice.VoiceID()) })
}

// TestGenerateChapterRowLastsUntilAudioDoneAndCancelDropsQueued covers
// waitForChapterAudio: the pipeline_generate_chapter row stays in flight
// while its chapter's paragraphs are still queued, and canceling that row
// removes those still-queued paragraphs rather than leaving them to run.
func TestGenerateChapterRowLastsUntilAudioDoneAndCancelDropsQueued(t *testing.T) {
	fake := ttsworkertest.New(t)
	s := openTestStore(t)
	mgr := NewManager(s, fake.Manager(), t.TempDir())
	book, chapterID := createBookAndChapter(t, s, "", 0, "First paragraph.", "Second paragraph.")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)
	mgr.Pause() // keep the paragraphs queued

	mgr.EnqueueChapter(book.ID, chapterID, 0)

	var rowID string
	waitFor(t, 2*time.Second, func() bool {
		inFlight, _ := mgr.Snapshot()
		for _, qt := range inFlight {
			if qt.Kind == "pipeline_generate_chapter" {
				rowID = qt.ID
				return true
			}
		}
		return false
	})
	time.Sleep(700 * time.Millisecond) // past waitForChapterAudio's empty-queue grace period
	if inFlight, _ := mgr.Snapshot(); len(inFlight) == 0 {
		t.Fatalf("expected the generate row to still be in flight while its paragraphs are queued")
	}

	if !mgr.Cancel(rowID) {
		t.Fatalf("Cancel(%q) found nothing", rowID)
	}
	waitFor(t, 2*time.Second, func() bool {
		inFlight, queued := mgr.Snapshot()
		return len(inFlight) == 0 && len(queued) == 0
	})
}

// TestMusicGenerationWaitsForWholeChapterVoiced covers
// MaybeAdvanceChapterMusic's whole-chapter gate: a region whose own
// paragraphs are all ready still doesn't generate while any other
// paragraph in the chapter lacks narration; once the last one is ready,
// the chapter's music batch is queued.
func TestMusicGenerationWaitsForWholeChapterVoiced(t *testing.T) {
	s := openTestStore(t)
	book, chapterID := createBookAndChapter(t, s, "", 0, "First paragraph.", "Second paragraph.")
	if err := s.UpdateVoice(book.ID, book.VoicePresetID, book.VoiceInstruct, book.VoiceLanguage, book.VoiceSeed, book.CloneModel, book.CharacterVoiceMode, false, true); err != nil {
		t.Fatalf("UpdateVoice(MusicEnabled=true): %v", err)
	}
	book, _ = s.GetBook(book.ID)
	// One region covering only paragraph 0.
	if _, err := s.AppendMusicRegions(chapterID, []store.MusicRegionInput{{StartIdx: 0, Mood: "calm", Prompt: "calm music"}}, 0); err != nil {
		t.Fatalf("AppendMusicRegions: %v", err)
	}

	mgr := newTestManagerWithStore(s)
	mgr.narration = narration.NewResolver(s)
	voice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil || len(paragraphs) != 2 {
		t.Fatalf("ListParagraphsRaw: %v (%d)", err, len(paragraphs))
	}
	musicQueued := func() bool {
		_, ok := mgr.queue.Find(func(c taskqueue.Task) bool { return c.Key() == "music_gen:"+chapterID })
		return ok
	}

	if err := s.SetParagraphReady(paragraphs[0].ID, voice.VoiceID(), 2.5); err != nil {
		t.Fatalf("SetParagraphReady(0): %v", err)
	}
	mgr.MaybeAdvanceChapterMusic(book.ID, chapterID)
	if musicQueued() {
		t.Fatalf("expected no music generation while paragraph 1 is still unvoiced")
	}

	if err := s.SetParagraphReady(paragraphs[1].ID, voice.VoiceID(), 3.0); err != nil {
		t.Fatalf("SetParagraphReady(1): %v", err)
	}
	mgr.MaybeAdvanceChapterMusic(book.ID, chapterID)
	if !musicQueued() {
		t.Fatalf("expected the chapter's music batch to be queued once every paragraph is voiced")
	}
}

// scareQuoteTestBlocks builds a chapter of the shape splitQuoteSegments
// would really produce for "Their so-called "expert," had no idea what he
// was doing." followed by an unrelated control paragraph: a narration
// fragment, an Inline quoted fragment (the eventual scare quote), another
// Inline narration fragment continuing the same original sentence, then a
// wholly separate paragraph with no Inline links to anything.
func scareQuoteTestBlocks() []store.BlockInput {
	return []store.BlockInput{
		{Kind: store.BlockText, Text: "Their so-called"},
		{Kind: store.BlockText, Text: "“expert,”", Inline: true, IsQuote: true},
		{Kind: store.BlockText, Text: "had no idea what he was doing.", Inline: true},
		{Kind: store.BlockText, Text: "Unrelated second paragraph, on its own."},
	}
}

func TestScareQuoteMergeGroupFindsFullRunFromAnyMember(t *testing.T) {
	s := openTestStore(t)
	_, chapterID := createBookWithBlocks(t, s, scareQuoteTestBlocks()...)
	if err := s.SetParagraphScareQuotes(chapterID, map[int]bool{1: true}); err != nil {
		t.Fatalf("SetParagraphScareQuotes: %v", err)
	}
	mgr := newTestManagerWithStore(s)

	for _, anchor := range []int{0, 1, 2} {
		all, err := s.ListParagraphsRaw(chapterID)
		if err != nil {
			t.Fatalf("ListParagraphsRaw: %v", err)
		}
		group := mgr.scareQuoteMergeGroup(chapterID, all[anchor])
		if len(group) != 3 {
			t.Fatalf("anchor idx %d: expected a 3-paragraph group, got %d", anchor, len(group))
		}
		for i, p := range group {
			if p.Idx != i {
				t.Fatalf("anchor idx %d: expected group in idx order [0,1,2], got idx %d at position %d", anchor, p.Idx, i)
			}
		}
	}

	// The unrelated fourth paragraph has no Inline links at all - always
	// alone regardless of the scare quote elsewhere in the chapter.
	all, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	if group := mgr.scareQuoteMergeGroup(chapterID, all[3]); len(group) != 1 {
		t.Fatalf("expected the unrelated paragraph to stay alone, got a group of %d", len(group))
	}
}

func TestScareQuoteMergeGroupRequiresAScareQuoteSomewhereInTheRun(t *testing.T) {
	s := openTestStore(t)
	// Same Inline shape as scareQuoteTestBlocks (an ordinary "quote, then
	// narration tag" split), but never flagged ScareQuote - the single most
	// common real shape in a book, which must never merge.
	_, chapterID := createBookWithBlocks(t, s,
		store.BlockInput{Kind: store.BlockText, Text: "“Let me free,”", IsQuote: true},
		store.BlockInput{Kind: store.BlockText, Text: "he sputtered.", Inline: true},
	)
	mgr := newTestManagerWithStore(s)
	all, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	if group := mgr.scareQuoteMergeGroup(chapterID, all[0]); len(group) != 1 {
		t.Fatalf("expected no merge without a ScareQuote flag anywhere in the run, got a group of %d", len(group))
	}
}

func TestScareQuoteMergeGroupStopsAtADifferentVoiceWithoutBlockingTheRest(t *testing.T) {
	s := openTestStore(t)
	_, chapterID := createBookWithBlocks(t, s, scareQuoteTestBlocks()...)
	if err := s.SetParagraphScareQuotes(chapterID, map[int]bool{1: true}); err != nil {
		t.Fatalf("SetParagraphScareQuotes: %v", err)
	}
	// A wrong/stray attribution landing on one member of the run (Speaker
	// set to a named character rather than Narrator) must never itself be
	// pulled into a merge - merging text meant for two different voices
	// into one clone call would render the wrong voice for part of it -
	// but it's only a wall the walk stops at, not a reason to refuse
	// merging anything else nearby: idx 0/1 still merge with each other
	// (nothing about their own boundary changed), they just never reach
	// across idx 2 to do it.
	if err := s.SetParagraphSpeakers(chapterID, map[int]string{2: "Fritz"}); err != nil {
		t.Fatalf("SetParagraphSpeakers: %v", err)
	}
	mgr := newTestManagerWithStore(s)
	all, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	for _, anchor := range []int{0, 1} {
		group := mgr.scareQuoteMergeGroup(chapterID, all[anchor])
		if len(group) != 2 || group[0].Idx != 0 || group[1].Idx != 1 {
			t.Fatalf("anchor idx %d: expected a 2-paragraph group [0,1] stopping at the Fritz-attributed paragraph, got %d paragraphs", anchor, len(group))
		}
	}
	if group := mgr.scareQuoteMergeGroup(chapterID, all[2]); len(group) != 1 {
		t.Fatalf("expected the Fritz-attributed paragraph itself to never merge, got a group of %d", len(group))
	}
}

// TestScareQuoteMergeGroupNeverConsumesActualDialogue confirms a genuine,
// still-spoken quote elsewhere in the same Inline run - one that's IsQuote
// but not itself ScareQuote - is never itself pulled into a merge, even
// before attribution has ever run over it (Speaker == "", indistinguishable
// from plain narration at that point) - and that it only walls off the
// walk from reaching it, not the whole chain either side of it. Without
// the wall, a reader manually scare-quoting one segment of an unattributed
// inline run (which needs no attribution to have happened first at all -
// see handleSetParagraphScareQuote) could otherwise pull someone else's
// real, not-yet-attributed line into the same merged, narrator-voiced clip.
func TestScareQuoteMergeGroupNeverConsumesActualDialogue(t *testing.T) {
	s := openTestStore(t)
	_, chapterID := createBookWithBlocks(t, s,
		store.BlockInput{Kind: store.BlockText, Text: "Their so-called"},
		store.BlockInput{Kind: store.BlockText, Text: "“expert,”", Inline: true, IsQuote: true},
		store.BlockInput{Kind: store.BlockText, Text: "said the narrator, but then", Inline: true},
		store.BlockInput{Kind: store.BlockText, Text: "“I really am one,”", Inline: true, IsQuote: true},
		store.BlockInput{Kind: store.BlockText, Text: "he insisted.", Inline: true},
	)
	if err := s.SetParagraphScareQuotes(chapterID, map[int]bool{1: true}); err != nil {
		t.Fatalf("SetParagraphScareQuotes: %v", err)
	}
	mgr := newTestManagerWithStore(s)
	all, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	// Idx 3 is a real quote (IsQuote), not scare-quoted, still Speaker == ""
	// - exactly the pre-attribution state a genuine line of dialogue sits
	// in before attributeChapter ever reaches it.
	if all[3].Speaker != "" || !all[3].IsQuote || all[3].ScareQuote {
		t.Fatalf("test setup: expected idx 3 to be an unattributed real quote, got %+v", all[3])
	}
	// Idx 0-2 (the scare quote's own narrator-voiced run, connected by
	// unterminated boundaries - see segmentEndsSentence) still merge with
	// each other; the walk just stops dead at idx 3's real dialogue rather
	// than crossing into it.
	for _, anchor := range []int{0, 1, 2} {
		group := mgr.scareQuoteMergeGroup(chapterID, all[anchor])
		if len(group) != 3 || group[0].Idx != 0 || group[1].Idx != 1 || group[2].Idx != 2 {
			t.Fatalf("anchor idx %d: expected a 3-paragraph group [0,1,2] stopping at the real quote, got %d paragraphs", anchor, len(group))
		}
	}
	for _, anchor := range []int{3, 4} {
		if group := mgr.scareQuoteMergeGroup(chapterID, all[anchor]); len(group) != 1 {
			t.Fatalf("anchor idx %d: expected no merge across a real, unattributed quote, got a group of %d", anchor, len(group))
		}
	}
}

func TestScareQuoteMergeGroupLeftOnlyWhenTheQuoteEndsItsOwnSentence(t *testing.T) {
	s := openTestStore(t)
	_, chapterID := createBookWithBlocks(t, s,
		store.BlockInput{Kind: store.BlockText, Text: "It was called"},
		store.BlockInput{Kind: store.BlockText, Text: "“a disaster.”", Inline: true, IsQuote: true},
		store.BlockInput{Kind: store.BlockText, Text: "Nothing else mattered.", Inline: true},
	)
	if err := s.SetParagraphScareQuotes(chapterID, map[int]bool{1: true}); err != nil {
		t.Fatalf("SetParagraphScareQuotes: %v", err)
	}
	mgr := newTestManagerWithStore(s)
	all, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	// The quote's own text ends its sentence ("a disaster.") right before
	// the closing quote mark, so it merges left into what led up to it but
	// not right into the fresh sentence that follows.
	group := mgr.scareQuoteMergeGroup(chapterID, all[1])
	if len(group) != 2 || group[0].Idx != 0 || group[1].Idx != 1 {
		t.Fatalf("expected a 2-paragraph left-only group [0,1], got %d paragraphs", len(group))
	}
}

func TestScareQuoteMergeGroupRightOnlyWhenThePreviousSegmentAlreadyEndedItsSentence(t *testing.T) {
	s := openTestStore(t)
	_, chapterID := createBookWithBlocks(t, s,
		store.BlockInput{Kind: store.BlockText, Text: "The rules changed overnight."},
		store.BlockInput{Kind: store.BlockText, Text: "“New rules,”", Inline: true, IsQuote: true},
		store.BlockInput{Kind: store.BlockText, Text: "drew immediate criticism.", Inline: true},
	)
	if err := s.SetParagraphScareQuotes(chapterID, map[int]bool{1: true}); err != nil {
		t.Fatalf("SetParagraphScareQuotes: %v", err)
	}
	mgr := newTestManagerWithStore(s)
	all, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	// The segment before the quote already ended its own sentence, so the
	// quote (itself ending in a bare comma, not a sentence of its own)
	// merges right into the sentence it starts, never left into the
	// already-complete one before it.
	group := mgr.scareQuoteMergeGroup(chapterID, all[1])
	if len(group) != 2 || group[0].Idx != 1 || group[1].Idx != 2 {
		t.Fatalf("expected a 2-paragraph right-only group [1,2], got %d paragraphs", len(group))
	}
}

func TestScareQuoteMergeGroupNeitherWhenBothSidesAreAlreadySentenceBoundaries(t *testing.T) {
	s := openTestStore(t)
	_, chapterID := createBookWithBlocks(t, s,
		store.BlockInput{Kind: store.BlockText, Text: "Then, silence."},
		store.BlockInput{Kind: store.BlockText, Text: "“Nothing.”", Inline: true, IsQuote: true},
		store.BlockInput{Kind: store.BlockText, Text: "Chaos returned moments later.", Inline: true},
	)
	if err := s.SetParagraphScareQuotes(chapterID, map[int]bool{1: true}); err != nil {
		t.Fatalf("SetParagraphScareQuotes: %v", err)
	}
	mgr := newTestManagerWithStore(s)
	all, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	// Both neighboring boundaries are already complete sentences on their
	// own (the previous segment ends in "." and so does the quote itself),
	// so nothing needs merging even though the paragraph is flagged a
	// scare quote - merging would be harmless but is simply unneeded here.
	if group := mgr.scareQuoteMergeGroup(chapterID, all[1]); len(group) != 1 {
		t.Fatalf("expected no merge when both boundaries are already sentence-complete, got a group of %d", len(group))
	}
}

// mergeTestWordSeconds is a fixed per-word duration this test's fake
// Generate/Align hooks both derive independently from the request text -
// keeping them mutually consistent (Align's own word End times always
// exactly matching Generate's own clip duration for the same text) is what
// lets splitMergedAudio locate a clean, in-bounds split point deterministically.
const mergeTestWordSeconds = 0.2

func wordConsistentTTS(t *testing.T, fake *ttsworkertest.Server) {
	t.Helper()
	fake.OnGenerate = func(req ttsproto.GenerateRequest) ([]byte, error) {
		n := len(strings.Fields(req.Text))
		seconds := float64(n) * mergeTestWordSeconds
		samples := make([]float32, int(seconds*mergeTestSampleRate)*mergeTestChannels)
		return wav.Encode(samples, mergeTestSampleRate, mergeTestChannels)
	}
	fake.OnAlign = func(req ttsproto.AlignRequest) ([]ttsproto.Word, error) {
		fields := strings.Fields(req.Text)
		words := make([]ttsproto.Word, len(fields))
		for i, f := range fields {
			words[i] = ttsproto.Word{Text: f, Start: float64(i) * mergeTestWordSeconds, End: float64(i+1) * mergeTestWordSeconds}
		}
		return words, nil
	}
}

const (
	mergeTestSampleRate = 16000
	mergeTestChannels   = 1
)

func TestManagerMergesScareQuoteGroupIntoOneGenerateCall(t *testing.T) {
	fake := ttsworkertest.New(t)
	wordConsistentTTS(t, fake)
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)

	book, chapterID := createBookWithBlocks(t, s, scareQuoteTestBlocks()...)
	if err := s.SetParagraphScareQuotes(chapterID, map[int]bool{1: true}); err != nil {
		t.Fatalf("SetParagraphScareQuotes: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)
	mgr.EnqueueChapter(book.ID, chapterID, 0)

	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	voiceID := bookVoice.VoiceID()
	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, voiceID) })

	// One merged Generate call for paragraphs 0-2, plus one independent
	// call for the unrelated paragraph 3 - never 4 separate calls.
	generateCalls := fake.GenerateCalls()
	if len(generateCalls) != 2 {
		t.Fatalf("expected 2 Generate calls (1 merged + 1 independent), got %d", len(generateCalls))
	}
	wantMergedText := "Their so-called “expert,” had no idea what he was doing."
	foundMerged := false
	for _, c := range generateCalls {
		if c.Text == wantMergedText {
			foundMerged = true
		}
	}
	if !foundMerged {
		t.Fatalf("expected one Generate call with the merged text %q, got calls: %+v", wantMergedText, generateCalls)
	}

	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}

	// Only the anchor (paragraph 0) ever gets a real file on disk - the
	// whole point of pointing every other member at it instead of slicing
	// out a separate, re-encoded clip per paragraph (see
	// jobs.Manager.handleMergedResult's own doc comment on why: a real,
	// observed source of audible artifacts at the cut point).
	anchorPath := audiopath.ParagraphFile(dataDir, book.ID, chapterID, voiceID, paragraphs[0].Idx)
	anchorData, err := os.ReadFile(anchorPath)
	if err != nil {
		t.Fatalf("read anchor audio file: %v", err)
	}
	anchorDur, err := clipDurationSeconds(anchorData)
	if err != nil {
		t.Fatalf("parse anchor duration: %v", err)
	}
	wantTotal := 10 * mergeTestWordSeconds // 2 + 1 + 7 words across the whole group
	if diff := anchorDur - wantTotal; diff > 0.02 || diff < -0.02 {
		t.Fatalf("anchor file: expected ~%.2fs (the whole merged clip), got %.2fs", wantTotal, anchorDur)
	}
	for i := 1; i <= 2; i++ {
		path := audiopath.ParagraphFile(dataDir, book.ID, chapterID, voiceID, paragraphs[i].Idx)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("paragraph %d: expected NO audio file of its own (should be a pointer), stat err: %v", i, err)
		}
	}

	ids := []string{paragraphs[0].ID, paragraphs[1].ID, paragraphs[2].ID}
	states, err := s.ParagraphAudioStatuses(ids, voiceID)
	if err != nil {
		t.Fatalf("ParagraphAudioStatuses: %v", err)
	}
	wantPointerOffset := []int{0, 1, 2}
	wantPointerSeconds := []float64{0, 2 * mergeTestWordSeconds, 3 * mergeTestWordSeconds} // word idx 2 and 3 start each later member
	wantDuration := []float64{2 * mergeTestWordSeconds, 1 * mergeTestWordSeconds, 7 * mergeTestWordSeconds}
	for i, id := range ids {
		st, ok := states[id]
		if !ok {
			t.Fatalf("paragraph %d: no audio state", i)
		}
		if st.Status != store.AudioReady {
			t.Fatalf("paragraph %d: expected ready, got %s", i, st.Status)
		}
		if st.PointerOffset != wantPointerOffset[i] {
			t.Fatalf("paragraph %d: expected pointerOffset %d, got %d", i, wantPointerOffset[i], st.PointerOffset)
		}
		if diff := st.PointerSeconds - wantPointerSeconds[i]; diff > 0.02 || diff < -0.02 {
			t.Fatalf("paragraph %d: expected pointerSeconds ~%.2f, got %.2f", i, wantPointerSeconds[i], st.PointerSeconds)
		}
		if diff := st.DurationSeconds - wantDuration[i]; diff > 0.02 || diff < -0.02 {
			t.Fatalf("paragraph %d: expected duration ~%.2f, got %.2f", i, wantDuration[i], st.DurationSeconds)
		}
		if st.WordTimings == "" || st.WordTimings == "[]" {
			t.Fatalf("paragraph %d: expected non-empty word timings", i)
		}
	}
}

func TestManagerFallsBackToIndependentGenerationWhenAlignmentFails(t *testing.T) {
	fake := ttsworkertest.New(t)
	fake.OnAlign = func(req ttsproto.AlignRequest) ([]ttsproto.Word, error) {
		return nil, fmt.Errorf("simulated alignment failure")
	}
	s := openTestStore(t)
	dataDir := t.TempDir()
	mgr := NewManager(s, fake.Manager(), dataDir)

	book, chapterID := createBookWithBlocks(t, s, scareQuoteTestBlocks()...)
	if err := s.SetParagraphScareQuotes(chapterID, map[int]bool{1: true}); err != nil {
		t.Fatalf("SetParagraphScareQuotes: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	mgr.Start(ctx)
	mgr.EnqueueChapter(book.ID, chapterID, 0)

	bookVoice, err := mgr.narration.BookVoice(book)
	if err != nil {
		t.Fatalf("BookVoice: %v", err)
	}
	voiceID := bookVoice.VoiceID()
	// Every paragraph still ends up with real, playable audio even though
	// every alignment call in this test fails outright - the whole point
	// of generateIndependently's own fallback.
	waitFor(t, 5*time.Second, func() bool { return allParagraphsReady(t, s, chapterID, voiceID) })

	paragraphs, err := s.ListParagraphsRaw(chapterID)
	if err != nil {
		t.Fatalf("ListParagraphsRaw: %v", err)
	}
	for _, p := range paragraphs {
		path := audiopath.ParagraphFile(dataDir, book.ID, chapterID, voiceID, p.Idx)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected a fallback-generated audio file for paragraph %d: %v", p.Idx, err)
		}
	}
	// The merged group's own combined Generate call still happens (and
	// succeeds - it's specifically alignment that fails here) - 1 for
	// that, then 3 more independent calls once splitMergedAudio can't
	// locate a boundary (generateIndependently's own per-paragraph loop),
	// plus 1 for the unrelated paragraph 3: 5 total, never a lone merged
	// call left unsplit with no way to recover its members' own audio.
	if got := len(fake.GenerateCalls()); got != 5 {
		t.Fatalf("expected 5 Generate calls (1 merged + 3 independent fallback + 1 unrelated), got %d", got)
	}
}

func clipDurationSeconds(data []byte) (float64, error) {
	d, err := oggopus.Duration(data)
	if err != nil {
		return 0, err
	}
	return d.Seconds(), nil
}

// TestVariantClipDependencyRendersThenFallsBack confirms an emotional
// dialogue line waits on its preset's emotion-variant render (one
// KindVoiceProvision task per preset+emotion), and that a variant marked
// failed stops blocking - the line then clones from the base clip.
func TestVariantClipDependencyRendersThenFallsBack(t *testing.T) {
	mgr := newTestManager()
	mgr.dataDir = t.TempDir()
	// Base clip already rendered, so only the variant is missing.
	if err := audiopath.EnsureVoiceRefDir(mgr.dataDir); err != nil {
		t.Fatalf("EnsureVoiceRefDir: %v", err)
	}
	if err := os.WriteFile(audiopath.VoicePresetRefFile(mgr.dataDir, "preset-a"), []byte("wav"), 0o644); err != nil {
		t.Fatalf("write base clip: %v", err)
	}

	clone := &task{
		kind: KindVoiceClone, tier: TierBackground, bookID: "book-1", chapterID: "ch-1", presetID: "preset-a",
		paragraph: store.Paragraph{ID: "p-1", Speaker: "Alice", IsQuote: true, Emotion: "angry"},
	}
	mgr.pushTask(clone)

	got, ok := mgr.queue.Pop()
	if !ok {
		t.Fatalf("expected the variant render to dispatch")
	}
	render := got.(*task)
	if render.kind != KindVoiceProvision || render.dedupKey() != "voice_provision:variant:preset-a:angry" {
		t.Fatalf("expected the angry variant render, got kind=%v key=%q", render.kind, render.dedupKey())
	}
	if _, ok := mgr.queue.Pop(); ok {
		t.Fatalf("expected the clone to stay blocked while its variant renders")
	}

	if err := voicerefs.MarkVariantFailed(mgr.dataDir, "preset-a", "angry"); err != nil {
		t.Fatalf("MarkVariantFailed: %v", err)
	}
	mgr.queue.Finish(render.dedupKey())
	got, ok = mgr.queue.Pop()
	if !ok || got.(*task) != clone {
		t.Fatalf("expected the clone to dispatch once its variant settled, got %v ok=%v", got, ok)
	}

	// A neutral or narration line never waits on a variant at all.
	neutral := &task{
		kind: KindVoiceClone, tier: TierBackground, bookID: "book-1", chapterID: "ch-1", presetID: "preset-a",
		paragraph: store.Paragraph{ID: "p-2", Emotion: "sad"},
	}
	mgr.pushTask(neutral)
	if got, ok := mgr.queue.Pop(); !ok || got.(*task) != neutral {
		t.Fatalf("expected a narration line to dispatch with no variant dependency, got %v ok=%v", got, ok)
	}
}
