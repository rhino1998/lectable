// Package jobs runs chapter text-to-speech generation in the background,
// one paragraph at a time, ordered by a priority queue rather than a plain
// FIFO - see Manager for why.
package jobs

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/sched"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsworker"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// LookaheadParagraphCount is how far ahead of the reader's current position
// EnqueueLookahead generates, regardless of chapter boundaries, when the
// caller doesn't ask for a specific count.
const LookaheadParagraphCount = 25

// LookaheadUrgentParagraphCount is how many paragraph positions, starting
// at the reader's own, EnqueueLookahead pushes at TierUrgent instead of
// TierLookahead (across chapter boundaries, like the window itself).
const LookaheadUrgentParagraphCount = 8

// MaxLookaheadParagraphCount caps a caller-requested lookahead window (see
// EnqueueLookahead), so one client setting can't queue an arbitrarily
// large chunk of a book at TierLookahead - generating a whole book is
// what EnqueueBook/TierBackground is for.
const MaxLookaheadParagraphCount = 1000

// This many paragraphs are kept in flight to tts-service at once: while
// one response is still crossing the network and being written to disk,
// tts-service can already be generating the next one (it releases its
// generation lock as soon as audio is produced, before WAV-encoding the
// response) - see task/handleResult. This is safe under tts-service's
// single generation lock: it still generates one paragraph at a time, just
// without a GPU-idle gap between requests. Deeper than 1-2 mainly buys
// slack against per-request overhead (network, WAV encoding, disk write)
// so a slow one doesn't leave the GPU waiting for its replacement.
// KindVoiceClone/KindVoiceDesign tasks draw from exactly this many slots -
// see poolGeneration/maxAttributionInFlight for why the two LLM kinds
// don't share this pool, and maxDesignInFlight for why KindVoiceProvision
// no longer does either.
//
// Briefly dropped to 1 while troubleshooting a misbehaving ttsworker, then
// restored to 4 once audioworker.Worker.runMu became an fcfsMutex (see its
// own doc comment): up to 4 requests piling up waiting on runMu
// concurrently was never unsafe on its own (Session.Run itself was always
// serialized to 1 regardless of this setting), but under a plain
// sync.Mutex the order they actually got to *run* in was only ever
// loosely tied to the order they arrived in - Go's own mutex doesn't
// guarantee FCFS under light/bursty contention (its starvation-mode
// fairness only kicks in once a waiter's been blocked over 1ms), so it
// could add its own extra reordering on top of whatever timing jitter 4
// independently-dispatched HTTP calls already have. runMu being strictly
// first-come-first-served now removes that extra, gratuitous source of
// reordering - it doesn't make end-to-end ordering perfectly
// deterministic (concurrent dispatch across up to maxInFlight goroutines
// still has its own inherent timing variance before any of them ever
// reach the lock), just no longer worse than that inherent variance.
//
// Raised to 8 for Higgs continuous batching (audioworker.higgsDecodeBatch):
// the one Higgs session decodes every in-flight paragraph in the same step,
// so this is what keeps its 8 batch slots full (throughput RTF 0.094 at 8
// vs 0.108 at 4, at ~1.6x the per-paragraph latency). Other clone families
// still cap themselves via their own session pools.
const maxInFlight = 8

