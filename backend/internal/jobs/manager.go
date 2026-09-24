// Package jobs runs chapter text-to-speech generation in the background,
// one paragraph at a time, ordered by a priority queue rather than a plain
// FIFO - see Manager for why.
package jobs

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/musicgen"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/taskqueue"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
	"github.com/rhino1998/lectable/backend/internal/ttsworker"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
	"github.com/rhino1998/lectable/backend/internal/voices"
	"github.com/rhino1998/lectable/backend/internal/wav"
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
const maxInFlight = 4

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
// cmd/ttsworker/main.go's SPEAKER_LLM_MAX_CONCURRENT) rather than staying
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
var maxDesignInFlight = envIntOr("QWEN_TTS_MAX_DESIGN_IN_FLIGHT", 2)

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
var maxSFXInFlight = envIntOr("QWEN_TTS_MAX_SFX_IN_FLIGHT", 4)

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
	KindMusicScoring:            poolLLM,
	KindMusicGeneration:         poolSFX,
	KindMusicLiveGeneration:     poolSFX,
	KindScareQuote:              poolLLM,
	KindDescription:             poolLLM,
	KindPronunciation:           poolLLM,
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

// Kind identifies what sort of work a task represents. The job system
// generalizes over several kinds of work rather than hardcoding "TTS
// generation" as the only thing this package's heap/worker dispatches -
// every Kind is queued through the same priority heap and dispatched into
// one of the worker's slots (see task.kind, QueueTask.Kind, poolFor,
// popTask/startTask/handleResult's own per-kind branches). Adding a new
// kind of job means extending those switches (and picking a taskPool for
// it), not inventing a parallel ad hoc tracking mechanism the way
// attribution's own tracking used to be a wholly separate, hardcoded
// map/struct pair before this type existed.
type Kind string

const (
	// KindVoiceClone is a paragraph rendered by cloning a preset's
	// reference clip (the common case - see Manager.generate).
	KindVoiceClone Kind = "voice_clone"
	// KindVoiceDesign is a paragraph rendered straight from a fully
	// custom instruct via VoiceDesign, with no reference clip/preset
	// backing it. poolDesign, not poolGeneration, despite being ordinary
	// paragraph audio like KindVoiceClone - see poolDesign's own doc
	// comment for why every VoiceDesign-model call shares that one slot
	// regardless of which Kind triggers it.
	KindVoiceDesign Kind = "voice_design"
	// KindSpeakerAttribution is an LLM speaker-attribution run for one
	// chapter (internal/speakerattr) - see EnqueueAttribution, the entry
	// point httpapi calls. Needs a caller-supplied AttributionFunc (this
	// package has no Store/Speaker access of its own), but is otherwise
	// fire-and-forget exactly like KindVoiceClone/KindVoiceDesign's own
	// Enqueue* methods - an earlier version blocked the caller until the
	// task finished (see EnqueueAttribution's own doc comment for why
	// that changed).
	KindSpeakerAttribution Kind = "speaker_attribution"
	// KindSpeakerCharacterization is an LLM voice-characterization run
	// for one character (speakerattr.Client.CharacterizeVoice) - see
	// RunCharacterization. Split out from KindSpeakerAttribution as its
	// own kind (rather than running inline as part of an attribution
	// task, the way auto-discovery's characterization step used to)
	// specifically so a reader can trigger/retry it for one character on
	// its own, visibly, through the same queue - see
	// httpapi.handleCharacterizeSpeaker.
	KindSpeakerCharacterization Kind = "speaker_characterization"
	// KindSpeechDirection is an LLM speech-direction-tagging run for one
	// chapter (internal/speakerattr.Client.DirectChapter) - see
	// EnqueueDirection, the entry point httpapi calls. Shares
	// KindSpeakerAttribution's exact shape: fire-and-forget, poolLLM,
	// requires a caller-supplied DirectionFunc since this package has no
	// Store/Speaker access of its own. A separate Kind (not folded into
	// KindSpeakerAttribution itself) so it shows up distinctly on the Jobs
	// dashboard and can be triggered/tracked independently of attribution -
	// unlike Describe, this isn't chained automatically after a successful
	// attribution run (see httpapi.directChapter's own doc comment for why).
	KindSpeechDirection Kind = "speech_direction"
	// KindSpeakerReattribution is an "Auto Split" run for one chapter
	// (httpapi.reattributeChapterSpeaker, reusing
	// speakerattr.Client.AttributeChapter itself rather than a second
	// prompt): re-judges every paragraph currently credited to one
	// specific speaker - a character believed not to be a real, distinct
	// individual, or literally "Unknown" (see EnqueueReattribution) - and
	// persists only the paragraphs that actually belonged to that speaker,
	// so a false-positive character's lines get redistributed to whichever
	// real character/Narrator/Unknown they actually belong to instead of
	// staying stuck under a name that should never have existed. Shares
	// KindSpeakerAttribution's exact shape (fire-and-forget, poolLLM,
	// caller-supplied ReattributionFunc, cooperative pause/requeue between
	// batches) but is its own Kind, both so it's visible/cancelable
	// distinctly on the Jobs dashboard and so its dedup key can be scoped
	// per (chapter, speaker) rather than per chapter alone (see
	// EnqueueReattribution's own doc comment).
	KindSpeakerReattribution Kind = "speaker-reattribute"
	// KindVoiceDesignPreview is a one-off VoiceDesign render with no
	// backing paragraph - the voice editor's "Test" button (see
	// RunVoiceDesignPreview, httpapi.handleTestVoiceDesign). Shares
	// KindVoiceDesign's underlying ttsworker call and poolDesign slot
	// pool/concurrency limit (see poolFor), but is its own Kind rather
	// than reusing KindVoiceDesign directly: a real KindVoiceDesign task is
	// always paragraph-scoped (chapterIdx/paragraphIdx meaningful, gets
	// persisted to the store on completion - see
	// Manager.handleResult), none of which applies to a preview that was
	// never asked to be saved anywhere. Blocks its caller like
	// RunCharacterization, not fire-and-forget like KindVoiceClone/
	// KindVoiceDesign's own Enqueue* methods - see RunVoiceDesignPreview's
	// own doc comment for why blocking is the right shape here.
	KindVoiceDesignPreview Kind = "voice_design_preview"
	// KindVoiceProvision is one character's voice provisioning or forced
	// regeneration (see EnqueueVoiceProvision, httpapi.
	// handleGenerateCharacterVoices/handleRegenerateCharacterVoices) -
	// creating (or, for regeneration, force-rerendering) a character's
	// voice preset and its reference clip. Shares KindVoiceDesignPreview's
	// general shape: no backing paragraph, fire-and-forget via
	// EnqueueVoiceProvision rather than blocking like RunCharacterization/
	// RunVoiceDesignPreview, since SpeakersPage's "Generate/Regenerate all
	// voices" buttons queue a whole roster's worth at once and don't want
	// to hold a request open per character - but poolDesign (shared with
	// KindVoiceDesign/KindVoiceDesignPreview - see its own doc comment for
	// why all three share one slot - maxDesignInFlight), not poolGeneration,
	// specifically so a whole roster's worth (or runBookPipeline's own
	// concurrent per-phase burst) queued at once can't claim every one of
	// poolGeneration's own maxInFlight slots and starve actual paragraph
	// audio a reader is waiting to hear. Still not
	// poolLLM, even though the underlying work (httpapi.
	// provisionCharacterVoice/regenerateCharacterVoice) can itself enqueue
	// a KindSpeakerCharacterization (poolLLM) task internally when a
	// character isn't characterized yet - queuing this as a second poolLLM
	// task instead would have it permanently occupy poolLLM's own slot(s)
	// waiting for its own inner call to get one, deadlocking (see
	// handleGenerateCharacterVoices' own doc comment, written before this
	// kind existed, for the fuller version of that reasoning). Using a TTS-
	// family pool also means this correctly participates in the worker's
	// own TTS/poolLLM mutual-exclusion (see worker's own fill)
	// instead of bypassing it the way a bare goroutine calling the TTS
	// worker directly would - the whole reason this kind exists rather than
	// staying a detached goroutine.
	KindVoiceProvision Kind = "voice_provision"
	// KindLengthEstimate is EnqueueLengthEstimate's own one-off, throwaway
	// PocketTTS-cloned sample (see task.skipDependencies/task.estimateChars
	// and Manager.estimateLength) - shares KindVoiceClone's exact
	// generate() path (same clone-a-reference-clip call) and poolGeneration
	// slot pool, since it's the same kind of real GPU/model work and
	// should count against that same concurrency budget, but has no
	// backing paragraph at all (see startGenerationTask/handleResult's own
	// early branches for this kind) and always sets skipDependencies -
	// none of characterizationDependency/speechDirectionDependency/
	// referenceClipDependency apply to a self-contained sample with its
	// own already-resolved preset and no chapter pipeline state to wait
	// on.
	KindLengthEstimate Kind = "length_estimate"
	// KindSFXGeneration renders and persists one paragraph's own
	// Stable Audio SFX sound-effect clip (see EnqueueSFXGeneration,
	// httpapi.handleGenerateParagraphSFX) - poolSFX, fire-and-forget like
	// KindVoiceProvision's own explicit callers (the closure httpapi
	// supplies does its own Store/audiopath persistence;
	// this package has none of that access itself), dedup-keyed by
	// paragraph.ID like an ordinary KindVoiceClone task (the default
	// dedupKey case) since a paragraph can only usefully have one SFX
	// render in flight at a time.
	KindSFXGeneration Kind = "sfx_generation"
	// KindSFXPreview is a one-off render with no backing paragraph, from
	// any of this app's generation-only "gen task" engines (ACE-Step,
	// Stable Audio - Music/SFX/Medium) - the standalone
	// POST /api/sfx/generate test surface (see RunSFXPreview,
	// httpapi.handleGenerateSFX). One shared kind across every engine, not
	// one per engine: KindVoiceDesignPreview's own exact shape (reusing
	// task.runPreview/previewWaiters verbatim - both are just "run a
	// caller-supplied closure through a pool slot, return audio bytes"),
	// and which engine actually ran is just whatever the caller's own
	// closure happened to call - this package never needs to know.
	KindSFXPreview Kind = "sfx_preview"
	// KindLLMPreview is a one-off raw system+user prompt call against this
	// app's own embedded speaker-attribution GGUF model, with no backing
	// paragraph/chapter - the standalone POST /api/llm/test surface (see
	// RunLLMPreview, httpapi.handleTestLLM). poolLLM, not a dedicated pool:
	// llmworker already serves concurrent calls natively via multi-sequence
	// batching (see backend/CLAUDE.md's "ttsworker / audioworker /
	// llmworker" section), so this mainly buys Jobs-dashboard visibility
	// and TierUrgent priority over queued background attribution/
	// characterization/direction-tagging work, not GPU-residency mutual
	// exclusion the way poolSFX exists for. KindSFXPreview's own exact
	// shape, reusing runPreviewText/previewTextWaiters instead of
	// runPreview/previewWaiters (a text result, not audio bytes).
	KindLLMPreview Kind = "llm_preview"
	// KindMusicScoring is an LLM tone-region-scoring run for one chapter
	// (internal/speakerattr.Client.ScoreMusic), the entry point behind the
	// reader's per-chapter background-music toggle - see EnqueueMusicScoring,
	// httpapi.scoreChapterMusic. KindSpeechDirection's exact shape
	// (fire-and-forget, poolLLM, defaultAttributionTier, dispatched through
	// the same generic runLLM/llmWaiters machinery handleResult's own
	// poolLLM branch already handles for every kind in that pool) - see its
	// own doc comment for the reasoning, which applies unchanged here.
	KindMusicScoring Kind = "music_scoring"
	// KindMusicGeneration renders and persists every one of a chapter's
	// currently-eligible-in-order background-music regions in one task
	// (internal/musicgen.GenerateRegion, once per region - see
	// EnqueueMusicGeneration/generateChapterMusicBatch) - one task per
	// chapter, not one per region: a region's own clip generates fast
	// (Stable Audio Medium, not a long-running LLM batch) and a
	// "continuation" region needs its own immediately-preceding region's
	// clip anyway, so there's no benefit to splitting a chapter's ready
	// regions across several separately-dispatched/retried tasks the way
	// an earlier per-region version did - see MaybeAdvanceChapterMusic's
	// own doc comment. poolSFX, not a dedicated pool - poolSFX's own doc
	// comment already covers why every Stable Audio engine (SFX or Medium)
	// shares one slot pool regardless of which persisted-vs-preview kind
	// dispatches it. Fire-and-forget like KindSFXGeneration's own explicit
	// caller (jobs.Manager.MaybeAdvanceChapterMusic, not an HTTP handler -
	// a region's generation is entirely background-triggered once its own
	// paragraphs' narration is ready, never a reader-initiated "generate
	// now" click the way KindSFXGeneration's TierUrgent is for), but
	// TierBackground rather than KindSFXGeneration's TierUrgent - nothing
	// the reader is looking at blocks on a region's music actually being
	// ready, the same reasoning TierBackground already carries for
	// KindSpeechDirection/KindSpeakerAttribution.
	KindMusicGeneration Kind = "music_generation"
	// KindMusicLiveGeneration is KindMusicGeneration's reader-driven
	// sibling: one music region at a time, starting from the region the
	// reader is in and chaining forward region by region, each generated
	// as soon as its own paragraphs are voiced rather than waiting for the
	// whole chapter the way KindMusicGeneration does - so live listening
	// gets music while the rest of the chapter's narration is still
	// generating. Same batch function (generateChapterMusicBatch, with a
	// one-region batch), same poolSFX, TierUrgent rather than
	// TierBackground since it's what the reader is hearing right now (the
	// reader's own playback is waiting on it, the same as a paragraph they
	// just jumped to). Keyed by
	// region; the moment one dispatches it queues the next region's task,
	// which depends on it (resolveDependencies) so it's ready to go the
	// instant this one finishes - see advanceLiveMusic. Both kinds can
	// reach the same region, so each region is claimed in memory
	// (Manager.claimMusicRegion) before generating and only ever generated
	// once. See MaybeAdvanceChapterMusic.
	KindMusicLiveGeneration Kind = "music_live_generation"
	// KindScareQuote is an LLM scare-quote-tagging run for one chapter
	// (internal/speakerattr.Client.ScareQuoteChapter, via
	// httpapi.scareQuoteChapterForJob) - see EnqueueScareQuote.
	// KindSpeechDirection's exact shape (fire-and-forget, poolLLM,
	// defaultAttributionTier, caller-supplied ScareQuoteFunc). Every
	// KindSpeakerAttribution/KindDescription task for a chapter depends on
	// this one (scareQuoteDependency): attribution needs to know which
	// quoted spans aren't dialogue before judging speakers, and a scare
	// quote is narration a description can be read from.
	KindScareQuote Kind = "scare_quote_tagging"
	// KindDescription is an LLM description-tagging run for one chapter
	// (internal/speakerattr.Client.DescribeChapter, via
	// httpapi.describeChapterForJob) - see EnqueueDescription. Split out of
	// KindSpeakerAttribution (which used to run it inline after a full
	// attribution pass) so it's independently visible/retryable on the
	// Jobs dashboard. Same shape as KindScareQuote, and depends on it the
	// same way attribution does - but not on attribution itself.
	KindDescription Kind = "description_tagging"
	// KindPronunciation is an LLM pronunciation-resolution run for one
	// chapter (internal/speakerattr.Client.ResolvePronunciation, via
	// httpapi.pronounceChapter) - see EnqueuePronunciation. Split out of
	// KindSpeechDirection (which used to run it as a third sub-pass) since
	// a plain word substitution applies under every clone model, while
	// direction tagging is Higgs-only.
	KindPronunciation Kind = "pronunciation"
)

// Tier is shared across every chapter/paragraph-scoped Kind - a
// KindSpeakerAttribution task carries one of these same values (see
// EnqueueAttribution/defaultAttributionTier) so it sorts through the
// shared heap by the same Urgent/Lookahead/Background rules a queued
// voice-clone/design task does, rather than a separate ad hoc ordering.
const (
	// TierUrgent is a single paragraph the reader explicitly asked for
	// right now - "regenerate this one" - not just "queue it high because
	// it's coming up soon" (TierLookahead). Always dispatched next, ahead
	// of even TierLookahead's own distance-from-current-position sort
	// (see task.Less): there's no "distance" to weigh for an explicit
	// one-off request, it just needs to go first. Negative, not 0, so
	// TierLookahead/TierBackground's own relative ordering (and every
	// existing `tier < tier` comparison between them) doesn't need to
	// change at all.
	TierUrgent = -1
	// TierLookahead is paragraphs the reader is (or will imminently be)
	// waiting on. Always preempts TierBackground work still sitting in the
	// queue - it never interrupts a paragraph tts-service has already
	// started, only picks the next one to dispatch.
	TierLookahead = 0
	// TierNormal sits between TierLookahead and TierBackground - nothing
	// is enqueued at it by default; it's reached only by a reader
	// explicitly promoting a task (PromoteTier, the Jobs page), for work
	// that should jump ahead of the bulk TierBackground backlog without
	// competing with what the reader is actually waiting on right now.
	// Counts as "higher priority" for HasHigherPriorityWork, same as any
	// tier more urgent than TierBackground.
	TierNormal = 1
	// TierBackground is bulk "generate this whole chapter" / upload-sample
	// work - useful to have done eventually, but never worth making the
	// reader wait behind.
	TierBackground = 2
)

// task is one unit of dispatched work - a paragraph's worth of generation
// (KindVoiceClone/KindVoiceDesign, resolved once - book voice settings,
// ref text, etc. - when the task is created rather than re-looked-up by
// the worker), one chapter's speaker attribution, or one character's
// voice characterization (the two LLM kinds - see EnqueueAttribution/
// RunCharacterization). Only the fields relevant to t.kind are populated;
// see generate/handleResult's own per-kind branches for which.
type task struct {
	kind       Kind
	tier       int
	chapterIdx int // this task's chapter's index in the book

	// skipDependencies opts t out of resolveDependencies entirely (see
	// that function's own early check) - set on KindLengthEstimate tasks,
	// whose sample has no chapter-pipeline state (characterization/
	// speech-direction/reference-clip) for those dependencies to
	// meaningfully check in the first place: it resolves and reads its own
	// reference clip itself (EnqueueLengthEstimate), before ever building
	// this task. Never set on any other Kind today, but deliberately a
	// plain field on task rather than a KindLengthEstimate-only special
	// case inside resolveDependencies, so a future one-off kind with the
	// same "self-contained, nothing to wait on" shape can opt in the same
	// way without resolveDependencies growing another kind check.
	skipDependencies bool

	// estimateChars is KindLengthEstimate's own sample's character count -
	// handleResult divides the generated clip's duration by this to get
	// the seconds-per-character Store.SetLengthEstimate persists. Unused
	// by every other Kind.
	estimateChars int

	// attempt counts genuine failures this exact unit of work has already
	// suffered, starting at 0 for a task's first ever dispatch - see
	// maxTaskAttempts/task.requeueCopy. handleResult checks it on a real
	// failure (not a cooperative pause, which doesn't count against it) and,
	// while budget remains, retries by pushing a fresh copy back onto this
	// same priority queue rather than
	// looping inside generate/runLLM/etc. itself: a retry this way is still
	// subject to the same tier/kindPriority ordering, pool mutual-exclusion,
	// and Jobs-dashboard visibility as any other queued task, instead of
	// silently blocking a worker slot in a private retry loop invisible to
	// everything else waiting on that slot.
	attempt int

	// cancel aborts this task's own dispatched context - set only once the
	// task is actually popped off the queue and dispatched into a worker
	// slot (see worker); nil while still queued, since a queued task has
	// no context of its own yet to cancel (Cancel/CancelAll instead just
	// remove it from the queue directly via taskqueue.Queue.Cancel/Drain -
	// see their own doc comments). Calling it doesn't guarantee the underlying work
	// stops immediately: a poolGeneration task's ttsworker HTTP call is
	// genuinely ctx-aware and aborts promptly (internal/ttsworker.Manager.do
	// uses http.NewRequestWithContext, same as poolLLM's own HTTP round
	// trip to ttsworker's /llm/generate now), but a poolLLM task's actual
	// LLM decode loop is not: llmworker.Scheduler checks ctx only before
	// admitting a request into a generation slot, never once decoding is
	// actually underway (llama.cpp's own decode loop is synchronous, with
	// no mid-call cancellation hook - see llamacpp.Scheduler.Generate's own
	// doc comment) - so cancelling an in-flight attribution/characterization task takes
	// effect only once its current batch/call finishes on its own. Either
	// way, the normal handleResult path delivers the resulting error (ctx
	// cancellation or otherwise) to this task's own waiters/store updates
	// exactly as any other failure would - Cancel/CancelAll don't bypass or
	// duplicate that delivery for an in-flight task, only for one still
	// queued (see deliverCanceled).
	cancel context.CancelFunc

	bookID    string
	chapterID string

	// seriesName/seriesIndex are poolLLM ("pre-processing") tasks only -
	// KindSpeakerAttribution/KindSpeakerCharacterization/KindSpeechDirection
	// - resolved from the book at enqueue time (see Manager.seriesFor) and
	// used by task.Less to order same-tier pre-processing tasks by a
	// book's place in its series rather than the arbitrary bookID ordering
	// generation tasks use. seriesName is "" for a book with no series, in
	// which case ordering falls back to that same arbitrary bookID rule -
	// there's nothing to sequence a standalone book's own pre-processing
	// against. Never set on a generation task (KindVoiceClone/KindVoiceDesign/
	// KindVoiceDesignPreview/KindVoiceProvision) - see task.Less's own
	// doc comment for why generation intentionally keeps its existing,
	// non-series-aware ordering.
	seriesName  string
	seriesIndex float64

	// KindVoiceClone/KindVoiceDesign only:
	voiceID   string
	paragraph store.Paragraph
	// mergeParagraphs is non-nil (len >= 2) when this task covers a scare-
	// quote merge group (see store.Paragraph.ScareQuote/
	// pushResolvedTask/scareQuoteMergeGroup) rather than one paragraph -
	// every member, including paragraph itself (always mergeParagraphs[0],
	// the group's anchor: dedupKey below keys off paragraph.ID, so every
	// push for any member of the same group has to agree on the same
	// anchor to actually dedup together). nil (the overwhelming common
	// case) means paragraph is generated alone exactly as before this
	// feature existed - see generate/handleMergedResult for the two
	// branches this drives.
	mergeParagraphs []store.Paragraph
	instruct        string
	presetID        string
	language        string
	refText         string
	speedMultiplier float64
	seed            int
	cloneModel      string
	designModel     string
	// cloneInstruct is a clone-time style instruction sent alongside the
	// reference clip on the actual /generate call (see narration.
	// ResolvedVoice.CloneInstruct's own doc comment for when this is
	// non-empty - only store.Book.InstructCharacterVoices' own fallback
	// path sets it today) - distinct from instruct above, which is a
	// design-time instruction only ever used to render a preset's own
	// standalone reference clip from scratch. Only honored by a family
	// with its own instructOption (breeze_tts - see audioworker.
	// cloneFamily.instructOption), silently ignored otherwise, same as
	// instruct/refText are already resent-but-ignored for a noReference
	// family.
	cloneInstruct string

	// KindSpeakerAttribution/KindSpeakerCharacterization only (the two LLM
	// kinds - see poolFor): the actual call (httpapi supplies this, since
	// it needs Store/Speaker access this package doesn't have), llmKey
	// (this task's dedupKey suffix - a chapterID for attribution, a
	// characterID for characterization), label (a human-readable
	// dashboard identifier for a kind that isn't chapter/paragraph-scoped
	// - "" for attribution, which already has a chapter to show), and
	// every caller currently blocked on this task's result - characterization
	// only, now that attribution is fire-and-forget too (EnqueueAttribution
	// never populates this). llmWaiters is usually one entry, but pushTask's
	// dedup can attach more: if a second RunCharacterization call for the
	// same character arrives while this one is still queued or in flight,
	// its own waiter is appended here (see pushTask) rather than
	// silently dropped, since every RunCharacterization caller needs a real
	// result, not just "someone will eventually handle this." handleResult
	// sends to every entry (a no-op for an attribution task, which has
	// none).
	runLLM     func(ctx context.Context) (attributed int, requeue func(), err error)
	llmKey     string
	label      string
	llmWaiters []chan llmOutcome

	// KindVoiceDesignPreview only: the actual render call (httpapi supplies
	// this, since it needs the exact instruct/text/seed/language a caller
	// typed in - see RunVoiceDesignPreview) and every caller blocked on its
	// result - mirrors llmWaiters' shape exactly, just carrying audio bytes
	// instead of an attributed count, since a preview render has no
	// meaningful "paragraphs attributed" to report. llmKey still doubles as
	// this kind's dedup suffix (see dedupKey) - a unique-per-call value
	// (RunVoiceDesignPreview never reuses one), so previewWaiters is always
	// exactly one entry in practice; the slice shape is kept anyway so
	// pushTask's own generic dedup/merge machinery doesn't need a special
	// case for this kind.
	runPreview     func(ctx context.Context) (audio []byte, err error)
	previewWaiters []chan previewOutcome

	// KindLLMPreview only: previewWaiters' own sibling for a kind that
	// returns raw text instead of audio bytes (see RunLLMPreview,
	// httpapi.handleTestLLM) - same reasoning/shape as runPreview/
	// previewWaiters above, just for a different result type.
	runPreviewText     func(ctx context.Context) (text string, err error)
	previewTextWaiters []chan previewTextOutcome

	// KindVoiceProvision only: the actual provision/regenerate call
	// (httpapi supplies this - see EnqueueVoiceProvision) and every caller
	// currently blocked on its result - mirrors llmWaiters/previewWaiters'
	// shape exactly. Usually empty (EnqueueVoiceProvision's own explicit,
	// batch-friendly callers - the Speakers page's "Generate/Regenerate
	// all voices" buttons, and httpapi.recharacterizeAndInvalidate's own
	// follow-up - are deliberately fire-and-forget, same reasoning as
	// KindSpeakerAttribution's own), but RunVoiceProvision (the lazy path,
	// provisionMissingCharacterVoices) blocks on one so a character's
	// voice resolution can use the freshly provisioned presetID right
	// away - and, as a side effect, makes that otherwise-invisible lazy
	// provisioning show up on the Jobs dashboard the same as an explicit
	// one does, addressing a real gap: it used to call m.provision
	// directly, bypassing the task queue (and so this dashboard) entirely.
	// runProvision's attempt param is this task's own t.attempt at dispatch
	// time (0 on the first try, incremented by requeueCopy on each retry) -
	// see EnqueueVoiceProvision/RunVoiceProvision's own doc comments for why
	// callers need it: VoiceDesign is fully deterministic on (instruct,
	// seed, refText, language), so a caller whose seed is fixed (a
	// preset's own stored one) needs to perturb it by attempt on a retry,
	// or a numerically-unstable crash ("no finite logits") just reproduces
	// identically on every attempt instead of actually getting a second
	// try.
	runProvision     func(ctx context.Context, attempt int) (presetID string, err error)
	provisionWaiters []chan provisionOutcome

	// KindSFXGeneration only: the actual generate-and-persist call
	// (httpapi supplies this - see EnqueueSFXGeneration; it needs Store/
	// TTS/audiopath access this package doesn't have, same reasoning
	// runProvision's own doc comment gives) and every caller currently
	// blocked on its result - mirrors provisionWaiters' shape exactly,
	// usually empty since EnqueueSFXGeneration's only caller
	// (httpapi.handleGenerateParagraphSFX) is fire-and-forget.
	runSFXGen     func(ctx context.Context, attempt int) error
	sfxGenWaiters []chan error

	// KindMusicGeneration only: runSFXGen/sfxGenWaiters' own exact sibling
	// for one chapter's own batch generate-and-persist call, covering every
	// region MaybeAdvanceChapterMusic found eligible at dispatch time (this
	// package builds and supplies the closure itself - see
	// EnqueueMusicGeneration) - always fire-and-forget in practice
	// (MaybeAdvanceChapterMusic never blocks on a waiter), kept as its own
	// field pair rather than reusing runSFXGen/sfxGenWaiters so Jobs-
	// dashboard/log messages naming "music generation" specifically don't
	// have to share a generic "sfx" label with paragraph-scoped
	// KindSFXGeneration tasks.
	runMusicGen     func(ctx context.Context, attempt int) error
	musicGenWaiters []chan error
	// KindMusicLiveGeneration only: the paragraph idx its region starts at
	// (where the chain continues from once it dispatches - see
	// continueLiveMusic), and the region right before it in the chain,
	// whose own still-queued/in-flight task it depends on (see
	// liveMusicDependency) - normally also its seed, except for a
	// chapter's first region, whose chain predecessor is the previous
	// chapter's last region (musicPrevChapterID), depended on only to keep
	// the chain in order.
	musicRegionStart   int
	musicPrevRegionID  string
	musicPrevChapterID string
}

// provisionOutcome is a KindVoiceProvision task's real result, delivered to
// every entry in task.provisionWaiters by handleResult - llmOutcome/
// previewOutcome's sibling for a kind that returns a voice preset id.
type provisionOutcome struct {
	presetID string
	err      error
}

// previewOutcome is a KindVoiceDesignPreview task's real result, delivered
// to every entry in task.previewWaiters by handleResult - llmOutcome's
// sibling for a kind that returns audio bytes instead of an attributed
// count.
type previewOutcome struct {
	audio []byte
	err   error
}

