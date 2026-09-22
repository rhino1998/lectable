package jobs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
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

// pipelineTaskKind distinguishes what a pipelineTask actually represents:
// one of book-preprocessing's own dependent phases (pipelinePhase*
// above - pipelineTaskPreprocessPhase, the zero value, keeps every
// existing preprocessing call site working unchanged), or one of the
// three independent "generate audio" meta-tasks below -
// EnqueueChapter/EnqueueBookGenerate/EnqueueRemaining, moved onto this
// same poolPipeline/pipelineQueue/pipelineWorker machinery instead of a
// bare fire-and-forget goroutine specifically so each shows up as its own
// cancelable Jobs-dashboard row, the same way a preprocessing phase
// already does - canceling one stops it from starting any chapter/
// paragraph it hasn't reached yet, the identical guarantee CancelPipeline
// already gives a canceled preprocessing phase (see that function's own
// doc comment). A generate meta-task's own run is the same shape as a
// preprocessing phase's PipelinePhaseFunc (fan out real work, check ctx
// between units, return once done/canceled) - it just isn't part of the
// phase dependency graph pipelineResolver enforces (see that
// function's own kind check) and must never count toward
// IsPipelineRunning (see isPipelineRunningLocked's own kind check), which
// specifically means "book-preprocessing run in progress" (the library
// page's preprocessing spinner) - a different concept from "generating
// audio" that a caller must be able to tell apart. pipelineTaskAutoSplit
// is the same idea for one "Auto Split" click - see EnqueueAutoSplit.
type pipelineTaskKind int

const (
	pipelineTaskPreprocessPhase pipelineTaskKind = iota
	pipelineTaskGenerateChapter
	pipelineTaskGenerateBook
	pipelineTaskGenerateRemaining
	pipelineTaskAutoSplit
)

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