// maxAttributionInFlight bounds poolLLM - the separate, smaller slot pool
// KindSpeakerAttribution/KindSpeakerCharacterization/KindSpeechDirection
// draw from, instead of maxInFlight's own pool. Deliberately not
// maxInFlight: a burst of attribution/characterization requests (e.g.
// attributing a whole book's worth of chapters back to back) used to be
// able to fill every one of maxInFlight's slots with LLM work, leaving
// zero slots for actual TTS paragraph generation until the LLM calls
// drained - a real regression once these kinds were folded into the same
// worker (see Kind's own doc comment history).
//
// Matches llmworker.Config's own default MaxConcurrent (2, see
// cmd/ttsworker/main.go's LECTABLE_SPEAKER_LLM_MAX_CONCURRENT) rather than staying
// at 1 the way it used to: that old value assumed speakerattr.Client fully
// serialized every call behind its own mutex regardless of how many tasks
// were "in flight" here, so a second poolLLM slot could only ever sit
// blocked, never truly run concurrently. That's no longer true -
// speakerattr's own model call now goes through ttsworker's
// internal/llmworker.Worker, which serves up to MaxConcurrent calls at
// once via real multi-sequence batching (llamacpp.Scheduler) against the
// one loaded model - so several poolLLM tasks (e.g. attribution running
// alongside a lazy characterization call, or two different chapters'
// direction-tagging) can now genuinely progress at the same time instead
// of queuing behind each other. Keep this in step with llmworker's own
// MaxConcurrent if that default ever changes - raising one without the
// other just leaves slots that can never actually be used concurrently,
// or tasks that queue here despite the model having a free slot.
//
// KindSpeakerAttribution itself is the one exception to "several poolLLM
// tasks can progress together": task.CanStart caps it to
// a single in-flight attribution task at a time, across every book, on
// top of (not instead of) this pool's own >1 capacity - a deliberate,
// narrower scheduling choice independent of the VRAM reasoning below, so
// raising maxAttributionInFlight further wouldn't let a second attribution
// task run concurrently with the first; it would only free up more room
// for characterization/direction/music-scoring to run alongside the one
// active attribution task.
//
// Lowered from an original 4 to 2: llmworker's own shared KV-cache pool
// (internal/llmworker.contextNCtx) has to reserve room for every one of
// MaxConcurrent slots to independently reach speakerattr.MaxOutputTokens()
// (32768 - the real ceiling directionMaxTokens/sfxMaxTokens legitimately
// need for a batch that echoes several long paragraphs back verbatim) at
// once, on top of every primed system prompt's own fixed cost - a real,
// observed "failed to allocate buffer for kv cache" at MaxConcurrent=4
// (a ~142k-token pool, easily tens of GB of VRAM on top of the model
// weights and whatever TTS clone model shares the same GPU) traced back to
// exactly this multiplication. 2 is a real, evidence-based number (the
// same pool successfully loaded, repeatedly, at MaxConcurrent=2 in
// production logs before it was raised back to 4), not an arbitrary guess
// - see llmworker.Config.MaxConcurrent's own doc comment for the fuller
// VRAM-sizing story.
const maxAttributionInFlight = 2

// envIntOr reads key from the environment and parses it as an int,
// falling back to def if unset or unparseable - internal/audioworker's own
// families.go helper of the same name/shape, duplicated here rather than
// exported across packages for one constant.
func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// maxDesignInFlight bounds poolDesign - the dedicated slot pool every
// VoiceDesign-model task (KindVoiceDesign/KindVoiceProvision/
// KindVoiceDesignPreview - see poolFor) draws from instead of
// poolGeneration's maxInFlight ones. Was a hard-coded const 1 - deliberately
// not just a scheduling-fairness choice the way maxAttributionInFlight's
// own pool split was: audiocpp's own VoiceDesign call (internal/
// audioworker.Worker.Design) used to load a *fresh* full model instance and
// close it again on every single call, unlike Generate/clone's own cached,
// session-reused path - several of these firing concurrently each
// transiently double-loaded a whole multi-GB model into VRAM at once, not
// just several cheap cache hits the way concurrent clones are; this is
// what actually crashed a live ttsworker process during this app's own
// development, under a burst of concurrent voice-provisioning calls
// (runBookPipeline's own "enqueue this whole phase at once" concurrency,
// phase 3) each hitting the same uncoordinated fresh-load path at once.
//
// Raised to 2 (env-configurable, matching internal/audioworker's own
// designPoolSize, which this should stay in step with the same way
// higgsClonePoolSize/maxInFlight already need to) now that Design's own
// root cause is fixed rather than just capped around: it reuses a warm,
// pooled model+session the same way Generate already does (see
// loadedDesign/Worker.getDesignModel, worker.go), so concurrent Design
// calls no longer each race their own full model load - internal/
// audioworker's own loadMu already serializes the one remaining shared
// registry.LoadModel call process-wide regardless of pool size here. No
// two pools (poolGeneration/poolDesign/poolLLM) are ever active together
// at all (see fill/tryFill's own doc comment), so poolDesign still never
// shares VRAM headroom with poolGeneration's own concurrent clones -
// raising this only lets an already-resident design engine's own pooled
// sessions actually run concurrently, the same throughput case
// higgsClonePoolSize's own doc comment makes for Generate.
var maxDesignInFlight = envIntOr("LECTABLE_MAX_DESIGN_IN_FLIGHT", 2)

// maxSFXInFlight bounds poolSFX - shared by every generation-only "gen
// task" engine's own standalone preview render (ACE-Step, Stable Audio -
// Music/SFX/Medium - see RunSFXPreview, POST /api/sfx/generate) as well
// as persisted, paragraph-scoped Stable Audio SFX generation
// (KindSFXGeneration). One shared pool/Kind across every engine rather
// than one pair per engine: audioworker's own auxEngine mutual exclusion
// (worker.go) already means only one of these engines can be resident at
// once regardless, so separate jobs-level pools would just be redundant
// bookkeeping, not real independent concurrency. Defaults to 4, matching
// the combined Stable Audio pool headroom (families.go's
// stableAudioMusicPoolSize/stableAudioSFXPoolSize/
// stableAudioMediumPoolSize) - each is a multi-step diffusion/generation
// call, not a single autoregressive decode, on hardware with no spare
// VRAM headroom to begin with (see backend/CLAUDE.md).
var maxSFXInFlight = envIntOr("LECTABLE_MAX_SFX_IN_FLIGHT", 4)