// previewTextOutcome is a KindLLMPreview task's real result, delivered to
// every entry in task.previewTextWaiters by handleResult - previewOutcome's
// own sibling for a kind that returns raw text instead of audio bytes.
type previewTextOutcome struct {
	text string
	err  error
}

// dedupKey identifies t for queue dedup and in-flight tracking (both owned by
// internal/taskqueue.Queue now, keyed by this) -
// a paragraph can only usefully be queued once (its own ID); a chapter's
// attribution, or a character's characterization, can each only usefully
// run once at a time (llmKey - a chapterID or characterID respectively,
// distinctly namespaced per kind so the different id spaces can never
// collide with each other or with a paragraph ID).
func (t *task) dedupKey() string {
	switch t.kind {
	case KindSpeakerAttribution:
		return "attr:" + t.llmKey
	case KindSpeakerCharacterization:
		return "characterize:" + t.llmKey
	case KindSpeechDirection:
		return "direction:" + t.llmKey
	case KindSpeakerReattribution:
		// t.llmKey already namespaces both the chapter and the speaker
		// being eliminated (see EnqueueReattribution) - two different
		// speakers' own "Auto Split" runs against the same chapter must
		// dedup independently of each other, unlike attribution's own
		// chapterID-only key, which only ever has one attribution run per
		// chapter to dedup against in the first place.
		return "reattr:" + t.llmKey
	case KindVoiceDesignPreview:
		// Always unique (see RunVoiceDesignPreview) - two different "Test"
		// clicks are two different, unrelated renders with nothing to
		// usefully dedup against each other, unlike a chapter or character.
		return "design_preview:" + t.llmKey
	case KindSFXPreview:
		// Same "always unique" reasoning as KindVoiceDesignPreview above.
		return "sfx_preview:" + t.llmKey
	case KindLLMPreview:
		return "llm_preview:" + t.llmKey
	case KindVoiceProvision:
		// llmKey already encodes both the character id and which mode
		// ("generate" vs "regenerate" - see EnqueueVoiceProvision) so a
		// generate call for a character and a regenerate call for that
		// same character never collide into one task (they mean different
		// things: idempotent-skip vs force-rerender), while two of the
		// same mode for the same character still correctly join.
		return "voice_provision:" + t.llmKey
	case KindLengthEstimate:
		// Keyed by book, not paragraph.ID (this task has none) - a second
		// upload-triggered estimate for the same book while one's still in
		// flight (shouldn't happen in practice, EnqueueLengthEstimate has
		// exactly one call site, but harmless either way) just dedups
		// instead of double-dispatching.
		return "length_estimate:" + t.bookID
	case KindMusicScoring:
		return "music_score:" + t.llmKey
	case KindMusicGeneration:
		return "music_gen:" + t.llmKey
	case KindMusicLiveGeneration:
		return "music_live:" + t.llmKey
	case KindScareQuote:
		return "scare_quote:" + t.llmKey
	case KindDescription:
		return "describe:" + t.llmKey
	case KindPronunciation:
		return "pronunciation:" + t.llmKey
	default:
		return t.paragraph.ID
	}
}

// requeueCopy returns a fresh *task carrying the same identity/work/waiters
// as t, suitable for pushing back onto the queue for another dispatch after
// a genuine failure - handleResult's own retry path (see maxTaskAttempts,
// which this counts against via attempt). A shallow copy, not a
// hand-picked field list the way its one call site used to build one:
// every field (including llmWaiters/previewWaiters/provisionWaiters, so a
// caller still blocked on RunCharacterization/RunVoiceDesignPreview/
// RunVoiceProvision keeps receiving this same unit of work's eventual
// outcome across a retry rather than being silently dropped) carries over
// automatically, so a future field added to task can't quietly go missing
// from a requeue the way a manually-listed struct literal could. cancel is
// always reset (a requeued copy hasn't been dispatched yet, so it has no
// live context to cancel).
func (t *task) requeueCopy() *task {
	cp := *t
	cp.cancel = nil
	cp.attempt++
	return &cp
}

// kindPriority ranks Kind for tie-breaking same-tier tasks against each
// other, inside task.Less - characterization first, then voice
// provisioning, then actual clone/design generation, the same dependency
// order this package's resolveDependencies enforces as a hard rule
// between pools (a character's voice needs characterizing before it's
// worth provisioning, and provisioned before paragraphs in it generate)
// applied here as a soft default among same-tier candidates more broadly,
// not just when they'd otherwise collide on the same character. Kinds not
// listed (attribution, design previews) rank last (0), tied with each
// other by Less's own further tiebreaks. Higher returned value sorts
// first.
func kindPriority(k Kind) int {
	switch k {
	case KindSpeakerCharacterization:
		return 3
	case KindVoiceProvision:
		return 2
	case KindVoiceClone, KindVoiceDesign:
		return 1
	default:
		return 0
	}
}

// Key/Pool/Tier/Promote satisfy taskqueue.Task - see that package's own
// doc comment for how these feed into dispatch order and priority
// promotion. Promote is only ever called by taskqueue.Queue itself, with
// its own lock already held (see Task.Promote's own doc comment) -
// nothing in this package calls it directly; a task that needs its own
// tier bumped for a different reason (pushTask's own dedup-merge, a
// fresh push at a more urgent tier joining an already-queued one) goes
// through taskqueue.Queue.WithTask instead, for the same lock-safety
// reason.
func (t *task) Key() string             { return t.dedupKey() }
func (t *task) Pool() taskqueue.PoolKey { return poolFor(t.kind) }
func (t *task) Tier() int               { return t.tier }
func (t *task) Promote(newTier int) {
	if newTier < t.tier {
		t.tier = newTier
	}
}

// kindCompare maps each Kind to its own tie-break ordering function, used
// by Less once kindPriority is already tied - a plain per-kind registry
// (like kindPool) rather than one shared function switching on every
// kind's own rule inline, so a future Kind with a genuinely different
// ordering need just registers its own function here instead of growing
// a shared if/else chain everyone else's ordering also has to read past.
// Each returns a cmp.Compare-style int (negative if a sorts first, zero
// if tied, positive if b does), the shape cmp.Or below needs to chain
// several of these as one ordered sequence of tiebreaks. Every kind
// registered below happens to share the same actual behavior today
// (comparePosition) - that's a statement about what these kinds currently
// need, not a reason to collapse them into one shared case; a kind with
// no entry (KindVoiceDesignPreview/KindVoiceProvision) has no
// finer-grained ordering of its own at all, falling straight through to
// taskqueue.Queue's own stable insertion-order tiebreak.
var kindCompare = map[Kind]func(a, b *task) int{
	KindVoiceClone:              comparePosition,
	KindVoiceDesign:             comparePosition,
	KindSpeakerAttribution:      comparePosition,
	KindSpeakerCharacterization: comparePosition,
	KindSpeechDirection:         comparePosition,
	KindSpeakerReattribution:    comparePosition,
	KindScareQuote:              comparePosition,
	KindDescription:             comparePosition,
	KindPronunciation:           comparePosition,
}

// Less breaks a same-pool, same-(promoted-)tier tie between t and other -
// taskqueue.Queue guarantees both, so this never has to re-check either
// itself (see taskqueue.Task.Less's own doc comment). cmp.Or evaluates
// each argument in order and returns the first non-zero one (0 if every
// argument is), the same "compare by kindPriority, and only if that ties
// move on to this kind's own finer-grained rule" chain an if/else ladder
// would otherwise spell out by hand: kindPriority first (characterization
// before provisioning before plain generation, the same order
// resolveDependencies' own dependency chain enforces as a hard block -
// this is just the same preference applied as a soft tiebreak more
// broadly), then whatever kindCompare has registered for t's own kind (a
// permanent tie, comparePosition's own zero value, if there's no entry).
func (t *task) Less(other taskqueue.Task) bool {
	o := other.(*task)
	compare := kindCompare[t.kind]
	if compare == nil {
		compare = func(*task, *task) int { return 0 }
	}
	return cmp.Or(
		cmp.Compare(kindPriority(o.kind), kindPriority(t.kind)), // higher kindPriority sorts first
		compare(t, o),
	) < 0
}

// comparePosition orders by the books' own series first (name, then
// index - see task.seriesName's own doc comment), checked unconditionally
// for every Kind that uses this, not gated to any particular pool: a
// series relationship between two books is a real fact about them
// regardless of what kind of work either task represents. Two tasks with
// no series at all (both "") or belonging to different series still get
// a perfectly well-defined, stable order out of this (alphabetically by
// name, "" sorting first) - there's no need to special-case "doesn't
// apply" separately from "tied", since ordering by name costs nothing
// when it's genuinely a tie (equal names) and gives a harmless, arbitrary
// answer when it isn't (see this function's own doc comment on the "no
// specific ordering guaranteed between series" contract that satisfies).
// Falls back to plain book/chapter/paragraph position once the series
// axis is exhausted too - not raw enqueue order, so a chapter whose
// "generate" request happens to reach the backend later (client-side
// scheduling, retries, network races) never jumps ahead of an earlier
// chapter's still-queued work. A genuine remaining tie (0) falls through
// to taskqueue.Queue's own stable insertion-order tiebreak.
func comparePosition(a, b *task) int {
	return cmp.Or(
		cmp.Compare(a.seriesName, b.seriesName),
		cmp.Compare(a.seriesIndex, b.seriesIndex),
		cmp.Compare(a.bookID, b.bookID),
		cmp.Compare(a.chapterIdx, b.chapterIdx),
		cmp.Compare(a.paragraph.Idx, b.paragraph.Idx),
	)
}

type taskResult struct {
	audio []byte // KindVoiceClone/KindVoiceDesign/KindVoiceDesignPreview/KindSFXPreview only
	// words is audio's own word timings when generate already aligned it
	// for its completeness check (see generateCloneChecked) - nil
	// otherwise, leaving alignment to saveParagraphAudio's own detached
	// pass.
	words      []ttsproto.Word
	text       string // KindLLMPreview only - the raw generated text
	presetID   string // KindVoiceProvision only - the character's (created or reused) voice preset id
	attributed int    // KindSpeakerAttribution only - paragraphs (re)attributed
	// requeue is KindSpeakerAttribution-only: non-nil when AttributionFunc
	// stopped early because HasHigherPriorityWork reported more urgent
	// work waiting (see AttributionFunc's own doc comment), with
	// paragraphs left it never got to. Calling it re-enqueues a follow-up
	// task for just those - handleResult calls it after finishTask has
	// cleared this task's own dedup entry, so the follow-up (same
	// dedupKey) doesn't get silently merged into this now-finishing task
	// instead of actually queuing (see pushTask's own dedup/inFlight
	// check). nil for every other outcome (finished normally, failed, or
	// any non-attribution kind).
	requeue func()
	err     error
}

// AttributionFunc performs one chapter's speaker attribution and reports
// how many paragraphs were (re)attributed - the actual work behind a
// KindSpeakerAttribution task. Supplied by the caller at EnqueueAttribution
// time rather than implemented in this package, since running it needs
// Store/Speaker access this package doesn't have (see
// internal/speakerattr, httpapi.Server.attributeChapter). ctx is always
// the worker loop's own long-lived context (see worker/startTask), not
// tied to any particular HTTP request - the whole point of
// EnqueueAttribution being fire-and-forget is that this keeps running
// regardless of whether the request that triggered it is still around.
//
// requeue is non-nil when this implementation checked HasHigherPriorityWork
// between batches, found some, and stopped before processing every
// paragraph it was given - calling it later (see taskResult.requeue's own
// doc comment on exactly when) queues a follow-up task to pick up whatever
// was left, effectively pausing this (TierBackground) run behind whatever
// more urgent work just showed up rather than making it wait out however
// many batches remain. nil means every paragraph was actually
// processed (successfully or not) - see httpapi.attributeChapter, the one
// implementation of this type, for how it decides.
type AttributionFunc func(ctx context.Context) (attributed int, requeue func(), err error)

// ReattributionFunc performs one chapter's "Auto Split" pass for a single
// speaker and reports how many of that speaker's own paragraphs actually
// got reassigned - the actual work behind a KindSpeakerReattribution task.
// Supplied by the caller at EnqueueReattribution time (see
// httpapi.reattributeChapterSpeaker), since this package has no Store/
// Speaker access of its own. Shares AttributionFunc's exact shape
// (requeue non-nil only when the implementation paused - see
// speakerattr.Client.AttributeChapter's own shouldPause - before judging
// every one of that speaker's paragraphs in this chapter) but is its own
// type, the same way DirectionFunc is kept distinct from AttributionFunc
// despite an identical shape, so the two stay independently documented and
// free to diverge later.
type ReattributionFunc func(ctx context.Context) (reattributed int, requeue func(), err error)

// DirectionFunc performs one chapter's speech-direction tagging and
// reports how many paragraphs were tagged - the actual work behind a
// KindSpeechDirection task. Supplied by the caller at EnqueueDirection
// time (see httpapi.directChapter) for the same reason AttributionFunc is:
// running it needs Store/Speaker access this package doesn't have. Shares
// AttributionFunc's exact shape (requeue non-nil only if the
// implementation paused before finishing) even though httpapi's current
// implementation never actually pauses mid-chapter yet (always nil
// requeue) - kept as a real function type of its own, not a reuse of
// AttributionFunc, so the two stay independently documented and free to
// diverge later.
type DirectionFunc func(ctx context.Context) (tagged int, requeue func(), err error)

// MusicScoreFunc performs one chapter's background-music tone-region
// scoring and reports how many regions were produced - the actual work
// behind a KindMusicScoring task. Supplied by the caller at
// EnqueueMusicScoring time (see httpapi.scoreChapterMusic), for the same
// reason AttributionFunc/DirectionFunc are - running it needs Store/
// Speaker access this package doesn't have. DirectionFunc's exact shape.
type MusicScoreFunc func(ctx context.Context) (scored int, requeue func(), err error)

// ScareQuoteFunc performs one chapter's scare-quote tagging and reports how
// many paragraphs' flags changed - the actual work behind a KindScareQuote
// task (see httpapi.scareQuoteChapterForJob). DirectionFunc's exact shape.
type ScareQuoteFunc func(ctx context.Context) (changed int, requeue func(), err error)

// DescriptionFunc performs one chapter's description tagging - the actual
// work behind a KindDescription task (see httpapi.describeChapterForJob).
// DirectionFunc's exact shape.
type DescriptionFunc func(ctx context.Context) (tagged int, requeue func(), err error)

// PronunciationFunc performs one chapter's pronunciation resolution - the
// actual work behind a KindPronunciation task (see
// httpapi.pronounceChapter). DirectionFunc's exact shape.
type PronunciationFunc func(ctx context.Context) (resolved int, requeue func(), err error)

// CharacterizationFunc performs one character's re-characterization (an
// LLM call - speakerattr.Client.CharacterizeVoice - plus persisting the
// result) - the actual work behind a KindSpeakerCharacterization task.
// Supplied by the caller at RunCharacterization time, since this package
// has no Store/Speaker access of its own; unlike AttributionFunc,
// RunCharacterization still blocks its caller on the real result (a single
// character's characterization is one short LLM call, not a whole
// chapter's worth of batches, so holding a request open for it is fine -
// see EnqueueAttribution's own doc comment for why attribution itself
// stopped doing this).
type CharacterizationFunc func(ctx context.Context) error

// llmOutcome is an LLM-kind task's (KindSpeakerAttribution or
// KindSpeakerCharacterization) real result, delivered to every entry in
// task.llmWaiters by handleResult so each RunCharacterization caller can
// block on it - unlike KindVoiceClone/KindVoiceDesign tasks, which are
// dispatched fire-and-forget by the Enqueue* methods and whose outcome
// only ever needs to reach the store, not a waiting caller.
// KindSpeakerAttribution tasks are fire-and-forget too now (see
// EnqueueAttribution) and so never populate llmWaiters, but still report
// attributed through this same struct on their way to handleResult's
// shared LLM-kind branch. attributed is always 0 for characterization.
type llmOutcome struct {
	attributed int
	err        error
}

// CharacterVoiceProvisioner lazily ensures char has an assigned voice for
// cloneModel - characterizing them first if they aren't yet, then creating
// and assigning a freshly-cloned preset - the first time their voice is
// actually needed for generation (see provisionMissingCharacterVoices,
// called from paragraphsNeedingGeneration/enqueueParagraphRegenerate)
// rather than eagerly the moment attribution discovers them. Supplied once
// at startup (see SetCharacterVoiceProvisioner), since it needs Store/
// Speaker/TTS access this package doesn't have (see
// httpapi.Server.provisionCharacterVoice, which also serializes concurrent
// calls for the same character - this package's own per-characterID
// dedup on KindSpeakerCharacterization tasks, alone, only covers the LLM
// characterization step, not the preset-creation step after it). A never-
// configured provisioner (nil) just means every character-scoped
// generation keeps falling back to the book's own voice until one is
// assigned some other way (attribution's old auto-assign behavior, or a
// reader assigning one manually) - the same as before this existed.
type CharacterVoiceProvisioner func(ctx context.Context, bookID string, char store.Character, cloneModel string) (presetID string, err error)

// CharacterCharacterizer lazily runs one character's own characterization
// (the LLM step behind CharacterizationFunc/RunCharacterization, persisting
// char.Summary/RefLine - see store.Character's own doc comment) the moment
// characterizationDependency discovers a KindVoiceClone/KindVoiceDesign/
// KindVoiceProvision task depends on a speaker who isn't characterized yet
// (char.Summary == "", the same "stored fact, not a task-completion signal"
// finished-state check referenceClipDependency already uses for reference
// clips - see its own doc comment) and finds no characterization task
// already queued or in flight for them either. Supplied once at startup
// (see SetCharacterCharacterizer), since it needs Store/Speaker access this
// package doesn't have - httpapi.Server wires in a wrapper around the same
// characterizeVoice/recharacterizeAndInvalidate pair RunCharacterization's
// own explicit callers use. A never-configured characterizer (nil) just
// means a clone/design/provision task for an uncharacterized speaker is
// never auto-blocked on one materializing - the same as before this
// existed, when only an explicit "Characterize"/"Regenerate" click or
// provisionMissingCharacterVoices' own lazy provisioning path (a different,
// blocking mechanism - see CharacterVoiceProvisioner's own doc comment)
// ever characterized anyone.
//
// A character with no attributed quotes/descriptions gathered yet
// (characterizeVoice's own "wait for more material" no-op, returning ("",
// nil) rather than an error) leaves char.Summary empty even after this
// runs successfully - characterizationDependency has no way to distinguish
// that from "never attempted," so a speaker stuck in that state keeps
// getting a fresh (fast, no-op) characterization task recreated every time
// something next depends on them, until enough material actually exists.
// Accepted as the same tradeoff CharacterVoiceProvisioner's own lazy
// provisioning already makes rather than tracking a separate "attempted
// and gave up" marker for what should be a self-resolving, rare, and cheap
// case in practice.
type CharacterCharacterizer func(ctx context.Context, bookID string, char store.Character) error

// ChapterDirector lazily runs one chapter's own speech-direction tagging
// (the work behind DirectionFunc/EnqueueDirection, persisting per-paragraph
// tags and - on a full, uninterrupted pass - setting Passes.Direction) the
// moment speechDirectionDependency discovers a KindVoiceClone/
// KindVoiceDesign task depends on a chapter that isn't tagged yet
// (!ch.Passes.Direction, the same stored-fact finished-state check
// CharacterCharacterizer/referenceClipDependency use) and finds no
// direction task already queued or in flight for it either.
// Supplied once at startup (see SetChapterDirector), since it needs Store/
// Speaker access this package doesn't have - httpapi.Server wires in a
// wrapper around the same directChapter explicit "Tag directions" clicks
// already use, with onlyIdx always nil (a fresh, full-chapter run - see
// directChapter's own doc comment for what a non-nil onlyIdx means, never
// applicable here since this is never itself a paused continuation).
// directChapter's own pause/resume handling (its own requeue closure,
// re-invoking EnqueueDirection directly) is unaffected by being reached
// this way instead of through an explicit click - it doesn't know or care
// who queued the task it's running as. A never-configured director (nil)
// just means a clone/design task for an untagged chapter is never
// auto-blocked on one materializing - the same as before this existed.
type ChapterDirector func(ctx context.Context, book *store.Book, ch *store.Chapter) (tagged int, requeue func(), err error)

// ChapterAttributor lazily runs one chapter's own speaker attribution (the
// work behind AttributionFunc/EnqueueAttribution, persisting per-paragraph
// speakers and - on a full, uninterrupted pass - setting Passes.Attribution)
// the moment attributionOrderDependency discovers a later chapter's own
// KindSpeakerAttribution task depends on an earlier one in the same book
// that isn't attributed yet (!ch.Passes.Attribution, the same stored-fact
// finished-state check CharacterCharacterizer/ChapterDirector use) and finds
// no attribution task already queued or in flight for it either. Supplied
// once at startup (see SetChapterAttributor), since it needs Store/Speaker
// access this package doesn't have - httpapi.Server wires in a wrapper
// around the same attributeChapter explicit "Attribute speakers" clicks
// already use, with onlyUnattributed always false (a fresh, full-chapter
// run, never itself a paused continuation this way). attributeChapter's own
// pause/resume handling (its own requeue closure, re-invoking
// EnqueueAttribution directly) is unaffected by being reached this way
// instead of through an explicit click - it doesn't know or care who queued
// the task it's running as. A never-configured attributor (nil) just means
// a later chapter's attribution is never auto-blocked on an earlier one
// materializing - the same as before this existed.
type ChapterAttributor func(ctx context.Context, book *store.Book, ch *store.Chapter) (attributed int, requeue func(), err error)

// MusicScorer lazily runs one chapter's own background-music tone-region
// scoring (the work behind MusicScoreFunc/EnqueueMusicScoring, persisting
// new store.MusicRegion rows and - via MaybeAdvanceChapterMusic, called
// from the same real implementation this wraps - dispatching whatever
// region generation that scoring just made eligible) the moment
// maybeScoreChapterMusic discovers a chapter about to have its own
// narration audio generated (enqueueChapter, i.e. every "generate this
// chapter's voice" entry point - EnqueueChapter/EnqueueBookGenerate/
// EnqueueRemaining) hasn't been scored yet (!ch.Passes.Music, the same
// stored-fact finished-state check CharacterCharacterizer/ChapterDirector/
// ChapterAttributor use) for a book with store.Book.MusicEnabled on.
// Supplied once at startup (see SetMusicScorer), since it needs Store/
// Speaker access this package doesn't have - httpapi.Server wires in the
// same scoreChapterMusic explicit "Score music" clicks already use.
// Unlike ChapterDirector/ChapterAttributor, this has no pause/resume
// continuation of its own to worry about being reached a different way -
// scoreChapterMusic's own requeue closure (same as directChapter/
// attributeChapter's) re-invokes EnqueueMusicScoring directly regardless
// of who queued the task it's running as. A never-configured scorer (nil)
// just means generating a chapter's voice never auto-triggers scoring for
// it - the same as before this existed, when a reader had to visit the
// Speakers page and score it (or wait for a bulk "Score all music" run)
// separately from generating its audio.
type MusicScorer func(ctx context.Context, book *store.Book, ch *store.Chapter) (scored int, requeue func(), err error)

// ChapterScareQuoter lazily runs one chapter's own scare-quote tagging (the
// work behind ScareQuoteFunc/EnqueueScareQuote, setting Passes.ScareQuote
// on a full pass) the moment scareQuoteDependency discovers a
// KindSpeakerAttribution/KindDescription task for a chapter that isn't
// tagged yet and finds no scare-quote task already queued or in flight for
// it - ChapterDirector's exact role for speechDirectionDependency. A
// never-configured quoter (nil) just means attribution/description never
// wait on scare-quote tagging.
type ChapterScareQuoter func(ctx context.Context, book *store.Book, ch *store.Chapter) (changed int, requeue func(), err error)

// ChapterPronouncer lazily runs one chapter's own pronunciation resolution
// (the work behind PronunciationFunc/EnqueuePronunciation, setting
// Passes.Pronunciation on a full pass) the moment pronunciationDependency
// discovers a KindVoiceClone/KindVoiceDesign task for a chapter that isn't
// resolved yet - ChapterDirector's exact role for
// speechDirectionDependency. A never-configured pronouncer (nil) just
// means generation never waits on pronunciation resolution.
type ChapterPronouncer func(ctx context.Context, book *store.Book, ch *store.Chapter) (resolved int, requeue func(), err error)

