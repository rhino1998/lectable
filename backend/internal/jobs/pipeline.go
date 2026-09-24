package jobs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"slices"
	"time"

	"github.com/rhino1998/lectable/backend/internal/taskqueue"
)

// poolPipeline is the one PoolKey every book-preprocessing "phase" task
// (pipelineTask below) dispatches through - but, unlike poolGeneration/
// poolDesign/poolLLM, it's registered on its own separate taskqueue.Queue
// (Manager.pipelineQueue, built in NewManager), not m.queue. That's
// deliberate, not an oversight: taskqueue.Queue enforces "only one of its
// own registered pools may have a task in flight at a time" (see that
// package's own "one pool at a time" doc comment) because poolGeneration/
// poolDesign/poolLLM genuinely do share one GPU/VRAM-bound ttsworker
// process that can't usefully run two of those three at once. A pipeline
// phase task does none of that work itself - it only fans out real
// per-chapter/per-character work into those three pools (via
// RunAttribution/RunCharacterization/RunVoiceProvision/RunDirection,
// dispatched exactly the way a reader's own individual "Attribute
// chapter"/"Regenerate voice" click already is) and blocks waiting for it,
// the same shape httpapi's old runBookPipeline goroutine already used
// (see that function's own now-superseded doc comment for the fuller
// "never occupy the pool slot you're waiting on" reasoning, which still
// applies unchanged here). Registering poolPipeline as a fourth PoolKey on
// m.queue would wrongly serialize book-preprocessing progress behind
// ordinary generation/attribution for no real resource reason - a second,
// independent Queue is what actually lets it run concurrently with the
// other three: its own "one pool at a time" rule only ever applies among
// its own registered pools, which today is just this one.
const poolPipeline taskqueue.PoolKey = "pipeline"

// maxPipelineInFlight bounds poolPipeline - how many books' preprocessing
// runs may have a phase actively fanning out work at once. Deliberately
// much higher than maxInFlight/maxAttributionInFlight/maxDesignInFlight:
// a phase task itself does no GPU/LLM work and holds no VRAM, it just
// blocks on a WaitGroup while its own real per-item tasks queue and
// dispatch through poolGeneration/poolDesign/poolLLM exactly like any
// other caller's - those three pools' own capacities are what actually
// bound real concurrent model work; this only bounds how many book-level
// phase goroutines may be blocked waiting at once, which is cheap. High
// enough that a reasonable number of books preprocessing at once (each
// pinned to one phase in flight at a time by its own dependency chain -
// see pipelineResolver) never queue behind each other for no resource
// reason - matching the old bare-goroutine runBookPipeline's own
// behavior, which had no cap on how many books could run concurrently at
// all.
//
// The "generate audio" meta-tasks (EnqueueChapter/EnqueueBookGenerate/
// EnqueueRemaining) share this pool and stay in flight until the paragraph
// audio they queued has finished (see waitForChapterAudio) - so a reader
// clicking "generate" on a few dozen chapters holds that many slots for a
// long time. Sized well above that so those rows can never starve a
// book's preprocessing phases of a slot; each is just a goroutine blocked
// on a channel.
const maxPipelineInFlight = 256

// Book preprocessing runs seven phases, related by pipelinePhaseDeps (see
// pipelineResolver): scare-quote tagging first, then attribution and
// description tagging (both need scare quotes flagged - the same edge
// Manager.scareQuoteDependency enforces per chapter), characterization
// once both of those are done (it reads a character's attributed dialogue
// and description paragraphs), then voice provisioning (needs
// characterization's own Summary). Direction tagging, pronunciation
// resolution and music scoring need nothing any other phase produces (see httpapi.directChapter/
// scoreChapterMusic - only the book's resolved voice and each paragraph's
// own IsQuote/Text), so they dispatch immediately alongside scare-quote
// tagging. The constants' numeric order is just identity, not execution
// order (new phases are appended rather than renumbering existing ones).
// pipelinePhaseNames doubles as each phase's own Key() suffix (see
// pipelineKey) and Snapshot/log display name.
const (
	pipelinePhaseAttribution = iota
	pipelinePhaseCharacterization
	pipelinePhaseVoiceProvision
	pipelinePhaseDirection
	pipelinePhaseMusic
	pipelinePhaseScareQuote
	pipelinePhaseDescription
	pipelinePhasePronunciation
	pipelinePhaseCount
)

var pipelinePhaseNames = [pipelinePhaseCount]string{
	pipelinePhaseAttribution:      "attribution",
	pipelinePhaseCharacterization: "characterization",
	pipelinePhaseVoiceProvision:   "voice_provision",
	pipelinePhaseDirection:        "direction",
	pipelinePhaseMusic:            "music",
	pipelinePhaseScareQuote:       "scare_quote",
	pipelinePhaseDescription:      "description",
	pipelinePhasePronunciation:    "pronunciation",
}