// maxTaskAttempts bounds how many total dispatches (the first try plus
// retries) a task gets after genuine failures before handleResult gives up
// and reports it as a real failure (AudioError for generation, the error
// itself for an LLM task's waiters) - see task.attempt/the scheduler's retry.
// Matches speakerattr's own maxGenerateRetries convention (2 retries, 3
// attempts total) for a malformed LLM response - same order of magnitude
// reasoning: a transient hiccup (a dropped ttsworker connection, a one-off
// bad sample) is usually not worth surfacing to the reader on the very
// first failure, but retrying forever would never actually give up on a
// genuinely broken paragraph/chapter/character.
const maxTaskAttempts = 3

// retryEligible reports whether a task that just failed with err should be
// retried via the queue (see maxTaskAttempts/the scheduler's retry) rather than
// reported as a real failure: err must be non-nil, err can't be a
// cancellation, and there has to be attempt budget left. The cancellation
// exclusion matters specifically for an explicit reader Cancel/CancelAll: a
// reader who explicitly canceled a task never wants it silently resurrected
// by a generic retry.
func retryEligible(t *task, err error) bool {
	// t.attempt counts dispatches already made, zero-indexed (0 on this
	// task's first ever dispatch), so this task has already been dispatched
	// t.attempt+1 times; retry only if one more dispatch still keeps the
	// total at or under maxTaskAttempts.
	return err != nil && !errors.Is(err, context.Canceled) && t.attempt+1 < maxTaskAttempts
}

// workerCorruptionSignatures are substrings of native ttsworker crash
// messages treated as evidence the whole worker *process* has gone bad -
// not just this one call's input - and needs restarting rather than just
// retried. "no finite logits" (audio.cpp's own message when every logit in
// a model's forward-pass output comes back NaN/Inf - see e.g.
// qwen3_tts/talker.cpp's sample_index, or the same check in
// higgs_audio_tts/vietneu_tts/hf_sampler.cpp and others) is the one
// actually observed live: a preset that failed identically across every
// seed task.runProvision's own seed-bump tried (VoiceDesign is otherwise
// fully deterministic on (instruct, seed, refText, language) - see
// findDerivationSource's own doc comment) went on to succeed against a
// freshly-restarted worker using that exact same seed, so the corruption
// was in the running process/GPU context, not the input.
var workerCorruptionSignatures = []string{"finite logits"}

func looksLikeWorkerCorruption(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, sig := range workerCorruptionSignatures {
		if strings.Contains(msg, sig) {
			return true
		}
	}
	return false
}

// maybeRecoverWorker restarts ttsworker when err looks like
// looksLikeWorkerCorruption - called unconditionally (nil/ordinary errors
// just no-op) from handleResult's own entry point so every task kind gets
// this recovery without duplicating the check across each kind's own
// failure branch. Fire-and-forget and deduplicated via recoveringWorker:
// ttsworker.Manager.Restart's own restartGate already blocks every other
// proxied call for its duration, so nothing here needs to wait for it
// before requeuing/reporting the task that triggered it - whatever
// dispatches next against the worker naturally queues behind the restart
// via that gate, and a second failure arriving while a restart is already
// underway just skips triggering its own redundant one.
func (m *Manager) maybeRecoverWorker(err error) {
	if !looksLikeWorkerCorruption(err) {
		return
	}
	if !m.recoveringWorker.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer m.recoveringWorker.Store(false)
		if rerr := m.tts.Restart(m.ctx, "recovering from a corrupted ttsworker process (\"finite logits\" crash)"); rerr != nil {
			log.Printf("jobs: worker recovery restart failed: %v", rerr)
		}
	}()
}

// Which Kind dispatches through which pool (kindPool) and how many of a
// pool's tasks run at once (poolCapacity) is this package's own knowledge:
// which work shares which GPU-bound resource. The scheduler only sees each
// task's Context and CanStart.
// poolKey names a worker pool: a scheduler context (one pool runs at a
// time) with its own capacity (poolCapacity).
type poolKey string