type Manager struct {
	// musicGenerating is the set of music region ids currently being
	// generated (claimMusicRegion/releaseMusicRegion) - kept in memory
	// only, never persisted as a status: nothing can still be generating
	// across a restart, so a stored "generating" would only ever go stale.
	musicGenMu      sync.Mutex
	musicGenerating map[string]bool

	store        *store.Store
	tts          *ttsworker.Manager
	dataDir      string
	narration    *narration.Resolver
	provision    CharacterVoiceProvisioner
	characterize CharacterCharacterizer
	direct       ChapterDirector
	attribute    ChapterAttributor
	scoreMusic   MusicScorer
	scareQuote   ChapterScareQuoter
	pronounce    ChapterPronouncer
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
	wake           chan struct{}
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

func NewManager(s *store.Store, tts *ttsworker.Manager, dataDir string) *Manager {
	m := &Manager{
		store:          s,
		tts:            tts,
		dataDir:        dataDir,
		narration:      narration.NewResolver(s),
		chapterPending: make(map[string]int),
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
	// (MODEL_IDLE_UNLOAD_AFTER) elapses on hardware with no VRAM headroom
	// to spare running both at once.
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

// SetCharacterVoiceProvisioner wires fn in as the lazy voice provisioner -
// see CharacterVoiceProvisioner's own doc comment. Called once from
// cmd/server/main.go after both this Manager and httpapi.Server exist
// (the provisioner needs the latter's Store/Speaker/TTS access), before
// the HTTP server starts accepting requests.
func (m *Manager) SetCharacterVoiceProvisioner(fn CharacterVoiceProvisioner) {
	m.provision = fn
}

// SetCharacterCharacterizer wires fn in as the lazy characterizer - see
// CharacterCharacterizer's own doc comment. Called once from
// httpapi.NewRouter, the same place/timing as SetCharacterVoiceProvisioner.
func (m *Manager) SetCharacterCharacterizer(fn CharacterCharacterizer) {
	m.characterize = fn
}

// SetChapterDirector wires fn in as the lazy director - see
// ChapterDirector's own doc comment. Called once from httpapi.NewRouter,
// the same place/timing as SetCharacterVoiceProvisioner.
func (m *Manager) SetChapterDirector(fn ChapterDirector) {
	m.direct = fn
}

// SetChapterAttributor wires fn in as the lazy attributor - see
// ChapterAttributor's own doc comment. Called once from httpapi.NewRouter,
// the same place/timing as SetCharacterVoiceProvisioner.
func (m *Manager) SetChapterAttributor(fn ChapterAttributor) {
	m.attribute = fn
}

// SetMusicScorer wires fn in as the lazy scorer - see MusicScorer's own
// doc comment. Called once from httpapi.NewRouter, the same place/timing
// as SetCharacterVoiceProvisioner.
func (m *Manager) SetMusicScorer(fn MusicScorer) {
	m.scoreMusic = fn
}

// SetChapterScareQuoter wires fn in as the lazy scare-quote tagger - see
// ChapterScareQuoter's own doc comment. Called once from
// httpapi.NewRouter, the same place/timing as SetChapterDirector.
func (m *Manager) SetChapterScareQuoter(fn ChapterScareQuoter) {
	m.scareQuote = fn
}

// SetChapterPronouncer wires fn in as the lazy pronunciation resolver - see
// ChapterPronouncer's own doc comment. Called once from httpapi.NewRouter,
// the same place/timing as SetChapterDirector.
func (m *Manager) SetChapterPronouncer(fn ChapterPronouncer) {
	m.pronounce = fn
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

// EnqueueChapter now lives in pipeline.go, as a cancelable poolPipeline
// bulk group (see EnqueueChapter's own doc comment).

// EnqueueSample generates only enough leading paragraphs of the chapter to
// cover roughly wordLimit words (whole paragraphs, so it may run a bit
// over), rather than the whole thing. Used right after upload to get a
// couple of real narrated paragraphs fast, which calibrates the book's
// duration estimate without committing to generating an entire chapter the
// reader might never open. Deliberately still a bare goroutine, not a
// pipeline task like EnqueueChapter: this is an internal calibration step
// triggered right after upload, not a user-facing "generate" action with
// anything worth showing/canceling on the Jobs dashboard.
func (m *Manager) EnqueueSample(bookID, chapterID string, chapterIdx, wordLimit int) {
	go m.enqueueChapter(m.ctx, bookID, chapterID, chapterIdx, wordLimit)
}

// EnqueueLengthEstimate renders a short, throwaway sample of chapterID's
// leading text through voices.FastPresetID (cloning via PocketTTS - see
// that preset's own doc comment) and stores the resulting seconds-per-
// character on bookID itself (Store.SetLengthEstimate), completely outside
// this package's normal per-voice paragraph_audio cache/pool machinery:
// the sample is measured and discarded, never written to disk or exposed
// to a reader.
//
// This exists so httpapi.estimateTotalSeconds has a real, generated-audio-
// calibrated number to show right after upload - regardless of how slow
// the book's own actually-selected engine is, and without waiting on
// EnqueueSample's own real narration (which still separately renders in
// the book's own voice for early listening, and still wins once it has
// enough ready audio to calibrate from - see estimateTotalSeconds's own
// doc comment for that tiering).
func (m *Manager) EnqueueLengthEstimate(bookID, chapterID string, wordLimit int) {
	go m.estimateLength(bookID, chapterID, wordLimit)
}

func (m *Manager) estimateLength(bookID, chapterID string, wordLimit int) {
	ctx := m.ctx
	if ctx == nil {
		ctx = context.Background()
	}

	paragraphs, err := m.store.ListParagraphsRaw(chapterID)
	if err != nil {
		log.Printf("jobs: length estimate for book %s: list paragraphs: %v", bookID, err)
		return
	}
	var sample strings.Builder
	chars := 0
	for _, p := range paragraphs {
		if strings.TrimSpace(p.Text) == "" {
			continue
		}
		if sample.Len() > 0 {
			sample.WriteString("\n\n")
		}
		sample.WriteString(p.Text)
		chars += len(p.Text)
		if len(strings.Fields(sample.String())) >= wordLimit {
			break
		}
	}
	if chars == 0 {
		return
	}

	fast := voices.PresetsByID[voices.FastPresetID]
	refPath, err := voicerefs.EnsureFile(ctx, m.tts, m.dataDir, fast.ID, fast.Instruct, fast.Seed, fast.RefText, fast.SpeedMultiplier, fast.DesignModel)
	if err != nil {
		log.Printf("jobs: length estimate for book %s: ensure fast preset reference clip: %v", bookID, err)
		return
	}
	refAudio, err := os.ReadFile(refPath)
	if err != nil {
		log.Printf("jobs: length estimate for book %s: read reference clip: %v", bookID, err)
		return
	}
	audio, err := m.generateClone(ctx, voices.FastCloneModel, refAudio, fast.RefText, "", sample.String(), "")
	if err != nil {
		log.Printf("jobs: length estimate for book %s: generate: %v", bookID, err)
		return
	}
	dur, err := wav.Duration(audio)
	if err != nil || dur <= 0 {
		log.Printf("jobs: length estimate for book %s: parse wav duration: %v", bookID, err)
		return
	}
	if err := m.store.SetLengthEstimate(bookID, dur.Seconds()/float64(chars)); err != nil {
		log.Printf("jobs: length estimate for book %s: save: %v", bookID, err)
	}
}

// resolvedParagraph pairs a paragraph with its own effective narration
// voice (internal/narration.Resolver) - a character's assigned voice can
// now override the book's own per paragraph, so unlike the book-wide
// voiceID this package used before, different paragraphs in one chapter
// can resolve to different voices.
type resolvedParagraph struct {
	paragraph store.Paragraph
	voice     narration.ResolvedVoice
}

// paragraphsNeedingGeneration resolves each of chapterID's paragraphs to
// its own effective voice (internal/narration.Resolver) and returns those
// whose audio isn't ready yet under that voice, in paragraph order -
// different paragraphs in the same chapter can resolve to different
// voices, so this can't be a single chapter-wide SQL query any more.
// Before resolving, lazily provisions a voice for any of this chapter's
// speakers who don't have one yet (see provisionMissingCharacterVoices) -
// this is the actual "first requested" moment for most paragraphs, since
// it's called from every Enqueue* path (enqueueChapter/enqueueLookahead).
//
// Voice resolution and the audio-status lookup are both batched across the
// whole chapter (mirroring httpapi.handleGetChapter's own batching, per
// narration.Resolver.ForParagraph's doc comment) rather than resolved one
// paragraph at a time: an earlier version called ForParagraph plus
// GetParagraphAudioStatus per paragraph here, 2-3 single-row store round
// trips each - individually cheap, but this runs from background
// enqueueChapter/enqueueLookahead goroutines right after every attribution
// run (attribution flips many paragraphs onto brand-new character speakers
// at once), so a long chapter meant O(paragraphs) serialized round trips on
// this package's single shared DuckDB connection, visibly slowing down
// every other concurrent request while it ran.
func (m *Manager) paragraphsNeedingGeneration(ctx context.Context, book *store.Book, chapterID string) ([]resolvedParagraph, error) {
	all, err := m.store.ListParagraphsRaw(chapterID)
	if err != nil {
		return nil, err
	}

	bookVoice, err := m.narration.BookVoice(book)
	if err != nil {
		return nil, err
	}
	speakers := make([]string, len(all))
	for i, p := range all {
		speakers[i] = p.Speaker
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)
	m.provisionMissingCharacterVoices(ctx, book, speakers, cloneModel)

	var presetIDByChar map[string]string
	var charByName map[string]store.Character
	if book.MultiVoice() {
		characters, err := m.store.ListCharacters(store.SeriesScope(book))
		if err != nil {
			return nil, err
		}
		charByName = make(map[string]store.Character, len(characters))
		characterIDs := make([]string, len(characters))
		for i, c := range characters {
			charByName[c.Name] = c
			characterIDs[i] = c.ID
		}
		presetIDByChar, err = m.store.CharacterVoicesForModel(characterIDs, cloneModel)
		if err != nil {
			return nil, err
		}
	}

	resolvedVoiceCache := map[string]narration.ResolvedVoice{} // character id -> resolved, avoids repeat lookups
	voiceByParagraph := make([]narration.ResolvedVoice, len(all))
	idsByVoice := map[string][]string{}
	for i, p := range all {
		v := bookVoice
		if book.MultiVoice() && p.Speaker != "" && p.Speaker != "Narrator" {
			if c, ok := charByName[p.Speaker]; ok {
				if cached, ok := resolvedVoiceCache[c.ID]; ok {
					v = cached
				} else if resolved, err := m.narration.ResolveCharacterVoice(book, bookVoice, c, cloneModel, presetIDByChar[c.ID]); err != nil {
					return nil, err
				} else {
					resolvedVoiceCache[c.ID] = resolved
					v = resolved
				}
			}
		}
		voiceByParagraph[i] = v
		vid := v.VoiceID()
		idsByVoice[vid] = append(idsByVoice[vid], p.ID)
	}

	audioStates := make(map[string]store.AudioState, len(all))
	for vid, ids := range idsByVoice {
		states, err := m.store.ParagraphAudioStatuses(ids, vid)
		if err != nil {
			return nil, err
		}
		for id, st := range states {
			audioStates[id] = st
		}
	}

	out := make([]resolvedParagraph, 0, len(all))
	for i, p := range all {
		if audioStates[p.ID].Status == store.AudioReady {
			continue
		}
		out = append(out, resolvedParagraph{paragraph: p, voice: voiceByParagraph[i]})
	}
	return out, nil
}

// pushResolvedTask is every caller's own single entry point for dispatching
// one not-yet-ready paragraph (enqueueChapter's whole-chapter loop,
// enqueueLookahead's windowed loop, and EnqueueParagraphRegenerate's single
// explicit paragraph) - and so the one place a scare-quote merge group (see
// store.Paragraph.ScareQuote) can be transparently substituted for rp's own
// lone paragraph, without any of those three callers needing to know or
// care. This matters beyond enqueueChapter's own bulk pass: without it,
// EnqueueParagraphRegenerate's "the reader didn't like this line" button
// clicked on any single member of an already-merged group would silently
// re-render just that one paragraph in isolation, fragmenting the group's
// carefully-merged audio right back into the disjointed clips this feature
// exists to avoid.
func (m *Manager) pushResolvedTask(bookID, chapterID string, chapterIdx, tier int, rp resolvedParagraph) {
	if group := m.scareQuoteMergeGroup(chapterID, rp.paragraph); len(group) > 1 {
		m.pushMergedTask(bookID, chapterID, chapterIdx, tier, rp.voice, group)
		return
	}
	m.pushSingleTask(bookID, chapterID, chapterIdx, tier, rp)
}

func (m *Manager) pushSingleTask(bookID, chapterID string, chapterIdx, tier int, rp resolvedParagraph) {
	if err := audiopath.EnsureVoiceDir(m.dataDir, bookID, chapterID, rp.voice.VoiceID()); err != nil {
		log.Printf("jobs: create audio dir: %v", err)
		return
	}
	kind := KindVoiceClone
	if rp.voice.PresetID == "" {
		kind = KindVoiceDesign
	}
	m.pushTask(&task{
		kind:            kind,
		chapterIdx:      chapterIdx,
		tier:            tier,
		bookID:          bookID,
		chapterID:       chapterID,
		voiceID:         rp.voice.VoiceID(),
		paragraph:       rp.paragraph,
		instruct:        rp.voice.Instruct,
		presetID:        rp.voice.PresetID,
		language:        rp.voice.Language,
		refText:         rp.voice.RefText,
		speedMultiplier: rp.voice.SpeedMultiplier,
		seed:            rp.voice.Seed,
		cloneModel:      rp.voice.CloneModel,
		designModel:     rp.voice.DesignModel,
		cloneInstruct:   rp.voice.CloneInstruct,
	})
}

// pushMergedTask is pushSingleTask's own counterpart for a scare-quote
// merge group (see scareQuoteMergeGroup) - one task covering every member
// in members, keyed (via task.dedupKey, which reads t.paragraph) off
// members[0] specifically so a second call landing on any other member of
// the same group (the common case: enqueueChapter's loop reaches every
// not-yet-ready member in turn) correctly dedups against the first rather
// than queuing a second, competing task for the same group.
func (m *Manager) pushMergedTask(bookID, chapterID string, chapterIdx, tier int, voice narration.ResolvedVoice, members []store.Paragraph) {
	if err := audiopath.EnsureVoiceDir(m.dataDir, bookID, chapterID, voice.VoiceID()); err != nil {
		log.Printf("jobs: create audio dir: %v", err)
		return
	}
	kind := KindVoiceClone
	if voice.PresetID == "" {
		kind = KindVoiceDesign
	}
	m.pushTask(&task{
		kind:            kind,
		chapterIdx:      chapterIdx,
		tier:            tier,
		bookID:          bookID,
		chapterID:       chapterID,
		voiceID:         voice.VoiceID(),
		paragraph:       members[0],
		mergeParagraphs: members,
		instruct:        voice.Instruct,
		presetID:        voice.PresetID,
		language:        voice.Language,
		refText:         voice.RefText,
		speedMultiplier: voice.SpeedMultiplier,
		seed:            voice.Seed,
		cloneModel:      voice.CloneModel,
		designModel:     voice.DesignModel,
		cloneInstruct:   voice.CloneInstruct,
	})
}

// InlineParagraphSet returns the full contiguous run of chapterID's
// paragraphs (in Idx order) that paragraph belongs to: paragraph itself,
// plus every paragraph reachable from it by following Inline in either
// direction (the same run internal/epub.splitQuoteSegments originally
// split out of one source paragraph - see store.Paragraph.Inline's own
// doc comment). Unlike scareQuoteMergeGroup below (which narrows this
// down to only when the run is actually eligible for scare-quote
// merging), this applies no ScareQuote/Speaker filtering at all - it's
// httpapi's own tool for invalidating every paragraph whose merge
// eligibility could change together whenever any one member's ScareQuote
// flag changes, not just to decide whether to merge them for generation.
//
// Returns []store.Paragraph{paragraph} for a paragraph with no Inline
// neighbors on either side (the overwhelming common case), and on any
// lookup error (logged, not returned) or an unexpected gap in an
// otherwise-contiguous Idx run (see store.Store's own "Idx is dense per
// chapter" invariant - should never happen).
//
// Deliberately reloads chapterID's whole paragraph list on every call
// (ListParagraphsRaw) rather than threading an already-fetched slice down
// from each caller - see scareQuoteMergeGroup's own doc comment for why
// that's an acceptable cost.
func (m *Manager) InlineParagraphSet(chapterID string, paragraph store.Paragraph) []store.Paragraph {
	alone := []store.Paragraph{paragraph}
	all, err := m.store.ListParagraphsRaw(chapterID)
	if err != nil {
		log.Printf("jobs: inline paragraph set lookup for chapter %s: %v", chapterID, err)
		return alone
	}
	byIdx := make(map[int]store.Paragraph, len(all))
	for _, p := range all {
		byIdx[p.Idx] = p
	}
	// paragraph itself may be a caller's own newer copy (just reset for
	// regenerate, or just flipped ScareQuote) than whatever
	// ListParagraphsRaw just read back - never let a stale re-read of
	// this one paragraph override the value the caller actually asked
	// to dispatch/invalidate.
	byIdx[paragraph.Idx] = paragraph

	start, end := paragraph.Idx, paragraph.Idx
	for {
		cur, ok := byIdx[start]
		if !ok || !cur.Inline {
			break
		}
		start--
	}
	for {
		next, ok := byIdx[end+1]
		if !ok || !next.Inline {
			break
		}
		end++
	}
	if start == end {
		return alone
	}

	group := make([]store.Paragraph, 0, end-start+1)
	for idx := start; idx <= end; idx++ {
		p, ok := byIdx[idx]
		if !ok {
			return alone
		}
		group = append(group, p)
	}
	return group
}

// sentenceTerminators are the trailing characters that end a segment's own
// sentence for scareQuoteMergeGroup's boundary rule below. Only .!?
// actually end a sentence: a colon/semicolon
// reads as part of the same continuous utterance ("He was blunt: 'Get
// out.'" merges into one clip), and an em dash/ellipsis usually signals an
// interrupted or trailing-off thought that keeps going rather than a hard
// stop - so ,;:—… all leave a boundary eligible to merge, only .!? close
// it off.
const sentenceTerminators = ".!?"

// segmentEndsSentence reports whether p's own text - the text just inside
// its closing quote mark, for a quote segment (store.Paragraph.IsQuote),
// or its own text directly otherwise - ends with one of
// sentenceTerminators - see sentenceTerminators' own doc comment.
func segmentEndsSentence(p store.Paragraph) bool {
	text := p.Text
	if p.IsQuote {
		runes := []rune(text)
		if len(runes) < 2 {
			return false
		}
		text = string(runes[:len(runes)-1])
	}
	trimmed := strings.TrimRightFunc(text, unicode.IsSpace)
	if trimmed == "" {
		return false
	}
	runes := []rune(trimmed)
	return strings.ContainsRune(sentenceTerminators, runes[len(runes)-1])
}

// scareQuoteMergeGroup narrows InlineParagraphSet down to just the
// contiguous run around paragraph that's actually eligible to merge into
// one narrator-voiced TTS call - not necessarily paragraph's *whole*
// inline chain (see InlineParagraphSet's own doc comment for what that
// covers). paragraph itself must be narration-voiced (narrationVoiced
// below); the run then grows outward from it, one boundary at a time,
// only as far as two conditions both hold at each step: the neighbor being
// pulled in is itself narration-voiced (never genuine, still-spoken
// dialogue - see narrationVoiced's own comment for why this checks
// IsQuote/ScareQuote directly, not just Speaker), and the boundary itself
// reads as the same sentence continuing rather than a fresh one starting -
// segmentEndsSentence on whichever segment sits on the earlier side of
// that boundary. A boundary merges segment i into segment i+1 exactly when
// segment i's own text doesn't end its sentence - symmetric and
// independent per boundary, so a scare quote naturally merges left only,
// right only, both, or neither depending purely on its own trailing
// punctuation and its immediate narration neighbors' own trailing
// punctuation, never on some other segment's ScareQuote status several
// paragraphs away in the same original source paragraph's inline chain.
//
// Deliberately narrower than an earlier, all-or-nothing version of this
// function (which returned paragraph's *entire* inline chain, or nothing
// at all, keyed on whether every member anywhere in that whole chain was
// narration-voiced): a single genuine line of dialogue or named-speaker
// paragraph anywhere in a long inline chain used to block merging for
// every scare quote in that chain, even ones nowhere near it. Now it's
// only ever a wall the walk below stops at, not a reason to refuse merging
// altogether - another scare quote in the same chain, on the far side of
// that wall from paragraph, still merges on its own once generation
// reaches it as its own anchor.
//
// Returns []store.Paragraph{paragraph} (no merge - callers treat len == 1
// as "generate paragraph alone, exactly as before this feature existed")
// whenever paragraph itself isn't narration-voiced, the resolved run has
// no scare-quoted paragraph in it, or every adjacent boundary reads as its
// own separate sentence.
func (m *Manager) scareQuoteMergeGroup(chapterID string, paragraph store.Paragraph) []store.Paragraph {
	alone := []store.Paragraph{paragraph}
	group := m.InlineParagraphSet(chapterID, paragraph)
	if len(group) <= 1 {
		return alone
	}

	anchor := -1
	for i, p := range group {
		if p.ID == paragraph.ID {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return alone
	}

	// narrationVoiced reports whether p can share the narrator's own TTS
	// call at all - deliberately on IsQuote/ScareQuote directly, not
	// (only) on Speaker: a quote's Speaker only reliably reads a real
	// character name/"Unknown" *after* attribution has actually run over
	// it (disallowNarratorForDialogue enforces that a genuine quote is
	// never left "Narrator") - before that, an unattributed real quote
	// sits at Speaker == "" indistinguishably from plain narration, and
	// manually toggling a paragraph's own ScareQuote flag
	// (handleSetParagraphScareQuote) needs no attribution to have
	// happened first at all. Without this, a reader flagging one segment
	// of an unattributed inline run as a scare quote could pull a
	// genuine, not-yet-(or never-)attributed line of someone else's
	// actual dialogue into the same merged, narrator-voiced clip -
	// permanently baking a real character's line into the wrong voice,
	// since a merged clip is a single continuous recording with no seam
	// to later re-cut once attribution catches up. Once IsQuote/
	// ScareQuote rule that out, the remaining Speaker check still matters
	// on its own (a Speaker can diverge from IsQuote/ScareQuote in the
	// other direction too - a stray non-Narrator attribution surviving on
	// a paragraph that's since been re-tagged): every merged member
	// confirmed Narrator/"" is what lets pushMergedTask's caller skip
	// resolving each member's own voice separately - internal/narration.
	// Resolver only ever overrides a paragraph's voice away from the
	// book's own for a named-character Speaker, so once every member is
	// confirmed Narrator/"", rp's own already-resolved voice is
	// guaranteed correct for the whole group without a second lookup.
	narrationVoiced := func(p store.Paragraph) bool {
		if p.IsQuote && !p.ScareQuote {
			return false
		}
		if p.Speaker != "" && p.Speaker != "Narrator" {
			return false
		}
		return true
	}
	if !narrationVoiced(group[anchor]) {
		return alone
	}

	lo, hi := anchor, anchor
	for lo > 0 && narrationVoiced(group[lo-1]) && !segmentEndsSentence(group[lo-1]) {
		lo--
	}
	for hi < len(group)-1 && narrationVoiced(group[hi+1]) && !segmentEndsSentence(group[hi]) {
		hi++
	}
	if lo == hi {
		return alone
	}

	run := group[lo : hi+1]
	hasScareQuote := false
	for _, p := range run {
		if p.ScareQuote {
			hasScareQuote = true
			break
		}
	}
	if !hasScareQuote {
		return alone
	}
	return run
}

func (m *Manager) enqueueChapter(ctx context.Context, bookID, chapterID string, chapterIdx, wordLimit int) {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil {
		log.Printf("jobs: book %s not found: %v", bookID, err)
		return
	}
	m.maybeScoreChapterMusic(bookID, chapterID, chapterIdx, book)
	resolved, err := m.paragraphsNeedingGeneration(ctx, book, chapterID)
	if err != nil {
		log.Printf("jobs: list paragraphs for chapter %s: %v", chapterID, err)
		return
	}
	if wordLimit > 0 {
		resolved = limitResolvedByWordCount(resolved, wordLimit)
	}
	for _, rp := range resolved {
		m.pushResolvedTask(bookID, chapterID, chapterIdx, TierBackground, rp)
	}
}

// maybeScoreChapterMusic fire-and-forget dispatches background-music
// scoring for chapterID (EnqueueMusicScoring, via m.scoreMusic - a no-op
// if never configured, see MusicScorer's own doc comment) the moment its
// own narration audio generation is requested, rather than requiring a
// reader to separately visit the Speakers page and score it (or run a
// bulk "Score all music") before its music actually ever generates. A
// no-op for a book with MusicEnabled off (the overwhelming common case -
// same cheap-to-over-call reasoning MaybeAdvanceChapterMusic's own doc
// comment gives) or a chapter already scored (!ch.Passes.Music) -
// scoring only ever needs to run once per chapter; MaybeAdvanceChapterMusic
// (called from inside the real scorer once regions persist, and again as
// each narration paragraph this same enqueueChapter call is about to
// dispatch finishes) is what actually advances already-scored regions,
// completely independent of this. EnqueueMusicScoring's own dedup-by-
// chapter means calling this on every enqueueChapter (including "Generate
// remaining"'s whole-book walk, and a reader simply re-opening a chapter
// they're already generating) is harmless even for a chapter whose
// scoring is already queued or in flight.
func (m *Manager) maybeScoreChapterMusic(bookID, chapterID string, chapterIdx int, book *store.Book) {
	if !book.MusicEnabled || m.scoreMusic == nil {
		return
	}
	ch, err := m.store.GetChapterByID(chapterID)
	if err != nil || ch == nil || ch.Passes.Music {
		return
	}
	chVal := *ch
	m.EnqueueMusicScoring(bookID, chapterID, chapterIdx, func(ctx context.Context) (int, func(), error) {
		return m.scoreMusic(ctx, book, &chVal)
	})
}

// EnqueueParagraphRegenerate re-renders one already-generated paragraph
// from scratch, for an explicit "regenerate this paragraph" request (the
// reader didn't like how a specific line came out) rather than the normal
// pending -> generating -> ready flow. Runs at TierUrgent - a single
// explicit, deliberate request, not just "this is coming up soon" - so it
// always goes next regardless of where it sits relative to the reader's
// actual current position.
func (m *Manager) EnqueueParagraphRegenerate(bookID, chapterID string, chapterIdx int, paragraph store.Paragraph) {
	go m.enqueueParagraphRegenerate(bookID, chapterID, chapterIdx, paragraph)
}

func (m *Manager) enqueueParagraphRegenerate(bookID, chapterID string, chapterIdx int, paragraph store.Paragraph) {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil {
		log.Printf("jobs: book %s not found: %v", bookID, err)
		return
	}
	if bookVoice, err := m.narration.BookVoice(book); err != nil {
		log.Printf("jobs: resolve book voice for %s: %v", bookID, err)
	} else {
		m.provisionMissingCharacterVoices(m.ctx, book, []string{paragraph.Speaker}, narration.EffectiveCloneModel(bookVoice))
	}
	v, err := m.narration.ForParagraph(book, paragraph.Speaker)
	if err != nil {
		log.Printf("jobs: resolve voice for paragraph %s: %v", paragraph.ID, err)
		return
	}
	voiceID := v.VoiceID()

	if err := m.store.ResetParagraphAudio(paragraph.ID, voiceID); err != nil {
		log.Printf("jobs: reset paragraph %s for regenerate: %v", paragraph.ID, err)
		return
	}
	m.pushResolvedTask(bookID, chapterID, chapterIdx, TierUrgent, resolvedParagraph{paragraph: paragraph, voice: v})
}

// EnqueueLookahead generates up to count upcoming paragraphs (count <= 0
// means LookaheadParagraphCount; anything above MaxLookaheadParagraphCount
// is clamped to it) starting at (fromChapterIdx, fromParagraphIdx), continuing
// into later chapters as needed instead of stopping at the current
// chapter's end - so there's always some runway ahead regardless of where
// a chapter boundary falls relative to the reader's position. Paragraphs
// before fromParagraphIdx in the starting chapter are left alone (no point
// generating audio for ones already played past).
//
// The paragraph at exactly (fromChapterIdx, fromParagraphIdx) - if it needs
// generation at all - is pushed at TierUrgent rather than TierLookahead:
// this is also the request the reader/mobile client fires the moment they
// select a not-yet-generated paragraph (see httpapi.handleLookahead), so
// "jump to this paragraph" should generate it before any other lookahead
// work. Every other paragraph in the window stays at TierLookahead, ordered
// among themselves like any other same-tier poolGeneration work - by plain
// book/chapter/paragraph position (see lessByPosition), not by distance
// from wherever the reader happens to be right now.
func (m *Manager) EnqueueLookahead(bookID string, fromChapterIdx, fromParagraphIdx, count int) {
	if count <= 0 {
		count = LookaheadParagraphCount
	}
	count = min(count, MaxLookaheadParagraphCount)
	go m.enqueueLookahead(bookID, fromChapterIdx, fromParagraphIdx, count)
}

func (m *Manager) enqueueLookahead(bookID string, fromChapterIdx, fromParagraphIdx, count int) {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil {
		log.Printf("jobs: book %s not found: %v", bookID, err)
		return
	}

	chapterIdx := fromChapterIdx
	remaining := count
	for remaining > 0 {
		select {
		case <-m.ctx.Done():
			return
		default:
		}

		ch, err := m.store.GetChapterByIdx(bookID, chapterIdx)
		if err != nil {
			log.Printf("jobs: lookahead: get chapter %d of book %s: %v", chapterIdx, bookID, err)
			return
		}
		if ch == nil {
			return // ran off the end of the book
		}

		resolved, err := m.paragraphsNeedingGeneration(m.ctx, book, ch.ID)
		if err != nil {
			log.Printf("jobs: list paragraphs for chapter %s: %v", ch.ID, err)
			return
		}
		if chapterIdx == fromChapterIdx && fromParagraphIdx > 0 {
			resolved = skipResolvedBeforeIdx(resolved, fromParagraphIdx)
		}
		if len(resolved) > remaining {
			resolved = resolved[:remaining]
		}
		for _, rp := range resolved {
			tier := TierLookahead
			if chapterIdx == fromChapterIdx && rp.paragraph.Idx == fromParagraphIdx {
				tier = TierUrgent
			}
			m.pushResolvedTask(bookID, ch.ID, chapterIdx, tier, rp)
		}
		if chapterIdx == fromChapterIdx {
			// Background music should be at least as responsive to reader
			// position as narration itself - a still-queued scoring/live
			// generation task for this chapter shouldn't be stuck waiting
			// out an unrelated TierBackground backlog (e.g. a whole-book
			// direction-tagging run) the way it otherwise would. Only the
			// reader's own current chapter, not the whole lookahead window
			// - see PromoteChapterMusicNear's own doc comment for why
			// that's enough.
			m.PromoteChapterMusicNear(bookID, ch.ID)
			// And start the music right where they are, without waiting
			// for the rest of the chapter to be voiced - see
			// KindMusicLiveGeneration. Live path only: the whole-chapter
			// batch stays TierBackground and is only ever kicked off by
			// MaybeAdvanceChapterMusic, never by reader position.
			m.advanceLiveChapterMusic(bookID, ch.ID, fromParagraphIdx)
		}
		remaining -= len(resolved)
		chapterIdx++
	}
}

// PromoteChapterMusicNear bumps to TierUrgent whichever still-queued
// background-music task (if any) is actually standing between the reader
// and having music in chapterID: a KindMusicScoring task for the chapter
// itself, if it hasn't been scored yet, and/or a KindMusicLiveGeneration
// task for the chapter - both keyed purely by chapterID. The whole-chapter
// KindMusicGeneration batch is deliberately left alone: reader position
// only drives the live path (the batch covers regions the reader may be
// nowhere near, and would otherwise hog poolSFX at urgent priority). A
// no-op, not an error, if neither exists (already dispatched/finished, not
// created yet, or the book doesn't have music on at all) - same shape as
// PromoteTier's own WithTask calls, just driven by reader position instead
// of an explicit dashboard action.
//
// Called from enqueueLookahead for the reader's own exact current
// position (not the whole lookahead window - a region generally spans far
// more than LookaheadParagraphCount paragraphs, and every position update
// re-triggers this anyway, so this chapter alone is enough to keep pace
// as they keep reading) - background music otherwise defaults to
// TierBackground end to end (EnqueueMusicScoring's own
// defaultAttributionTier, EnqueueMusicGeneration's own TierBackground -
// see MaybeAdvanceChapterMusic), since nothing about scoring/generating it
// is reader-blocking on its own the way an unready narration paragraph is.
func (m *Manager) PromoteChapterMusicNear(bookID, chapterID string) {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil || !book.MusicEnabled {
		return
	}
	promote := func(id string) {
		m.queue.WithTask(
			func(c taskqueue.Task) bool { return c.Key() == id },
			func(c taskqueue.Task) {
				if TierUrgent < c.Tier() {
					c.Promote(TierUrgent)
				}
			},
		)
	}
	promote("music_score:" + chapterID)
	m.queue.WithEachTask(
		func(c taskqueue.Task) bool { return isLiveMusicTask(c, chapterID) },
		func(c taskqueue.Task) {
			if TierUrgent < c.Tier() {
				c.Promote(TierUrgent)
			}
		},
	)
	// Unconditional, same "cheap to over-call" reasoning
	// MaybeAdvanceChapterMusic's own doc comment gives - simpler than
	// threading back whether either promote() call actually found and
	// promoted something, and the Jobs dashboard's own poll absorbs a
	// harmless extra notify fine.
	m.notifyChanged()
}

// EnqueueRemaining and EnqueueBookGenerate now live in pipeline.go, as
// cancelable bulk groups (EnqueueRemaining/EnqueueBookGenerate) - see their
// own doc comments there.

// pushTask registers a task as queued (skipping it if already
// queued/in-flight under the same dedupKey, to avoid dispatching the same
// paragraph - or the same chapter's attribution, or the same character's
// characterization - twice when overlapping calls both see it as pending)
// and adds it to the priority queue. A duplicate push still merges its
// llmWaiters (if any - only RunCharacterization populates them, see
// llmWaiters' own doc comment) onto whichever existing task owns key, in
// flight or still queued, so a second RunCharacterization call for an
// already-running character joins that run's result instead of being
// dropped with no way to ever complete, and bumps that existing task's own
// tier up in place if t's is more urgent - a whole chapter's worth of
// TierBackground work is a common case an explicit "play this paragraph
// now" click can land in the middle of, and without this, the dedup below
// would just silently drop that click's TierLookahead push, since the
// paragraph already has *a* queue entry, just the wrong-priority one. A
// duplicate EnqueueAttribution push for an already-queued/in-flight
// chapter has no waiter to merge and is already at defaultAttributionTier
// either way - it's just silently deduped, which is fine since nothing is
// blocked waiting on it anyway.
//
// For KindVoiceClone/KindVoiceDesign specifically (dedupKey's default
// case, keyed by paragraph ID alone - see its own doc comment), a merge
// also refreshes the existing queued task's voice-defining fields
// (voiceID/instruct/presetID/etc., even kind itself) from t's freshly
// re-resolved ones, via the narrower WithQueuedTask rather than WithTask.
// Without this, a paragraph queued once (e.g. by lookahead) and then
// re-pushed after something changes which voice it actually resolves to
// (speaker attribution reassigning it to a character, a character's own
// voice reassignment, book/MultiVoice changes) would still generate under
// whatever voice it first queued under: the merge branch used to touch
// only waiters/tier, silently keeping the stale voice fields, so the
// dispatched render came out under the wrong voice and paragraphsNeeding
// Generation - checking the *current* resolution - reported it not ready
// and queued yet another, correct render right behind it. From a reader's
// perspective that showed up as a paragraph that had just finished
// generating flipping back to pending and re-rendering. Deliberately
// restricted to a still-queued match (never in-flight): an in-flight
// task's own generate() goroutine reads these same fields with no lock of
// its own, on the assumption nothing mutates them after dispatch, so
// there's nothing to correct once a render is already underway - the
// eventual mismatch there still self-heals the same way it always did,
// via the next check finding it not-ready under the current voice.
func (m *Manager) pushTask(t *task) {
	if !m.queue.Push(t) {
		// Via WithTask, not a plain Find + direct field mutation: this
		// task's own tier/chapterIdx/waiters are read concurrently by
		// taskqueue itself (Tier/Pool/Less, called from inside Pop/
		// Snapshot under its own lock) - mutating them after Find already
		// returned and released that lock would race against those reads.
		m.queue.WithTask(func(c taskqueue.Task) bool { return c.Key() == t.Key() }, func(c taskqueue.Task) {
			et := c.(*task)
			et.llmWaiters = append(et.llmWaiters, t.llmWaiters...)
			et.provisionWaiters = append(et.provisionWaiters, t.provisionWaiters...)
			et.previewWaiters = append(et.previewWaiters, t.previewWaiters...)
			et.previewTextWaiters = append(et.previewTextWaiters, t.previewTextWaiters...)
			if t.tier < et.tier {
				et.tier = t.tier
				et.chapterIdx = t.chapterIdx
			}
		})
		if t.kind == KindVoiceClone || t.kind == KindVoiceDesign {
			m.queue.WithQueuedTask(func(c taskqueue.Task) bool { return c.Key() == t.Key() }, func(c taskqueue.Task) {
				et := c.(*task)
				et.kind = t.kind
				et.voiceID = t.voiceID
				et.instruct = t.instruct
				et.presetID = t.presetID
				et.language = t.language
				et.refText = t.refText
				et.speedMultiplier = t.speedMultiplier
				et.seed = t.seed
				et.cloneModel = t.cloneModel
				et.designModel = t.designModel
				et.cloneInstruct = t.cloneInstruct
			})
		}
		m.notifyChanged()
		return
	}
	m.notePushed(t)
	select {
	case m.wake <- struct{}{}:
	default:
	}
	m.notifyChanged()
}

// notePushed increments m.chapterPending for a freshly (not a dedup
// merge), successfully queued t - shared by pushTask itself and this
// package's own dependency resolvers below, whose own lazily-created
// dependency tasks go straight through taskqueue.LockedQueue.Push (from
// inside a taskqueue.Resolver call) rather than pushTask, but still need
// the exact same bookkeeping so IsGenerating's own count includes a
// chapter's auto-created reference-clip/characterization/direction-
// tagging dependency the same as anything explicitly enqueued.
// finishTask decrements it uniformly for any task's completion regardless
// of which of these created it.
func (m *Manager) notePushed(t *task) {
	m.mu.Lock()
	m.chapterPending[t.chapterID]++
	m.mu.Unlock()
}

// resolveDependencies is this package's taskqueue.Resolver - see that
// type's own doc comment for the general contract, and Kind's own doc
// comment ("adding a new kind of job means extending those switches") for
// what this replaces: three near-identical, hand-copied functions
// (characterizationBlockerFor/speechDirectionBlockerFor/
// referenceClipBlockerFor, before this package existed), each separately
// implementing the same "look for a queued/in-flight match, else check
// whether the real, persisted state this dependency represents is already
// done, else lazily create one" shape - and each one a place that check
// could be (and, in this app's own history, was) forgotten, recreating a
// "finished" dependency's task forever. Now there's one entry point, and
// each dependency kind below is just the business-specific part of that
// shape (what counts as "done", what the fresh task looks like) with the
// generic queueing/dedup mechanics handled once, by taskqueue itself.
func (m *Manager) resolveDependencies(lq *taskqueue.LockedQueue, tk taskqueue.Task) []taskqueue.Task {
	t := tk.(*task)
	if t.skipDependencies {
		return nil
	}
	var deps []taskqueue.Task
	if d := m.characterizationDependency(lq, t); d != nil {
		deps = append(deps, d)
	}
	if t.kind == KindVoiceClone || t.kind == KindVoiceDesign {
		if d := m.speechDirectionDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
		if d := m.pronunciationDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
	}
	if t.kind == KindVoiceClone {
		if d := m.referenceClipDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
		if d := m.variantClipDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
	}
	if t.kind == KindSpeakerAttribution || t.kind == KindDescription {
		if d := m.scareQuoteDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
	}
	if t.kind == KindMusicLiveGeneration {
		if d := m.liveMusicDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
	}
	if t.kind == KindSpeakerAttribution {
		if d := m.attributionOrderDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
		if d := m.globalAttributionSlotDependency(lq, t); d != nil {
			deps = append(deps, d)
		}
	}
	return deps
}

// pushDependency pushes nt (a freshly-built, not-yet-queued dependency
// task) via lq, falling back to whatever's already there under the same
// key on the rare chance something else in this same resolveDependencies
// pass already created it first (see taskqueue.Resolver's own doc comment
// on why lq.Find is checked before this in every caller below - this is
// just the defensive "and if that raced anyway" fallback, not the
// primary dedup path) - shared by every dependency kind below instead of
// each hand-rolling the same fallback.
func (m *Manager) pushDependency(lq *taskqueue.LockedQueue, nt *task) taskqueue.Task {
	if lq.Push(nt) {
		m.notePushed(nt)
		return nt
	}
	if existing, ok := lq.Get(nt.Key()); ok {
		return existing
	}
	return nil
}

// The memo* helpers below wrap every store/disk lookup the dependency
// resolvers make in taskqueue.LockedQueue.Memo, so each distinct book/
// chapter/character/preset is looked up once per resolve pass instead of
// once per queued task - with hundreds of a book's paragraphs queued,
// that's the difference between a handful of queries and thousands on
// every single Pop (and every Snapshot/HasHigherPriorityWork), all on the
// store's one shared DuckDB connection. Errors are memoized alongside the
// value; every caller already treats a lookup error as "no dependency".

type bookLookup struct {
	book *store.Book
	err  error
}

func (m *Manager) memoBook(lq *taskqueue.LockedQueue, bookID string) (*store.Book, error) {
	r := lq.Memo("book:"+bookID, func() any {
		b, err := m.store.GetBook(bookID)
		return bookLookup{b, err}
	}).(bookLookup)
	return r.book, r.err
}

type chapterLookup struct {
	ch  *store.Chapter
	err error
}

func (m *Manager) memoChapterByID(lq *taskqueue.LockedQueue, chapterID string) (*store.Chapter, error) {
	r := lq.Memo("chapter:"+chapterID, func() any {
		ch, err := m.store.GetChapterByID(chapterID)
		return chapterLookup{ch, err}
	}).(chapterLookup)
	return r.ch, r.err
}

type chapterIdxKey struct {
	bookID string
	idx    int
}

func (m *Manager) memoChapterByIdx(lq *taskqueue.LockedQueue, bookID string, idx int) (*store.Chapter, error) {
	r := lq.Memo(chapterIdxKey{bookID, idx}, func() any {
		ch, err := m.store.GetChapterByIdx(bookID, idx)
		return chapterLookup{ch, err}
	}).(chapterLookup)
	return r.ch, r.err
}

type characterKey struct{ scope, name string }

type characterLookup struct {
	char *store.Character
	err  error
}

func (m *Manager) memoCharacterByName(lq *taskqueue.LockedQueue, scope, name string) (*store.Character, error) {
	r := lq.Memo(characterKey{scope, name}, func() any {
		c, err := m.store.GetCharacterByName(scope, name)
		return characterLookup{c, err}
	}).(characterLookup)
	return r.char, r.err
}

// characterizationTaskByLabel indexes every queued/in-flight
// KindSpeakerCharacterization task by label, built once per resolve pass -
// characterizationDependency matches on speaker name rather than an exact
// Key, so without this each clone task paid a full linear scan of the
// queue. A characterization task pushed later in the same pass isn't in
// the index, but a second lookup for that speaker still finds it: it
// falls through to pushDependency, whose Push dedups on Key and returns
// the existing task.
type characterizationIndex struct{}

func characterizationTaskByLabel(lq *taskqueue.LockedQueue, name string) (taskqueue.Task, bool) {
	idx := lq.Memo(characterizationIndex{}, func() any {
		byLabel := map[string]taskqueue.Task{}
		lq.Find(func(c taskqueue.Task) bool {
			if ct := c.(*task); ct.kind == KindSpeakerCharacterization {
				if _, ok := byLabel[ct.label]; !ok {
					byLabel[ct.label] = c
				}
			}
			return false
		})
		return byLabel
	}).(map[string]taskqueue.Task)
	t, ok := idx[name]
	return t, ok
}

// characterizationDependency returns t's own speaker's currently queued
// or in-flight KindSpeakerCharacterization task, if any, or lazily creates
// one via m.characterize (a no-op, returning no dependency at all, if that
// was never configured - see CharacterCharacterizer's own doc comment) the
// first time it discovers the speaker isn't characterized yet
// (char.Summary == "" - a stored fact, not a task-completion signal) -
// this package's one implementation of the "check before creating" rule
// resolveDependencies' own doc comment describes. name is
// t.paragraph.Speaker for a KindVoiceClone/KindVoiceDesign task or
// t.label for a KindVoiceProvision task (see EnqueueVoiceProvision); any
// other kind never has a name to look up.
//
// A clone/design/provision task blocked on this never races ahead of a
// characterization that's about to change what that voice even sounds
// like: recharacterizing rewrites the assigned preset's own Instruct (see
// httpapi.recharacterizeAndInvalidate), which also deletes any paragraph
// audio already generated under the now-stale characterization - a clone
// dispatched moments before that finishes would just get deleted again
// immediately, wasting the render.
func (m *Manager) characterizationDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	var name string
	switch t.kind {
	case KindVoiceClone, KindVoiceDesign:
		name = t.paragraph.Speaker
	case KindVoiceProvision:
		name = t.label
	}
	if name == "" || name == "Narrator" {
		return nil
	}
	if found, ok := characterizationTaskByLabel(lq, name); ok {
		return found
	}
	if m.characterize == nil {
		return nil
	}
	book, err := m.memoBook(lq, t.bookID)
	if err != nil || book == nil {
		return nil
	}
	char, err := m.memoCharacterByName(lq, store.SeriesScope(book), name)
	if err != nil || char == nil {
		return nil
	}
	if char.Summary != "" {
		return nil // already characterized
	}
	charVal := *char
	nt := &task{
		kind:        KindSpeakerCharacterization,
		tier:        t.tier,
		bookID:      t.bookID,
		llmKey:      charVal.ID,
		label:       charVal.Name,
		runLLM:      func(ctx context.Context) (int, func(), error) { return 0, nil, m.characterize(ctx, t.bookID, charVal) },
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	}
	return m.pushDependency(lq, nt)
}

// speechDirectionDependency is characterizationDependency's own sibling,
// one level up: t's own chapter's currently queued or in-flight
// KindSpeechDirection (emotion labeling) task, if any, or a lazily-created
// one via m.direct (a no-op, returning no dependency at all, if that was
// never configured - see ChapterDirector's own doc comment) the first time
// it discovers the chapter isn't labeled yet (!Passes.Direction). Any
// clone model: an emotion picks which reference clip a line clones from
// (see variantClipDependency), so a line generated before its chapter is
// labeled would just be invalidated and re-rendered once it is - the
// exact same wasted-render shape characterizationDependency prevents for
// a recharacterization changing what a voice sounds like mid-flight.
func (m *Manager) speechDirectionDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	chapterID := t.chapterID
	// Every KindSpeechDirection task is keyed by its own chapterID (see
	// dedupKey), so an exact-key lookup finds the same task a scan by
	// kind+chapterID would.
	if found, ok := lq.Get("direction:" + chapterID); ok {
		return found
	}
	if m.direct == nil {
		return nil
	}
	book, err := m.memoBook(lq, t.bookID)
	if err != nil || book == nil {
		return nil
	}
	// book.SpeechDirection is this dependency's own explicit opt-in - off
	// by default (see its own doc comment), so a paragraph clones
	// immediately rather than waiting on an LLM tagging pass most books
	// never need. A reader who does want tagged audio turns this on
	// (httpapi.handleUpdateVoice, which also invalidates any audio already
	// generated without that guarantee).
	if !book.SpeechDirection {
		return nil
	}
	ch, err := m.memoChapterByID(lq, chapterID)
	if err != nil || ch == nil {
		return nil
	}
	if ch.Passes.Direction {
		return nil // already tagged
	}
	chVal := *ch
	nt := &task{
		kind:        KindSpeechDirection,
		tier:        t.tier,
		bookID:      t.bookID,
		chapterID:   chapterID,
		chapterIdx:  t.chapterIdx,
		llmKey:      chapterID,
		runLLM:      func(ctx context.Context) (int, func(), error) { return m.direct(ctx, book, &chVal) },
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	}
	return m.pushDependency(lq, nt)
}

// pronunciationDependency is speechDirectionDependency's sibling for
// pronunciation resolution: t's own chapter's queued or in-flight
// KindPronunciation task, if any, or a lazily-created one via m.pronounce
// the first time it finds the chapter's Passes.Pronunciation unset.
// Gated on the same book.SpeechDirection opt-in ("wait for tagging before
// generating"), but not on the clone model - a pronunciation fix applies
// to every one. Resolving a pronunciation invalidates the chapter's
// already-generated audio (see httpapi.pronounceChapter), so a clone
// dispatched before it finishes would just be deleted again.
func (m *Manager) pronunciationDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	chapterID := t.chapterID
	if found, ok := lq.Get("pronunciation:" + chapterID); ok {
		return found
	}
	if m.pronounce == nil {
		return nil
	}
	book, err := m.memoBook(lq, t.bookID)
	if err != nil || book == nil || !book.SpeechDirection {
		return nil
	}
	ch, err := m.memoChapterByID(lq, chapterID)
	if err != nil || ch == nil {
		return nil
	}
	if ch.Passes.Pronunciation {
		return nil // already resolved
	}
	chVal := *ch
	nt := &task{
		kind:        KindPronunciation,
		tier:        t.tier,
		bookID:      t.bookID,
		chapterID:   chapterID,
		chapterIdx:  t.chapterIdx,
		llmKey:      chapterID,
		runLLM:      func(ctx context.Context) (int, func(), error) { return m.pronounce(ctx, book, &chVal) },
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	}
	return m.pushDependency(lq, nt)
}

// scareQuoteDependency makes t's own chapter's scare-quote tagging
// (KindScareQuote) a hard dependency of a KindSpeakerAttribution/
// KindDescription task: returns that chapter's currently queued or
// in-flight KindScareQuote task, if any, or lazily creates one via
// m.scareQuote (a no-op if never configured) the first time it finds the
// chapter's Passes.ScareQuote unset - speechDirectionDependency's exact
// shape. Attribution needs scare quotes flagged first so it never assigns
// a character to a quoted span that isn't dialogue (httpapi.
// attributeChapter forces those to Narrator), and description tagging
// reads narration, which a scare quote is part of. A scare-quote run that
// fails leaves Passes.ScareQuote unset, so the next resolution recreates
// it and the dependent task keeps waiting - deliberate: the pass is
// sampled, so a retry is expected to succeed eventually, and running
// attribution against unflagged scare quotes would bake in wrong speakers.
func (m *Manager) scareQuoteDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	chapterID := t.chapterID
	if found, ok := lq.Get("scare_quote:" + chapterID); ok {
		return found
	}
	if m.scareQuote == nil {
		return nil
	}
	ch, err := m.memoChapterByID(lq, chapterID)
	if err != nil || ch == nil {
		return nil
	}
	if ch.Passes.ScareQuote {
		return nil // already tagged
	}
	book, err := m.memoBook(lq, t.bookID)
	if err != nil || book == nil {
		return nil
	}
	chVal := *ch
	nt := &task{
		kind:        KindScareQuote,
		tier:        t.tier,
		bookID:      t.bookID,
		chapterID:   chapterID,
		chapterIdx:  t.chapterIdx,
		llmKey:      chapterID,
		runLLM:      func(ctx context.Context) (int, func(), error) { return m.scareQuote(ctx, book, &chVal) },
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	}
	return m.pushDependency(lq, nt)
}

// referenceClipDependency returns t's own "ensure reference clip" task -
// this package's third dependency kind, creating one the first time some
// paragraph discovers its own resolved preset has no cached reference
// clip rendered yet (checked directly against disk - os.Stat - the
// "already done" precheck for this dependency kind, no store lookup
// needed), so it's never dispatched into a poolGeneration slot only to
// sit there blocking on a nested, cross-pool call to finish: this used to
// be a real, confirmed-live deadlock for a TierUrgent/TierLookahead outer
// task (nested inside generate() itself, occupying its own poolGeneration
// slot for as long as that blocking call took, with no way for its own
// nested poolDesign child to ever dispatch while poolGeneration stayed
// active - see taskqueue.Queue's own "one pool at a time" doc comment).
// Routing the render through a real, independently-dispatched poolDesign
// task instead - one t merely *depends on*, never blocks inside - means t
// itself is simply left queued (not occupying any slot at all) until that
// dependency clears, the same "never actually put in flight until it can
// really run" treatment characterization/direction dependencies already
// get above. Only ever called for t.kind == KindVoiceClone (see
// resolveDependencies) - KindVoiceDesign renders straight from an
// instruct, no reference clip involved at all.
func (m *Manager) referenceClipDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	if t.presetID == "" {
		// A real KindVoiceClone task always has one - pushResolvedTask only
		// ever picks this Kind when rp.voice.PresetID is non-empty (falling
		// back to KindVoiceDesign otherwise) - so this only ever fires for
		// a task built by hand without one (test setups that construct a
		// bare task{kind: KindVoiceClone} literal directly, bypassing the
		// real resolution path entirely), where there's no real preset to
		// check or render anything for.
		return nil
	}
	rendered := lq.Memo("refclip:"+t.presetID, func() any {
		_, err := os.Stat(audiopath.VoicePresetRefFile(m.dataDir, t.presetID))
		return err == nil
	}).(bool)
	if rendered {
		return nil // already rendered - the overwhelmingly common case
	}
	if found, ok := lq.Get("voice_provision:generate:" + t.presetID); ok {
		return found
	}
	// Nobody's rendering it yet - create the task now, carrying everything
	// it needs straight from t's own already-resolved fields (see
	// pushResolvedTask), no httpapi/character context required.
	label := t.paragraph.Speaker
	if label == "" {
		label = "Narrator"
	}
	nt := &task{
		kind:   KindVoiceProvision,
		tier:   t.tier,
		bookID: t.bookID,
		llmKey: "generate:" + t.presetID,
		label:  label,
		runProvision: func(ctx context.Context, attempt int) (string, error) {
			// VoiceDesign is fully deterministic on (instruct, seed,
			// refText, language) - see EnqueueVoiceProvision's own doc
			// comment on why a retry (attempt > 0) needs a perturbed seed
			// rather than reproducing a numerically-unstable crash
			// identically every time. Persisted back onto the preset row
			// on success so a later regenerate/edit-save-unchanged picks
			// up the seed that's actually on disk instead of reverting to
			// the original, crash-triggering one.
			seed := t.seed + attempt
			path, err := voicerefs.EnsureFile(ctx, m.tts, m.dataDir, t.presetID, t.instruct, seed, t.refText, t.speedMultiplier, t.designModel)
			if err == nil && attempt > 0 {
				if uerr := m.store.UpdateVoicePresetSeed(t.presetID, seed); uerr != nil {
					log.Printf("jobs: persist bumped seed for preset %s: %v", t.presetID, uerr)
				}
			}
			return path, err
		},
	}
	return m.pushDependency(lq, nt)
}

