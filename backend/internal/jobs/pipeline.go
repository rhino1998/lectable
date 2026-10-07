package jobs

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"time"

	"github.com/rhino1998/lectable/backend/internal/sched"
)

// Preprocessing phases and bulk groups (pipelineTask) are tasks in the same
// scheduler as everything else, but need no context (sched.Task.Context is
// ""): a group does no model work itself, it only asks for the real
// per-chapter/per-character tasks and waits for them, so it runs beside
// whatever pool is active and never holds one of its slots. The tasks it
// asks for through the context its run is given are adopted by it (see
// withParent, Manager.pushTask): they take its priority while it runs, and
// are cancelled with it unless something else wants them.

// Book preprocessing runs its phases related by pipelinePhaseDeps (see
// pipelineResolver): scare-quote tagging first, then attribution (it needs
// scare quotes flagged - the same edge Manager.scareQuoteDependency
// enforces per chapter), characterization once that is done (it reads a
// character's attributed dialogue), then voice provisioning (needs
// characterization's own Summary). Direction tagging, pronunciation
// resolution and music scoring need nothing any other phase produces (see httpapi.directChapter/
// ambienceChapter - only the book's resolved voice and each paragraph's
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
	pipelinePhaseAmbience
	pipelinePhaseScareQuote
	pipelinePhasePronunciation
	pipelinePhaseSummary
	pipelinePhaseCount
)

var pipelinePhaseNames = [pipelinePhaseCount]string{
	pipelinePhaseAttribution:      "attribution",
	pipelinePhaseCharacterization: "characterization",
	pipelinePhaseVoiceProvision:   "voice_provision",
	pipelinePhaseDirection:        "direction",
	pipelinePhaseAmbience:         "ambience",
	pipelinePhaseScareQuote:       "scare_quote",
	pipelinePhasePronunciation:    "pronunciation",
	pipelinePhaseSummary:          "summary",
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
	PhaseCharacterization = pipelinePhaseCharacterization
	PhaseVoiceProvision   = pipelinePhaseVoiceProvision
	PhaseDirection        = pipelinePhaseDirection
	PhaseAmbience         = pipelinePhaseAmbience
	PhasePronunciation    = pipelinePhasePronunciation
	PhaseSummary          = pipelinePhaseSummary
)

// pipelinePhaseDeps lists, per phase, the phases of the same book's run
// that must finish before it may dispatch - see the pipelinePhase*
// constants' doc comment for why each edge exists. A phase with no entry
// dispatches immediately.
var pipelinePhaseDeps = [pipelinePhaseCount][]int{
	pipelinePhaseAttribution:      {pipelinePhaseScareQuote, pipelinePhaseSummary},
	pipelinePhaseAmbience:         {pipelinePhaseSummary},
	pipelinePhaseCharacterization: {pipelinePhaseAttribution},
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
	pipelinePhaseAmbience:         "pipeline_ambience",
	pipelinePhaseScareQuote:       "pipeline_scare_quote",
	pipelinePhasePronunciation:    "pipeline_pronunciation",
	pipelinePhaseSummary:          "pipeline_summary",
}

// PipelinePhaseFunc is one phase's own real work for one book - fan out
// whatever per-chapter/per-character tasks that phase covers (via
// RunScareQuote/RunAttribution/RunCharacterization/
// RunVoiceProvision/RunDirection/RunAmbience) and block until every one of them finishes or ctx is
// canceled. Supplied by the caller at EnqueuePipeline time (httpapi's own
// preprocessAttributionPhase/preprocessCharacterizationPhase/
// preprocessVoiceProvisionPhase/preprocessDirectionPhase/
// preprocessAmbiencePhase), since running
// any given phase needs Store/Speaker access this package doesn't have -
// the same reason AttributionFunc/DirectionFunc/CharacterizationFunc are
// all caller-supplied too. A non-nil return is logged by pipelineTask.Run
// but never blocks a later phase from starting (see pipelineResolver's own
// doc comment on what "depends on" means here) - every phase function
// httpapi supplies today is itself best-effort and always returns nil,
// logging its own per-item failures internally exactly as
// runBookPipeline's old inline code blocks did.
//
// tier reports this phase's own pipelineTask.Tier() live, read fresh at
// whatever moment the phase actually dispatches each of its own
// RunAttribution/RunDirection/RunAmbience calls - not a value snapshot
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