const (
	// poolGeneration is KindVoiceClone's pool (maxInFlight slots) -
	// GPU-bound paragraph-audio cloning, dispatched to the ttsworker
	// process against a cached, session-reused clone model (audioworker's
	// own LRU + 8-slot session cache) - cheap enough per call that several
	// of these running at once is the whole reason maxInFlight > 1 exists.
	poolGeneration poolKey = "generation"
	// poolDesign is every VoiceDesign-model task's own single dedicated
	// slot (see maxDesignInFlight): KindVoiceDesign (a paragraph rendered
	// straight from a fully custom instruct, no preset/reference clip),
	// KindVoiceProvision (creating/re-rendering a character's own preset
	// reference clip), and KindVoiceDesignPreview (the voice editor's
	// "Test" button) - all three ultimately call the exact same
	// audioworker.Worker.Design, which (unlike poolGeneration's own cached
	// clone path) loads a fresh full model instance for every single call
	// (see maxDesignInFlight's own doc comment for why that makes this
	// pool's slot count, not just its existence, matter). Still TTS-model
	// work like poolGeneration (the same ttsworker process/VRAM budget), not
	// LLM work, so it shares poolGeneration's own mutual exclusion with
	// poolLLM below (enforced generically now by the scheduler itself -
	// see its own "one pool at a time" doc comment - not by anything this
	// package still has to implement), just with its own independent, much
	// smaller slot count.
	poolDesign poolKey = "design"
	// poolSFX is KindSFXGeneration/KindSFXPreview's own shared slot
	// (maxSFXInFlight) - every generation-only "gen task" engine (ACE-Step,
	// Stable Audio - Music/SFX/Medium - see audioworker.Worker's own
	// auxEngine mechanism, worker.go) alongside
	// the clone/design engines poolGeneration/poolDesign already cover
	// (this app's GPU never holds more than one heavy model family
	// resident at once). familyOf defaults an unrecognized pool to "tts",
	// so this needs no explicit case there to correctly share
	// poolGeneration/poolDesign's own mutual exclusion against poolLLM.
	poolSFX poolKey = "sfx"
	// poolLLM is KindSpeakerAttribution/KindSpeakerCharacterization/
	// KindSpeechDirection's pool (maxAttributionInFlight slots) - LLM-bound
	// work dispatched to the ttsworker process via internal/speakerattr
	// (which itself proxies to internal/llmworker, hosted alongside
	// poolGeneration/poolDesign's own TTS models - see backend/
	// CLAUDE.md's "ttsworker / audioworker" section).
	poolLLM poolKey = "llm"
)

// kindPool declares which pool each Kind dispatches through - see
// poolFor, and Kind's own doc comment ("adding a new kind of job means
// extending those switches") for why this is a plain map rather than a
// switch statement: registering a new Kind's pool is now one line here,
// not a second hand-maintained switch case to remember alongside
// dedupKey/the scheduler's retry's own kind-specific branches.
var kindPool = map[Kind]poolKey{
	KindVoiceClone:              poolGeneration,
	KindVoiceDesign:             poolDesign,
	KindSpeakerAttribution:      poolLLM,
	KindSpeakerCharacterization: poolLLM,
	KindSpeakerReattribution:    poolLLM,
	KindSpeechDirection:         poolLLM,
	KindVoiceDesignPreview:      poolDesign,
	KindVoiceProvision:          poolDesign,
	KindLengthEstimate:          poolGeneration,
	KindSFXGeneration:           poolSFX,
	KindSFXPreview:              poolSFX,
	KindLLMPreview:              poolLLM,
	KindAmbience:                poolLLM,
	KindMusicGeneration:         poolSFX,
	KindMusicLiveGeneration:     poolSFX,
	KindScareQuote:              poolLLM,
	KindPronunciation:           poolLLM,
	KindSummary:                 poolLLM,
	KindBookSetting:             poolLLM,
}

// poolFor reports which slot pool kind draws from - see kindPool.
// Defaults to poolGeneration for an unregistered Kind, matching this
// function's own earlier behavior.
func poolFor(kind Kind) poolKey {
	if p, ok := kindPool[kind]; ok {
		return p
	}
	return poolGeneration
}

// familyOf reports which native model family (see backend/CLAUDE.md's
// "ttsworker / audioworker / llmworker" section) pool shares ttsworker's
// one VRAM budget through: poolGeneration and poolDesign both dispatch
// against audioworker's own TTS clone models, so a switch between just
// those two is not a real model-family crossing at all (and unloading on
// every one of those would be pure churn - see poolDesign's own doc
// comment on how often/cheaply it preempts poolGeneration); only crossing
// into or out of poolLLM (llmworker's own GGUF model) is. Used by
// onSwitch.
func familyOf(pool poolKey) string {
	if pool == poolLLM {
		return "llm"
	}
	return "tts"
}