// variantClipDependency is referenceClipDependency's own counterpart for
// an emotional line: when t's paragraph has an effective emotion
// (store.Paragraph.EffectiveEmotion - real dialogue only) and its preset's
// variant clip for that emotion hasn't been rendered yet, it returns the
// KindVoiceProvision task rendering it (voicerefs.EnsureVariantFile - a
// BreezeTTS instructed clone of the base clip), creating one keyed by
// (preset, emotion) if nobody's rendering it yet. Variants are provisioned
// lazily this way, one per emotion a voice's lines actually use, rather
// than every emotion for every voice up front.
//
// A render that fails on its final attempt marks the variant failed
// (voicerefs.MarkVariantFailed), which clears this dependency: lines in
// that emotion then clone from the base clip (see generate) instead of
// blocking forever. Skipped for a scare-quote merge group (one call
// renders narration and quote together, so no single emotion applies).
func (m *Manager) variantClipDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	if t.presetID == "" || len(t.mergeParagraphs) > 1 {
		return nil
	}
	emotion := t.paragraph.EffectiveEmotion()
	if emotion == emotions.Neutral {
		return nil
	}
	settled := lq.Memo("variantclip:"+t.presetID+":"+emotion, func() any {
		return voicerefs.VariantExists(m.dataDir, t.presetID, emotion) || voicerefs.VariantFailed(m.dataDir, t.presetID, emotion)
	}).(bool)
	if settled {
		return nil
	}
	if found, ok := lq.Get("voice_provision:" + variantKey(t.presetID, emotion)); ok {
		return found
	}
	label := t.paragraph.Speaker
	if label == "" {
		label = "Narrator"
	}
	base := voicerefs.BaseClip{
		PresetID:        t.presetID,
		Instruct:        t.instruct,
		Seed:            t.seed,
		RefText:         t.refText,
		SpeedMultiplier: t.speedMultiplier,
		DesignModel:     t.designModel,
	}
	return m.pushDependency(lq, m.newVariantTask(t.tier, t.bookID, label, base, emotion))
}

// variantKey is an emotion variant render's llmKey - one per (preset,
// emotion), so the lazy dependency path and an explicit regenerate
// (EnqueueEmotionVariant) join rather than rendering twice.
func variantKey(presetID, emotion string) string {
	return "variant:" + presetID + ":" + emotion
}

// newVariantTask builds the KindVoiceProvision task rendering base's
// emotion variant (voicerefs.EnsureVariantFile). A render that fails on
// its final attempt marks the variant failed so its lines fall back to the
// base clip - see variantClipDependency.
func (m *Manager) newVariantTask(tier int, bookID, speaker string, base voicerefs.BaseClip, emotion string) *task {
	return &task{
		kind:   KindVoiceProvision,
		tier:   tier,
		bookID: bookID,
		llmKey: variantKey(base.PresetID, emotion),
		label:  speaker + " (" + emotion + ")",
		runProvision: func(ctx context.Context, attempt int) (string, error) {
			path, err := voicerefs.EnsureVariantFile(ctx, m.tts, m.dataDir, base, emotion)
			if err != nil && !errors.Is(err, context.Canceled) && attempt+1 >= maxTaskAttempts {
				log.Printf("jobs: %s variant for preset %s failed for good, falling back to the base clip: %v", emotion, base.PresetID, err)
				if merr := voicerefs.MarkVariantFailed(m.dataDir, base.PresetID, emotion); merr != nil {
					log.Printf("jobs: mark variant failed: %v", merr)
				}
			}
			return path, err
		},
	}
}

// EnqueueEmotionVariant renders base's emotion variant now rather than
// waiting for the next line in that emotion to need it - the Speakers
// page's explicit "regenerate variant" action (after
// voicerefs.DeleteVariant), so the reader can hear the new take right
// away. Fire-and-forget at TierUrgent; joins a render already queued for
// the same (preset, emotion).
func (m *Manager) EnqueueEmotionVariant(bookID, speaker string, base voicerefs.BaseClip, emotion string) {
	m.pushTask(m.newVariantTask(TierUrgent, bookID, speaker, base, emotion))
}

// attributionOrderDependency makes chapter t.chapterIdx-1 of the same
// book's own speaker attribution (KindSpeakerAttribution) a hard dependency
// of t's own. httpapi.attributeChapter reads its "already known characters"
// list live from the store at call-start (Store.ListCharacters), so if two
// chapters of the same book ran attribution concurrently, or out of the
// order a reader would actually read them in (exactly what an unpatched
// preprocessAttributionPhase/"Attribute all" does today - each not-yet-
// attributed chapter fires its own call at once, relying only on poolLLM's
// slot admission to gate how many actually run together, never on chapter
// order), a character first introduced in the earlier one might not exist
// in that list yet by the time the later one's own call reads it -
// silently fragmenting one recurring character into two different names
// within the same book, worse than the cross-book series-scope duplicate-
// name problem canonicalizeSpeakerNames already exists to patch up, since
// there's no equivalent patch for a within-book split.
//
// Skipped for a book's own first chapter (chapterIdx 0 - nothing to wait
// on) and once the previous chapter's own store.Passes.Attribution is
// already true (already done, same finished-state check
// characterizationDependency/referenceClipDependency/speechDirectionDependency
// use). Otherwise returns that chapter's own currently queued/in-flight
// KindSpeakerAttribution task if there is one - the common case, since
// bulk preprocessing/"Attribute all" pushes every not-yet-attributed
// chapter's own task before any of them actually dispatches - or lazily
// creates one via m.attribute (a no-op, returning no dependency at all, if
// that was never configured - see ChapterAttributor's own doc comment),
// covering a lone "Attribute speakers" click on a later chapter whose own
// earlier chapter was never separately queued.
func (m *Manager) attributionOrderDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	prevIdx := t.chapterIdx - 1
	if prevIdx < 0 {
		return nil
	}
	prevCh, err := m.memoChapterByIdx(lq, t.bookID, prevIdx)
	if err != nil || prevCh == nil {
		return nil
	}
	if found, ok := lq.Get("attr:" + prevCh.ID); ok {
		return found
	}
	if prevCh.Passes.Attribution {
		return nil // already done
	}
	if m.attribute == nil {
		return nil
	}
	book, err := m.memoBook(lq, t.bookID)
	if err != nil || book == nil {
		return nil
	}
	chVal := *prevCh
	nt := &task{
		kind:        KindSpeakerAttribution,
		tier:        t.tier,
		bookID:      t.bookID,
		chapterID:   prevCh.ID,
		chapterIdx:  prevIdx,
		llmKey:      prevCh.ID,
		runLLM:      func(ctx context.Context) (int, func(), error) { return m.attribute(ctx, book, &chVal) },
		seriesName:  t.seriesName,
		seriesIndex: t.seriesIndex,
	}
	return m.pushDependency(lq, nt)
}

// globalAttributionSlotDependency makes t (a KindSpeakerAttribution task)
// depend on any OTHER currently in-flight KindSpeakerAttribution task,
// regardless of book - attributionOrderDependency's own sibling, but
// narrower on purpose: that one only orders a book's own chapters against
// each other, so two DIFFERENT books' attribution tasks (no shared
// attributionOrderDependency chain between them at all) could otherwise
// still dispatch concurrently, each claiming one of poolLLM's own
// maxAttributionInFlight slots. This caps attribution specifically to a
// single in-flight task at a time, deliberately independent of poolLLM's
// own overall capacity - maxAttributionInFlight can still be >1 so
// characterization/direction/music-scoring keep genuine concurrency with
// each other (and with the one active attribution task), rather than
// tying that down to attribution's own narrower constraint.
//
// Deliberately FindInFlight, not Find: two attribution tasks merely queued
// alongside each other (neither dispatched yet - e.g. attributionOrderDependency's
// own lazily-created chapter-0 task sitting next to an already-queued
// chapter-1 task for the same book) would otherwise each find the other as
// a "match" under Find and mutually deadlock, since neither has actually
// started yet. Restricting this to in-flight tasks only breaks that
// symmetry - see FindInFlight's own doc comment - letting the queue's own
// ordering (plus attributionOrderDependency's explicit same-book chain)
// pick one to dispatch first; every other still-queued attribution task
// then finds that one in flight and correctly waits.
//
// Never lazily creates a task the way attributionOrderDependency does
// (there's nothing to conjure here - either some other attribution task is
// already in flight, or there isn't), so this is a pure lq.FindInFlight, no
// pushDependency call.
func (m *Manager) globalAttributionSlotDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	found, ok := lq.FindInFlight(func(c taskqueue.Task) bool {
		other, ok := c.(*task)
		return ok && other.kind == KindSpeakerAttribution && other.Key() != t.Key()
	})
	if !ok {
		return nil
	}
	return found
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