// pipelineTask is one group task: either one of a book's
// preprocessing phases (group == nil - see the pipelinePhase* constants), or
// a bulk group (see BulkGroup). Carries no paragraph/voice/LLM-call state of
// its own, just enough identity for pipelineResolver's dependency lookup and
// CancelPipeline/IsPipelineRunning, plus the actual work (run) and its own
// dispatch context (cancel, set once popped - nil while still queued, since
// a queued task has no context of its own yet to cancel).
type pipelineTask struct {
	m      *Manager
	bookID string
	phase  int        // one of the pipelinePhase* constants - only meaningful when group == nil
	group  *BulkGroup // nil for a preprocessing phase
	tier   int
	run    PipelinePhaseFunc
}

func pipelineKey(bookID string, phase int) string {
	return "pipeline:" + bookID + ":" + pipelinePhaseNames[phase]
}

// currentPipelineTier is the group task key's effective tier right now -
// what a phase passes to the Run* calls it makes, so a group bumped while
// it runs asks for the rest of its work at the new tier.
func (m *Manager) currentPipelineTier(key string) int {
	if p, ok := m.sched.Priority(key); ok && p != sched.NoPriority {
		return p
	}
	return defaultAttributionTier
}

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
// sched.Scheduler.Request's own dedup-by-Key doc comment for why this is also
// what makes a duplicate click while one's already queued/in flight a
// harmless no-op.
func (t *pipelineTask) Key() string {
	if t.group != nil {
		return "pipeline:" + t.bookID + ":" + t.group.Key
	}
	return pipelineKey(t.bookID, t.phase)
}

// Context, CanStart, Less, Prepare and Run make pipelineTask a sched.Task -
// see the top of this file.
func (t *pipelineTask) Context() string            { return "" }
func (t *pipelineTask) CanStart([]sched.Task) bool { return true }
func (t *pipelineTask) Less(sched.Task, int) bool  { return false }

// Prepare makes a preprocessing phase wait for the phases
// pipelinePhaseDeps lists for it that are queued or running for the same
// book (EnqueuePipeline pushes them first). A phase function reports its
// own per-item failures and returns nil, so an earlier phase only fails -
// and takes the later ones with it - by being cancelled.
func (t *pipelineTask) Prepare() ([]sched.Need, error) {
	if t.group != nil {
		return nil, nil
	}
	var needs []sched.Need
	for _, phase := range pipelinePhaseDeps[t.phase] {
		needs = append(needs, sched.Need{Key: pipelineKey(t.bookID, phase)})
	}
	return needs, nil
}

func (t *pipelineTask) Run(ctx context.Context, _ int) error {
	key := t.Key()
	err := t.run(withParent(ctx, key), func() int { return t.m.currentPipelineTier(key) })
	if err != nil && ctx.Err() == nil {
		log.Printf("jobs: pipeline task %s: %v", key, err)
	}
	return nil
}

// ErrPipelineAlreadyRunning is EnqueuePipeline's own error when bookID
// already has a preprocessing run in progress - httpapi's
// handlePreprocessBook maps this to a 409, the same status its own old
// Server.pipelineRunning-based dedup returned.
var ErrPipelineAlreadyRunning = errors.New("a preprocessing run is already in progress for this book")

// EnqueuePipeline queues bookID's preprocessing phases (see the
// pipelinePhase* constants) as dependent group tasks and
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
	// Each phase after the phases it waits for, so its Prepare finds them.
	pushed := map[int]bool{}
	var push func(phase int)
	push = func(phase int) {
		if pushed[phase] || phases[phase] == nil {
			return
		}
		pushed[phase] = true
		for _, dep := range pipelinePhaseDeps[phase] {
			push(dep)
		}
		m.sched.Request(sourceDirect, TierBackground, &pipelineTask{
			m:      m,
			bookID: bookID,
			phase:  phase,
			tier:   TierBackground,
			run:    phases[phase],
		})
	}
	for phase := range phases {
		push(phase)
	}
	return nil
}

// IsPipelineRunning reports whether bookID currently has any preprocessing
// phase task queued or in flight - the read side of EnqueuePipeline,
// replacing Server.pipelineRunning's own sync.Map presence check now that
// a run's presence is tracked by the scheduler itself rather than a
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
	running := false
	m.sched.Each(func(c sched.Task, _ sched.State) {
		if t, ok := c.(*pipelineTask); ok && t.bookID == bookID && t.group == nil {
			running = true
		}
	})
	return running
}

// PromotePipeline raises every queued or running phase of bookID's
// preprocessing run to tier, each as PromoteTier would - so the tasks a
// phase has asked for follow. A phase already that urgent is left alone.
// What a group that runs whole pipelines (a series' in-order run) does to
// pass its own promotion on.
func (m *Manager) PromotePipeline(bookID string, tier int) {
	for phase := range pipelinePhaseCount {
		_ = m.PromoteTier(pipelineKey(bookID, phase), tier)
	}
}