type Manager struct {
	// clips guards the shared clip store - see sharedclips.go.
	clips sharedClips

	// throughput measures recent narration speed - see Throughput.
	throughput narrationThroughput

	// musicGenerating is the set of music region ids currently being
	// generated (claimMusicRegion/releaseMusicRegion) - kept in memory
	// only, never persisted as a status: nothing can still be generating
	// across a restart, so a stored "generating" would only ever go stale.
	musicGenMu      sync.Mutex
	musicGenerating map[string]bool
	// ambienceLocks: see lockAmbience.
	ambienceLocks sync.Map

	store         store.Store
	tts           *ttsworker.Manager
	dataDir       string
	narration     *narration.Resolver
	provision     CharacterVoiceProvisioner
	characterize  CharacterCharacterizer
	direct        ChapterDirector
	attribute     ChapterAttributor
	runAmbience   AmbienceRunner
	scareQuote    ChapterScareQuoter
	pronounce     ChapterPronouncer
	summarize     ChapterSummarizer
	deriveSetting BookSettingDeriver
	// directWindow/pronounceWindow: see SetChapterWindowDirector.
	directWindow    ChapterWindowPass
	pronounceWindow ChapterWindowPass
	// passWindows records, per pass kind and chapter, the paragraph idxs a
	// window run has already covered in this process (guarded by mu) - see
	// windowDependency.
	passWindows map[string]map[int]bool
	// settingTried is the set of books (bookID -> true) whose setting pass
	// has run in this process, whatever the outcome - see
	// bookSettingDependency.
	settingTried sync.Map
	ctx          context.Context // set by Start; used by fire-and-forget Enqueue* resolution goroutines

	// sched is the whole priority/dependency/dispatch engine (see
	// internal/sched and docs/scheduler-design.md). This package is the
	// business logic on top: what a Kind is, what each task needs first
	// (needs), and what happens when it runs (task.Run, handleResult).
	sched *sched.Scheduler
	// started is set by Start; nothing dispatches before it.
	started bool
	// resuming maps a paused pass's key to its task while that task's
	// requeue callback runs - see task.Run and pushTask.
	resuming map[string]*task

	mu             sync.Mutex
	chapterPending map[string]int // chapterID -> count of queued+in-flight tasks, for IsGenerating

	// lookaheads coalesces EnqueueLookahead per book (guarded by
	// lookaheadMu): at most one enqueueLookahead runs per book, and
	// requests arriving meanwhile collapse into one pending follow-up
	// holding the newest position - see EnqueueLookahead.
	lookaheadMu sync.Mutex
	lookaheads  map[string]*lookaheadRun
	// paused stops any new task from being dispatched (see Pause/Resume,
	// gate). Whatever's already in flight keeps running to completion.
	paused bool
	// drained is set once a Pause has seen its in-flight work finish and
	// the models it left resident unloaded (unloadAllOnceIdle), cleared
	// again by Pause/Resume. While paused and drained, worker still
	// dispatches explicit previews (isExplicitPreview) - one at a time in
	// their own pool, with nothing else running - so a batch of them (an
	// attribution eval) never interleaves with generation, never triggers
	// a TTS<->LLM model swap per call, and never has a model unloaded out
	// from under it. See WaitDrained.
	drained bool

	// changeMu/changeSubs back SubscribeChanges/notifyChanged (the Jobs
	// WebSocket's own signal that Snapshot's result just changed) -
	// deliberately a separate mutex from mu, not reused, so notifyChanged
	// can safely be called from any context without any lock-ordering
	// risk.
	changeMu   sync.Mutex
	changeSubs map[chan struct{}]struct{}

	// recoveringWorker guards maybeRecoverWorker against firing several
	// redundant ttsworker restarts back to back when a burst of tasks
	// (e.g. a whole book's worth of voice provisioning) all hit the same
	// underlying corrupted-process crash in quick succession - only the
	// first failure actually triggers a restart; the rest just retry (or
	// wait, via ttsworker.Manager's own restartGate) behind that same
	// in-flight one instead of each queuing a fresh, wasted restart.
	recoveringWorker atomic.Bool

	// pipelineMu serializes EnqueuePipeline's own check-then-push against
	// itself and against CancelPipeline - see EnqueuePipeline's own doc
	// comment for the race it closes.
	pipelineMu sync.Mutex
}

// poolCapacity is how many of a pool's tasks run at once - see
// maxInFlight/maxAttributionInFlight/maxDesignInFlight/maxSFXInFlight for
// why each is sized the way it is.
var poolCapacity = map[poolKey]int{
	poolGeneration: maxInFlight,
	poolLLM:        maxAttributionInFlight,
	poolDesign:     maxDesignInFlight,
	poolSFX:        maxSFXInFlight,
}

// contextDwell is how long after the scheduler switches pools only
// TierUrgent work may force another switch - each one can unload one model
// and load another (tens of seconds with a large LLM).
const contextDwell = time.Minute