// runLLMTask is RunCharacterization's blocking implementation (attribution
// used to share this too - see EnqueueAttribution's own doc comment for
// why it no longer does, and RunAttribution's for the one caller that now
// legitimately can) - see RunCharacterization's own doc comment for the
// caller-facing behavior (join-in-place on a duplicate call,
// ctx-cancellation-safe). tier is the caller's choice, not always
// TierUrgent (RunCharacterization's own callers all want TierUrgent, but
// runLLMTaskDirect's callers - see its own doc comment - share this same
// push-and-wait shape at other tiers). llmKey/label become task.llmKey/
// task.label - see task's own doc comment for what each kind uses them
// for.
func (m *Manager) runLLMTask(ctx context.Context, kind Kind, tier int, bookID, chapterID string, chapterIdx int, llmKey, label string, fn func(ctx context.Context) (int, error)) (int, error) {
	resultCh := make(chan llmOutcome, 1)
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:       kind,
		tier:       tier,
		chapterIdx: chapterIdx,
		bookID:     bookID,
		chapterID:  chapterID,
		llmKey:     llmKey,
		label:      label,
		// Adapts fn (a CharacterizationFunc's own (int, error) shape,
		// see RunCharacterization) to task.runLLM's shared signature -
		// characterization never pauses/requeues mid-call the way
		// attribution can (see AttributionFunc's own doc comment), so
		// requeue is always nil here.
		runLLM: func(ctx context.Context) (int, func(), error) {
			attributed, err := fn(ctx)
			return attributed, nil, err
		},
		llmWaiters:  []chan llmOutcome{resultCh},
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case out := <-resultCh:
		return out.attributed, out.err
	}
}

// runLLMTaskDirect is runLLMTask's counterpart for AttributionFunc/
// DirectionFunc, whose own (int, func(), error) shape (pause/resume via a
// non-nil requeue) already matches task.runLLM's field type exactly - see
// EnqueueAttribution/EnqueueDirection's own comments to that effect -
// unlike CharacterizationFunc, which runLLMTask itself adapts from a
// simpler, never-pauses (int, error) shape. Used by RunAttribution/
// RunDirection, the blocking siblings EnqueueAttribution/EnqueueDirection
// don't otherwise have - see RunAttribution's own doc comment for why a
// blocking form is safe again now, for the one caller that needs it.
//
// If fn pauses partway (non-nil requeue), the partial outcome is still
// delivered here as soon as handleResult processes it - same as any other
// llmWaiter - while the actual continuation runs later, fully detached,
// via EnqueueAttribution/EnqueueDirection's own fire-and-forget requeue
// closure (see attributeChapter/directChapter). This call does not, and
// cannot, wait for that continuation too: it's a fresh, independently
// dispatched task with no channel of its own back to here. A caller that
// cares about the *whole* chapter finishing, not just this one dispatch,
// has to accept that a paused-and-resumed chapter may report done before
// its continuation actually lands - see RunAttribution's own doc comment.
func (m *Manager) runLLMTaskDirect(ctx context.Context, kind Kind, tier int, bookID, chapterID string, chapterIdx int, llmKey, label string, fn func(ctx context.Context) (int, func(), error)) (int, error) {
	resultCh := make(chan llmOutcome, 1)
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:        kind,
		tier:        tier,
		chapterIdx:  chapterIdx,
		bookID:      bookID,
		chapterID:   chapterID,
		llmKey:      llmKey,
		label:       label,
		runLLM:      fn,
		llmWaiters:  []chan llmOutcome{resultCh},
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case out := <-resultCh:
		return out.attributed, out.err
	}
}

// defaultAttributionTier is EnqueueAttribution's own tier - TierBackground,
// not TierUrgent: unlike EnqueueParagraphRegenerate (a single paragraph the
// reader is waiting to actually hear right now), attribution never blocks
// anything the reader is looking at - SpeakersPage's "Attribute all" alone
// can enqueue a whole book's worth of chapters at once, and there's no
// reason that bulk background work should preempt a more urgent poolLLM
// task (RunCharacterization, still TierUrgent) queued behind it.
const defaultAttributionTier = TierBackground

// EnqueueAttribution queues fn as a KindSpeakerAttribution task - sharing
// the same priority heap as voice-clone/design generation, but dispatched
// into poolLLM's own dedicated slot(s) rather than poolGeneration's (see
// Kind's doc comment and maxAttributionInFlight) - and returns immediately
// without waiting for it to run, same fire-and-forget shape as
// EnqueueChapter. If a chapter is already queued or in flight for
// attribution (e.g. two "attribute this chapter" clicks racing, or
// SpeakersPage's "Attribute all" re-enqueuing an already-queued chapter),
// pushTask's own dedup silently joins it rather than starting a second run
// - fine here since nothing is waiting on a result to join anyway. Always
// queued at defaultAttributionTier (see its own doc comment).
//
// Deliberately not the caller's HTTP request context, or blocking on one at
// all - an earlier version (RunAttribution, since reintroduced below for a
// different kind of caller - see its own doc comment) took ctx and blocked
// the calling goroutine until the task finished; the task itself never
// actually depended on that ctx (it ran via the worker loop's own
// long-lived context regardless - see worker/startTask), but the *HTTP
// handler* did block on it for as long as attribution took, which for a
// several-hundred-paragraph chapter can be minutes. SpeakersPage's
// "Attribute all" fires one such request per unattributed chapter in a
// book at once - for a 100-chapter book that meant up to 99 requests
// sitting open at once behind poolLLM's single slot, for as long as the
// whole book took to attribute; browsers also cap concurrent connections
// per origin (typically 6 for HTTP/1.1), so most of those requests
// wouldn't even reach the server until an earlier one's connection freed
// up. A page reload while that backlog was still draining aborted every
// not-yet-sent request outright - since those had never even reached
// pushTask, that attribution never happened at all, not just paused.
// Progress is observable via GET /api/jobs (already reports queued/
// in-flight speaker_attribution tasks per chapter) and by refetching the
// book's speaker table once a chapter's task clears - not from this call's
// own return, which carries no result.
func (m *Manager) EnqueueAttribution(bookID, chapterID string, chapterIdx int, fn AttributionFunc) {
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:        KindSpeakerAttribution,
		tier:        defaultAttributionTier,
		chapterIdx:  chapterIdx,
		bookID:      bookID,
		chapterID:   chapterID,
		llmKey:      chapterID,
		runLLM:      fn, // AttributionFunc's underlying type already matches runLLM's field type exactly
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
}

// RunAttribution is EnqueueAttribution's blocking sibling - unlike that
// function's own doc comment above (explaining why an earlier blocking
// version was removed), blocking here is safe again because the intended
// caller, httpapi.runBookPipeline, is itself a long-lived background
// goroutine the server started, not an HTTP handler with a browser
// connection behind it: nothing times out or gets dropped on a page reload
// if this takes minutes, and there's exactly one such goroutine per
// in-progress pipeline run, not one per chapter the way SpeakersPage's
// "Attribute all" fires. Same join-on-duplicate and requeue-on-pause
// behavior as EnqueueAttribution (see runLLMTaskDirect's own doc comment
// for what blocking here does and doesn't wait for when fn pauses partway
// through). tier, unlike EnqueueAttribution's own hardcoded
// defaultAttributionTier, is caller-supplied - httpapi's pipeline phase
// passes its own pipelineTask's live Tier() at dispatch time, so a
// preprocessing run already promoted (or promoted later - see
// Manager.PromoteTier's own cascade) actually dispatches its real
// per-chapter work at that tier instead of silently staying at
// TierBackground regardless of how urgent the phase itself now claims to
// be.
func (m *Manager) RunAttribution(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, fn AttributionFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindSpeakerAttribution, tier, bookID, chapterID, chapterIdx, chapterID, "", fn)
}

// defaultReattributionTier mirrors defaultAttributionTier's own reasoning:
// "Auto Split" never blocks anything the reader is looking at right now,
// so this bulk background work shouldn't preempt a more urgent poolLLM
// task (RunCharacterization, still TierUrgent) queued behind it.
const defaultReattributionTier = TierBackground

// EnqueueReattribution queues fn as a KindSpeakerReattribution task -
// EnqueueAttribution's sibling for the "Auto Split" action (see
// httpapi.handleReattributeSpeaker, the one caller, which enqueues one of
// these per chapter containing a paragraph currently credited to the
// speaker being eliminated), sharing its fire-and-forget shape, poolLLM
// slot(s), and join-on-duplicate dedup - see EnqueueAttribution's own doc
// comment for the full "why fire-and-forget, not blocking" rationale,
// which applies identically here (a whole book's worth of chapters can be
// queued by one Auto Split click, the same "don't hold dozens of HTTP
// requests open behind poolLLM's one slot" concern).
//
// speakerKey namespaces the dedup key alongside chapterID (see
// task.dedupKey's KindSpeakerReattribution case): a character's own ID, or
// the literal "unknown" sentinel for a run targeting Unknown-attributed
// paragraphs (Unknown has no characters-table row of its own to key off -
// see store.Paragraph.Speaker's own doc comment). Without this, two
// different speakers' own reattribution runs against the same chapter
// would collide into one task under EnqueueAttribution's own bare
// chapterID-only key, which is safe there only because a chapter never has
// more than one attribution run meaningfully in flight at once. label is
// the speaker's own display name, for the Jobs dashboard (see
// QueueTask.Label).
func (m *Manager) EnqueueReattribution(bookID, chapterID string, chapterIdx int, speakerKey, label string, fn ReattributionFunc) {
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:        KindSpeakerReattribution,
		tier:        defaultReattributionTier,
		chapterIdx:  chapterIdx,
		bookID:      bookID,
		chapterID:   chapterID,
		llmKey:      chapterID + "|" + speakerKey,
		label:       label,
		runLLM:      fn,
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
}

// RunReattribution is EnqueueReattribution's blocking sibling,
// RunAttribution's exact counterpart for "Auto Split" - used by the
// per-chapter fan-out inside an Auto Split bulk group (the one caller), a
// background pipeline task rather than an HTTP handler, so blocking is
// safe for the same reason RunAttribution's own doc comment gives. Same
// (chapter, speaker)-scoped dedup key as EnqueueReattribution, so a
// paused chapter's own fire-and-forget continuation (see
// httpapi.reattributeChapterSpeaker) joins/collides with it exactly as
// before. tier is caller-supplied (the wrapping task's own live tier -
// see PipelinePhaseFunc).
func (m *Manager) RunReattribution(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, speakerKey, label string, fn ReattributionFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindSpeakerReattribution, tier, bookID, chapterID, chapterIdx, chapterID+"|"+speakerKey, label, fn)
}

// EnqueueDirection queues fn as a KindSpeechDirection task - EnqueueAttribution's
// exact fire-and-forget shape (same dedup-by-chapter, same poolLLM
// dispatch, same defaultAttributionTier), for speech-direction tagging
// instead of speaker attribution. See EnqueueAttribution's own doc comment
// for the full reasoning (why fire-and-forget, why TierBackground, how
// progress is observed via GET /api/jobs) - it applies here unchanged.
func (m *Manager) EnqueueDirection(bookID, chapterID string, chapterIdx int, fn DirectionFunc) {
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:        KindSpeechDirection,
		tier:        defaultAttributionTier,
		chapterIdx:  chapterIdx,
		bookID:      bookID,
		chapterID:   chapterID,
		llmKey:      chapterID,
		runLLM:      fn, // DirectionFunc's underlying type already matches runLLM's field type exactly
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
}

// RunDirection is EnqueueDirection's blocking sibling, RunAttribution's
// exact counterpart for speech-direction tagging - see RunAttribution's
// own doc comment for why blocking is safe for its one caller
// (httpapi.runBookPipeline) despite EnqueueAttribution/EnqueueDirection's
// own fire-and-forget shape existing specifically to avoid that for an
// HTTP handler, and for why tier is caller-supplied rather than
// defaultAttributionTier.
func (m *Manager) RunDirection(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, fn DirectionFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindSpeechDirection, tier, bookID, chapterID, chapterIdx, chapterID, "", fn)
}

// EnqueueMusicScoring queues fn as a KindMusicScoring task -
// EnqueueDirection's exact fire-and-forget shape (same dedup-by-chapter,
// same poolLLM dispatch, same defaultAttributionTier), for background-
// music tone-region scoring instead of speech-direction tagging. See
// EnqueueAttribution's own doc comment for the full reasoning behind that
// shape - it applies here unchanged.
func (m *Manager) EnqueueMusicScoring(bookID, chapterID string, chapterIdx int, fn MusicScoreFunc) {
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:        KindMusicScoring,
		tier:        defaultAttributionTier,
		chapterIdx:  chapterIdx,
		bookID:      bookID,
		chapterID:   chapterID,
		llmKey:      chapterID,
		runLLM:      fn, // MusicScoreFunc's underlying type already matches runLLM's field type exactly
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
}

// RunMusicScoring is EnqueueMusicScoring's blocking sibling, RunAttribution's
// exact counterpart for background-music tone-region scoring - see
// RunAttribution's own doc comment for why blocking is safe for its
// pipeline-phase caller (httpapi.preprocessMusicPhase) despite
// EnqueueMusicScoring's own fire-and-forget shape existing specifically to
// avoid that for an HTTP handler, and for why tier is caller-supplied
// rather than defaultAttributionTier.
func (m *Manager) RunMusicScoring(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, fn MusicScoreFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindMusicScoring, tier, bookID, chapterID, chapterIdx, chapterID, "", fn)
}

// EnqueueScareQuote queues fn as a KindScareQuote task - EnqueueDirection's
// exact fire-and-forget shape (dedup-by-chapter, poolLLM,
// defaultAttributionTier). See EnqueueAttribution's own doc comment for the
// reasoning behind that shape.
func (m *Manager) EnqueueScareQuote(bookID, chapterID string, chapterIdx int, fn ScareQuoteFunc) {
	m.enqueueChapterLLM(KindScareQuote, bookID, chapterID, chapterIdx, fn)
}

// RunScareQuote is EnqueueScareQuote's blocking sibling - RunDirection's
// exact counterpart, for httpapi.preprocessScareQuotePhase.
func (m *Manager) RunScareQuote(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, fn ScareQuoteFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindScareQuote, tier, bookID, chapterID, chapterIdx, chapterID, "", fn)
}

// EnqueueDescription queues fn as a KindDescription task - same shape as
// EnqueueScareQuote. Blocked on the chapter's scare-quote tagging via
// scareQuoteDependency.
func (m *Manager) EnqueueDescription(bookID, chapterID string, chapterIdx int, fn DescriptionFunc) {
	m.enqueueChapterLLM(KindDescription, bookID, chapterID, chapterIdx, fn)
}

// RunDescription is EnqueueDescription's blocking sibling, for
// httpapi.preprocessDescriptionPhase.
func (m *Manager) RunDescription(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, fn DescriptionFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindDescription, tier, bookID, chapterID, chapterIdx, chapterID, "", fn)
}

// EnqueuePronunciation queues fn as a KindPronunciation task - same shape
// as EnqueueScareQuote.
func (m *Manager) EnqueuePronunciation(bookID, chapterID string, chapterIdx int, fn PronunciationFunc) {
	m.enqueueChapterLLM(KindPronunciation, bookID, chapterID, chapterIdx, fn)
}

// RunPronunciation is EnqueuePronunciation's blocking sibling, for
// httpapi.preprocessPronunciationPhase.
func (m *Manager) RunPronunciation(ctx context.Context, bookID, chapterID string, chapterIdx, tier int, fn PronunciationFunc) (int, error) {
	return m.runLLMTaskDirect(ctx, KindPronunciation, tier, bookID, chapterID, chapterIdx, chapterID, "", fn)
}

// enqueueChapterLLM is EnqueueScareQuote/EnqueueDescription/
// EnqueuePronunciation's shared
// fire-and-forget push - a chapter-keyed poolLLM task at
// defaultAttributionTier.
func (m *Manager) enqueueChapterLLM(kind Kind, bookID, chapterID string, chapterIdx int, fn func(ctx context.Context) (int, func(), error)) {
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:        kind,
		tier:        defaultAttributionTier,
		chapterIdx:  chapterIdx,
		bookID:      bookID,
		chapterID:   chapterID,
		llmKey:      chapterID,
		runLLM:      fn,
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
}

// HasHigherPriorityWork reports whether *any* pool currently has a task
// queued (not counting whatever's already in flight - a task already
// dispatched into poolLLM's own slot is necessarily the caller itself) at a
// tier more urgent than TierBackground, AND actually dispatchable right
// now (no unresolved dependency of its own - see taskqueue.Queue.
// AnyEligibleQueued). AttributionFunc/DirectionFunc implementations check
// this between batches to decide whether to pause a long-running
// background run and requeue the rest, rather than make a just-arrived,
// more urgent task wait out however many batches remain in this one
// first - see EnqueueAttribution/defaultAttributionTier and
// httpapi.attributeChapter/directChapter, its callers.
//
// The "actually dispatchable" half matters concretely, not just in
// theory: a chapter's own in-flight KindSpeechDirection task must never
// count that exact same chapter's own paragraphs (TierUrgent/
// TierLookahead, but blocked on this very task via
// speechDirectionDependency) as a reason to pause - yielding would
// accomplish nothing (poolGeneration's own top candidate would still be
// blocked the instant it tried to dispatch), so this used to just pause
// after every single batch, requeue, and get immediately redispatched
// again since nothing else outranked it - a confirmed-live, pointless
// pause/resume churn on every batch of every chapter with its own
// lookahead work queued, not a genuine yield to anything.
//
// Checks every TTS-family pool, not just poolLLM: an in-flight
// attribution/characterization/direction-tagging task should yield just as
// readily for a reader's own genuinely-eligible urgent paragraph
// (TierUrgent/TierLookahead KindVoiceClone/KindVoiceDesign) or an urgent
// voice-provisioning task (poolDesign) as for another urgent poolLLM task,
// since fill's own mutual exclusion means neither can even start until
// this one actually stops. A TTS-family task never needs an equivalent of
// its own: it isn't a long-running loop that can check this between
// batches the way an attribution/direction run is, so its only lever for
// this same contention is the passive one - taskqueue.Queue's own
// "graceful drain" simply refusing to backfill a pool with more
// lower-priority work while another pool's own more urgent (and eligible)
// work waits on it to drain.
func (m *Manager) HasHigherPriorityWork() bool {
	return m.queue.AnyEligibleQueued(func(c taskqueue.Task) bool { return c.(*task).tier < TierBackground })
}

// RunCharacterization queues fn as a KindSpeakerCharacterization task -
// EnqueueAttribution's sibling for the other LLM kind, sharing its poolLLM
// slot(s) and join-on-duplicate behavior, but (unlike EnqueueAttribution)
// still blocks the caller until a slot dispatches it and fn returns,
// returning ctx.Err() if ctx is cancelled first without leaking the
// still-queued or in-flight task itself (resultCh is buffered, so
// handleResult's send into it once fn actually finishes never blocks on a
// caller who stopped listening) - see CharacterizationFunc's own doc
// comment for why blocking is fine here even though it stopped being fine
// for attribution. Differences from EnqueueAttribution are otherwise just
// shape: keyed by characterID rather than a chapter (a character isn't
// chapter/paragraph-scoped), no chapterIdx, and fn reports only an error,
// not a count. label is a human-readable identifier (the character's
// name) for the Jobs dashboard, which otherwise has no chapter/paragraph
// to show for this kind - see QueueTask.Label.
func (m *Manager) RunCharacterization(ctx context.Context, bookID, characterID, label string, fn CharacterizationFunc) error {
	return m.RunCharacterizationAt(ctx, TierUrgent, bookID, characterID, label, fn)
}

// RunCharacterizationAt is RunCharacterization at a caller-chosen tier - for
// a bulk group's own per-character fan-out (httpapi.handleBulkAction), which
// dispatches at the group's own live tier rather than TierUrgent.
func (m *Manager) RunCharacterizationAt(ctx context.Context, tier int, bookID, characterID, label string, fn CharacterizationFunc) error {
	_, err := m.runLLMTask(ctx, KindSpeakerCharacterization, tier, bookID, "", 0, characterID, label, func(ctx context.Context) (int, error) {
		return 0, fn(ctx)
	})
	return err
}

// EnqueueCharacterization queues fn as a KindSpeakerCharacterization task -
// RunCharacterization's fire-and-forget sibling, for a caller that wants to
// queue many characters' worth of work in one request rather than holding
// an HTTP connection open per character (see EnqueueAttribution's own doc
// comment for the full rationale, which applies identically here: a
// "Recharacterize all" firing one blocking request per character hits the
// same browser per-origin connection cap, and a page reload before every
// request even reached pushTask would silently drop whichever characters
// hadn't been dispatched yet - not just fail to report their result).
// TierBackground, not RunCharacterization's TierUrgent - same reasoning as
// defaultAttributionTier: bulk background work like this shouldn't preempt
// a reader's own explicit single "Regenerate" click (still TierUrgent via
// RunCharacterization) queued behind it. Same llmKey-based dedup as
// RunCharacterization - a duplicate call for a character already queued or
// in flight silently joins it via pushTask, fine here since nothing is
// waiting on a result to join anyway (no llmWaiters is set, unlike
// RunCharacterization's own resultCh). Progress is observable via GET
// /api/jobs and by refetching the book's speaker table once each
// character's task clears, same as EnqueueAttribution.
func (m *Manager) EnqueueCharacterization(bookID, characterID, label string, fn CharacterizationFunc) {
	seriesName, seriesIndex := m.seriesFor(bookID)
	m.pushTask(&task{
		kind:        KindSpeakerCharacterization,
		tier:        TierBackground,
		bookID:      bookID,
		llmKey:      characterID,
		label:       label,
		runLLM:      func(ctx context.Context) (int, func(), error) { return 0, nil, fn(ctx) },
		seriesName:  seriesName,
		seriesIndex: seriesIndex,
	})
}

// previewSeq gives every RunVoiceDesignPreview call its own dedup key
// (see task.dedupKey's KindVoiceDesignPreview case) - a preview has no
// natural shared identity like a chapter or character id to dedup
// against, so each one always queues as new rather than risking two
// unrelated "Test" clicks joining into one task and getting each other's
// audio back.
var previewSeq int64