// CancelPipeline cancels bookID's in-progress preprocessing run and bulk
// groups, if any - httpapi.handleDeleteBook's best-effort stop for a book
// that's about to stop existing. A queued one is removed; a running one has
// its context cancelled, and the work it asked for that nothing else wants
// goes with it.
func (m *Manager) CancelPipeline(bookID string) {
	m.pipelineMu.Lock()
	defer m.pipelineMu.Unlock()
	m.sched.CancelWhere(func(c sched.Task) bool {
		t, ok := c.(*pipelineTask)
		return ok && t.bookID == bookID
	})
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
// A bulk group dispatches through the group tasks like a preprocessing phase,
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
	// Children picks out the tasks this group's run dispatches, so
	// promoting the group's row (Manager.PromoteTier) promotes them too -
	// see cascadePromotion. nil means promotion doesn't cascade.
	Children *ChildFilter
}

// ChildFilter matches a bulk group's (or preprocessing phase's) own child
// tasks in the queue, always within the group's own book. Every non-zero
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
	pipelinePhaseAmbience:      KindAmbience,
	pipelinePhaseScareQuote:    KindScareQuote,
	pipelinePhasePronunciation: KindPronunciation,
	pipelinePhaseSummary:       KindSummary,
}

// children reports which the queue tasks a promotion of t cascades to, or nil.
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
// it's one single task, and sched.Scheduler.Request already dedupes by Key(),
// which is exactly the idempotent "already running this? then this call is
// a harmless no-op" behavior a repeated click wants.
func (m *Manager) EnqueueBulk(bookID string, g BulkGroup, run PipelinePhaseFunc) {
	m.sched.Request(sourceDirect, g.Tier, &pipelineTask{
		m:      m,
		bookID: bookID,
		group:  &g,
		tier:   g.Tier,
		run:    run,
	})
}

// BulkID is the task id (QueueTask.ID - what Cancel and PromoteTier take)
// of bookID's bulk group with BulkGroup.Key key.
func BulkID(bookID, key string) string { return "pipeline:" + bookID + ":" + key }

// BulkState reports whether bookID's bulk group with BulkGroup.Key key is
// queued or running.
func (m *Manager) BulkState(bookID, key string) (queued, running bool) {
	_, st, ok := m.sched.Get(BulkID(bookID, key))
	return ok && st != sched.Running, ok && st == sched.Running
}

// RenderChapters queues generation for every paragraph of bookID's
// chapters (by index, in order) whose audio isn't ready - failed ones
// included - and blocks until none of those chapters' audio is queued or
// in flight: the "fully rendered" dependency of work that needs finished
// chapters (a book export). Canceling ctx removes their still-queued audio,
// the same as canceling a "generate" row (see waitForChapterAudio). A
// paragraph whose generation fails for good is left failed - callers
// check the result themselves.
func (m *Manager) RenderChapters(ctx context.Context, bookID string, chapterIdxs []int) error {
	pushed := map[string]bool{}
	defer func() { m.waitForChapterAudio(ctx, pushed) }()
	for _, idx := range chapterIdxs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		ch, err := m.store.GetChapterByIdx(bookID, idx)
		if err != nil {
			return err
		}
		if ch == nil {
			return fmt.Errorf("chapter %d not found", idx)
		}
		m.enqueueChapter(ctx, bookID, ch.ID, idx, 0)
		pushed[ch.ID] = true
	}
	return nil
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
		// Anything chapter-scoped in the queue under this chapterID is part of
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

		if err := m.ensureAttributed(ctx, book, ch, TierBackground); err != nil {
			log.Printf("jobs: remaining: chapter %s: attribution before narration (strict order): %v", ch.ID, err)
			return
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
			m.joinGroup(ctx, m.pushResolvedTask(bookID, ch.ID, chapterIdx, TierBackground, rp, all, ""))
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
	isChapterAudio := func(c sched.Task) bool {
		t, ok := c.(*task)
		return ok && slices.Contains(kinds, t.kind) && chapterIDs[t.chapterID]
	}
	pending := func() bool {
		found := false
		m.sched.Each(func(c sched.Task, _ sched.State) { found = found || isChapterAudio(c) })
		return found
	}
	changes := m.SubscribeChanges()
	defer m.UnsubscribeChanges(changes)
	for pending() {
		select {
		case <-ctx.Done():
			// Queued ones only: a paragraph already rendering finishes.
			for _, key := range m.taskKeys(func(t *task, st sched.State) bool { return st != sched.Running && isChapterAudio(t) }) {
				m.sched.Cancel(key)
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