func NewManager(s store.Store, tts *ttsworker.Manager, dataDir string) *Manager {
	return newManager(s, tts, dataDir, false)
}

// newManager is NewManager; manual makes the scheduler hand tasks out
// through Pop/Finish instead of running them, for tests that step dispatch.
func newManager(s store.Store, tts *ttsworker.Manager, dataDir string, manual bool) *Manager {
	m := &Manager{
		store:          s,
		tts:            tts,
		dataDir:        dataDir,
		chapterPending: make(map[string]int),
		lookaheads:     make(map[string]*lookaheadRun),
		resuming:       make(map[string]*task),
		passWindows:    make(map[string]map[int]bool),
		changeSubs:     make(map[chan struct{}]struct{}),
	}
	if s != nil {
		m.narration = narration.NewResolver(s)
	}
	m.sched = sched.New(context.Background(), sched.Config{
		MaxAttempts: maxTaskAttempts,
		Dwell:       contextDwell,
		DwellExempt: TierUrgent,
		OnSwitch:    m.onSwitch,
		OnChange:    m.notifyChanged,
		Manual:      manual,
	})
	m.sched.SetGate(m.gate)
	return m
}

// SubscribeChanges returns a channel that receives a signal every time the
// queue's state changes in a way that could change what Snapshot returns -
// a task pushed, dispatched into a worker slot, finished, or cancelled
// (see notifyChanged's own call sites). httpapi's Jobs WebSocket handler
// uses this to know when to push a fresh Snapshot to connected dashboard
// clients, rather than diffing individual mutations itself: every signal
// just means "call Snapshot again," never carries what changed, so the
// handler (and so the frontend) always re-syncs to the current full state
// rather than trying to apply a partial patch. Call UnsubscribeChanges
// with the same channel once the connection closes.
func (m *Manager) SubscribeChanges() chan struct{} {
	ch := make(chan struct{}, 1)
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	m.changeSubs[ch] = struct{}{}
	return ch
}

func (m *Manager) UnsubscribeChanges(ch chan struct{}) {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	delete(m.changeSubs, ch)
}

// notifyChanged signals every SubscribeChanges subscriber - buffered
// (size 1) + non-blocking send, so a slow subscriber can never block the queue: a
// subscriber that hasn't consumed the previous signal yet just coalesces
// into one pending "something changed," which is fine since the next
// Snapshot call always reflects the full current state regardless of how
// many changes happened since the last signal it actually consumed.
func (m *Manager) notifyChanged() {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	for ch := range m.changeSubs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// provisionMissingCharacterVoices lazily provisions (via m.provision - a
// no-op if never configured) a voice for cloneModel for every distinct
// real speaker (not "", not "Narrator") among speakers who doesn't have
// one yet - the first time their dialogue is actually about to be
// generated, rather than eagerly during attribution (see
// httpapi.Server.attributeChapter, which no longer does this itself).
// Only meaningful when book.MultiVoice is on; a no-op otherwise since
// narration.Resolver.ForParagraph would resolve every paragraph to the
// book's own voice regardless of any character's assignment. Best-effort:
// a failure for one character is logged and skipped, not returned -
// paragraphsNeedingGeneration's/enqueueParagraphRegenerate's own
// subsequent ForParagraph resolution just falls back to the book's voice
// for that character a little longer, same as if this provisioner didn't
// exist at all.
func (m *Manager) provisionMissingCharacterVoices(ctx context.Context, book *store.Book, speakers []string, cloneModel string) {
	if !book.MultiVoice() || m.provision == nil {
		return
	}
	seen := map[string]bool{}
	for _, name := range speakers {
		if name == "" || name == "Narrator" || seen[name] {
			continue
		}
		seen[name] = true
		char, err := m.store.GetCharacterByName(store.SeriesScope(book), name)
		if err != nil {
			log.Printf("jobs: look up character %q: %v", name, err)
			continue
		}
		// An invalid character's lingering lines are awaiting
		// redistribution (see store.Character.Invalid) - not worth
		// provisioning a voice for.
		if char == nil || char.Invalid {
			continue
		}
		presetID, err := m.store.CharacterVoiceForModel(char.ID, cloneModel)
		if err != nil {
			log.Printf("jobs: check voice for character %q: %v", name, err)
			continue
		}
		if presetID != "" {
			continue
		}
		if (book.InstructCharacterVoices() || book.InstructAllCharacterVoices()) && char.Summary != "" && cloneModel == voices.InstructedCloneModel {
			// Already characterized, and this book resolves this character
			// straight to the narrator's own voice for this clone model
			// instead of a dedicated preset (see narration.Resolver.
			// ResolveCharacterVoice's own InstructUnassigned/InstructAll
			// branches) - nothing left to provision. m.provision below is
			// still reached (not skipped by this check) while char.Summary
			// is still "" - characterization itself has to happen
			// somewhere, and m.provision's own characterize-then-create
			// path is it; the "create" half becomes a no-op for these modes
			// - see httpapi.provisionCharacterVoiceAttempt's own doc
			// comment.
			continue
		}
		// Routed through the task queue (RunVoiceProvision), not called
		// directly - still blocks this same way, but now shows up on the
		// Jobs dashboard like any other task instead of running as an
		// invisible side effect of chapter/lookahead enqueueing.
		charVal := *char
		if _, err := m.RunVoiceProvision(ctx, book.ID, charVal.ID, "generate", charVal.Name, func(ctx context.Context, attempt int) (string, error) {
			return m.provision(ctx, book.ID, charVal, cloneModel)
		}); err != nil {
			log.Printf("jobs: provision voice for character %q: %v", name, err)
		}
	}
}

// seriesFor resolves bookID's current SeriesName/SeriesIndex for ordering
// poolLLM ("pre-processing") tasks against others in the same series - see
// task.seriesName/seriesIndex and task.Less. Returns ("", 0) if the
// book can't be found (e.g. deleted between the HTTP request and this
// enqueue call) - the same as a book with no series at all, so a lookup
// failure just falls back to ordinary bookID-based ordering rather than
// failing the enqueue outright.
func (m *Manager) seriesFor(bookID string) (name string, index float64) {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil {
		return "", 0
	}
	return book.SeriesName, book.SeriesIndex
}

// Start lets the scheduler dispatch; tasks run under ctx's lifetime (the
// scheduler stops when ctx ends).
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.started = true
	m.mu.Unlock()
	go func() {
		<-ctx.Done()
		m.sched.Close()
	}()
	m.sched.SetManual(false) // a test manager stepped by hand until now
}