// RunVoiceDesignPreview runs fn (rendering one caller-supplied instruct/
// text/seed/language via the TTS worker - see httpapi.handleTestVoiceDesign,
// the only caller) as a real KindVoiceDesignPreview task in poolGeneration,
// the same slot pool and concurrency limit (maxInFlight) real paragraph
// generation draws from - rather than calling the TTS worker directly,
// uncoordinated with everything else competing for it, the way this used
// to. Still blocks the caller until the render finishes, same shape as
// RunCharacterization: see EnqueueAttribution's own doc comment for why
// fire-and-forget doesn't fit here either - a single preview render is
// exactly the "one request, holding the connection open while the reader
// waits to actually hear it is fine" case that doc comment describes, not
// the unbounded-bulk-work case that made attribution stop blocking.
func (m *Manager) RunVoiceDesignPreview(ctx context.Context, instruct string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resultCh := make(chan previewOutcome, 1)
	seq := atomic.AddInt64(&previewSeq, 1)
	m.pushTask(&task{
		kind:   KindVoiceDesignPreview,
		tier:   TierUrgent,
		llmKey: fmt.Sprintf("%d", seq),
		// Doubles as httpapi's job-queue dashboard identifier for this
		// kind (see QueueTask.Label's own doc comment) and as what makes
		// two concurrent previews distinguishable there at all -
		// JobsPage's own taskKey relies on (kind, bookId, chapterIdx,
		// paragraphIdx, label) being unique per task, and every one of
		// those besides label is a shared zero value for this kind.
		label: fmt.Sprintf("preview #%d", seq),
		// Purely for the dashboard's own "Voice" column (see
		// toQueueTask/QueueTask.Instruct) - not read by runPreview itself,
		// which already has this and everything else it needs baked into
		// its own closure.
		instruct:       instruct,
		runPreview:     fn,
		previewWaiters: []chan previewOutcome{resultCh},
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-resultCh:
		return out.audio, out.err
	}
}

// EnqueueVoiceProvision queues fn (one character's voice provisioning or
// forced regeneration - see httpapi.provisionCharacterVoice/
// regenerateCharacterVoice, the only callers) as a KindVoiceProvision task -
// fire-and-forget, like EnqueueAttribution/EnqueueCharacterization, and for
// the same reason: SpeakersPage's "Generate/Regenerate all voices" can
// queue a whole roster at once, and blocking a request per character hits
// the same browser connection-cap/page-reload problems EnqueueAttribution's
// own doc comment describes. TierBackground - same reasoning as
// defaultAttributionTier/EnqueueCharacterization's own tier choice: bulk
// background work like this shouldn't preempt more urgent poolGeneration
// work (an actual paragraph the reader is waiting to hear).
//
// mode ("generate" or "regenerate" - httpapi's only two callers) becomes
// part of llmKey/dedupKey (see task.dedupKey's KindVoiceProvision case) so
// a generate call and a regenerate call for the same character are tracked
// as distinct tasks - joining them would be wrong, since one wants "skip
// if already provisioned" and the other wants "always re-render." label is
// the character's name, for the Jobs dashboard - this kind has no
// chapter/paragraph of its own to show, same reason
// KindSpeakerCharacterization carries one.
func (m *Manager) EnqueueVoiceProvision(bookID, characterID, mode, label string, fn func(ctx context.Context, attempt int) (string, error)) {
	m.pushTask(&task{
		kind:         KindVoiceProvision,
		tier:         TierBackground,
		bookID:       bookID,
		llmKey:       mode + ":" + characterID,
		label:        label,
		runProvision: fn,
	})
}

// RunVoiceProvision is EnqueueVoiceProvision's blocking sibling -
// provisionMissingCharacterVoices (the lazy, automatic-during-reading
// path) uses this instead so it can use the freshly (or already)
// provisioned presetID immediately once this returns, the same way
// RunVoiceDesignPreview blocks that method's own caller for its render.
// Same TierUrgent/dedup-join shape as RunCharacterization/
// RunVoiceDesignPreview: a lazy request for a character already being
// provisioned - by this same lazy path racing itself, or an explicit
// "Generate voice" click sharing the same "generate" mode dedup key (see
// EnqueueVoiceProvision/task.dedupKey) - joins that existing task's
// waiters instead of starting a second one (see pushTask's own doc
// comment), rather than being dropped the way a duplicate
// EnqueueVoiceProvision push would be. Unlike EnqueueVoiceProvision's
// fire-and-forget callers, routing the lazy path through here (instead of
// calling the provisioner function directly, which is what this
// replaced) is what makes lazy provisioning show up on the Jobs
// dashboard at all - see provisionWaiters' own doc comment.
func (m *Manager) RunVoiceProvision(ctx context.Context, bookID, characterID, mode, label string, fn func(ctx context.Context, attempt int) (string, error)) (string, error) {
	return m.RunVoiceProvisionAt(ctx, TierUrgent, bookID, characterID, mode, label, fn)
}

// RunVoiceProvisionAt is RunVoiceProvision at a caller-chosen tier - see
// RunCharacterizationAt.
func (m *Manager) RunVoiceProvisionAt(ctx context.Context, tier int, bookID, characterID, mode, label string, fn func(ctx context.Context, attempt int) (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	resultCh := make(chan provisionOutcome, 1)
	m.pushTask(&task{
		kind:             KindVoiceProvision,
		tier:             tier,
		bookID:           bookID,
		llmKey:           mode + ":" + characterID,
		label:            label,
		runProvision:     fn,
		provisionWaiters: []chan provisionOutcome{resultCh},
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case out := <-resultCh:
		return out.presetID, out.err
	}
}

// EnqueueSFXGeneration queues fn (render this paragraph's own Stable Audio
// SFX clip and persist it - see httpapi.handleGenerateParagraphSFX, the only
// caller) as a KindSFXGeneration task in poolSFX - fire-and-forget, same
// shape as EnqueueVoiceProvision: the reader's "Generate" click in the
// SFX panel gets an immediate 202 rather than holding the HTTP request
// open for the render itself (a multi-step diffusion call, materially
// slower than an ordinary clone). TierUrgent, not TierBackground: unlike
// EnqueueVoiceProvision's own bulk "regenerate every character" callers,
// this is always one reader clicking one paragraph's own "Generate" button
// right now, the same urgency EnqueueParagraphRegenerate already assumes
// for the analogous narration action.
func (m *Manager) EnqueueSFXGeneration(bookID, chapterID string, chapterIdx int, paragraph store.Paragraph, fn func(ctx context.Context, attempt int) error) {
	m.pushTask(&task{
		kind:       KindSFXGeneration,
		tier:       TierUrgent,
		bookID:     bookID,
		chapterID:  chapterID,
		chapterIdx: chapterIdx,
		paragraph:  paragraph,
		runSFXGen:  fn,
	})
}

// EnqueueMusicGeneration queues fn as a KindMusicGeneration task - render
// and persist every one of a chapter's currently-eligible background-music
// regions in one task (see generateChapterMusicBatch). Its only caller is
// advanceWholeChapterMusic, once the whole chapter is voiced and it's found
// at least one pending region that (for a "continuation" transition)
// whose immediately preceding region has already finished or will finish
// earlier in this same batch - see MusicRegion's own doc comment for the
// full readiness contract. TierBackground, not TierUrgent the way
// EnqueueSFXGeneration's own explicit reader click is - see
// KindMusicGeneration's own doc comment for why. chapterID is this task's
// own dedup key suffix (see task.dedupKey) - a chapter can only usefully
// have one music-generation batch in flight at a time, the same "one at a
// time per chapter" shape KindMusicScoring already has.
func (m *Manager) EnqueueMusicGeneration(bookID, chapterID string, chapterIdx int, fn func(ctx context.Context, attempt int) error) {
	m.enqueueMusicGeneration(bookID, chapterID, chapterIdx, TierBackground, fn)
}

func (m *Manager) enqueueMusicGeneration(bookID, chapterID string, chapterIdx, tier int, fn func(ctx context.Context, attempt int) error) {
	m.pushTask(&task{
		kind:        KindMusicGeneration,
		tier:        tier,
		bookID:      bookID,
		chapterID:   chapterID,
		chapterIdx:  chapterIdx,
		llmKey:      chapterID,
		runMusicGen: fn,
	})
}

// pendingMusicRegion is one region MaybeAdvanceChapterMusic found eligible
// to generate right now, plus the target duration it computed for it (the
// sum of that region's own paragraphs' real narration durations) - carried
// from the scan into generateChapterMusicBatch so the batch task itself
// never needs to re-derive it.
type pendingMusicRegion struct {
	region         store.MusicRegion
	targetDuration float64
}

// MaybeAdvanceChapterMusic checks chapterID's own music regions (if
// bookID's own store.Book.MusicEnabled - background music is a book-wide
// toggle, not a per-chapter one, unlike scoring itself, which stays
// chapter-scoped) and dispatches whatever is eligible to generate, along
// two independent paths:
//
//   - Whole chapter (advanceWholeChapterMusic, KindMusicGeneration,
//     TierBackground): once every paragraph in the chapter has ready
//     narration, every eligible region goes out as one batch.
//   - Live (advanceLiveMusic, KindMusicLiveGeneration, TierUrgent): when
//     the reader's position is in this chapter, regions go out one task
//     each, chained forward from the one they're in, as soon as each
//     region's own paragraphs are voiced, so music starts while the rest
//     of the chapter is still being narrated.
//
// Both honor store.MusicRegion's own continuation contract: a
// MusicTransitionContinuation region's immediately preceding region must
// already have settled (AudioReady or AudioError) or be earlier in the
// same batch, so its clip exists to seed this one's first chunk from
// (advanceLiveMusic's own doc comment has its one exception). Both can
// reach the same region, so generateChapterMusicBatch claims each region
// in memory before generating it (claimMusicRegion) - a region is only ever
// generated once, and a batch that finds its next region claimed by the
// other path stops there and is picked up again when that task finishes.
//
// Unlike the LLM kinds (attribution/direction/music-scoring), the actual
// generation call needs nothing httpapi doesn't already give this package
// directly - m.tts satisfies musicgen.Backend, and m.store/m.dataDir are
// this package's own fields - so this builds and dispatches the whole
// closure itself with no caller-supplied function required.
//
// Called from: httpapi.scoreChapterMusic, after each batch of freshly-
// scored regions persists (a chapter already fully narrated before music
// was turned on has no future paragraph-completion event to kick it off);
// saveParagraphAudio/handleMergedResult, once a paragraph finishes
// narrating, in case it was the last one some region needed; handleResult,
// once either music kind's batch finishes, to advance past it; and
// enqueueLookahead (via advanceLiveChapterMusic, live path only, with the
// reader's exact position). Cheap to over-call: a book with MusicEnabled false (the
// overwhelming common case) costs one row lookup and returns immediately,
// and both kinds' own per-chapter dedup keys mean a chapter is never
// queued twice for the same path.
func (m *Manager) MaybeAdvanceChapterMusic(bookID, chapterID string) {
	st, err := m.loadChapterMusic(bookID, chapterID)
	if err != nil || st == nil || !st.book.MusicEnabled || len(st.regions) == 0 {
		return
	}
	// Live path follows the book's saved position, if it's in this chapter.
	if st.book.PosChapterIdx == st.ch.Idx {
		m.advanceLiveMusic(bookID, chapterID, st.ch.Idx, st.regions, st.book.PosParagraphIdx, st.regionDuration, liveChainCarry{})
	}
	if st.allReady {
		m.advanceWholeChapterMusic(bookID, chapterID, st.ch.Idx, st.regions, st.regionDuration, TierBackground)
	}
}

// advanceLiveChapterMusic advances only the live music path from livePos,
// the paragraph idx the reader is at right now in chapterID
// (enqueueLookahead, which knows it before the book's saved position
// catches up). Never touches the whole-chapter batch - see
// enqueueLookahead.
func (m *Manager) advanceLiveChapterMusic(bookID, chapterID string, livePos int) {
	st, err := m.loadChapterMusic(bookID, chapterID)
	if err != nil || st == nil || !st.book.MusicEnabled || len(st.regions) == 0 {
		return
	}
	m.advanceLiveMusic(bookID, chapterID, st.ch.Idx, st.regions, livePos, st.regionDuration, liveChainCarry{})
}

// chapterMusicState is everything both music paths need to decide what's
// eligible in one chapter - see loadChapterMusic.
type chapterMusicState struct {
	book    *store.Book
	ch      *store.Chapter
	regions []store.MusicRegion
	// allReady reports whether every paragraph in the chapter has ready
	// narration - the whole-chapter path's own gate.
	allReady bool
	// regionDuration is a region's narration length, or ok=false if any of
	// its paragraphs isn't voiced yet (or it points past the chapter's own
	// paragraphs - stale scoring).
	regionDuration func(store.MusicRegion) (float64, bool)
}

// loadChapterMusic gathers chapterMusicState for chapterID, or nil if the
// book/chapter doesn't exist. Doesn't check store.Book.MusicEnabled -
// the automatic path does, the explicit GenerateChapterMusic doesn't.
func (m *Manager) loadChapterMusic(bookID, chapterID string) (*chapterMusicState, error) {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil {
		return nil, err
	}
	ch, err := m.store.GetChapterByID(chapterID)
	if err != nil || ch == nil {
		return nil, err
	}
	regions, err := m.store.ListMusicRegions(chapterID)
	if err != nil {
		return nil, err
	}
	if len(regions) == 0 {
		// Not scored - nothing either path could do, so skip the
		// narration-status queries below entirely.
		return &chapterMusicState{book: book, ch: ch}, nil
	}
	paragraphs, err := m.store.ListParagraphsRaw(chapterID)
	if err != nil {
		return nil, err
	}
	// Ready narration durations, keyed by paragraph id - under the voice
	// each paragraph currently resolves to; one status query per distinct
	// voice (usually one or a handful per chapter). They size each region
	// below. allReady gates the whole-chapter batch: that one only starts
	// once the *whole* chapter is voiced, so it never competes with the
	// chapter's own narration for the GPU (poolSFX and poolGeneration are
	// mutually exclusive TTS-family pools). The live batch only needs its
	// own regions' paragraphs.
	idsByVoice := map[string][]string{}
	for _, p := range paragraphs {
		voice, verr := m.narration.ForParagraph(book, p.Speaker)
		if verr != nil {
			return nil, verr
		}
		idsByVoice[voice.VoiceID()] = append(idsByVoice[voice.VoiceID()], p.ID)
	}
	durationByID := make(map[string]float64, len(paragraphs))
	allReady := true
	for voiceID, ids := range idsByVoice {
		states, serr := m.store.ParagraphAudioStatuses(ids, voiceID)
		if serr != nil {
			return nil, serr
		}
		for _, id := range ids {
			if st, ok := states[id]; ok && st.Status == store.AudioReady {
				durationByID[id] = st.DurationSeconds
			} else {
				allReady = false
			}
		}
	}
	byIdx := make(map[int]store.Paragraph, len(paragraphs))
	for _, p := range paragraphs {
		byIdx[p.Idx] = p
	}
	regionDuration := func(region store.MusicRegion) (float64, bool) {
		total := 0.0
		for idx := region.StartIdx; idx <= region.EndIdx; idx++ {
			p, ok := byIdx[idx]
			if !ok {
				return 0, false
			}
			d, ok := durationByID[p.ID]
			if !ok {
				return 0, false
			}
			total += d
		}
		return total, true
	}
	return &chapterMusicState{book: book, ch: ch, regions: regions, allReady: allReady, regionDuration: regionDuration}, nil
}

// ErrChapterNotVoiced/ErrChapterNotScored are GenerateChapterMusic's own
// "can't yet" answers: a region's clip is sized to its own narration, so
// music can't generate before the chapter's narration exists, and there
// are no regions at all before scoring.
var (
	ErrChapterNotVoiced = errors.New("chapter narration isn't fully generated yet")
	ErrChapterNotScored = errors.New("chapter background music hasn't been scored yet")
)

// GenerateChapterMusic is the explicit, reader-clicked whole-chapter
// music run (the Speakers page's per-chapter button): unlike the automatic
// path (MaybeAdvanceChapterMusic), it runs regardless of the book-wide
// MusicEnabled toggle - the same "always allowed" treatment scoring
// already gets - and first resets every AudioError region back to pending
// so a retry actually retries them; the automatic path treats an errored
// region as settled and never goes back to it. TierNormal, above the
// automatic path's TierBackground. Returns how many regions it queued (0
// if every region already has music).
func (m *Manager) GenerateChapterMusic(bookID, chapterID string) (int, error) {
	st, err := m.loadChapterMusic(bookID, chapterID)
	if err != nil {
		return 0, err
	}
	if st == nil {
		return 0, ErrTaskNotFound
	}
	if len(st.regions) == 0 {
		return 0, ErrChapterNotScored
	}
	if !st.allReady {
		return 0, ErrChapterNotVoiced
	}
	for i, r := range st.regions {
		if r.Status == store.AudioError && !m.musicRegionGenerating(r.ID) {
			if err := m.store.ResetMusicRegionAudio(r.ID); err != nil {
				return 0, err
			}
			st.regions[i].Status = store.AudioPending
		}
	}
	return m.advanceWholeChapterMusic(bookID, chapterID, st.ch.Idx, st.regions, st.regionDuration, TierNormal), nil
}

// musicRegionPending reports whether a region still needs generating.
// "generating" is never written any more (see Manager.musicGenerating) but
// may still be on a row left over from before that, and means the same
// thing now: nothing is actually generating it.
func musicRegionPending(status string) bool {
	return status == store.AudioPending || status == store.AudioGenerating
}

// advanceWholeChapterMusic dispatches every currently-eligible region of a
// fully-voiced chapter as one KindMusicGeneration batch. The scan walks
// regions in order and stops the moment it hits one it can't safely batch
// because the region before it neither has settled nor is itself part of
// this batch - regions further along the chapter can never be eligible
// before that point either. Everything gathered before that point (zero,
// one, or several regions) becomes one batch.
func (m *Manager) advanceWholeChapterMusic(bookID, chapterID string, chapterIdx int, regions []store.MusicRegion, regionDuration func(store.MusicRegion) (float64, bool), tier int) int {
	batch, initialSeedRegionID := selectWholeChapterMusic(regions, regionDuration, m.musicRegionGenerating)
	if len(batch) == 0 {
		return 0
	}
	m.enqueueMusicGeneration(bookID, chapterID, chapterIdx, tier, func(ctx context.Context, attempt int) error {
		return m.generateChapterMusicBatch(ctx, bookID, chapterID, batch, initialSeedRegionID)
	})
	return len(batch)
}

// selectWholeChapterMusic is advanceWholeChapterMusic's own scan, pulled
// out as a pure function so it's testable without a real Store - generating
// reports whether a region is claimed right now (Manager.musicRegionGenerating).
func selectWholeChapterMusic(regions []store.MusicRegion, regionDuration func(store.MusicRegion) (float64, bool), generating func(string) bool) (batch []pendingMusicRegion, initialSeedRegionID string) {
	// initialSeedRegionID is the batch's own first item's seed, if any -
	// read from whatever real, already-settled region (from an earlier
	// batch, or the chapter's own start) immediately precedes it, since
	// generateChapterMusicBatch's own loop only ever tracks a seed forward
	// from one batch item to the next and has no earlier history of its
	// own to fall back on for the very first one.
	lastBatchedIdx := -1
	for i, region := range regions {
		// A region the live batch is generating right now counts as
		// unsettled - the scan continues past it only to break on the
		// region after it, so nothing gets batched behind it until its
		// own task finishes and re-runs this.
		if !musicRegionPending(region.Status) || generating(region.ID) {
			continue
		}
		if i > 0 {
			prev := regions[i-1]
			settled := !musicRegionPending(prev.Status)
			inThisBatch := lastBatchedIdx == i-1
			if !settled && !inThisBatch {
				break
			}
		}
		total, ok := regionDuration(region)
		if !ok {
			// Stale scoring (a region pointing past the chapter's own
			// paragraphs) - nothing further along can be trusted either.
			break
		}
		if len(batch) == 0 && i > 0 && regions[i-1].Status == store.AudioReady {
			initialSeedRegionID = regions[i-1].ID
		}
		batch = append(batch, pendingMusicRegion{region: region, targetDuration: total})
		lastBatchedIdx = i
	}
	return batch, initialSeedRegionID
}

// advanceLiveMusic queues the next region of the live music chain as its
// own KindMusicLiveGeneration task: the first region at or after the one
// the reader is in (the one whose [StartIdx, EndIdx] contains pos) that
// still needs music and isn't already being generated or queued - see
// selectLiveMusic. At most one live task per chapter is ever queued (not
// yet dispatched) at a time; the chain advances as each one dispatches
// (continueLiveMusic queues its successor, which waits on it via
// liveMusicDependency) and finishes (handleResult's MaybeAdvanceChapterMusic),
// and as more paragraphs get voiced. The chain is naturally bounded by
// narration: a region only goes out once its own paragraphs are voiced.
//
// A still-queued live task for a region the reader has already moved past
// is canceled first, so a jump forward never waits behind stale music.
//
// Crosses chapter boundaries: once every region from pos to the end of
// this chapter is settled or covered, the chain continues into the next
// chapter's first region (see liveChainCarry), skipping over at most
// maxLiveMusicChapterHops chapters in one call.
func (m *Manager) advanceLiveMusic(bookID, chapterID string, chapterIdx int, regions []store.MusicRegion, pos int, regionDuration func(store.MusicRegion) (float64, bool), carry liveChainCarry) {
	behind := map[string]bool{}
	for _, r := range regions {
		if r.EndIdx < pos {
			behind[r.ID] = true
		}
	}
	for {
		tk, ok := m.queue.Cancel(func(c taskqueue.Task) bool {
			return isLiveMusicTask(c, chapterID) && behind[c.(*task).llmKey]
		})
		if !ok {
			break
		}
		m.dropCanceledQueued(tk.(*task))
	}
	if m.queue.AnyQueued(func(c taskqueue.Task) bool { return isLiveMusicTask(c, chapterID) }) {
		return
	}

	next, prevID, ok, exhausted := selectLiveMusic(regions, pos, regionDuration, m.musicRegionGenerating, m.liveMusicTaskExists)
	if !ok {
		if exhausted {
			m.advanceLiveMusicIntoNextChapter(bookID, chapterID, chapterIdx, regions[len(regions)-1].ID, carry.hops)
		}
		return
	}
	// Seeded from prevID's stems, layer by layer (musicRegionSeeds decides
	// which, from the transition and the two ambience prompts) - read at
	// run time, by which point liveMusicDependency has made sure prevID's
	// own generation (if any was pending) is done.
	seedID := prevID
	prevChapterID := chapterID
	if prevID == "" && carry.prevRegionID != "" && next.region.ID == regions[0].ID {
		// First region of a chapter the chain crossed into: wait on the
		// previous chapter's last region, but don't seed from it.
		prevID, prevChapterID = carry.prevRegionID, carry.prevChapterID
	}
	region := next.region
	m.pushTask(&task{
		kind:               KindMusicLiveGeneration,
		tier:               TierUrgent,
		bookID:             bookID,
		chapterID:          chapterID,
		chapterIdx:         chapterIdx,
		llmKey:             region.ID,
		musicRegionStart:   region.StartIdx,
		musicPrevRegionID:  prevID,
		musicPrevChapterID: prevChapterID,
		runMusicGen: func(ctx context.Context, attempt int) error {
			if m.readerPassedRegion(bookID, chapterIdx, region) {
				return nil
			}
			return m.generateChapterMusicBatch(ctx, bookID, chapterID, []pendingMusicRegion{next}, seedID)
		},
	})
}

// maxLiveMusicChapterHops bounds how many chapters one advanceLiveMusic
// call walks past the one it started in, looking for the chain's next
// region - only chapters whose music is already all settled/covered are
// ever walked past, so this just caps store reads when several upcoming
// chapters already have music.
const maxLiveMusicChapterHops = 2

// liveChainCarry is what advanceLiveMusic carries from one chapter into the
// next when the chain crosses a chapter boundary.
type liveChainCarry struct {
	prevChapterID string
	prevRegionID  string
	hops          int
}

// advanceLiveMusicIntoNextChapter continues the live chain from the end of
// chapter chapterIdx (whose last region is lastRegionID) into the next
// chapter, from its first paragraph. A next chapter that isn't scored yet
// stops the chain, but gets its scoring promoted so the chain can cross
// into it once scoring lands (httpapi.scoreChapterMusic's own
// MaybeAdvanceChapterMusic call picks it back up then).
func (m *Manager) advanceLiveMusicIntoNextChapter(bookID, chapterID string, chapterIdx int, lastRegionID string, hops int) {
	if hops >= maxLiveMusicChapterHops {
		return
	}
	next, err := m.store.GetChapterByIdx(bookID, chapterIdx+1)
	if err != nil || next == nil {
		return
	}
	st, err := m.loadChapterMusic(bookID, next.ID)
	if err != nil || st == nil || !st.book.MusicEnabled {
		return
	}
	if len(st.regions) == 0 {
		m.queue.WithTask(
			func(c taskqueue.Task) bool { return c.Key() == "music_score:"+next.ID },
			func(c taskqueue.Task) {
				if TierLookahead < c.Tier() {
					c.Promote(TierLookahead)
				}
			},
		)
		return
	}
	m.advanceLiveMusic(bookID, next.ID, next.Idx, st.regions, 0, st.regionDuration, liveChainCarry{
		prevChapterID: chapterID,
		prevRegionID:  lastRegionID,
		hops:          hops + 1,
	})
}

// continueLiveMusic is a live task's dispatch-time hook: queue the chain's
// next region now, rather than only once t finishes, so it's already
// waiting (on t, via liveMusicDependency) the moment t's slot frees up.
// Continues from t's own region, or from the reader's saved position if
// that's further along in this chapter.
func (m *Manager) continueLiveMusic(t *task) {
	pos := t.musicRegionStart
	if book, err := m.store.GetBook(t.bookID); err == nil && book != nil && book.PosChapterIdx == t.chapterIdx && book.PosParagraphIdx > pos {
		pos = book.PosParagraphIdx
	}
	m.advanceLiveChapterMusic(t.bookID, t.chapterID, pos)
}

// readerPassedRegion reports whether the reader's saved position is already
// past region (a later chapter, or a later paragraph in this one) - a live
// task that finally dispatches for music nobody will hear skips it; the
// whole-chapter batch still covers it later.
func (m *Manager) readerPassedRegion(bookID string, chapterIdx int, region store.MusicRegion) bool {
	book, err := m.store.GetBook(bookID)
	if err != nil || book == nil {
		return false
	}
	return book.PosChapterIdx > chapterIdx || (book.PosChapterIdx == chapterIdx && book.PosParagraphIdx > region.EndIdx)
}

// isLiveMusicTask reports whether c is a KindMusicLiveGeneration task for
// chapterID.
func isLiveMusicTask(c taskqueue.Task, chapterID string) bool {
	t := c.(*task)
	return t.kind == KindMusicLiveGeneration && t.chapterID == chapterID
}

// liveMusicTaskExists reports whether a live task for regionID is queued or
// in flight.
func (m *Manager) liveMusicTaskExists(regionID string) bool {
	_, ok := m.queue.Find(func(c taskqueue.Task) bool { return c.Key() == "music_live:"+regionID })
	return ok
}

// liveMusicDependency makes a live task wait for whatever is producing its
// predecessor region's clip (its seed): the predecessor's own live task,
// queued or in flight, or - if the whole-chapter batch has claimed the
// predecessor - that batch.
func (m *Manager) liveMusicDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	if t.musicPrevRegionID == "" {
		return nil
	}
	if d, ok := lq.Get("music_live:" + t.musicPrevRegionID); ok {
		return d
	}
	if m.musicRegionGenerating(t.musicPrevRegionID) {
		if d, ok := lq.Get("music_gen:" + t.musicPrevChapterID); ok {
			return d
		}
	}
	return nil
}

// selectLiveMusic is advanceLiveMusic's own scan, pulled out as a pure
// function for the same reason as selectWholeChapterMusic. Walks forward
// from the reader's region past everything already settled or covered
// (generating, or with a live task queued/in flight - queued) and returns
// the first region that still needs one, plus the region right before it
// (the seed/dependency - see liveMusicDependency), or ok=false if that
// region isn't voiced yet. exhausted reports the chain ran off the end of
// the chapter (everything from the reader's region on is settled or
// covered) - the caller's cue to continue into the next chapter. The
// reader's own region is the one exception
// to "predecessor must be settled or covered": if the region before it
// was never generated at all (the reader jumped straight into the middle
// of the chapter), it generates unseeded rather than making the reader
// wait on music for a stretch they've skipped.
func selectLiveMusic(regions []store.MusicRegion, pos int, regionDuration func(store.MusicRegion) (float64, bool), generating, queued func(string) bool) (next pendingMusicRegion, prevRegionID string, ok, exhausted bool) {
	cur := -1
	for i, r := range regions {
		if r.StartIdx <= pos && pos <= r.EndIdx {
			cur = i
			break
		}
	}
	if cur < 0 {
		return pendingMusicRegion{}, "", false, false
	}
	covered := func(id string) bool { return generating(id) || queued(id) }
	for i := cur; i < len(regions); i++ {
		region := regions[i]
		if !musicRegionPending(region.Status) || covered(region.ID) {
			continue
		}
		total, voiced := regionDuration(region)
		if !voiced {
			return pendingMusicRegion{}, "", false, false
		}
		if i > 0 {
			prev := regions[i-1]
			if !musicRegionPending(prev.Status) || covered(prev.ID) {
				prevRegionID = prev.ID
			}
			// Otherwise i == cur (anything later has a settled/covered
			// predecessor, or the loop would have stopped there) - go
			// unseeded, see this function's own doc comment.
		}
		return pendingMusicRegion{region: region, targetDuration: total}, prevRegionID, true, false
	}
	return pendingMusicRegion{}, "", false, true
}

// claimMusicRegion marks id as being generated, reporting false if some
// other task already has it - the whole-chapter and live batches can both
// reach the same region, and poolSFX runs several tasks at once.
func (m *Manager) claimMusicRegion(id string) bool {
	m.musicGenMu.Lock()
	if m.musicGenerating[id] {
		m.musicGenMu.Unlock()
		return false
	}
	if m.musicGenerating == nil {
		m.musicGenerating = map[string]bool{}
	}
	m.musicGenerating[id] = true
	m.musicGenMu.Unlock()
	m.notifyChanged()
	return true
}

func (m *Manager) releaseMusicRegion(id string) {
	m.musicGenMu.Lock()
	delete(m.musicGenerating, id)
	m.musicGenMu.Unlock()
	m.notifyChanged()
}

func (m *Manager) musicRegionGenerating(id string) bool {
	m.musicGenMu.Lock()
	defer m.musicGenMu.Unlock()
	return m.musicGenerating[id]
}

// MusicRegionGenerating reports whether region id is being generated right
// now - the in-memory state httpapi overlays onto a region's stored status
// (which never holds "generating" itself).
func (m *Manager) MusicRegionGenerating(id string) bool {
	return m.musicRegionGenerating(id)
}

// generateChapterMusicBatch is MaybeAdvanceChapterMusic's own dispatched
// work: generate and persist every region in batch, strictly in order,
// within one task. A "continuation" region's seed is read from whichever
// region immediately precedes it on disk (audiopath.MusicRegionFile) right
// before generating it, not precomputed up front - by the time this loop
// reaches that region, the previous one has either already settled before
// this batch was even built (initialSeedRegionID, only ever consulted for
// batch's own first item - see MaybeAdvanceChapterMusic), or was itself
// just generated (and its file just written) earlier in this very loop, so
// reading fresh each time covers both cases with the same code path.
//
// Stops early (leaving whatever's left in batch untouched, still
// AudioPending) if ctx is canceled between regions - a preempted/paused
// batch simply gets picked up again by a later MaybeAdvanceChapterMusic
// call, the same "cooperative pause, not a failure" shape every other
// batched pass in this package follows.
//
// A single region's own generation failure no longer aborts the rest of
// the batch the way an earlier version of this function did (return err
// immediately, relying on handleResult's own retryEligible/requeueCopy to
// retry the *whole* batch from its own first item). That shape was fine
// for a small, per-region task, but got statistically much worse once
// music generation moved to one task per chapter covering every one of
// its currently-eligible regions at once (splitLongRegions can put 15-20+
// regions in a single long chapter's own batch): one region hitting a
// purely transient failure (e.g. ttsworker mid-restart) meant every region
// after it in dispatch order got zero attempts that round, not just the
// one that actually failed, and a persistent failure on an early region
// could burn through the whole task's own maxTaskAttempts budget - three
// full-batch retries, each one needlessly re-generating every already-
// Ready region before ever reaching the one still broken - while every
// later region sat untouched the entire time. generateMusicRegionWithRetry
// below now retries an individual region's own generation in place (its
// own bounded maxTaskAttempts budget, independent of every other region's
// and of this task's own outer retry), logs whatever it ultimately can't
// recover from, and this loop then moves on to the next region regardless
// - so one region's bad luck can no longer starve its siblings. Every
// region's own Status (Ready/Error) is set by generateMusicRegion itself
// on every attempt, so the DB always ends up correct regardless of how
// this loop as a whole concludes.
func (m *Manager) generateChapterMusicBatch(ctx context.Context, bookID, chapterID string, batch []pendingMusicRegion, initialSeedRegionID string) error {
	prevRegionID := initialSeedRegionID
	for _, item := range batch {
		if ctx.Err() != nil {
			return nil
		}
		// The other music kind may have this region right now - stop
		// here rather than generate what comes after it without its clip
		// to seed from; that task's own completion re-runs
		// MaybeAdvanceChapterMusic, which picks up where this left off.
		if !m.claimMusicRegion(item.region.ID) {
			return nil
		}
		// Re-read now that it's claimed: it may have been generated since
		// this batch was built.
		current, gerr := m.store.GetMusicRegion(item.region.ID)
		if gerr != nil || current == nil || !musicRegionPending(current.Status) {
			m.releaseMusicRegion(item.region.ID)
			if current != nil && current.Status == store.AudioReady {
				prevRegionID = current.ID
			}
			continue
		}
		seeds := m.musicRegionSeeds(bookID, chapterID, item.region, prevRegionID)
		err := m.generateMusicRegionWithRetry(ctx, bookID, chapterID, item.region, item.targetDuration, seeds)
		m.releaseMusicRegion(item.region.ID)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			// Already logged (and persisted as AudioError) inside
			// generateMusicRegionWithRetry - move on rather than let this
			// one region's exhausted retries starve every region after it.
			continue
		}
		prevRegionID = item.region.ID
	}
	return nil
}

// musicRegionRetryDelay is the pause generateMusicRegionWithRetry takes
// between one region's own failed attempt and its next - long enough to
// give a crashed/restarting ttsworker (see internal/ttsworker's own
// watchdog) a real chance to be back up by the next attempt, short enough
// not to noticeably slow a batch down when the failure is something else
// entirely (a bad prompt, a genuinely broken request).
const musicRegionRetryDelay = 2 * time.Second

// generateMusicRegionWithRetry retries generateMusicRegion up to
// maxTaskAttempts times for region alone - see generateChapterMusicBatch's
// own doc comment for why this now lives at the individual-region level
// instead of the whole chapter-batch task retrying from its own first
// item. A cancellation is never retried (mirrors retryEligible's own
// exclusion) and returns immediately so the caller's own ctx.Err() check
// treats this the same cooperative-pause way as everywhere else. Every
// attempt after the first, and the final permanent failure if every
// attempt is exhausted, gets its own explicit log line - previously only
// the outer whole-task failure was ever logged, which (now that a batch
// can cover 15-20+ regions) made an individual region's own permanent
// failure easy to lose track of.
func (m *Manager) generateMusicRegionWithRetry(ctx context.Context, bookID, chapterID string, region store.MusicRegion, targetDuration float64, seeds musicSeeds) error {
	var err error
	for attempt := 0; attempt < maxTaskAttempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		err = m.generateMusicRegion(ctx, bookID, chapterID, region, targetDuration, seeds)
		if err == nil {
			return nil
		}
		if errors.Is(err, context.Canceled) {
			return err
		}
		if attempt+1 >= maxTaskAttempts {
			break
		}
		log.Printf("jobs: music region %s (chapter %s) generation failed (attempt %d/%d), retrying: %v", region.ID, chapterID, attempt+1, maxTaskAttempts, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(musicRegionRetryDelay):
		}
	}
	log.Printf("jobs: music region %s (chapter %s) generation permanently failed after %d attempts: %v", region.ID, chapterID, maxTaskAttempts, err)
	return err
}