// PipelinePhases is EnqueuePipeline's argument - one PipelinePhaseFunc per
// phase, indexed by the exported Phase* constants below so a caller builds
// it as a keyed literal rather than relying on positional order.
type PipelinePhases = [pipelinePhaseCount]PipelinePhaseFunc

// Exported names for the pipelinePhase* indices, for building a
// PipelinePhases literal outside this package.
const (
	PhaseScareQuote       = pipelinePhaseScareQuote
	PhaseAttribution      = pipelinePhaseAttribution
	PhaseDescription      = pipelinePhaseDescription
	PhaseCharacterization = pipelinePhaseCharacterization
	PhaseVoiceProvision   = pipelinePhaseVoiceProvision
	PhaseDirection        = pipelinePhaseDirection
	PhaseMusic            = pipelinePhaseMusic
	PhasePronunciation    = pipelinePhasePronunciation
)

// pipelinePhaseDeps lists, per phase, the phases of the same book's run
// that must finish before it may dispatch - see the pipelinePhase*
// constants' doc comment for why each edge exists. A phase with no entry
// dispatches immediately.
var pipelinePhaseDeps = [pipelinePhaseCount][]int{
	pipelinePhaseAttribution:      {pipelinePhaseScareQuote},
	pipelinePhaseDescription:      {pipelinePhaseScareQuote},
	pipelinePhaseCharacterization: {pipelinePhaseAttribution, pipelinePhaseDescription},
	pipelinePhaseVoiceProvision:   {pipelinePhaseCharacterization},
}

// pipelineKindNames is pipelinePhaseNames' own counterpart in this
// package's public Kind namespace (see QueueTask.Kind/toPipelineQueueTask)
// - "pipeline_"-prefixed so a phase's own Kind can never collide with an
// ordinary per-item Kind that happens to share a phase's bare name
// (KindVoiceProvision is literally "voice_provision", the exact string
// pipelinePhaseVoiceProvision's own bare name would otherwise be) -
// httpapi's Jobs dashboard keys its display (KIND_LABELS) on this same
// string.
var pipelineKindNames = [pipelinePhaseCount]Kind{
	pipelinePhaseAttribution:      "pipeline_attribution",
	pipelinePhaseCharacterization: "pipeline_characterization",
	pipelinePhaseVoiceProvision:   "pipeline_voice_provision",
	pipelinePhaseDirection:        "pipeline_direction",
	pipelinePhaseMusic:            "pipeline_music",
	pipelinePhaseScareQuote:       "pipeline_scare_quote",
	pipelinePhaseDescription:      "pipeline_description",
	pipelinePhasePronunciation:    "pipeline_pronunciation",
}

// PipelinePhaseFunc is one phase's own real work for one book - fan out
// whatever per-chapter/per-character tasks that phase covers (via
// RunScareQuote/RunAttribution/RunDescription/RunCharacterization/
// RunVoiceProvision/RunDirection/RunMusicScoring) and block until every one of them finishes or ctx is
// canceled. Supplied by the caller at EnqueuePipeline time (httpapi's own
// preprocessAttributionPhase/preprocessCharacterizationPhase/
// preprocessVoiceProvisionPhase/preprocessDirectionPhase/
// preprocessMusicPhase), since running
// any given phase needs Store/Speaker access this package doesn't have -
// the same reason AttributionFunc/DirectionFunc/CharacterizationFunc are
// all caller-supplied too. A non-nil return is logged by pipelineWorker
// but never blocks a later phase from starting (see pipelineResolver's own
// doc comment on what "depends on" means here) - every phase function
// httpapi supplies today is itself best-effort and always returns nil,
// logging its own per-item failures internally exactly as
// runBookPipeline's old inline code blocks did.
//
// tier reports this phase's own pipelineTask.Tier() live, read fresh at
// whatever moment the phase actually dispatches each of its own
// RunAttribution/RunDirection/RunMusicScoring calls - not a value snapshot
// at phase-start, since a phase can sit queued or already fanning out for
// a while before a reader (or a caller of Manager.PromoteTier) bumps its
// priority, and every per-item task dispatched before that point would
// otherwise stay silently stuck at whatever tier it started with even
// after the phase itself claims to be more urgent. RunCharacterization/
// RunVoiceProvision need no such thing - both already always dispatch at
// TierUrgent regardless of caller (see their own doc comments), so
// preprocessCharacterizationPhase/preprocessVoiceProvisionPhase accept and
// ignore this parameter.
type PipelinePhaseFunc func(ctx context.Context, tier func() int) error