// pipelineTask is one book's one phase - taskqueue.Task's minimal
// implementation for poolPipeline, deliberately much thinner than this
// package's own real per-item task type: a phase carries no
// paragraph/voice/LLM-call state of its own, just enough identity
// (bookID, phase) for pipelineResolver's dependency lookup and
// CancelPipeline/IsPipelineRunning's own lookups, plus the actual work
// (run) and its own dispatch context (cancel, set once popped - mirrors
// task.cancel's own doc comment: nil while still queued, since a queued
// phase has no context of its own yet to cancel).
type pipelineTask struct {
	bookID string
	kind   pipelineTaskKind
	phase  int // one of the pipelinePhase* constants above - only meaningful when kind == pipelineTaskPreprocessPhase
	// chapterIdx/chapterID identify which chapter this task covers - only
	// meaningful when kind == pipelineTaskGenerateChapter (every other
	// kind covers the whole book, so both are left at their zero value).
	// chapterID feeds toPipelineQueueTask's own QueueTask.ChapterID, so
	// the Jobs dashboard resolves this chapter's real stored title
	// (buildJobsSnapshot's own resolveChapter) instead of guessing one
	// from chapterIdx alone - a book's own chapter titles aren't
	// guaranteed to be "Chapter <idx+1>" (already false for this app's
	// own "Information" front-matter chapter at idx 0, and for any book
	// using its own Arc/Part naming), so an earlier version's
	// fmt.Sprintf("Chapter %d", chapterIdx+1) fallback could and did show
	// a wrong title.
	chapterIdx int
	chapterID  string
	// speakerKey/speakerName identify which speaker an Auto Split run
	// covers - only meaningful when kind == pipelineTaskAutoSplit.
	// speakerKey (a character's own ID, or the "unknown" sentinel - see
	// EnqueueReattribution) namespaces Key() so two different speakers'
	// runs on the same book stay independent; speakerName is the Jobs
	// dashboard's own Label, the same name each child
	// KindSpeakerReattribution row already carries.
	speakerKey  string
	speakerName string
	tier        int
	run         PipelinePhaseFunc
	cancel      context.CancelFunc
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
// row shape, shared with every ordinary per-item task (see toQueueTask) so
// Manager.Snapshot can report both kinds of task through one list without
// httpapi needing two separate DTOs. A phase has no chapter/paragraph/
// preset/instruct/attempt of its own (all left at their zero value) - just
// enough to identify and cancel it (ID, Kind, BookID, Tier); Label is set
// to a fixed, human-readable "Whole book" rather than left "" (unlike an
// ordinary task, where "" means "this kind already has a chapter/
// paragraph to show" - a phase has neither, so leaving Label empty would
// make the dashboard's own chapter/target column fall back to "Chapter 1"
// for every phase, which is actively misleading: a phase covers every
// chapter/character in the book, not chapter index 0).
func toPipelineQueueTask(t *pipelineTask, tier int) QueueTask {
	kind := pipelineKindNames[t.phase]
	label := "Whole book"
	chapterIdx := 0
	chapterID := ""
	switch t.kind {
	case pipelineTaskGenerateChapter:
		// Left "" (not a synthesized "Chapter <idx+1>" guess - an earlier
		// version did that, and it could and did show a wrong title: a
		// book's own chapter titles aren't guaranteed to line up with
		// chapterIdx+1, e.g. this app's own "Information" front-matter
		// chapter at idx 0, or a book using its own Arc/Part naming) so
		// ChapterTitle resolves this chapter's real stored title instead,
		// the same as every other chapter-scoped kind (speech_direction,
		// music_scoring, ...) already leaves Label empty for - see
		// queueTaskDTO.Label's own doc comment.
		kind = "pipeline_generate_chapter"
		label = ""
		chapterIdx = t.chapterIdx
		chapterID = t.chapterID
	case pipelineTaskGenerateBook:
		kind = "pipeline_generate_book"
	case pipelineTaskGenerateRemaining:
		kind = "pipeline_generate_remaining"
		label = "Remaining"
	case pipelineTaskAutoSplit:
		kind = "pipeline_auto_split"
		label = t.speakerName
	}
	return QueueTask{
		ID:         t.Key(),
		Kind:       kind,
		Label:      label,
		BookID:     t.bookID,
		ChapterID:  chapterID,
		ChapterIdx: chapterIdx,
		Tier:       tier,
	}
}

// Key namespaces a generate meta-task by kind (and, for
// pipelineTaskGenerateChapter, by chapterIdx too, since two different
// chapters of the same book must be independently cancelable/concurrent)
// so it can never collide with a same-book preprocessing phase's own
// pipelineKey, nor with another generate kind's - see
// taskqueue.Queue.Push's own dedup-by-Key doc comment for why this is
// also what makes a duplicate "generate this again" call while one's
// already queued/in flight a harmless no-op.
func (t *pipelineTask) Key() string {
	switch t.kind {
	case pipelineTaskGenerateChapter:
		return fmt.Sprintf("pipeline:%s:generate_chapter:%d", t.bookID, t.chapterIdx)
	case pipelineTaskGenerateBook:
		return "pipeline:" + t.bookID + ":generate_book"
	case pipelineTaskGenerateRemaining:
		return "pipeline:" + t.bookID + ":generate_remaining"
	case pipelineTaskAutoSplit:
		return "pipeline:" + t.bookID + ":auto_split:" + t.speakerKey
	default:
		return pipelineKey(t.bookID, t.phase)
	}
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
	// A generate meta-task (any kind other than pipelineTaskPreprocessPhase)
	// has no dependency at all, the same as pipelinePhaseDirection below -
	// it doesn't participate in book-preprocessing's own phase ordering.
	if t.kind != pipelineTaskPreprocessPhase {
		return nil
	}
	var deps []taskqueue.Task
	for _, phase := range pipelinePhaseDeps[t.phase] {
		depKey := pipelineKey(t.bookID, phase)
		if dep, ok := lq.Find(func(c taskqueue.Task) bool { return c.Key() == depKey }); ok {
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
	// Deliberately scoped to pipelineTaskPreprocessPhase only: a generate
	// meta-task (EnqueueChapter/EnqueueBookGenerate/EnqueueRemaining)
	// sharing this same queue must never trip bookSummaryDTO.Preprocessing
	// - "generating audio" and "book-preprocessing run in progress" are
	// different concepts a caller needs to tell apart (see
	// pipelineTaskKind's own doc comment).
	_, ok := m.pipelineQueue.Find(func(c taskqueue.Task) bool {
		t := c.(*pipelineTask)
		return t.bookID == bookID && t.kind == pipelineTaskPreprocessPhase
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

// pushGenerateTask is the shared push+wake+notify shape every
// EnqueueChapter/EnqueueBookGenerate/EnqueueRemaining call below reduces
// to - unlike EnqueuePipeline, a generate meta-task needs no pipelineMu-
// guarded check-then-push atomicity (there's no multi-task "all four
// phases at once" invariant to protect here, just one single task), so a
// plain Push suffices: taskqueue.Queue.Push already dedupes by Key() (see
// its own doc comment), which is exactly the idempotent "already
// generating this? then this call is a harmless no-op" behavior these
// three want - same as before any of them were pipeline tasks.
func (m *Manager) pushGenerateTask(t *pipelineTask) {
	m.pipelineQueue.Push(t)
	select {
	case m.pipelineWake <- struct{}{}:
	default:
	}
	m.notifyChanged()
}

// EnqueueChapter enqueues background TTS generation for every not-yet-
// generated paragraph in one chapter, as a cancelable poolPipeline task
// (pipelineTaskGenerateChapter) - the reader/mobile client's own "Generate
// chapter audio" action, and the per-chapter building block
// EnqueueBookGenerate/EnqueueRemaining below fan out into. Moved off a
// bare fire-and-forget goroutine specifically so it shows up as its own
// row in the Jobs dashboard (Kind "pipeline_generate_chapter"), cancelable
// there the same way a preprocessing phase already is - though for a
// single chapter, whose own listing+pushing is normally fast, that mostly
// buys dashboard visibility and consistency with the other two rather
// than a long window to actually cancel it in. Idempotent per chapter, as
// before: a second call while the first's own task is still queued/in
// flight for this exact chapter is silently dropped (see
// pushGenerateTask's own doc comment).
func (m *Manager) EnqueueChapter(bookID, chapterID string, chapterIdx int) {
	m.pushGenerateTask(&pipelineTask{
		bookID:     bookID,
		kind:       pipelineTaskGenerateChapter,
		chapterIdx: chapterIdx,
		chapterID:  chapterID,
		tier:       TierBackground,
		run: func(ctx context.Context, _ func() int) error {
			m.enqueueChapter(ctx, bookID, chapterID, chapterIdx, 0)
			m.waitForChapterAudio(ctx, map[string]bool{chapterID: true})
			return nil
		},
	})
}

// EnqueueBookGenerate enqueues background TTS generation for every
// chapter in a book at once, as a single cancelable poolPipeline task
// (pipelineTaskGenerateBook) - the library page's "Generate audio" ->
// "All" option (see EnqueueRemaining below for the "Remaining" sibling,
// and handleGenerateBook, which used to loop EnqueueChapter over every
// chapter itself before this existed). Walks every chapter in bookID in
// order, checking ctx between each one so canceling this task's own
// dashboard row stops it from starting any chapter it hasn't reached yet
// - whatever it already pushed into poolGeneration for earlier chapters
// keeps running independently and isn't itself touched, the same
// guarantee CancelPipeline already gives a canceled preprocessing phase.
func (m *Manager) EnqueueBookGenerate(bookID string) {
	m.pushGenerateTask(&pipelineTask{
		bookID: bookID,
		kind:   pipelineTaskGenerateBook,
		tier:   TierBackground,
		run: func(ctx context.Context, _ func() int) error {
			m.enqueueBookGenerate(ctx, bookID)
			return nil
		},
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
// PosChapterIdx/PosParagraphIdx) through the end, as a single cancelable
// poolPipeline task (pipelineTaskGenerateRemaining) - the library page's
// "Generate audio" -> "Remaining" option alongside EnqueueBookGenerate's
// "All". Unlike EnqueueLookahead, there's no LookaheadParagraphCount cap
// (it walks every chapter to the end, not just a short runway) and every
// paragraph pushes at TierBackground rather than TierUrgent/TierLookahead
// - this is bulk background work a reader kicked off deliberately, not
// "the reader is waiting on this specific paragraph right now", the same
// tier EnqueueBookGenerate's own "All" uses. Paragraphs before the stored
// position are left alone, same reasoning as EnqueueLookahead's own doc
// comment: no point generating audio for ones already played past.
// Checks ctx between chapters, same cancellation shape as
// EnqueueBookGenerate above.
func (m *Manager) EnqueueRemaining(bookID string) {
	m.pushGenerateTask(&pipelineTask{
		bookID: bookID,
		kind:   pipelineTaskGenerateRemaining,
		tier:   TierBackground,
		run: func(ctx context.Context, _ func() int) error {
			m.enqueueRemaining(ctx, bookID)
			return nil
		},
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

		resolved, err := m.paragraphsNeedingGeneration(ctx, book, ch.ID)
		if err != nil {
			log.Printf("jobs: list paragraphs for chapter %s: %v", ch.ID, err)
			return
		}
		if chapterIdx == fromChapterIdx && book.PosParagraphIdx > 0 {
			resolved = skipResolvedBeforeIdx(resolved, book.PosParagraphIdx)
		}
		for _, rp := range resolved {
			m.pushResolvedTask(bookID, ch.ID, chapterIdx, TierBackground, rp)
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
	if len(chapterIDs) == 0 {
		return
	}
	isChapterAudio := func(c taskqueue.Task) bool {
		t, ok := c.(*task)
		return ok && (t.kind == KindVoiceClone || t.kind == KindVoiceDesign) && chapterIDs[t.chapterID]
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

// EnqueueAutoSplit queues one "Auto Split" click (httpapi.
// handleReattributeSpeaker) as a single cancelable poolPipeline task
// (pipelineTaskAutoSplit, Kind "pipeline_auto_split") wrapping every
// per-chapter KindSpeakerReattribution task that click fans out into -
// the same "one Jobs-dashboard row for the whole action" grouping
// EnqueueBookGenerate already gives "Generate audio" -> "All", so a
// reader can see (and cancel, or promote) a whole speaker's split at
// once instead of hunting down one row per chapter. run is the caller's
// own fan-out (RunReattribution per chapter, blocking until each
// finishes - see PipelinePhaseFunc), since this package has no Store/
// Speaker access to build the per-chapter work itself. Canceling this row
// cancels run's ctx, which RunReattribution's own blocked waits and any
// not-yet-dispatched chapters observe; a chapter's already-dispatched
// child task keeps running, the same guarantee CancelPipeline gives a
// preprocessing phase. Idempotent per (bookID, speakerKey): a second click
// while the first run is still queued/in flight is a harmless no-op (see
// pushGenerateTask's own doc comment).
func (m *Manager) EnqueueAutoSplit(bookID, speakerKey, speakerName string, run PipelinePhaseFunc) {
	m.pushGenerateTask(&pipelineTask{
		bookID:      bookID,
		kind:        pipelineTaskAutoSplit,
		speakerKey:  speakerKey,
		speakerName: speakerName,
		tier:        defaultReattributionTier,
		run:         run,
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