// musicSeeds are one region's continuation seeds, one per layer - each nil
// when that layer starts fresh. See musicRegionSeeds.
type musicSeeds struct {
	music    []byte
	ambience []byte
}

// musicRegionSeeds reads the continuation seeds for region from the region
// immediately before it (prevRegionID, "" for none), layer by layer: the
// music layer continues only across a "continuation" transition, while
// the ambience layer continues whenever both regions have the identical
// ambience prompt - the place hasn't changed, so its sound shouldn't
// restart even where the music hard-cuts (speakerattr's describe pass
// keeps an unchanged setting's prompt byte-identical for exactly this).
// Each layer is seeded from its own stem (audiopath.MusicRegionStemFile),
// never the mix, so the other layer never bleeds into the continuation; a
// region generated before stems existed falls back to its served clip for
// the music layer, which was music only then.
func (m *Manager) musicRegionSeeds(bookID, chapterID string, region store.MusicRegion, prevRegionID string) musicSeeds {
	var seeds musicSeeds
	if prevRegionID == "" {
		return seeds
	}
	if region.Transition == store.MusicTransitionContinuation {
		if data, err := os.ReadFile(audiopath.MusicRegionStemFile(m.dataDir, bookID, chapterID, prevRegionID, "music")); err == nil {
			seeds.music = data
		} else if data, err := os.ReadFile(audiopath.MusicRegionFile(m.dataDir, bookID, chapterID, prevRegionID)); err == nil {
			seeds.music = data
		}
	}
	if region.Ambience != "" {
		if prev, err := m.store.GetMusicRegion(prevRegionID); err == nil && prev != nil && prev.Ambience == region.Ambience {
			if data, err := os.ReadFile(audiopath.MusicRegionStemFile(m.dataDir, bookID, chapterID, prevRegionID, "ambience")); err == nil {
				seeds.ambience = data
			}
		}
	}
	return seeds
}

// generateMusicRegion is generateChapterMusicBatch's own per-region work:
// render region's full-duration clip via internal/musicgen (chaining as
// many Stable Audio Medium calls as targetDuration needs - see that
// package's own doc comment) and persist it exactly like an ordinary
// paragraph's own audio (saveParagraphAudio's shape, just against
// music_regions/audiopath.MusicRegionFile instead of paragraph_audio/
// audiopath.ParagraphFile). A region with an ambience prompt renders that
// layer as a second, separate GenerateRegion call and serves the two
// mixed (musicgen.MixAmbience); both stems are kept on disk to seed the
// next region's layers (musicRegionSeeds).
func (m *Manager) generateMusicRegion(ctx context.Context, bookID, chapterID string, region store.MusicRegion, targetDuration float64, seeds musicSeeds) error {
	music, err := musicgen.GenerateRegion(ctx, m.tts, musicgen.Region{
		Prompt:                region.Prompt,
		TargetDurationSeconds: targetDuration,
		Seed:                  seeds.music,
	})
	if err != nil {
		_ = m.store.SetMusicRegionError(region.ID, err.Error())
		return err
	}
	audio := music
	var ambience []byte
	if region.Ambience != "" {
		ambience, err = musicgen.GenerateRegion(ctx, m.tts, musicgen.Region{
			Prompt:                region.Ambience,
			TargetDurationSeconds: targetDuration,
			Seed:                  seeds.ambience,
		})
		if err != nil {
			_ = m.store.SetMusicRegionError(region.ID, "ambience: "+err.Error())
			return fmt.Errorf("ambience: %w", err)
		}
		audio, err = musicgen.MixAmbience(music, ambience)
		if err != nil {
			_ = m.store.SetMusicRegionError(region.ID, "mix ambience: "+err.Error())
			return fmt.Errorf("mix ambience: %w", err)
		}
	}
	if err := audiopath.EnsureMusicDir(m.dataDir, bookID, chapterID); err != nil {
		_ = m.store.SetMusicRegionError(region.ID, "failed to create music dir: "+err.Error())
		return err
	}
	stems := map[string][]byte{"music": music, "ambience": ambience}
	for stem, data := range stems {
		path := audiopath.MusicRegionStemFile(m.dataDir, bookID, chapterID, region.ID, stem)
		if data == nil {
			_ = os.Remove(path)
			continue
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			_ = m.store.SetMusicRegionError(region.ID, "failed to save "+stem+" stem: "+err.Error())
			return err
		}
	}
	outPath := audiopath.MusicRegionFile(m.dataDir, bookID, chapterID, region.ID)
	if err := os.WriteFile(outPath, audio, 0o644); err != nil {
		_ = m.store.SetMusicRegionError(region.ID, "failed to save audio: "+err.Error())
		return err
	}
	dur, err := wav.Duration(audio)
	if err != nil {
		log.Printf("jobs: parse wav duration for music region %s: %v", region.ID, err)
	}
	if err := m.store.SetMusicRegionReady(region.ID, dur.Seconds()); err != nil {
		log.Printf("jobs: mark music region %s ready: %v", region.ID, err)
	}
	return nil
}

// sfxPreviewSeq is previewSeq's own counterpart for KindSFXPreview - see
// its own doc comment for why a preview always needs a fresh, unique
// dedup key rather than sharing one with anything else.
var sfxPreviewSeq int64

// RunSFXPreview runs fn (a standalone render with no backing paragraph,
// from any of this app's generation-only engines - ACE-Step, Stable Audio
// (Music/SFX/Medium) - see httpapi.handleGenerateSFX, the only
// caller) as a real KindSFXPreview task in poolSFX, blocking the caller
// until it finishes - RunVoiceDesignPreview's own exact shape (see its
// doc comment for why blocking, not fire-and-forget, is the right shape
// for a single "test this prompt" click). engine is purely a label for
// the Jobs dashboard (e.g. "ace_step", "stable_audio_sfx") - this package never
// branches on it; which engine actually runs is entirely up to fn.
func (m *Manager) RunSFXPreview(ctx context.Context, engine, prompt string, fn func(ctx context.Context) ([]byte, error)) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resultCh := make(chan previewOutcome, 1)
	seq := atomic.AddInt64(&sfxPreviewSeq, 1)
	m.pushTask(&task{
		kind:           KindSFXPreview,
		tier:           TierUrgent,
		llmKey:         fmt.Sprintf("%d", seq),
		label:          fmt.Sprintf("%s preview #%d", engine, seq),
		instruct:       prompt,
		runPreview:     fn,
		previewWaiters: []chan previewOutcome{resultCh},
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-resultCh:
		return out.audio, out.err
	}
}

// llmPreviewSeq is sfxPreviewSeq's own counterpart for KindLLMPreview.
var llmPreviewSeq int64