// pipelineTask is one poolPipeline task: either one of a book's
// preprocessing phases (group == nil - see the pipelinePhase* constants), or
// a bulk group (see BulkGroup). Carries no paragraph/voice/LLM-call state of
// its own, just enough identity for pipelineResolver's dependency lookup and
// CancelPipeline/IsPipelineRunning, plus the actual work (run) and its own
// dispatch context (cancel, set once popped - nil while still queued, since
// a queued task has no context of its own yet to cancel).
type pipelineTask struct {
	bookID string
	phase  int        // one of the pipelinePhase* constants - only meaningful when group == nil
	group  *BulkGroup // nil for a preprocessing phase
	tier   int
	run    PipelinePhaseFunc
	cancel context.CancelFunc
}

// pipelineOutcome is a dispatched pipelineTask's real result, delivered
// over the task's own private channel to pipelineWorker - a struct
// wrapping err (rather than a bare `chan error`) specifically so
// pipelineWorker's own reflect.Select loop can type-assert the received
// value without the nil-interface panic a bare `chan error` would risk
// (asserting a reflect.Value's Interface() back to the interface type
// error panics when the sent value was a true nil error - the same reason
// this package's own taskResult/llmOutcome/previewOutcome/provisionOutcome
// are all concrete structs rather than bare error/interface channels).
type pipelineOutcome struct {
	err error
}

func pipelineKey(bookID string, phase int) string {
	return "pipeline:" + bookID + ":" + pipelinePhaseNames[phase]
}

// currentPipelineTier safely reads the pipelineTask keyed key's own
// current tier, under pipelineQueue's own lock (via WithTask) - the
// lock-safe way for a phase's own dispatch goroutine (running with no
// lock of its own) to read whatever tier Manager.PromoteTier most
// recently set, rather than racing a bare field read (pipelineTask.tier)
// against Promote's own concurrent mutation, which real per-item task's
// own Tier()/Promote() doc comments already flag as unsafe outside
// taskqueue.Queue's own lock-held code paths. Falls back to
// defaultAttributionTier if the phase's own task is somehow already gone
// (finished and Finish()ed between Pop and this call) - a phase always
// calls this from inside its own still-running PipelinePhaseFunc, so this
// is only ever a narrow, harmless race against the task's own imminent
// completion, never a real "phase not found" case.
func (m *Manager) currentPipelineTier(key string) int {
	tier := defaultAttributionTier
	m.pipelineQueue.WithTask(
		func(c taskqueue.Task) bool { return c.Key() == key },
		func(c taskqueue.Task) { tier = c.Tier() },
	)
	return tier
}

// toPipelineQueueTask reports t as a QueueTask - the Jobs dashboard's own
// row shape, shared with every ordinary per-item task (see toQueueTask). A
// preprocessing phase covers the whole book, so its Label is the fixed
// "Whole book" rather than "" (which would make the dashboard's chapter
// column fall back to "Chapter 1"); a bulk group reports its own Kind/Label/
// chapter (see BulkGroup).
func toPipelineQueueTask(t *pipelineTask, tier int) QueueTask {
	qt := QueueTask{
		ID:     t.Key(),
		Kind:   pipelineKindNames[t.phase],
		Label:  "Whole book",
		BookID: t.bookID,
		Tier:   tier,
	}
	if g := t.group; g != nil {
		qt.Kind = g.Kind
		qt.Label = g.Label
		qt.ChapterID = g.ChapterID
		qt.ChapterIdx = g.ChapterIdx
	}
	return qt
}

// Key namespaces a bulk group by its own BulkGroup.Key so it can never
// collide with a same-book preprocessing phase's pipelineKey - see
// taskqueue.Queue.Push's own dedup-by-Key doc comment for why this is also
// what makes a duplicate click while one's already queued/in flight a
// harmless no-op.
func (t *pipelineTask) Key() string {
	if t.group != nil {
		return "pipeline:" + t.bookID + ":" + t.group.Key
	}
	return pipelineKey(t.bookID, t.phase)
}
func (t *pipelineTask) Pool() taskqueue.PoolKey { return poolPipeline }

// Tier returns t's own current tier - like the real per-item task's own
// Tier()/Promote() (see their doc comments), safe to call only from
// taskqueue.Queue's own lock-held code paths (Pop/Snapshot/WithTask/
// computeState). A phase's own dispatch goroutine, which holds no lock at
// all, must go through Manager.currentPipelineTier (WithTask-backed)
// instead of calling this directly - see PipelinePhaseFunc's own doc
// comment on its tier parameter.
func (t *pipelineTask) Tier() int { return t.tier }

// Less never distinguishes two phase tasks - there's no meaningful
// "closer" between two different books' (or two different phases')
// preprocessing runs the way comparePosition orders same-tier generation
// work by book/chapter/paragraph position; a plain stable insertion-order
// tiebreak (taskqueue.Queue's own last resort) is all this needs.
func (t *pipelineTask) Less(other taskqueue.Task) bool { return false }

func (t *pipelineTask) Promote(newTier int) {
	if newTier < t.tier {
		t.tier = newTier
	}
}