// gate is the scheduler's admission gate: nothing before Start; while
// paused, only groups (which do no model work themselves) and, once the
// pause has drained, explicit previews.
func (m *Manager) gate(t sched.Task) bool {
	m.mu.Lock()
	started, paused, drained := m.started, m.paused, m.drained
	m.mu.Unlock()
	if !started {
		return false
	}
	if _, group := t.(*pipelineTask); group || !paused {
		return true
	}
	return drained && isExplicitPreview(t)
}

// logTaskTiming logs one line per finished task: how long it sat queued
// (including time blocked on dependencies) and how long it ran.
func logTaskTiming(t *task, err error) {
	now := time.Now()
	var waited time.Duration
	if !t.pushedAt.IsZero() {
		waited = t.startedAt.Sub(t.pushedAt)
	}
	ran := now.Sub(t.startedAt)
	taskRunSeconds.WithLabelValues(string(t.kind), taskOutcome(err)).Observe(ran.Seconds())
	taskWaitSeconds.WithLabelValues(string(t.kind), tierName(t.tier)).Observe(waited.Seconds())
	status := "ok"
	if err != nil {
		status = "err: " + err.Error()
	}
	log.Printf("jobs: timing %s %s tier=%d attempt=%d waited=%s ran=%s (%s)",
		t.kind, t.dedupKey(), t.tier, t.attempt, waited.Round(time.Millisecond), ran.Round(time.Millisecond), status)
}

func (m *Manager) IsGenerating(chapterID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.chapterPending[chapterID] > 0
}

// Pause stops worker's own fill loop from dispatching any *new* task -
// httpapi's own "pause queue processing" action (a reader wanting
// generation/attribution/etc. to stop drawing GPU/LLM time for a while,
// without losing anything already queued). Whatever's already in flight
// keeps running to completion; nothing here force-cancels it, the same
// non-destructive philosophy this package's own graceful-drain already
// follows elsewhere. Wakes the scheduler immediately (rather than
// waiting for it to notice on its own next iteration) so a Pause called
// while every slot happens to be idle takes effect without any delay,
// and calls notifyChanged so the Jobs dashboard reflects the new state
// right away.
//
// Also frees every native model that might still be resident (both the
// TTS family and the LLM - see unloadAllOnceIdle), once whatever's
// already in flight actually drains: a paused queue means no more GPU/LLM
// work is coming for a while, so there's no reason to leave a model
// sitting in VRAM until ttsworker's own idle timer (MODEL_IDLE_UNLOAD_
// AFTER) eventually gets around to it.
func (m *Manager) Pause() {
	m.mu.Lock()
	wasPaused := m.paused
	m.paused = true
	m.mu.Unlock()
	if wasPaused {
		return // a drain (and its unload) is already under way or done
	}
	m.sched.Kick()
	go m.unloadAllOnceIdle()
}