// RunLLMPreview runs fn (a standalone raw system+user prompt call against
// this app's own embedded speaker-attribution GGUF model - see
// httpapi.handleTestLLM, the only caller) as a real KindLLMPreview task in
// poolLLM, blocking the caller until it finishes - RunSFXPreview's own
// exact shape (see its doc comment for why blocking is the right shape
// for a single "test this prompt" click), just carrying a text result
// instead of audio bytes. userPrompt is purely a Jobs-dashboard label
// (mirrors RunSFXPreview's own prompt param) - this package never
// inspects it.
func (m *Manager) RunLLMPreview(ctx context.Context, userPrompt string, fn func(ctx context.Context) (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	resultCh := make(chan previewTextOutcome, 1)
	seq := atomic.AddInt64(&llmPreviewSeq, 1)
	m.pushTask(&task{
		kind:               KindLLMPreview,
		tier:               TierUrgent,
		llmKey:             fmt.Sprintf("%d", seq),
		label:              fmt.Sprintf("llm preview #%d", seq),
		instruct:           userPrompt,
		runPreviewText:     fn,
		previewTextWaiters: []chan previewTextOutcome{resultCh},
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case out := <-resultCh:
		return out.text, out.err
	}
}

// QueueTask is one task as seen from outside the package (httpapi's
// job-queue dashboard) - just the fields needed to identify and display
// it, not the internal dispatch bookkeeping (refText/seed/etc.) task
// itself carries. ParagraphIdx/PresetID/Instruct are meaningless (zero
// value) for the two LLM kinds, which aren't paragraph-scoped - httpapi's
// DTO layer keys its display on Kind. ChapterIdx/ChapterID are also
// meaningless (zero value) for KindSpeakerCharacterization specifically,
// which is character- rather than chapter-scoped; Label carries a
// human-readable identifier for that case instead (see task.label's own
// doc comment) - "" for every other kind, which already has a chapter/
// paragraph to show. PresetID/Instruct are already resolved onto task at
// enqueue time (see pushResolvedTask), so exposing them here is free - no
// extra store lookup needed, unlike book/chapter titles which httpapi
// resolves separately.
type QueueTask struct {
	// ID is t.dedupKey() - already a stable, unique-per-task identifier
	// used internally for dedup (see task.dedupKey's own doc comment), so
	// exposing it verbatim is the natural handle for Cancel/CancelAll and
	// for a frontend to key a dashboard row on, rather than reconstructing
	// an ad hoc composite key from the other, display-oriented fields.
	ID           string
	Kind         Kind
	Label        string
	BookID       string
	ChapterID    string
	ChapterIdx   int
	ParagraphIdx int
	Tier         int    // TierUrgent (-1), TierLookahead (0), TierNormal (1), or TierBackground (2)
	PresetID     string // "" if this voice is a pure custom instruct with no preset backing it
	Instruct     string
	// Attempt counts genuine failures this exact unit of work has already
	// suffered (see task.attempt's own doc comment) - 0 for a task on its
	// first ever dispatch. Deliberately NOT bumped by a cooperative pause
	// (AttributionFunc/DirectionFunc requeuing the rest of a chapter
	// between batches - see taskResult.requeue) - only by a genuine retry
	// after failure (see retryEligible/maxTaskAttempts) - so a reader
	// watching this stay at 0 across many requeues of the same long
	// chapter can tell that's normal batch-by-batch progress, not a stuck
	// retry loop.
	Attempt int
	// Emotion is the emotion id whose voice variant this generation clones
	// from ("" for neutral, or anything that isn't a single emotional line
	// - see taskEmotion), so the dashboard can show "Fritz (Sad)".
	Emotion string
}

// taskEmotion is the emotion t's line generates in, for display: the
// paragraph's effective emotion as of enqueue, under the same conditions
// variantClipDependency/emotionVariant apply one (a preset-backed voice,
// not a scare-quote merge group). A manual override after enqueue
// re-enqueues the line, so this rarely lags the store.
func taskEmotion(t *task) string {
	if t.kind != KindVoiceClone && t.kind != KindVoiceDesign {
		return ""
	}
	if t.presetID == "" || len(t.mergeParagraphs) > 1 {
		return ""
	}
	if e := t.paragraph.EffectiveEmotion(); e != emotions.Neutral {
		return e
	}
	return ""
}

func toQueueTask(t *task, tier int) QueueTask {
	paragraphIdx := t.paragraph.Idx
	if t.kind == KindMusicLiveGeneration {
		// One region per task - report where that region starts.
		paragraphIdx = t.musicRegionStart
	}
	return QueueTask{
		ID: t.dedupKey(), Kind: t.kind, Label: t.label, BookID: t.bookID, ChapterID: t.chapterID,
		ChapterIdx: t.chapterIdx, ParagraphIdx: paragraphIdx, Tier: tier,
		PresetID: t.presetID, Instruct: t.instruct, Attempt: t.attempt,
		Emotion: taskEmotion(t),
	}
}

// Snapshot reports the queue's current state for a job-queue dashboard:
// InFlight is whatever's actually dispatched to a worker slot right now,
// Queued is everything still waiting, ordered by taskqueue.Queue's own
// Snapshot (see its own doc comment for exactly what "ordered" means here
// and the approximation it admits to - popTask's real dispatch behavior
// also depends on which pool is active, each pool's own capacity, and the
// graceful-drain/minimize-transitions rules around switching pools, none
// of which a single sorted list can fully express). InFlight has no
// "what's next" ordering to reflect at all (every entry there is already
// concurrently active, not waiting in line), so it keeps the simple
// readable grouping.
//
// Also merges in pipelineQueue's own current state (toPipelineQueueTask) -
// a book's four preprocessing phases (see EnqueuePipeline) dispatch through
// a wholly separate Queue from m.queue (see poolPipeline's own doc
// comment), but they're still real, cancelable units of work a reader
// should be able to see and cancel from the same Jobs dashboard, not a
// second, invisible tracking mechanism the way the old bare-goroutine
// runBookPipeline was. pipelineQueue's own queued ordering isn't merged
// into qs's - the two Queues have no shared notion of relative priority -
// so a phase row's position in Queued is just "after every m.queue row",
// not evidence of real dispatch order between the two; InFlight's own
// explicit re-sort below still applies uniformly to both, since a phase
// with no ChapterIdx/ParagraphIdx of its own (both zero) sorts predictably
// to the front of its book's own group.
func (m *Manager) Snapshot() (inFlight, queued []QueueTask) {
	qs, ifs := m.queue.Snapshot()
	pqs, pifs := m.pipelineQueue.Snapshot()

	for _, e := range ifs {
		inFlight = append(inFlight, toQueueTask(e.Task.(*task), e.Tier))
	}
	for _, e := range pifs {
		inFlight = append(inFlight, toPipelineQueueTask(e.Task.(*pipelineTask), e.Tier))
	}
	sort.Slice(inFlight, func(i, j int) bool {
		a, b := inFlight[i], inFlight[j]
		if a.Tier != b.Tier {
			return a.Tier < b.Tier
		}
		if a.BookID != b.BookID {
			return a.BookID < b.BookID
		}
		if a.ChapterIdx != b.ChapterIdx {
			return a.ChapterIdx < b.ChapterIdx
		}
		return a.ParagraphIdx < b.ParagraphIdx
	})

	for _, e := range qs {
		queued = append(queued, toQueueTask(e.Task.(*task), e.Tier))
	}
	for _, e := range pqs {
		queued = append(queued, toPipelineQueueTask(e.Task.(*pipelineTask), e.Tier))
	}
	return inFlight, queued
}

// deliverCanceled sends a context.Canceled outcome to every one of t's own
// waiters (llmWaiters/previewWaiters - a no-op for any kind that doesn't
// use them, e.g. KindVoiceClone/KindVoiceDesign/KindSpeakerAttribution/
// KindVoiceProvision, all of which are fire-and-forget) - the queued-task
// half of Cancel/CancelAll's own cancellation delivery. Every channel here
// is buffered (size 1, one send ever - see RunCharacterization/
// RunVoiceDesignPreview), so these sends never block. Only ever called for
// a task pulled straight out of the heap (never dispatched, so
// handleResult will never run for it and can't double-deliver) - an
// in-flight task's cancellation instead flows through the normal
// handleResult path once t.cancel's context is noticed (see task.cancel's
// own doc comment), which already knows how to deliver to these same
// waiters for any other failure.
func deliverCanceled(t *task) {
	for _, w := range t.llmWaiters {
		w <- llmOutcome{err: context.Canceled}
	}
	for _, w := range t.previewWaiters {
		w <- previewOutcome{err: context.Canceled}
	}
	for _, w := range t.previewTextWaiters {
		w <- previewTextOutcome{err: context.Canceled}
	}
}

// Cancel cancels one task by its QueueTask.ID (== task.dedupKey(), or a
// pipelineTask's own Key() - see cancelPipelineTask) - httpapi's DELETE
// /api/jobs/{id}, the Jobs dashboard's per-row "Cancel" button. A
// still-queued task is removed from the heap outright (it never ran at
// all - store state is untouched, same as if it had never been
// enqueued) and its own waiters, if any, are delivered a canceled outcome
// directly (see deliverCanceled). An in-flight task can't be un-dispatched
// - a worker slot is already running it - so this instead cancels its own
// dispatch context (t.cancel) and lets the task's normal completion path
// (handleResult) discover that and clean up exactly as it would for any
// other failure; see task.cancel's own doc comment for why that isn't
// always instant. Falls back to cancelPipelineTask if id doesn't match
// anything in m.queue - a dashboard row's ID alone doesn't say which
// Queue it came from, so this just tries both; the two id spaces can never
// collide (see pipelineKey's own "pipeline:" prefix vs. task.dedupKey's
// plain paragraph/llmKey-based ones). Returns false if id matches nothing
// queued or in flight in either Queue (already finished, or never
// existed).
func (m *Manager) Cancel(id string) bool {
	if t, ok := m.queue.Cancel(func(c taskqueue.Task) bool { return c.Key() == id }); ok {
		m.dropCanceledQueued(t.(*task))
		return true
	}
	// Not queued any more (Cancel above only removes queued work) - if id
	// still names anything at all, it must be in flight instead.
	if found, ok := m.queue.Find(func(c taskqueue.Task) bool { return c.Key() == id }); ok {
		if bt := found.(*task); bt.cancel != nil {
			bt.cancel()
		}
		return true
	}
	return m.cancelPipelineTask(id)
}

// CancelChapter cancels every queued task for chapterID and every in-flight
// one's context, then waits (until ctx is done) for the in-flight ones to
// actually finish - httpapi's single-chapter re-import, which is about to
// replace the chapter's paragraphs and can't have a task still writing
// results keyed by the old paragraphs' idx into the new ones. Keeps
// cancelling while it waits, since a still-running whole-book bulk group
// can push fresh tasks for this chapter in the meantime. Book-level
// pipeline tasks themselves are left alone: whatever they enqueue after
// the replace reads the new content. Returns false if ctx ran out with
// work for chapterID still in flight.
func (m *Manager) CancelChapter(ctx context.Context, chapterID string) bool {
	ofChapter := func(c taskqueue.Task) bool { return c.(*task).chapterID == chapterID }
	for {
		for {
			t, ok := m.queue.Cancel(ofChapter)
			if !ok {
				break
			}
			m.dropCanceledQueued(t.(*task))
		}
		for _, t := range m.queue.InFlightTasks() {
			if bt := t.(*task); bt.chapterID == chapterID && bt.cancel != nil {
				bt.cancel()
			}
		}
		if !m.IsGenerating(chapterID) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// dropCanceledQueued does the bookkeeping for bt, a task just removed from
// m.queue while still queued (taskqueue.Queue.Cancel): chapterPending,
// a change notification, and delivering a canceled outcome to any waiter.
func (m *Manager) dropCanceledQueued(bt *task) {
	m.mu.Lock()
	m.chapterPending[bt.chapterID]--
	if m.chapterPending[bt.chapterID] <= 0 {
		delete(m.chapterPending, bt.chapterID)
	}
	m.mu.Unlock()
	m.notifyChanged()
	deliverCanceled(bt)
}

// CancelAll cancels every queued and in-flight task at once, across both
// m.queue and pipelineQueue - httpapi's DELETE /api/jobs, the Jobs
// dashboard's "Cancel all" button. Exactly Cancel applied to every task
// currently in either queue/in flight, just batched into one pass per
// queue (one lock acquisition, one heap replacement each) rather than one
// Cancel call per id - functionally identical outcome, including the same
// in-flight cancellation caveats (see task.cancel's own doc comment).
// Returns how many tasks were acted on in total (queued removals plus
// in-flight cancellations requested, across both queues) - not a
// guarantee that many are done disappearing yet, since an in-flight
// task's own cancellation still runs its course through handleResult (or,
// for a pipeline phase, pipelineWorker's own completion path).
func (m *Manager) CancelAll() int {
	drained := m.queue.Drain()
	m.mu.Lock()
	for _, t := range drained {
		bt := t.(*task)
		m.chapterPending[bt.chapterID]--
		if m.chapterPending[bt.chapterID] <= 0 {
			delete(m.chapterPending, bt.chapterID)
		}
	}
	m.mu.Unlock()
	if len(drained) > 0 {
		m.notifyChanged()
	}

	inFlightCancels := make([]context.CancelFunc, 0, len(m.queue.InFlightTasks()))
	for _, t := range m.queue.InFlightTasks() {
		if bt := t.(*task); bt.cancel != nil {
			inFlightCancels = append(inFlightCancels, bt.cancel)
		}
	}

	for _, t := range drained {
		deliverCanceled(t.(*task))
	}
	for _, cancel := range inFlightCancels {
		cancel()
	}
	return len(drained) + len(inFlightCancels) + m.cancelAllPipelineTasks()
}

// ErrTaskNotFound is PromoteTier's own error when id doesn't name
// anything currently queued or in flight in either queue - httpapi maps
// this to 404, the same status Cancel's own false return does.
var ErrTaskNotFound = errors.New("jobs: task not found")

// ErrInvalidTier is PromoteTier's own error for a newTier that isn't one
// of TierUrgent/TierLookahead/TierBackground.
var ErrInvalidTier = errors.New("jobs: invalid tier")

// ErrTierNotMoreUrgent is PromoteTier's own error when newTier isn't
// strictly more urgent than the task's current tier - see PromoteTier's
// own doc comment for why this method only ever allows raising urgency,
// never lowering it.
var ErrTierNotMoreUrgent = errors.New("jobs: requested tier is not more urgent than the task's current tier")

// PromoteTier raises task id's own tier (queued or in-flight, an ordinary
// per-item task in m.queue or a preprocessing phase task in
// pipelineQueue - see EnqueuePipeline) to newTier, provided newTier is
// strictly more urgent than its current one - httpapi's PUT
// /api/jobs/{id}/tier, the Jobs dashboard's own per-row priority menu
// ("Bump to Urgent"/"Bump to Lookahead").
//
// Deliberately upgrade-only, never a general "set tier to anything":
// dependency-driven priority promotion (see taskqueue's own "priority
// promotion" section, and Task.Promote's own doc comment - "once
// promoted, a task stays promoted") already relies on a task's tier never
// dropping back down once something else has promoted it urgent on that
// task's behalf; a reader-triggered "set to anything" here could quietly
// violate that invariant for a task something else is still depending on
// being that urgent. Routed through taskqueue.Queue.WithTask (never
// Task.Promote directly - Promote's own doc comment: "never call this
// yourself from outside"), which finds id and holds the queue's lock
// across the read-then-maybe-write, so a concurrent Pop/Snapshot can
// never observe a half-updated tier.
//
// Tries m.queue first, then pipelineQueue - id's own prefix already
// disambiguates which one actually holds it (see pipelineKey), but this
// has no reason to duplicate that parsing when trying both in turn is
// just as cheap and one less thing to keep in sync if either key format
// ever changes. Returns ErrInvalidTier for a newTier that isn't one of
// TierUrgent/TierLookahead/TierNormal/TierBackground, ErrTaskNotFound if id matches
// nothing in either queue, or ErrTierNotMoreUrgent if it does but newTier
// isn't actually more urgent than what it already has.
func (m *Manager) PromoteTier(id string, newTier int) error {
	if newTier != TierUrgent && newTier != TierLookahead && newTier != TierNormal && newTier != TierBackground {
		return ErrInvalidTier
	}

	var found, applied bool
	var promoted *pipelineTask
	mutate := func(c taskqueue.Task) {
		found = true
		if newTier < c.Tier() {
			c.Promote(newTier)
			applied = true
			if pt, ok := c.(*pipelineTask); ok {
				promoted = pt
			}
		}
	}
	pred := func(c taskqueue.Task) bool { return c.Key() == id }

	if !m.queue.WithTask(pred, mutate) {
		m.pipelineQueue.WithTask(pred, mutate)
	}
	if !found {
		return ErrTaskNotFound
	}
	if !applied {
		return ErrTierNotMoreUrgent
	}
	if promoted != nil {
		if f := promoted.children(); f != nil {
			m.cascadePromotion(promoted.bookID, *f, newTier)
		}
	}
	m.notifyChanged()
	return nil
}

// cascadePromotion is PromoteTier's own follow-up for a promoted pipeline
// task (a preprocessing phase or a bulk group): bumping the pipeline task
// itself only reprioritizes poolPipeline's own bookkeeping, which barely
// matters (maxPipelineInFlight is generous, and pipelineTask.Less never
// distinguishes two of them) - the real work a reader wants sped up is
// whatever m.queue tasks its fan-out already dispatched, which stay at
// whatever tier they were dispatched at (see PipelinePhaseFunc's own doc
// comment on its tier parameter) without this. f (see pipelineTask.children)
// picks those children out; every match gets the same bump, including a
// paused chapter's detached continuation the pipeline task's own live
// tier() can't otherwise reach.
//
// A child's own blockers (e.g. a clone task's speechDirectionDependency)
// need no cascade here: taskqueue's dependency-tier propagation already
// relaxes a blocker's effective tier to match whatever depends on it.
func (m *Manager) cascadePromotion(bookID string, f ChildFilter, newTier int) {
	m.queue.WithEachTask(
		func(c taskqueue.Task) bool {
			ct, ok := c.(*task)
			return ok && ct.bookID == bookID && f.matches(ct)
		},
		func(c taskqueue.Task) {
			if newTier < c.Tier() {
				c.Promote(newTier)
			}
		},
	)
}

// startTask dispatches t into a just-freed worker slot without waiting for
// the result, branching on poolFor(t.kind): the two LLM kinds run
// t.runLLM directly (see startLLMTask), everything else marks the
// paragraph generating and fires its generation call (see
// startGenerationTask).
func (m *Manager) startTask(ctx context.Context, t *task) chan taskResult {
	if poolFor(t.kind) == poolLLM {
		return m.startLLMTask(ctx, t)
	}
	return m.startGenerationTask(ctx, t)
}

func (m *Manager) startLLMTask(ctx context.Context, t *task) chan taskResult {
	ch := make(chan taskResult, 1)
	// KindLLMPreview has no backing chapter/character at all - none of the
	// attribution-specific runLLM/requeue machinery below applies, just run
	// the caller-supplied raw prompt call directly (see startGenerationTask's
	// own early KindVoiceDesignPreview/KindSFXPreview branches for the same
	// shape on the generation side).
	if t.kind == KindLLMPreview {
		go func() {
			text, err := t.runPreviewText(ctx)
			ch <- taskResult{text: text, err: err}
		}()
		return ch
	}
	go func() {
		attributed, requeue, err := t.runLLM(ctx)
		ch <- taskResult{attributed: attributed, requeue: requeue, err: err}
	}()
	return ch
}

func (m *Manager) startGenerationTask(ctx context.Context, t *task) chan taskResult {
	ch := make(chan taskResult, 1)
	// KindVoiceDesignPreview has no backing paragraph at all - none of the
	// store bookkeeping below applies (see handleResult's own early
	// branch for this kind), just run the caller-supplied render directly.
	if t.kind == KindVoiceDesignPreview {
		go func() {
			audio, err := t.runPreview(ctx)
			ch <- taskResult{audio: audio, err: err}
		}()
		return ch
	}
	// KindVoiceProvision likewise has no backing paragraph - see
	// EnqueueVoiceProvision's own doc comment.
	if t.kind == KindVoiceProvision {
		go func() {
			presetID, err := t.runProvision(ctx, t.attempt)
			ch <- taskResult{presetID: presetID, err: err}
		}()
		return ch
	}
	// KindSFXPreview has no backing paragraph either - same shape as
	// KindVoiceDesignPreview above, just under its own kind/pool identity.
	if t.kind == KindSFXPreview {
		go func() {
			audio, err := t.runPreview(ctx)
			ch <- taskResult{audio: audio, err: err}
		}()
		return ch
	}
	// KindSFXGeneration does carry a backing paragraph (for dedup and the
	// Jobs dashboard - see EnqueueSFXGeneration), but none of the
	// narration-specific store bookkeeping below applies to it: the
	// caller-supplied closure does its own persistence entirely (see
	// runSFXGen's own doc comment).
	if t.kind == KindSFXGeneration {
		go func() {
			err := t.runSFXGen(ctx, t.attempt)
			ch <- taskResult{err: err}
		}()
		return ch
	}
	// KindMusicGeneration is runSFXGen's own exact sibling shape, just
	// under its own runMusicGen field - see EnqueueMusicGeneration's own
	// doc comment.
	if t.kind == KindMusicGeneration || t.kind == KindMusicLiveGeneration {
		go func() {
			if t.kind == KindMusicLiveGeneration {
				// Queue the next region now, while this one generates,
				// so it dispatches the moment this finishes.
				m.continueLiveMusic(t)
			}
			err := t.runMusicGen(ctx, t.attempt)
			ch <- taskResult{err: err}
		}()
		return ch
	}
	// KindLengthEstimate has no backing paragraph either - t.paragraph.ID
	// is "" (see EnqueueLengthEstimate), so SetParagraphGenerating/publish
	// below would write/broadcast nothing meaningful (SetParagraphGenerating
	// would just match zero rows, and publishing a ParagraphUpdate at
	// t.chapterIdx/t.paragraph.Idx - both 0 - would misleadingly look like
	// that chapter's real paragraph 0 started generating). generate()
	// itself still runs exactly as it would for a real KindVoiceClone task.
	if t.kind == KindLengthEstimate {
		go func() {
			audio, _, err := m.generate(ctx, t)
			ch <- taskResult{audio: audio, err: err}
		}()
		return ch
	}
	// For a scare-quote merge group (see pushResolvedTask), mark/publish
	// every member as generating, not just the anchor t.paragraph - all of
	// them are about to be produced by the same single TTS call, and
	// leaving the others looking "pending" until the merged result splits
	// would be a visibly stale/misleading status in the meantime.
	generating := []store.Paragraph{t.paragraph}
	if len(t.mergeParagraphs) > 1 {
		generating = t.mergeParagraphs
	}
	for _, p := range generating {
		if err := m.store.SetParagraphGenerating(p.ID, t.voiceID); err != nil {
			log.Printf("jobs: mark generating: %v", err)
		}
	}
	go func() {
		audio, words, err := m.generate(ctx, t)
		ch <- taskResult{audio: audio, words: words, err: err}
	}()
	return ch
}

// generate resolves t into audio, dispatching on t.kind (set once at
// creation - see pushResolvedTask): KindVoiceClone clones t.presetID's
// stable reference clip via voicerefs.EnsureFile; KindVoiceDesign is a fully
// custom instruct with no preset backing it at all, rendered straight via
// VoiceDesign instead, mirroring the old tts-service /synthesize's own
// no-clone-prompt fallback.
//
// EnsureFile still has its own cache-miss fallback that renders the clip via
// ttsworker.Manager.Design (and this call runs *inside* an
// already-dispatched poolGeneration task), but by the time a KindVoiceClone
// task actually reaches this code that fallback is dead code in practice:
// referenceClipDependency makes "the preset's reference clip exists" a
// tracked task dependency (see resolveDependencies), so Pop never dispatches
// a KindVoiceClone task whose clip is missing in the first place - it
// instead dispatches (and, via priority promotion, prioritizes) a
// KindVoiceProvision task to render the clip first, and only dispatches the
// clone task once that dependency clears. That's what closes the deadlock
// an earlier version of this hit: that version routed the cache-miss
// ensure-ref-clip step through a second, nested jobs.Manager.
// RunVoiceProvision call, which was safe for a TierBackground outer
// KindVoiceClone task but a real, confirmed-live deadlock for a
// TierUrgent/TierLookahead one - the outer task could never be canceled to
// let its own nested child dispatch (this package never force-cancels
// in-flight work at all - see taskqueue.Queue's own "one pool at a time"
// doc comment), and the child could never dispatch while the outer task
// kept poolGeneration active, a standoff only an explicit manual Cancel
// could break. The dependency system fixes this structurally instead of
// serializing around it: the clone task simply never occupies a pool slot
// until its dependency is resolved, so there's nothing left to deadlock.
// mergedGenerationText joins a scare-quote merge group's own members'
// ResolveGenerationText into the single string one TTS call renders for
// the whole group - see generate/handleMergedResult. Plain single-space
// joining, not a byte-exact reconstruction of the original source
// paragraph epub.splitQuoteSegments split apart (trailing/leading
// whitespace was already trimmed off each segment at import time) - fine
// here, since the goal is one naturally-flowing sentence for the TTS
// engine to render, not recovering the original markup.
func mergedGenerationText(members []store.Paragraph, cloneModel string) string {
	texts := make([]string, len(members))
	for i, p := range members {
		texts[i] = p.ResolveGenerationText(cloneModel)
	}
	return strings.Join(texts, " ")
}

func (m *Manager) generate(ctx context.Context, t *task) ([]byte, []ttsproto.Word, error) {
	if t.kind == KindVoiceDesign {
		var seed *int64
		// 0 means no seed anchor (see httpapi.resolveVoiceSeed) - leave nil
		// so the worker picks one at random rather than literally seeding
		// with 0.
		if t.seed != 0 {
			v := int64(t.seed)
			seed = &v
		}
		text := t.paragraph.Text
		if len(t.mergeParagraphs) > 1 {
			text = mergedGenerationText(t.mergeParagraphs, "")
		}
		audio, err := m.tts.Design(ctx, text, t.instruct, t.language, t.designModel, seed)
		return audio, nil, err
	}

	cloneModel := t.cloneModel
	if cloneModel == "" {
		cloneModel = voices.DefaultCloneModel
	}
	refPath, err := voicerefs.EnsureFile(ctx, m.tts, m.dataDir, t.presetID, t.instruct, t.seed, t.refText, t.speedMultiplier, t.designModel)
	if err != nil {
		return nil, nil, fmt.Errorf("ensure reference clip: %w", err)
	}
	refText := t.refText
	if path, line, ok := m.emotionVariant(t); ok {
		refPath, refText = path, line
	}
	refAudio, err := os.ReadFile(refPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read reference clip: %w", err)
	}
	// t.paragraph.ResolveGenerationText applies pronunciation/emphasis
	// substitutions (and, for Higgs, pause tags) to t.paragraph.Text, or
	// returns it unchanged if there are none (the common case) - only the
	// text actually sent for cloning. Deliberately NOT applied to t.paragraph.Text itself:
	// alignParagraph (see its own call site) uses that field directly for
	// forced alignment/word-timing, which must stay the plain spoken words -
	// an inserted tag token there would show up as a spurious "word" with no
	// corresponding audio.
	//
	// For a scare-quote merge group (t.mergeParagraphs, len > 1), the same
	// treatment is applied to every member and joined into one string - one
	// TTS call renders the whole group together, later split back into each
	// member's own audio file by handleMergedResult.
	//
	// alignText is what the completeness check (generateCloneChecked)
	// aligns against - the same text each result path would otherwise
	// align itself (alignParagraph/handleMergedResult), so the check's own
	// word timings are reused rather than aligning twice. A length
	// estimate's throwaway sample is never saved, so it skips the check.
	text := t.paragraph.ResolveGenerationText(cloneModel)
	alignText := t.paragraph.Text
	if len(t.mergeParagraphs) > 1 {
		text = mergedGenerationText(t.mergeParagraphs, cloneModel)
		alignText = text
	}
	if t.kind == KindLengthEstimate {
		alignText = ""
	}
	return m.generateCloneChecked(ctx, cloneModel, refAudio, refText, t.language, text, t.cloneInstruct, alignText)
}

// emotionVariant returns the emotion-variant reference clip (and the line
// it speaks, its transcript) t should clone from instead of its preset's
// base clip, if t's paragraph currently has an effective emotion whose
// variant has been rendered (see variantClipDependency) - re-read from the
// store rather than trusted from t.paragraph, since a manual override can
// land between enqueue and dispatch. ok is false for a neutral line, a
// scare-quote merge group, or a variant that failed to render (the base
// clip is used then).
func (m *Manager) emotionVariant(t *task) (path, refLine string, ok bool) {
	if t.presetID == "" || len(t.mergeParagraphs) > 1 {
		return "", "", false
	}
	p, err := m.store.GetParagraph(t.paragraph.ID)
	if err != nil || p == nil {
		return "", "", false
	}
	e, known := emotions.Get(p.EffectiveEmotion())
	if !known || !voicerefs.VariantExists(m.dataDir, t.presetID, e.ID) {
		return "", "", false
	}
	return audiopath.VoicePresetVariantFile(m.dataDir, t.presetID, e.ID), e.RefLine, true
}

func (m *Manager) handleResult(t *task, result taskResult) {
	// Not deferred: result.requeue (below) pushes a follow-up task under
	// this same task's own dedupKey, which needs finishTask to have
	// already cleared this one out of the queue first - pushTask's
	// own dedup would otherwise see this task still "in flight" (true right
	// up until finishTask runs) and silently merge the follow-up into it
	// instead of actually queuing anything, since this task is seconds away
	// from finishing with exactly the paused/partial result that follow-up
	// is meant to complete.
	m.finishTask(t)
	m.maybeRecoverWorker(result.err)
	if t.kind == KindLengthEstimate {
		// Best-effort, throwaway: no waiters, no paragraph, nothing to
		// retry for - a failed or implausible sample just leaves
		// Book.EstimateSecPerChar at its previous value (0, on a book's
		// first upload), and estimateTotalSeconds's own fallback tiering
		// already handles that.
		if result.err != nil {
			log.Printf("jobs: length estimate for book %s: %v", t.bookID, result.err)
			return
		}
		dur, err := wav.Duration(result.audio)
		if err != nil || dur <= 0 || t.estimateChars == 0 {
			log.Printf("jobs: length estimate for book %s: parse wav duration: %v", t.bookID, err)
			return
		}
		if err := m.store.SetLengthEstimate(t.bookID, dur.Seconds()/float64(t.estimateChars)); err != nil {
			log.Printf("jobs: length estimate for book %s: save: %v", t.bookID, err)
		}
		return
	}
	if t.kind == KindVoiceDesignPreview {
		// A genuine (non-preempted) failure gets retried via the queue
		// itself - see maxTaskAttempts/task.requeueCopy - before ever
		// delivering to previewWaiters: RunVoiceDesignPreview's caller is
		// blocked on exactly one outcome, so retrying has to happen before
		// that single delivery, not race it.
		if retryEligible(t, result.err) {
			log.Printf("jobs: voice design preview failed (attempt %d/%d), retrying via queue: %v", t.attempt+1, maxTaskAttempts, result.err)
			m.pushTask(t.requeueCopy())
			return
		}
		// Same delivery pattern as the poolLLM branch below, just carrying
		// audio bytes (previewWaiters/previewOutcome) instead of an
		// attributed count (llmWaiters/llmOutcome) - see RunVoiceDesignPreview.
		m.mu.Lock()
		waiters := t.previewWaiters
		m.mu.Unlock()
		outcome := previewOutcome{audio: result.audio, err: result.err}
		for _, waiter := range waiters {
			waiter <- outcome
		}
		if result.err != nil {
			log.Printf("jobs: voice design preview failed: %v", result.err)
		}
		return
	}
	if t.kind == KindSFXPreview {
		// Same retry-before-delivery reasoning as the design-preview
		// branch above - RunSFXPreview's caller is blocked on exactly one
		// outcome.
		if retryEligible(t, result.err) {
			log.Printf("jobs: %s failed (attempt %d/%d), retrying via queue: %v", t.kind, t.attempt+1, maxTaskAttempts, result.err)
			m.pushTask(t.requeueCopy())
			return
		}
		m.mu.Lock()
		waiters := t.previewWaiters
		m.mu.Unlock()
		outcome := previewOutcome{audio: result.audio, err: result.err}
		for _, waiter := range waiters {
			waiter <- outcome
		}
		if result.err != nil {
			log.Printf("jobs: %s failed: %v", t.kind, result.err)
		}
		return
	}
	if t.kind == KindSFXGeneration {
		if retryEligible(t, result.err) {
			log.Printf("jobs: sfx generation task %s failed (attempt %d/%d), retrying via queue: %v", t.dedupKey(), t.attempt+1, maxTaskAttempts, result.err)
			m.pushTask(t.requeueCopy())
			return
		}
		m.mu.Lock()
		waiters := t.sfxGenWaiters
		m.mu.Unlock()
		for _, waiter := range waiters {
			waiter <- result.err
		}
		if result.err != nil {
			log.Printf("jobs: sfx generation task %s failed: %v", t.dedupKey(), result.err)
		}
		return
	}
	if t.kind == KindMusicGeneration || t.kind == KindMusicLiveGeneration {
		if retryEligible(t, result.err) {
			log.Printf("jobs: music generation task %s failed (attempt %d/%d), retrying via queue: %v", t.dedupKey(), t.attempt+1, maxTaskAttempts, result.err)
			m.pushTask(t.requeueCopy())
			return
		}
		m.mu.Lock()
		waiters := t.musicGenWaiters
		m.mu.Unlock()
		for _, waiter := range waiters {
			waiter <- result.err
		}
		if result.err != nil {
			log.Printf("jobs: music generation task %s failed: %v", t.dedupKey(), result.err)
		}
		// Whether this batch just succeeded or permanently failed, every
		// region it reached is no longer AudioPending/AudioGenerating
		// (generateMusicRegion itself sets Ready/Error per region as it
		// goes, before this ever runs) - advance to whatever comes after
		// them in this chapter now, the same way a paragraph finishing does
		// (saveParagraphAudio/handleMergedResult).
		m.MaybeAdvanceChapterMusic(t.bookID, t.chapterID)
		return
	}
	if t.kind == KindVoiceProvision {
		if retryEligible(t, result.err) {
			log.Printf("jobs: voice provision task %s failed (attempt %d/%d), retrying via queue: %v", t.dedupKey(), t.attempt+1, maxTaskAttempts, result.err)
			m.pushTask(t.requeueCopy())
			return
		}
		// Deliver to any RunVoiceProvision callers currently waiting
		// (usually none - EnqueueVoiceProvision's own fire-and-forget
		// callers never populate this - see provisionWaiters' own doc
		// comment) - buffered sends, same no-block-on-a-stopped-caller
		// reasoning as the preview/LLM branches above.
		m.mu.Lock()
		waiters := t.provisionWaiters
		m.mu.Unlock()
		outcome := provisionOutcome{presetID: result.presetID, err: result.err}
		for _, waiter := range waiters {
			waiter <- outcome
		}
		if result.err != nil {
			log.Printf("jobs: voice provision task %s failed: %v", t.dedupKey(), result.err)
		}
		return
	}
	if t.kind == KindLLMPreview {
		// Same retry-before-delivery reasoning as the preview/provision
		// branches above - RunLLMPreview's caller is blocked on exactly one
		// outcome.
		if retryEligible(t, result.err) {
			log.Printf("jobs: llm preview failed (attempt %d/%d), retrying via queue: %v", t.attempt+1, maxTaskAttempts, result.err)
			m.pushTask(t.requeueCopy())
			return
		}
		m.mu.Lock()
		waiters := t.previewTextWaiters
		m.mu.Unlock()
		outcome := previewTextOutcome{text: result.text, err: result.err}
		for _, waiter := range waiters {
			waiter <- outcome
		}
		if result.err != nil {
			log.Printf("jobs: llm preview failed: %v", result.err)
		}
		return
	}
	if poolFor(t.kind) == poolLLM {
		// Same retry-before-delivery reasoning as the preview/provision
		// branches above - a cooperative pause (non-nil result.requeue)
		// isn't a failure at all, so only a genuine failure with budget
		// remaining takes this path.
		if result.requeue == nil && retryEligible(t, result.err) {
			log.Printf("jobs: %s task %s failed (attempt %d/%d), retrying via queue: %v", t.kind, t.dedupKey(), t.attempt+1, maxTaskAttempts, result.err)
			m.pushTask(t.requeueCopy())
			return
		}
		// Deliver the real result to every RunCharacterization caller
		// currently waiting on this task (usually one, more if a duplicate
		// call joined it - see llmWaiters' own doc comment) - buffered
		// sends, never blocking even on a caller whose ctx was already
		// cancelled (see RunCharacterization's own comment). A no-op for a
		// KindSpeakerAttribution task, which never has any waiters (see
		// EnqueueAttribution).
		m.mu.Lock()
		waiters := t.llmWaiters
		m.mu.Unlock()
		outcome := llmOutcome{attributed: result.attributed, err: result.err}
		for _, waiter := range waiters {
			waiter <- outcome
		}
		if result.err != nil {
			log.Printf("jobs: %s task %s failed permanently after %d attempt(s): %v", t.kind, t.dedupKey(), t.attempt+1, result.err)
		}
		if result.requeue != nil {
			result.requeue()
		}
		return
	}
	if result.err != nil {
		// A merged group's own combined generation call failing (e.g. the
		// worker chokes on the longer combined text) falls back straight to
		// generating every member independently, exactly today's ordinary
		// path, rather than retrying the same merged call - merging is
		// purely an audio-quality optimization, never something a
		// paragraph's own audio correctness should depend on succeeding.
		if len(t.mergeParagraphs) > 1 {
			log.Printf("jobs: merged generation for chapter %s failed (%d paragraphs), falling back to independent generation: %v", t.chapterID, len(t.mergeParagraphs), result.err)
			m.generateIndependently(t)
			return
		}
		if retryEligible(t, result.err) {
			log.Printf("jobs: synthesize paragraph %s failed (attempt %d/%d), retrying via queue: %v", t.paragraph.ID, t.attempt+1, maxTaskAttempts, result.err)
			m.pushTask(t.requeueCopy())
			return
		}
		m.failParagraph(t, t.paragraph, result.err)
		return
	}
	if len(t.mergeParagraphs) > 1 {
		m.handleMergedResult(t, result.audio, result.words)
		return
	}
	m.saveParagraphAudio(t, t.paragraph, result.audio, result.words)
}

// failParagraph marks one paragraph AudioError and publishes it - shared by
// the ordinary single-paragraph failure path and generateIndependently's
// own per-paragraph fallback loop.
func (m *Manager) failParagraph(t *task, paragraph store.Paragraph, err error) {
	log.Printf("jobs: synthesize paragraph %s: %v", paragraph.ID, err)
	_ = m.store.SetParagraphError(paragraph.ID, t.voiceID, err.Error())
}

// saveParagraphAudio persists one paragraph's own finished clip - file on
// disk, ready status/duration, published update, detached word-alignment -
// shared by the ordinary single-paragraph success path, handleMergedResult
// (once per split-out clip), and generateIndependently's own fallback loop,
// so every way a paragraph's audio can end up on disk goes through exactly
// one write/publish/error-handling path. words, when non-nil, are audio's
// own already-computed word timings (generateCloneChecked's), saved
// directly instead of running alignParagraph.
func (m *Manager) saveParagraphAudio(t *task, paragraph store.Paragraph, audio []byte, words []ttsproto.Word) {
	outPath := audiopath.ParagraphFile(m.dataDir, t.bookID, t.chapterID, t.voiceID, paragraph.Idx)
	if err := os.WriteFile(outPath, audio, 0o644); err != nil {
		log.Printf("jobs: write audio file: %v", err)
		_ = m.store.SetParagraphError(paragraph.ID, t.voiceID, "failed to save audio: "+err.Error())
		return
	}
	dur, err := wav.Duration(audio)
	if err != nil {
		log.Printf("jobs: parse wav duration for %s: %v", paragraph.ID, err)
	}
	if err := m.store.SetParagraphReady(paragraph.ID, t.voiceID, dur.Seconds()); err != nil {
		log.Printf("jobs: mark ready: %v", err)
	}
	// This paragraph's own narration may be the last piece some chapter
	// tone region needed before it can start generating - see
	// MaybeAdvanceChapterMusic's own doc comment. Cheap no-op for the
	// overwhelming common case (MusicEnabled false).
	m.MaybeAdvanceChapterMusic(t.bookID, t.chapterID)

	// Word-level alignment is a separate, best-effort step against
	// tts-service's own generation lock (see its POST /align) - it runs
	// detached rather than inline here so a slow/failed alignment never
	// delays audio becoming playable or blocks this worker goroutine from
	// picking up the next result. Re-aligns paragraph.Text directly against
	// this specific clip regardless of whether it came from a lone
	// generation call or a split-out slice of a merged one - a merged
	// clip's own split-out piece is by this point just an ordinary,
	// independent wav file on disk, so there's no merged-alignment offset
	// bookkeeping to carry through here.
	if words != nil {
		m.saveWordTimings(t, paragraph, words)
		return
	}
	go m.alignParagraph(t, paragraph, audio, dur.Seconds())
}

// alignParagraph fetches word-level timing for a just-generated paragraph
// and, if successful, saves it and pushes a follow-up ParagraphUpdate
// carrying the same full ready-state (status/duration/URL) plus Words -
// resending the unchanged fields rather than an alignment-only patch, so
// the frontend's straightforward field-overwrite merge (useBookUpdates.ts)
// can't accidentally blank them out. Failure is logged and otherwise
// silent: the paragraph stays playable, just without real word timing -
// ParagraphText.tsx falls back to its character-position estimate.
func (m *Manager) alignParagraph(t *task, paragraph store.Paragraph, audio []byte, durationSeconds float64) {
	words, err := m.alignWithSplit(m.ctx, paragraph.Text, audio, t.language)
	if err != nil {
		log.Printf("jobs: align paragraph %s: %v", paragraph.ID, err)
		return
	}
	m.saveWordTimings(t, paragraph, words)
}

// saveWordTimings persists one paragraph's own word timings.
func (m *Manager) saveWordTimings(t *task, paragraph store.Paragraph, words []ttsproto.Word) {
	wordsJSON, err := json.Marshal(words)
	if err != nil {
		log.Printf("jobs: marshal word timings for %s: %v", paragraph.ID, err)
		return
	}
	if err := m.store.SetParagraphWordTimings(paragraph.ID, t.voiceID, string(wordsJSON)); err != nil {
		log.Printf("jobs: save word timings for %s: %v", paragraph.ID, err)
		return
	}
}

// handleMergedResult saves mergedAudio - one shared clip generate already
// rendered for the whole scare-quote merge group in a single TTS call (see
// generate/mergedGenerationText) - as the ONE physical audio file for the
// group, written under the anchor paragraph's own path
// (t.mergeParagraphs[0]). Every other member gets no file of its own at
// all: Store.SetParagraphReadyPointer records where its own content starts
// within the anchor's file (in seconds, from one shared forced-alignment
// call over the whole merged text/audio - see alignMergedAudio), and
// httpapi resolves its audioUrl straight to the anchor's, so a scare quote
// (see store.Paragraph.ScareQuote) plays in the same breath as the
// narration around it with the underlying recording never touched, cut, or
// re-encoded at all.
//
// This deliberately replaces an earlier version that decoded, sliced, and
// re-encoded a separate file per member at each alignment-derived
// boundary - a real, observed source of audible artifacts: forced-
// alignment word timing is precise enough for karaoke-highlighting (being
// off by a few tens of milliseconds doesn't matter there) but not for
// actually splicing audio, where the same slop produces an audible click
// or clips real speech. Pointing every member at one untouched recording
// instead has no cut to get slightly wrong in the first place.
//
// Falls back to generateIndependently - today's ordinary one-call-per-
// paragraph path, each member with its own real file and its own ordinary
// alignment - if alignment fails, or if the aligner's own word count
// doesn't match mergedGenerationText's own simple whitespace tokenization
// closely enough to locate every member's own boundary with confidence:
// merging is an audio-quality optimization, never something a paragraph's
// own audio correctness should depend on.
func (m *Manager) handleMergedResult(t *task, mergedAudio []byte, precomputed []ttsproto.Word) {
	cloneModel := t.cloneModel
	if cloneModel == "" {
		cloneModel = voices.DefaultCloneModel
	}
	texts := make([]string, len(t.mergeParagraphs))
	for i, p := range t.mergeParagraphs {
		texts[i] = p.ResolveGenerationText(cloneModel)
	}
	mergedText := strings.Join(texts, " ")

	words, ok := m.alignMergedAudio(t, texts, mergedText, mergedAudio, precomputed)
	if !ok {
		log.Printf("jobs: could not align merged clip for chapter %s (%d paragraphs); regenerating independently", t.chapterID, len(t.mergeParagraphs))
		m.generateIndependently(t)
		return
	}

	anchor := t.mergeParagraphs[0]
	outPath := audiopath.ParagraphFile(m.dataDir, t.bookID, t.chapterID, t.voiceID, anchor.Idx)
	if err := os.WriteFile(outPath, mergedAudio, 0o644); err != nil {
		log.Printf("jobs: write merged audio file: %v", err)
		for _, p := range t.mergeParagraphs {
			m.failParagraph(t, p, fmt.Errorf("failed to save audio: %w", err))
		}
		return
	}
	totalSeconds := 0.0
	if dur, err := wav.Duration(mergedAudio); err != nil {
		log.Printf("jobs: parse wav duration for merged clip (chapter %s): %v", t.chapterID, err)
	} else {
		totalSeconds = dur.Seconds()
	}

	wordIdx := 0
	for i, p := range t.mergeParagraphs {
		n := len(strings.Fields(texts[i]))
		memberWords := words[wordIdx : wordIdx+n]
		wordIdx += n

		// The anchor owns everything from the very top of the file
		// (offset 0), even if the first spoken word starts a fraction of a
		// second in (leading silence) - every other member starts exactly
		// where its own first word does.
		start := 0.0
		if i > 0 && len(memberWords) > 0 {
			start = memberWords[0].Start
		}
		end := totalSeconds
		if i < len(t.mergeParagraphs)-1 {
			if next := words[wordIdx:]; len(next) > 0 {
				end = next[0].Start
			}
		}
		duration := end - start

		wordsJSON, err := json.Marshal(memberWords)
		if err != nil {
			log.Printf("jobs: marshal word timings for %s: %v", p.ID, err)
			wordsJSON = []byte("[]")
		}

		if i == 0 {
			if err := m.store.SetParagraphReady(p.ID, t.voiceID, duration); err != nil {
				log.Printf("jobs: mark ready: %v", err)
			}
		} else if err := m.store.SetParagraphReadyPointer(p.ID, t.voiceID, i, start, duration); err != nil {
			log.Printf("jobs: mark ready (pointer): %v", err)
		}
		if err := m.store.SetParagraphWordTimings(p.ID, t.voiceID, string(wordsJSON)); err != nil {
			log.Printf("jobs: save word timings for %s: %v", p.ID, err)
		}
	}
	// Same reasoning as saveParagraphAudio's own identical call - any of
	// this group's members finishing may be the last piece some chapter
	// tone region needed.
	m.MaybeAdvanceChapterMusic(t.bookID, t.chapterID)
}

// alignMergedAudio runs forced alignment once over the whole merged
// (mergedText, mergedAudio) - every member's own word timings and pointer
// boundary are sliced from this single result (handleMergedResult) rather
// than each being separately re-aligned against its own short text, which
// would be a genuine mismatch once that text no longer matches the shared,
// longer audio it's now part of.
//
// Returns ok=false - safe to treat exactly like a generation failure,
// never a partial/best-guess result - whenever the result can't be sliced
// per member with confidence: alignment itself failing, or the aligner
// returning a different number of words than strings.Fields(mergedText)
// (its own tokenization can legitimately diverge from a plain whitespace
// split - e.g. splitting a contraction, or dropping a stray punctuation-
// only token - and this is the one case with no graceful degradation,
// since a boundary word is genuinely unlocatable at that point).
func (m *Manager) alignMergedAudio(t *task, texts []string, mergedText string, mergedAudio []byte, precomputed []ttsproto.Word) ([]ttsproto.Word, bool) {
	words := precomputed
	if words == nil {
		var err error
		words, err = m.alignWithSplit(m.ctx, mergedText, mergedAudio, t.language)
		if err != nil {
			log.Printf("jobs: align merged clip for chapter %s: %v", t.chapterID, err)
			return nil, false
		}
	}
	wantWords := 0
	for _, txt := range texts {
		wantWords += len(strings.Fields(txt))
	}
	if wantWords == 0 || len(words) != wantWords {
		return nil, false
	}
	return words, true
}

// generateIndependently is handleMergedResult's own safety fallback (also
// used directly when the merged generation call itself fails) - renders
// and saves every member of t.mergeParagraphs one at a time, exactly the
// same generateClone/saveParagraphAudio path a non-merged task would use,
// so a merge/split failure degrades to "no merging happened" rather than
// ever leaving a paragraph without audio. Runs synchronously, inline in
// handleResult, rather than pushing fresh single-paragraph tasks back onto
// the queue: a requeue would need its own mechanism to guarantee it never
// re-attempts the same merge (scareQuoteMergeGroup is a pure function of
// stored paragraph state, which hasn't changed) - a real but rare extra
// slow step here (a handful of TTS calls) is a simpler, safer trade than
// that bookkeeping for a failure path this infrequent.
func (m *Manager) generateIndependently(t *task) {
	cloneModel := t.cloneModel
	if cloneModel == "" {
		cloneModel = voices.DefaultCloneModel
	}
	if t.kind == KindVoiceDesign {
		var seed *int64
		if t.seed != 0 {
			v := int64(t.seed)
			seed = &v
		}
		for _, p := range t.mergeParagraphs {
			audio, err := m.tts.Design(m.ctx, p.Text, t.instruct, t.language, t.designModel, seed)
			if err != nil {
				m.failParagraph(t, p, err)
				continue
			}
			m.saveParagraphAudio(t, p, audio, nil)
		}
		return
	}
	refPath, err := voicerefs.EnsureFile(m.ctx, m.tts, m.dataDir, t.presetID, t.instruct, t.seed, t.refText, t.speedMultiplier, t.designModel)
	if err != nil {
		for _, p := range t.mergeParagraphs {
			m.failParagraph(t, p, fmt.Errorf("ensure reference clip: %w", err))
		}
		return
	}
	refAudio, err := os.ReadFile(refPath)
	if err != nil {
		for _, p := range t.mergeParagraphs {
			m.failParagraph(t, p, fmt.Errorf("read reference clip: %w", err))
		}
		return
	}
	for _, p := range t.mergeParagraphs {
		text := p.ResolveGenerationText(cloneModel)
		audio, words, err := m.generateCloneChecked(m.ctx, cloneModel, refAudio, t.refText, t.language, text, t.cloneInstruct, p.Text)
		if err != nil {
			m.failParagraph(t, p, err)
			continue
		}
		m.saveParagraphAudio(t, p, audio, words)
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

// skipResolvedBeforeIdx drops paragraphs before fromIdx. resolved is
// already ordered by Idx ascending (paragraphsNeedingGeneration), so this
// is just finding the first one at or past fromIdx.
func skipResolvedBeforeIdx(resolved []resolvedParagraph, fromIdx int) []resolvedParagraph {
	for i, rp := range resolved {
		if rp.paragraph.Idx >= fromIdx {
			return resolved[i:]
		}
	}
	return nil
}

// limitResolvedByWordCount returns a prefix of resolved covering at least
// wordLimit words, including whichever whole paragraph crosses that
// threshold (never cuts a paragraph short - generation is per-paragraph).
func limitResolvedByWordCount(resolved []resolvedParagraph, wordLimit int) []resolvedParagraph {
	total := 0
	for i, rp := range resolved {
		total += len(strings.Fields(rp.paragraph.Text))
		if total >= wordLimit {
			return resolved[:i+1]
		}
	}
	return resolved
}