// pipelineResolver is pipelineQueue's own taskqueue.Resolver - a phase
// can't dispatch until every phase pipelinePhaseDeps lists for it, for
// that same book, has finished (see the pipelinePhase* constants' own doc
// comment for the graph). Never lazily creates a task
// the way Manager.resolveDependencies' own
// dependency kinds do (characterizationDependency et al.) - every phase
// for a given run is pushed up front by EnqueuePipeline, so there's
// nothing here to conjure into existence, only to look up: a still-
// queued-or-in-flight earlier phase is a real dependency; once it
// Finishes (removed from pipelineQueue entirely, success or failure - see
// PipelinePhaseFunc's own doc comment on why failure doesn't block a
// dependent phase either), Find stops finding it and the dependent phase
// becomes eligible.
func pipelineResolver(lq *taskqueue.LockedQueue, tk taskqueue.Task) []taskqueue.Task {
	t := tk.(*pipelineTask)
	// A bulk group has no dependency at all - it doesn't participate in
	// book-preprocessing's own phase ordering.
	if t.group != nil {
		return nil
	}
	var deps []taskqueue.Task
	for _, phase := range pipelinePhaseDeps[t.phase] {
		if dep, ok := lq.Get(pipelineKey(t.bookID, phase)); ok {
			deps = append(deps, dep)
		}
	}
	return deps
}

// ErrPipelineAlreadyRunning is EnqueuePipeline's own error when bookID
// already has a preprocessing run in progress - httpapi's
// handlePreprocessBook maps this to a 409, the same status its own old
// Server.pipelineRunning-based dedup returned.
var ErrPipelineAlreadyRunning = errors.New("a preprocessing run is already in progress for this book")

// EnqueuePipeline queues bookID's preprocessing phases (see the
// pipelinePhase* constants) as dependent poolPipeline tasks and
// returns immediately - httpapi.handlePreprocessBook's own "run
// everything" meta-task (POST .../preprocess), reimplemented as real,
// dependency-ordered tasks instead of one long-lived bare goroutine
// (runBookPipeline) sequencing four inline code blocks by hand. phases[i]
// is phase i's own real work (see PipelinePhaseFunc; nil skips that
// phase) - httpapi supplies all of them at once since it's the only caller with the Store/Speaker
// access any of them need.
//
// Returns ErrPipelineAlreadyRunning, pushing nothing, if bookID already
// has any phase queued or in flight - checked and pushed atomically under
// pipelineMu (not just IsPipelineRunning followed by a separate push,
// which would race two concurrent calls for the same book into both
// passing the check before either pushes): pipelineTask.Key() already
// namespaces every phase both by bookID and by phase, so two full pushes
// for the same book landing back to back would otherwise interleave two
// independent four-phase chains rather than cleanly rejecting the second.
func (m *Manager) EnqueuePipeline(bookID string, phases PipelinePhases) error {
	m.pipelineMu.Lock()
	defer m.pipelineMu.Unlock()

	if m.isPipelineRunningLocked(bookID) {
		return ErrPipelineAlreadyRunning
	}
	for phase, fn := range phases {
		if fn == nil {
			// An omitted phase is simply absent from this run - any phase
			// depending on it finds nothing to wait on (see
			// pipelineResolver).
			continue
		}
		m.pipelineQueue.Push(&pipelineTask{
			bookID: bookID,
			phase:  phase,
			tier:   TierBackground,
			run:    fn,
		})
	}
	select {
	case m.pipelineWake <- struct{}{}:
	default:
	}
	m.notifyChanged()
	return nil
}

// IsPipelineRunning reports whether bookID currently has any preprocessing
// phase task queued or in flight - the read side of EnqueuePipeline,
// replacing Server.pipelineRunning's own sync.Map presence check now that
// a run's presence is tracked by pipelineQueue itself rather than a
// second, parallel bookkeeping structure httpapi used to maintain by
// hand. Used by httpapi's bookSummaryDTO.Preprocessing (the library page's
// per-book spinner).
func (m *Manager) IsPipelineRunning(bookID string) bool {
	m.pipelineMu.Lock()
	defer m.pipelineMu.Unlock()
	return m.isPipelineRunningLocked(bookID)
}

func (m *Manager) isPipelineRunningLocked(bookID string) bool {
	// Deliberately scoped to preprocessing phases only: a bulk group
	// sharing this same queue must never trip bookSummaryDTO.Preprocessing
	// - "generating audio" and "book-preprocessing run in progress" are
	// different concepts a caller needs to tell apart.
	_, ok := m.pipelineQueue.Find(func(c taskqueue.Task) bool {
		t := c.(*pipelineTask)
		return t.bookID == bookID && t.group == nil
	})
	return ok
}

