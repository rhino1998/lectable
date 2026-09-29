// Package jobs runs chapter text-to-speech generation in the background,
// one paragraph at a time, ordered by a priority queue rather than a plain
// FIFO - see Manager for why.
package jobs

import (
	"context"
	"errors"
	"log"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/taskqueue"
	"github.com/rhino1998/lectable/backend/internal/ttsworker"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// LookaheadParagraphCount is how far ahead of the reader's current position
// EnqueueLookahead generates, regardless of chapter boundaries, when the
// caller doesn't ask for a specific count.
const LookaheadParagraphCount = 25

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
// tasks can progress together": globalAttributionSlotDependency caps it to
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
// itself for an LLM task's waiters) - see task.attempt/task.requeueCopy.
// Matches speakerattr's own maxGenerateRetries convention (2 retries, 3
// attempts total) for a malformed LLM response - same order of magnitude
// reasoning: a transient hiccup (a dropped ttsworker connection, a one-off
// bad sample) is usually not worth surfacing to the reader on the very
// first failure, but retrying forever would never actually give up on a
// genuinely broken paragraph/chapter/character.
const maxTaskAttempts = 3

// retryEligible reports whether a task that just failed with err should be
// retried via the queue (see maxTaskAttempts/task.requeueCopy) rather than
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

// poolGeneration/poolDesign/poolLLM are this package's three worker slot
// pools, now just taskqueue.PoolKey values (see kindPool/poolFor) rather
// than a closed, hand-maintained enum - internal/taskqueue itself has no
// idea these three are special, and a caller could introduce a fourth
// simply by having some Kind report a new key, with no changes needed to
// taskqueue's own dispatch machinery at all (see its own doc comment).
// What still IS hand-maintained here, deliberately, is which Kind maps to
// which of these three (kindPool) and each pool's own capacity
// (NewManager's RegisterPool calls, using maxInFlight/
// maxAttributionInFlight/maxDesignInFlight above) - that mapping is real
// business knowledge (which kind of work shares which GPU-bound resource
// and how many of it can run at once), not something a generic scheduler
// could infer on its own.
const (
	// poolGeneration is KindVoiceClone's pool (maxInFlight slots) -
	// GPU-bound paragraph-audio cloning, dispatched to the ttsworker
	// process against a cached, session-reused clone model (audioworker's
	// own LRU + 8-slot session cache) - cheap enough per call that several
	// of these running at once is the whole reason maxInFlight > 1 exists.
	poolGeneration taskqueue.PoolKey = "generation"
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
	// poolLLM below (enforced generically now by taskqueue.Queue itself -
	// see its own "one pool at a time" doc comment - not by anything this
	// package still has to implement), just with its own independent, much
	// smaller slot count.
	poolDesign taskqueue.PoolKey = "design"
	// poolSFX is KindSFXGeneration/KindSFXPreview's own shared slot
	// (maxSFXInFlight) - every generation-only "gen task" engine (ACE-Step,
	// Stable Audio - Music/SFX/Medium - see audioworker.Worker's own
	// auxEngine mechanism, worker.go) alongside
	// the clone/design engines poolGeneration/poolDesign already cover
	// (this app's GPU never holds more than one heavy model family
	// resident at once). familyOf defaults an unrecognized pool to "tts",
	// so this needs no explicit case there to correctly share
	// poolGeneration/poolDesign's own mutual exclusion against poolLLM.
	poolSFX taskqueue.PoolKey = "sfx"
	// poolLLM is KindSpeakerAttribution/KindSpeakerCharacterization/
	// KindSpeechDirection's pool (maxAttributionInFlight slots) - LLM-bound
	// work dispatched to the ttsworker process via internal/speakerattr
	// (which itself proxies to internal/llmworker, hosted alongside
	// poolGeneration/poolDesign's own TTS models - see backend/
	// CLAUDE.md's "ttsworker / audioworker" section).
	poolLLM taskqueue.PoolKey = "llm"
)

// kindPool declares which pool each Kind dispatches through - see
// poolFor, and Kind's own doc comment ("adding a new kind of job means
// extending those switches") for why this is a plain map rather than a
// switch statement: registering a new Kind's pool is now one line here,
// not a second hand-maintained switch case to remember alongside
// dedupKey/requeueCopy's own kind-specific branches.
var kindPool = map[Kind]taskqueue.PoolKey{
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
	KindDescription:             poolLLM,
	KindPronunciation:           poolLLM,
	KindSummary:                 poolLLM,
	KindBookSetting:             poolLLM,
}

// poolFor reports which slot pool kind draws from - see kindPool.
// Defaults to poolGeneration for an unregistered Kind, matching this
// function's own pre-taskqueue behavior.
func poolFor(kind Kind) taskqueue.PoolKey {
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
// onPoolActivate.
func familyOf(pool taskqueue.PoolKey) string {
	if pool == poolLLM {
		return "llm"
	}
	return "tts"
}

type Manager struct {
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
	// settingTried is the set of books (bookID -> true) whose setting pass
	// has run in this process, whatever the outcome - see
	// bookSettingDependency.
	settingTried sync.Map
	ctx          context.Context // set by Start; used by fire-and-forget Enqueue* resolution goroutines

	// queue is this package's whole priority/dependency/pool-dispatch
	// engine, generalized into internal/taskqueue (see its own doc
	// comment) rather than hand-rolled here - this package's own job is
	// just the business logic on top: what a Kind is, how each dependency
	// kind decides "is this already done" before lazily creating a fresh
	// task for it (resolveDependencies, taskqueue.Resolver's one
	// implementation here), and what actually happens when a task
	// dispatches/finishes (generate/handleResult).
	queue *taskqueue.Queue

	mu             sync.Mutex
	chapterPending map[string]int // chapterID -> count of queued+in-flight tasks, for IsGenerating

	// lookaheads coalesces EnqueueLookahead per book (guarded by
	// lookaheadMu): at most one enqueueLookahead runs per book, and
	// requests arriving meanwhile collapse into one pending follow-up
	// holding the newest position - see EnqueueLookahead.
	lookaheadMu sync.Mutex
	lookaheads  map[string]*lookaheadRun
	wake        chan struct{}
	// paused, when true, stops worker's own fill loop from dispatching
	// any *new* task at all (see Pause/Resume) - deliberately not a
	// property of m.queue itself (internal/taskqueue has no pause
	// concept, and shouldn't need one just for this): pausing is a
	// business-level "the reader wants generation to stop for a while"
	// action, not a scheduling primitive as fundamental as pools/tiers/
	// dependencies. Whatever's already in flight when Pause is called
	// keeps running to completion, exactly like this package's own
	// graceful-drain behavior elsewhere - nothing here ever force-cancels
	// in-flight work.
	paused bool

	// activeFamily/activeFamilyKnown track which native model family
	// (see familyOf) the queue's currently-active pool last belonged to -
	// see onPoolActivate, which uses this to unload the family being left
	// behind only on a genuine TTS<->LLM crossing, not a same-family
	// poolGeneration<->poolDesign switch.
	activeFamily      string
	activeFamilyKnown bool

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

	// pipelineQueue/pipelineWake/pipelineMu back EnqueuePipeline/
	// CancelPipeline/IsPipelineRunning - see pipeline.go's own doc comment
	// for why book-preprocessing's four ordered phases live on a wholly
	// separate taskqueue.Queue (their own pool, own worker loop) instead of
	// one more PoolKey registered on m.queue above.
	pipelineQueue *taskqueue.Queue
	pipelineWake  chan struct{}
	// pipelineMu serializes EnqueuePipeline's own check-then-push against
	// itself and against CancelPipeline - see EnqueuePipeline's own doc
	// comment for the race it closes.
	pipelineMu sync.Mutex
}

func NewManager(s store.Store, tts *ttsworker.Manager, dataDir string) *Manager {
	m := &Manager{
		store:          s,
		tts:            tts,
		dataDir:        dataDir,
		narration:      narration.NewResolver(s),
		chapterPending: make(map[string]int),
		lookaheads:     make(map[string]*lookaheadRun),
		wake:           make(chan struct{}, 1),
		changeSubs:     make(map[chan struct{}]struct{}),
	}
	m.queue = taskqueue.NewQueue(m.resolveDependencies)
	// Capacities match this package's own pre-taskqueue behavior exactly -
	// see maxInFlight/maxAttributionInFlight/maxDesignInFlight's own doc
	// comments for why each is sized the way it is. OnActivate is wired on
	// every pool so a genuine TTS<->LLM model-family crossing (not a
	// same-family poolGeneration<->poolDesign switch - see familyOf/
	// onPoolActivate) frees the family being left behind right away,
	// instead of leaving it resident until ttsworker's own idle timeout
	// (LECTABLE_MODEL_IDLE_UNLOAD_AFTER) elapses on hardware with no VRAM headroom
	// to spare running both at once.
	// Pools of equal priority take turns in stretches, not task by task:
	// a switch between the TTS and LLM families unloads one and loads the
	// other (onPoolActivate) - tens of seconds with a large LLM - so a tie
	// keeps the active pool for up to poolTieSwitchAfter.
	m.queue.SetTieSwitchAfter(poolTieSwitchAfter)
	m.queue.RegisterPool(poolGeneration, taskqueue.PoolConfig{Capacity: maxInFlight, OnActivate: func() { m.onPoolActivate(poolGeneration) }})
	m.queue.RegisterPool(poolLLM, taskqueue.PoolConfig{Capacity: maxAttributionInFlight, OnActivate: func() { m.onPoolActivate(poolLLM) }})
	m.queue.RegisterPool(poolDesign, taskqueue.PoolConfig{Capacity: maxDesignInFlight, OnActivate: func() { m.onPoolActivate(poolDesign) }})
	m.queue.RegisterPool(poolSFX, taskqueue.PoolConfig{Capacity: maxSFXInFlight, OnActivate: func() { m.onPoolActivate(poolSFX) }})

	// A second, independent Queue (see pipeline.go) - deliberately not one
	// more pool registered on m.queue above, which would wrongly subject
	// book-preprocessing to the same "one pool active at a time" mutual
	// exclusion poolGeneration/poolDesign/poolLLM need for a real shared
	// resource (see poolPipeline's own doc comment).
	m.pipelineQueue = taskqueue.NewQueue(pipelineResolver)
	m.pipelineQueue.RegisterPool(poolPipeline, taskqueue.PoolConfig{Capacity: maxPipelineInFlight})
	m.pipelineWake = make(chan struct{}, 1)
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

// Start launches the single background worker. TTS generation is
// serialized on the Python side anyway (one GPU), so one worker here is
// enough and keeps ordering predictable - it just now pulls from a
// priority queue instead of a FIFO channel.
func (m *Manager) Start(ctx context.Context) {
	m.ctx = ctx
	go m.worker(ctx)
	go m.pipelineWorker(ctx)
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

func (m *Manager) finishTask(t *task) {
	m.queue.Finish(t.dedupKey())
	m.mu.Lock()
	m.chapterPending[t.chapterID]--
	if m.chapterPending[t.chapterID] <= 0 {
		delete(m.chapterPending, t.chapterID)
	}
	m.mu.Unlock()
	m.notifyChanged()
	// Always safe to call more than once/on a nil-context task (t.cancel is
	// only unset for a task that was Cancel()-removed straight out of the
	// queue and never dispatched at all, which never reaches finishTask -
	// see handleResult/worker) - releases the context's own resources now
	// that this task is done, cancelled or not.
	if t.cancel != nil {
		t.cancel()
	}
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
// follows elsewhere. Wakes the worker loop immediately (rather than
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
	m.paused = true
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
	m.notifyChanged()
	go m.unloadAllOnceIdle()
}

// Resume undoes Pause, letting worker's fill loop dispatch new work
// again.
func (m *Manager) Resume() {
	m.mu.Lock()
	m.paused = false
	m.mu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
	m.notifyChanged()
}

// Paused reports whether Pause is currently in effect - see Snapshot,
// which reports this alongside the rest of the queue's state for the
// Jobs dashboard.
func (m *Manager) Paused() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.paused
}

// poolTieSwitchAfter is how long a pool keeps the queue against equally
// urgent work in another pool - see taskqueue.Queue.SetTieSwitchAfter and
// NewManager. Long next to a model swap (a 16.5GB LLM loads in ~15s), short
// next to a whole-book background run.
const poolTieSwitchAfter = 5 * time.Minute

// onPoolActivate is registered as every pool's own taskqueue.PoolConfig.
// OnActivate (see NewManager) - fires synchronously, with the queue's own
// lock held, the moment pool becomes the queue's single active one
// coming from some other pool (never merely for another dispatch from a
// pool that was already active - see PoolConfig.OnActivate's own doc
// comment). Takes only m.mu here (briefly, and never calls back into
// m.queue), never the queue's own lock a second time, so this stays safe
// under OnActivate's own "non-reentrant into Queue" rule.
//
// Compares familyOf(pool) against whichever family was active before:
// poolGeneration<->poolDesign is the same family (see familyOf) and never
// triggers anything here; only a genuine TTS<->LLM crossing schedules
// unloadFamily for whichever family is now being left behind, off the
// hot path in its own goroutine (an HTTP round trip to ttsworker has no
// business blocking this hook).
func (m *Manager) onPoolActivate(pool taskqueue.PoolKey) {
	family := familyOf(pool)

	m.mu.Lock()
	prev := m.activeFamily
	known := m.activeFamilyKnown
	m.activeFamily = family
	m.activeFamilyKnown = true
	m.mu.Unlock()

	if !known || prev == family {
		return
	}
	go m.unloadFamily(prev)
}

// unloadFamily frees family's own native model (audioworker's TTS clone
// models, or llmworker's GGUF model - see familyOf) via m.tts, logging
// rather than propagating any failure: this only ever runs as a best-
// effort VRAM-reclaiming side effect (of a genuine pool-family switch, see
// onPoolActivate, or of Pause, see unloadAllOnceIdle), never something a
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
// then frees both native model families. Polling rather than a callback:
// taskqueue.Queue only has a per-pool OnActivate/OnDeactivate hook fired
// on a *transition* (see onPoolActivate) - "every pool now idle, with
// nothing else to transition to" never fires one, since pausing isn't a
// transition to a different pool at all. This only ever runs once per
// Pause call, so a short poll loop is simpler than adding a new
// Queue-level primitive just for this one caller.
func (m *Manager) unloadAllOnceIdle() {
	const pollInterval = 500 * time.Millisecond
	for len(m.queue.InFlightTasks()) > 0 {
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}

	m.mu.Lock()
	m.activeFamilyKnown = false
	m.mu.Unlock()

	if err := m.tts.UnloadTTS(m.ctx); err != nil {
		log.Printf("jobs: unload TTS models on pause: %v", err)
	}
	if err := m.tts.UnloadLLM(m.ctx); err != nil {
		log.Printf("jobs: unload LLM models on pause: %v", err)
	}
}

// totalSlots is every worker slot across all pools - see taskPool. Slots
// [0, maxInFlight) draw from poolGeneration, the next maxAttributionInFlight
// from poolLLM, and the rest (maxDesignInFlight of them) from poolDesign -
// see poolForSlot, worker's only reader of this specific layout.
// worker keeps pulling the next dispatchable task from m.queue
// (taskqueue.Queue.Pop, which by itself already enforces every pool's own
// capacity, the one-pool-at-a-time mutual exclusion the three real pools
// need against ttsworker's shared VRAM budget, graceful draining, and
// priority/dependency ordering - see that package's own doc comment) and
// starting it, until Pop reports nothing left to dispatch right now, then
// waits for either a fresh push (m.wake) or one of the currently-dispatched
// tasks to finish before reconsidering. Unlike this package's own
// pre-taskqueue version, there's no separate fixed slot layout or
// per-pool tryFill/bestPool logic to maintain here at all - Pop already
// picks the single right thing to dispatch next, pool included, every
// time it's called.
//
// Waiting on a dynamic number of result channels needs reflect.Select -
// Go's select statement itself only takes a fixed, literal set of cases -
// but this runs a handful of times a second at most, so the reflection
// overhead is irrelevant.
func (m *Manager) worker(ctx context.Context) {
	type inflight struct {
		task *task
		ch   chan taskResult
	}
	dispatched := map[string]*inflight{}

	fill := func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			m.mu.Lock()
			paused := m.paused
			m.mu.Unlock()
			if paused {
				return
			}
			tk, ok := m.queue.Pop()
			if !ok {
				return
			}
			t := tk.(*task)
			taskCtx, cancel := context.WithCancel(ctx)
			t.cancel = cancel
			t.startedAt = time.Now()
			m.notifyChanged()
			dispatched[t.dedupKey()] = &inflight{task: t, ch: m.startTask(taskCtx, t)}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		fill()

		// m.wake is always a case here, not just when nothing is dispatched:
		// a task pushed while every eligible slot happens to already be
		// occupied - the exact shape of an urgent voice-provisioning task
		// arriving while poolLLM is busy with a long background attribution
		// run - would otherwise have no way to make this loop notice it
		// until some *unrelated* already-dispatched task happened to finish
		// on its own and unblock the select below; for a large attribution
		// batch that can be minutes away, during which fill() never got a
		// chance to reconsider the queue despite the new arrival. Waking on
		// every push (pushTask already sends to m.wake unconditionally)
		// instead means fill() reconsiders the queue immediately, regardless
		// of what else is still busy. A wake signal that turns out to change
		// nothing (fill() finds nothing new to do) just costs one cheap
		// extra loop iteration.
		cases := make([]reflect.SelectCase, 0, len(dispatched)+2)
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())})
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(m.wake)})
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
			continue // woke by a fresh push - let the next iteration's fill() reconsider the queue
		}
		key := keys[chosen]
		d := dispatched[key]
		delete(dispatched, key)
		m.handleResult(d.task, recv.Interface().(taskResult))
	}
}