// Resume undoes Pause, letting worker's fill loop dispatch new work
// again.
func (m *Manager) Resume() {
	m.mu.Lock()
	m.paused = false
	m.drained = false
	m.mu.Unlock()
	m.sched.Kick()
}

// Paused reports whether Pause is currently in effect - see Snapshot,
// which reports this alongside the rest of the queue's state for the
// Jobs dashboard.
func (m *Manager) Paused() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.paused
}

// onSwitch is the scheduler's OnSwitch: the running pool changed. Only a
// genuine TTS<->LLM crossing unloads anything (familyOf) - the family being
// left, so the one arriving has the VRAM - and off the scheduler's
// goroutine, since it's an HTTP round trip to ttsworker.
func (m *Manager) onSwitch(from, to string) {
	if m.tts == nil || from == "" || familyOf(poolKey(from)) == familyOf(poolKey(to)) {
		return
	}
	go m.unloadFamily(familyOf(poolKey(from)))
}

// unloadFamily frees family's own native model (audioworker's TTS clone
// models, or llmworker's GGUF model - see familyOf) via m.tts, logging
// rather than propagating any failure: this only ever runs as a best-
// effort VRAM-reclaiming side effect (of a genuine pool-family switch, see
// onSwitch, or of Pause, see unloadAllOnceIdle), never something a
// caller is blocked waiting on - a failed unload just means that model
// stays resident a while longer, no worse than before this existed.
func (m *Manager) unloadFamily(family string) {
	var err error
	if family == "llm" {
		err = m.tts.UnloadLLM(m.ctx)
	} else {
		err = m.tts.UnloadTTS(m.ctx)
	}
	if err != nil {
		log.Printf("jobs: unload %s models: %v", family, err)
	}
}

// unloadAllOnceIdle is Pause's own follow-up (see its doc comment): waits
// for every currently in-flight task to actually finish - Pause itself
// never force-cancels anything, so unloading before that would risk
// yanking a model out from under a request still in flight against it -
// then frees both native model families. It polls: this runs once per
// Pause, so a short loop is simpler than a scheduler hook for "nothing is
// running any more".
func (m *Manager) unloadAllOnceIdle() {
	const pollInterval = 500 * time.Millisecond
	for m.runningTasks() > 0 {
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}

	if err := m.tts.UnloadTTS(m.ctx); err != nil {
		log.Printf("jobs: unload TTS models on pause: %v", err)
	}
	if err := m.tts.UnloadLLM(m.ctx); err != nil {
		log.Printf("jobs: unload LLM models on pause: %v", err)
	}

	m.mu.Lock()
	m.drained = m.paused // a Resume meanwhile means there's nothing to report
	m.mu.Unlock()
	m.sched.Kick() // previews queued meanwhile can go now
}

// runningTasks counts dispatched tasks, groups aside.
func (m *Manager) runningTasks() int {
	return len(m.taskKeys(func(_ *task, st sched.State) bool { return st == sched.Running }))
}

// taskKeys lists the keys of the queued and running tasks (never groups)
// match accepts. match runs under the scheduler's lock.
func (m *Manager) taskKeys(match func(*task, sched.State) bool) []string {
	var keys []string
	m.sched.Each(func(c sched.Task, st sched.State) {
		if t, ok := c.(*task); ok && match(t, st) {
			keys = append(keys, t.Key())
		}
	})
	return keys
}

// WaitDrained blocks until a Pause has fully drained - everything that was
// in flight has finished and resident models are unloaded - or ctx ends,
// or the queue is resumed (reported as an error: there's nothing to wait
// for). From then until Resume, only explicit previews run, alone (see
// Manager.drained): a caller about to queue many of them (an eval run)
// pauses, waits here, runs them, then resumes.
func (m *Manager) WaitDrained(ctx context.Context) error {
	for {
		m.mu.Lock()
		paused, drained := m.paused, m.drained
		m.mu.Unlock()
		switch {
		case !paused:
			return errors.New("job queue is not paused")
		case drained:
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Drained reports whether the queue is paused and fully drained - see
// WaitDrained.
func (m *Manager) Drained() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.paused && m.drained
}

// isExplicitPreview reports whether t is a one-off preview a person or an
// eval asked for (a voice-design or SFX test render, an LLM prompt or
// attribution/scare-quote preview) - the only work a paused, drained queue
// still dispatches.
func isExplicitPreview(c sched.Task) bool {
	t, ok := c.(*task)
	if !ok {
		return false
	}
	switch t.kind {
	case KindLLMPreview, KindVoiceDesignPreview, KindSFXPreview:
		return true
	}
	return false
}