// CancelPipeline cancels bookID's in-progress preprocessing run, if any -
// httpapi.handleDeleteBook's own best-effort stop for a book that's about
// to stop existing, the same role Server.cancelPipeline played before
// preprocessing phases were real tasks. A still-queued phase (waiting on
// an earlier phase to finish, or not yet reached by pipelineWorker) is
// removed outright, same as Manager.CancelAll's own queued half; an
// already-dispatched phase can't be un-dispatched, so its own context is
// canceled instead and its fan-out work (RunAttribution/
// RunCharacterization/RunVoiceProvision/RunDirection, all already
// ctx-aware) winds down on its own once it next checks ctx - mirrors
// Manager.Cancel's two-case shape for an ordinary task. A no-op if bookID
// has no phase queued or in flight.
func (m *Manager) CancelPipeline(bookID string) {
	m.pipelineMu.Lock()
	defer m.pipelineMu.Unlock()

	removed := false
	for {
		if _, ok := m.pipelineQueue.Cancel(func(c taskqueue.Task) bool { return c.(*pipelineTask).bookID == bookID }); !ok {
			break
		}
		removed = true
	}
	if removed {
		m.notifyChanged()
	}
	for _, tk := range m.pipelineQueue.InFlightTasks() {
		t := tk.(*pipelineTask)
		if t.bookID == bookID && t.cancel != nil {
			t.cancel()
		}
	}
}

// cancelPipelineTask cancels/removes exactly one phase task by its own
// Key() (e.g. "pipeline:<bookID>:attribution") - Manager.Cancel's own
// fallback once m.queue reports no match, so the Jobs dashboard's per-row
// Cancel button works for a pipeline-phase row the same way it already
// does for an ordinary task row. Unlike CancelPipeline (which stops every
// remaining phase of a book's run at once), this only touches the one
// named phase - removing/canceling it still lets a later, still-queued
// phase for the same book start once pipelineResolver stops finding this
// one, the same as a normal finish (see PipelinePhaseFunc's own doc
// comment on why a non-nil/canceled outcome doesn't block a later phase
// either).
func (m *Manager) cancelPipelineTask(id string) bool {
	if _, ok := m.pipelineQueue.Cancel(func(c taskqueue.Task) bool { return c.Key() == id }); ok {
		m.notifyChanged()
		return true
	}
	if found, ok := m.pipelineQueue.Find(func(c taskqueue.Task) bool { return c.Key() == id }); ok {
		if t := found.(*pipelineTask); t.cancel != nil {
			t.cancel()
		}
		return true
	}
	return false
}

// cancelAllPipelineTasks cancels every queued and in-flight preprocessing
// phase task across every book at once - Manager.CancelAll's own
// pipeline-side half, now that phase tasks are on the same Jobs dashboard
// its "Cancel all" button already clears. Mirrors CancelAll's own
// two-part shape (drain queued, cancel in-flight contexts) but has no
// waiters to deliver a canceled outcome to (phase tasks are
// fire-and-forget, like EnqueueAttribution's own tasks) and no
// chapterPending bookkeeping (phase tasks never touch it - see
// pipelineTask's own doc comment). Returns how many phase tasks were
// acted on.
func (m *Manager) cancelAllPipelineTasks() int {
	drained := m.pipelineQueue.Drain()
	if len(drained) > 0 {
		m.notifyChanged()
	}
	inFlightCancels := make([]context.CancelFunc, 0, len(m.pipelineQueue.InFlightTasks()))
	for _, t := range m.pipelineQueue.InFlightTasks() {
		if pt := t.(*pipelineTask); pt.cancel != nil {
			inFlightCancels = append(inFlightCancels, pt.cancel)
		}
	}
	for _, cancel := range inFlightCancels {
		cancel()
	}
	return len(drained) + len(inFlightCancels)
}

// BulkGroup identifies one bulk group - a single cancelable, promotable
// Jobs-dashboard row wrapping every per-item task one whole-book (or
// whole-chapter) action fans out into, so a reader can see, cancel, or
// promote the whole action at once instead of hunting down one row per
// chapter/character. Every such action shares this one shape: the library
// page's "Generate audio" (EnqueueBookGenerate/EnqueueRemaining), the
// reader's per-chapter generate (EnqueueChapter), Auto Split, and the
// Speakers page's bulk buttons (httpapi.handleBulkAction).
//
// A bulk group dispatches through poolPipeline like a preprocessing phase,
// but never joins the phase dependency graph (see pipelineResolver) and never
// counts toward IsPipelineRunning (see isPipelineRunningLocked). Its run is a
// PipelinePhaseFunc: fan out the real work, block until it finishes, and
// honor ctx - canceling the row stops any item it hasn't reached yet, while
// an already-dispatched child keeps running, the same guarantee
// CancelPipeline gives a canceled phase.
type BulkGroup struct {
	// Kind is the row's QueueTask.Kind - "pipeline_"-prefixed by convention
	// (see pipelineKindNames), and what clients key display/"is this still
	// running" checks on.
	Kind Kind
	// Key namespaces the task within its book (Key() prepends
	// "pipeline:<bookID>:"); a second EnqueueBulk with the same Key while
	// the first is still queued/in flight is dropped.
	Key string
	// Label is the row's QueueTask.Label. Leave it "" for a chapter-scoped
	// group so the dashboard resolves the chapter's real stored title from
	// ChapterID instead (see queueTaskDTO.Label).
	Label      string
	ChapterID  string
	ChapterIdx int
	Tier       int
	// Children picks out the m.queue tasks this group's run dispatches, so
	// promoting the group's row (Manager.PromoteTier) promotes them too -
	// see cascadePromotion. nil means promotion doesn't cascade.
	Children *ChildFilter
}

// ChildFilter matches a bulk group's (or preprocessing phase's) own child
// tasks in m.queue, always within the group's own book. Every non-zero
// field must match.
type ChildFilter struct {
	Kinds     []Kind // empty matches any kind
	ChapterID string
	Label     string // e.g. the speaker name an Auto Split's children carry
}

func (f ChildFilter) matches(t *task) bool {
	return (len(f.Kinds) == 0 || slices.Contains(f.Kinds, t.kind)) &&
		(f.ChapterID == "" || t.chapterID == f.ChapterID) &&
		(f.Label == "" || t.label == f.Label)
}

// phaseChildKinds is each preprocessing phase's own child task kind, for
// cascadePromotion. Characterization/voice provisioning are deliberately
// absent: RunCharacterization/RunVoiceProvision always dispatch at
// TierUrgent already, so a further promotion could never apply.
var phaseChildKinds = map[int]Kind{
	pipelinePhaseAttribution:   KindSpeakerAttribution,
	pipelinePhaseDirection:     KindSpeechDirection,
	pipelinePhaseMusic:         KindMusicScoring,
	pipelinePhaseScareQuote:    KindScareQuote,
	pipelinePhaseDescription:   KindDescription,
	pipelinePhasePronunciation: KindPronunciation,
}

// children reports which m.queue tasks a promotion of t cascades to, or nil.
func (t *pipelineTask) children() *ChildFilter {
	if t.group != nil {
		return t.group.Children
	}
	if kind, ok := phaseChildKinds[t.phase]; ok {
		return &ChildFilter{Kinds: []Kind{kind}}
	}
	return nil
}

// EnqueueBulk queues run as one bulk group for bookID (see BulkGroup).
// Needs no pipelineMu-guarded check-then-push the way EnqueuePipeline does -
// it's one single task, and taskqueue.Queue.Push already dedupes by Key(),
// which is exactly the idempotent "already running this? then this call is
// a harmless no-op" behavior a repeated click wants.
func (m *Manager) EnqueueBulk(bookID string, g BulkGroup, run PipelinePhaseFunc) {
	m.pipelineQueue.Push(&pipelineTask{
		bookID: bookID,
		group:  &g,
		tier:   g.Tier,
		run:    run,
	})
	select {
	case m.pipelineWake <- struct{}{}:
	default:
	}
	m.notifyChanged()
}

// EnqueueChapter enqueues background TTS generation for every not-yet-
// generated paragraph in one chapter, as a bulk group (Kind
// "pipeline_generate_chapter") that stays in flight until that audio
// finishes (see waitForChapterAudio) - the reader/mobile client's own
// "Generate chapter audio" action. Idempotent per chapter: a second call
// while the first is still queued/in flight is dropped.
func (m *Manager) EnqueueChapter(bookID, chapterID string, chapterIdx int) {
	m.EnqueueBulk(bookID, BulkGroup{
		Kind:       "pipeline_generate_chapter",
		Key:        fmt.Sprintf("generate_chapter:%d", chapterIdx),
		ChapterID:  chapterID,
		ChapterIdx: chapterIdx,
		Tier:       TierBackground,
		// Anything chapter-scoped in m.queue under this chapterID is part of
		// "generate this chapter" work.
		Children: &ChildFilter{ChapterID: chapterID},
	}, func(ctx context.Context, _ func() int) error {
		m.enqueueChapter(ctx, bookID, chapterID, chapterIdx, 0)
		m.waitForChapterAudio(ctx, map[string]bool{chapterID: true})
		return nil
	})
}

// EnqueueBookGenerate enqueues background TTS generation for every chapter
// in a book at once, as one bulk group (Kind "pipeline_generate_book") - the
// library page's "Generate audio" -> "All" and the Speakers page's bulk
// generate. Walks chapters in order, checking ctx between each.
func (m *Manager) EnqueueBookGenerate(bookID string) {
	m.EnqueueBulk(bookID, BulkGroup{
		Kind:  "pipeline_generate_book",
		Key:   "generate_book",
		Label: "Whole book",
		Tier:  TierBackground,
	}, func(ctx context.Context, _ func() int) error {
		m.enqueueBookGenerate(ctx, bookID)
		return nil
	})
}

func (m *Manager) enqueueBookGenerate(ctx context.Context, bookID string) {
	chapters, err := m.store.ListChapterSummaries(bookID, "", nil)
	if err != nil {
		log.Printf("jobs: generate book %s: list chapters: %v", bookID, err)
		return
	}
	pushed := make(map[string]bool, len(chapters))
	defer func() { m.waitForChapterAudio(ctx, pushed) }()
	for _, cs := range chapters {
		select {
		case <-ctx.Done():
			return
		default:
		}
		m.enqueueChapter(ctx, bookID, cs.Chapter.ID, cs.Chapter.Idx, 0)
		pushed[cs.Chapter.ID] = true
	}
}

// EnqueueRemaining enqueues background TTS generation for every paragraph
// in a book from its current stored reading position (store.Book.
// PosChapterIdx/PosParagraphIdx) through the end, as one bulk group (Kind
// "pipeline_generate_remaining") - the library page's "Generate audio" ->
// "Remaining". Unlike EnqueueLookahead there's no paragraph cap, and every
// paragraph pushes at TierBackground: this is bulk work a reader kicked off
// deliberately, not something they're waiting on right now. Paragraphs
// before the stored position are left alone - no point generating audio for
// ones already played past.
func (m *Manager) EnqueueRemaining(bookID string) {
	m.EnqueueBulk(bookID, BulkGroup{
		Kind:  "pipeline_generate_remaining",
		Key:   "generate_remaining",
		Label: "Remaining",
		Tier:  TierBackground,
	}, func(ctx context.Context, _ func() int) error {
		m.enqueueRemaining(ctx, bookID)
		return nil
	})
}

func (m *Manager) enqueueRemaining(ctx context.Context, bookID string) {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil {
		log.Printf("jobs: book %s not found: %v", bookID, err)
		return
	}

	fromChapterIdx := book.PosChapterIdx
	chapterIdx := fromChapterIdx
	pushed := map[string]bool{}
	defer func() { m.waitForChapterAudio(ctx, pushed) }()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ch, err := m.store.GetChapterByIdx(bookID, chapterIdx)
		if err != nil {
			log.Printf("jobs: remaining: get chapter %d of book %s: %v", chapterIdx, bookID, err)
			return
		}
		if ch == nil {
			return // ran off the end of the book
		}

		resolved, all, err := m.paragraphsNeedingGeneration(ctx, book, ch.ID)
		if err != nil {
			log.Printf("jobs: list paragraphs for chapter %s: %v", ch.ID, err)
			return
		}
		if chapterIdx == fromChapterIdx && book.PosParagraphIdx > 0 {
			resolved = skipResolvedBeforeIdx(resolved, book.PosParagraphIdx)
		}
		for _, rp := range resolved {
			m.pushResolvedTask(bookID, ch.ID, chapterIdx, TierBackground, rp, all)
		}
		pushed[ch.ID] = true
		chapterIdx++
	}
}

// waitForChapterAudio blocks a "generate audio" meta-task (EnqueueChapter/
// EnqueueBookGenerate/EnqueueRemaining) until no paragraph-audio task
// (KindVoiceClone/KindVoiceDesign) for any of chapterIDs is queued or in
// flight any more - so the meta-task's own Jobs-dashboard row, and the
// frontend's per-chapter "generating" state derived from it
// (useGeneratingChapters), last as long as the audio it asked for is
// actually still being produced, rather than vanishing the instant its
// paragraphs were merely queued (a whole chapter queues in well under a
// second). Waits on the queue's own change notifications, with a periodic
// recheck as a backstop.
//
// If ctx is canceled first (this row's own Cancel button, CancelAll, or
// CancelPipeline), every still-queued paragraph-audio task for chapterIDs
// is removed too - canceling "generate this chapter" means stop generating
// it, not just stop watching. Already-dispatched paragraphs finish on
// their own, the same in-flight caveat every other cancel here has.
func (m *Manager) waitForChapterAudio(ctx context.Context, chapterIDs map[string]bool) {
	m.waitForChapterTasks(ctx, chapterIDs, KindVoiceClone, KindVoiceDesign)
}

// waitForChapterTasks is waitForChapterAudio generalized over which task
// kinds it waits on (and, on cancel, removes) - also used by
// EnqueueBookMusicGeneration for KindMusicGeneration.
func (m *Manager) waitForChapterTasks(ctx context.Context, chapterIDs map[string]bool, kinds ...Kind) {
	if len(chapterIDs) == 0 {
		return
	}
	isChapterAudio := func(c taskqueue.Task) bool {
		t, ok := c.(*task)
		return ok && slices.Contains(kinds, t.kind) && chapterIDs[t.chapterID]
	}
	changes := m.SubscribeChanges()
	defer m.UnsubscribeChanges(changes)
	for {
		if _, ok := m.queue.Find(isChapterAudio); !ok {
			// handleResult finishes a failed task before pushing its
			// retry (see requeueCopy), so an empty moment can be just
			// the chapter's last paragraph between attempts - only
			// treat it as done if it's still empty a moment later.
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
			if _, ok := m.queue.Find(isChapterAudio); !ok {
				return
			}
		}
		select {
		case <-ctx.Done():
			for {
				t, ok := m.queue.Cancel(isChapterAudio)
				if !ok {
					break
				}
				m.dropCanceledQueued(t.(*task))
			}
			return
		case <-changes:
		case <-time.After(5 * time.Second):
		}
	}
}

// EnqueueBookMusicGeneration queues the whole-chapter music run
// (GenerateChapterMusic) for every chapter of bookID that's scored, fully
// narrated, and missing any region's music, as one bulk group (Kind
// "pipeline_bulk_music_generation") that stays in flight until those
// KindMusicGeneration tasks finish - the Speakers page's "Generate missing
// music". Chapters not yet eligible are skipped silently.
func (m *Manager) EnqueueBookMusicGeneration(bookID string) {
	m.EnqueueBulk(bookID, BulkGroup{
		Kind:     "pipeline_bulk_music_generation",
		Key:      "bulk:music_generation",
		Label:    "Missing music",
		Tier:     TierBackground,
		Children: &ChildFilter{Kinds: []Kind{KindMusicGeneration}},
	}, func(ctx context.Context, _ func() int) error {
		chapters, err := m.store.ListChapterSummaries(bookID, "", nil)
		if err != nil {
			return fmt.Errorf("list chapters: %w", err)
		}
		pushed := map[string]bool{}
		defer func() { m.waitForChapterTasks(ctx, pushed, KindMusicGeneration) }()
		for _, cs := range chapters {
			if ctx.Err() != nil {
				return nil
			}
			queued, err := m.GenerateChapterMusic(bookID, cs.Chapter.ID)
			switch {
			case errors.Is(err, ErrChapterNotScored), errors.Is(err, ErrChapterNotVoiced):
			case err != nil:
				log.Printf("jobs: generate music for book %s chapter %d: %v", bookID, cs.Chapter.Idx, err)
			case queued > 0:
				pushed[cs.Chapter.ID] = true
			}
		}
		return nil
	})
}

// pipelineWorker mirrors Manager.worker's own dispatch-loop shape almost
// exactly (see that function's own doc comment for the reasoning behind
// each piece) - fill() pops every currently-eligible phase task and
// dispatches it into its own goroutine, then reflect.Select blocks on
// ctx.Done, a fresh push (m.pipelineWake), or any dispatched phase
// finishing, looping back to fill() after any of those. The one thing
// this doesn't need that worker() does is retry/store bookkeeping
// (handleResult's own job) - a phase task has no persisted state of its
// own to update and is never retried (PipelinePhaseFunc's own doc comment
// covers why a non-nil return doesn't block later phases either), so
// finishing a phase is just Finish() plus a log line on failure.
func (m *Manager) pipelineWorker(ctx context.Context) {
	type inflight struct {
		task *pipelineTask
		ch   chan pipelineOutcome
	}
	dispatched := map[string]*inflight{}

	fill := func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			tk, ok := m.pipelineQueue.Pop()
			if !ok {
				return
			}
			t := tk.(*pipelineTask)
			taskCtx, cancel := context.WithCancel(ctx)
			t.cancel = cancel
			m.notifyChanged()
			ch := make(chan pipelineOutcome, 1)
			go func() {
				key := t.Key()
				ch <- pipelineOutcome{err: t.run(taskCtx, func() int { return m.currentPipelineTier(key) })}
			}()
			dispatched[t.Key()] = &inflight{task: t, ch: ch}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		fill()

		cases := make([]reflect.SelectCase, 0, len(dispatched)+2)
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())})
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(m.pipelineWake)})
		keys := make([]string, 2, len(dispatched)+2) // keys[0]/[1] unused (ctx.Done/wake cases)
		for key, d := range dispatched {
			cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(d.ch)})
			keys = append(keys, key)
		}

		chosen, recv, _ := reflect.Select(cases)
		switch chosen {
		case 0:
			return // ctx.Done
		case 1:
			continue // woke by a fresh EnqueuePipeline push - let the next fill() reconsider the queue
		}
		key := keys[chosen]
		delete(dispatched, key)
		m.pipelineQueue.Finish(key)
		m.notifyChanged()
		if out := recv.Interface().(pipelineOutcome); out.err != nil {
			log.Printf("jobs: pipeline task %s: %v", key, out.err)
		}
	}
}
