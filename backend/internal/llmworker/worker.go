// Package llmworker is the only package in this module that imports
// llamacpp-go (a cgo binding around llama.cpp, see ../../../llamacpp-go).
// It runs exclusively inside the ttsworker binary (cmd/ttsworker) -
// alongside internal/audioworker's own audio.cpp models, never inside
// cmd/server - so that both native model families share one process's
// VRAM budget and can be idle-unloaded against each other instead of both
// sitting permanently resident on top of whatever the other needs. See
// backend/CLAUDE.md's "ttsworker / audioworker" section for the full
// reasoning (llama.cpp itself carries none of audio.cpp's confirmed leak;
// living in ttsworker anyway is about VRAM sharing and idle-unload, not
// leak containment).
//
// Package speakerattr (imported only by cmd/server) is this package's
// client, over the same loopback HTTP connection cmd/server already
// maintains to ttsworker for TTS calls (internal/ttsworker.Manager, which
// this package's Worker.LLMGenerate method signature deliberately matches -
// see speakerattr's own llmBackend interface).
package llmworker

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rhino1998/lectable/llamacpp-go/llamacpp"
)

// Config controls the embedded GGUF model this Worker loads on first use.
type Config struct {
	ModelPath string
	// NGPULayers is the number of transformer layers to offload to GPU -
	// 0 = CPU only, negative = offload everything a GPU backend can take
	// (llamacpp.ModelParams.NGPULayers).
	NGPULayers int32
	// NCtx is the per-slot budget in tokens - how much room one concurrent
	// generation (MaxConcurrent's own slots, not a primed system-prompt
	// sequence) needs for its own output, matched against
	// speakerattr.MaxOutputTokens's own doc comment (the largest maxTokens
	// any caller will ever request - real margin for that slot's own input
	// prompt too, since a batch's prompt is always far smaller than its own
	// maxTokens ceiling). 0 means "use the model's own trained context
	// size" (see speakerattr.Config.NCtx's own doc comment for why that
	// default is usually the wrong choice).
	//
	// This is NOT passed straight through to llamacpp.ContextParams.NCtx -
	// load computes that separately (contextNCtx). ContextParams.KVUnified
	// is true (required for CopySeq - see load's own comment), and
	// llama.cpp's actual behavior for a unified context is
	// cparams.n_ctx_seq = cparams.n_ctx: every sequence shares ONE pool of
	// that many cells total, not NCtx cells each (confirmed against
	// llama-context.cpp's own NCtx-vs-n_seq_max handling - kv_unified
	// skips the per-sequence division the non-unified branch does). A
	// context created with ContextParams.NCtx: NCtx directly - what an
	// earlier version of this code did - starves as soon as the
	// permanently-primed SystemPrompts sequences plus a couple of
	// concurrent generation slots together exceed that one shared
	// NCtx-sized pool: confirmed in production as a real ttsworker crash
	// loop on a long book running several concurrent speech-direction/
	// attribution tasks ("llama_decode failed: decode: failed to find a
	// memory slot for batch of size 3/4"). contextNCtx instead sizes the
	// real shared pool as (every primed prefix's own exactly-measured
	// token cost) + NCtx*MaxConcurrent, guaranteeing every one of
	// MaxConcurrent slots can independently reach this budget on top of
	// every primed prefix's own fixed cost - deliberately NOT
	// MaxConcurrent's own worst case times nSeqMax (MaxConcurrent +
	// len(SystemPrompts)): an earlier attempt at this fix multiplied NCtx
	// (then defaulted to speakerattr.DefaultNCtx, sized for input+output
	// combined) by MaxConcurrent alone and still overshot available VRAM
	// ("failed to allocate buffer for kv cache") - defaulting NCtx to
	// speakerattr.MaxOutputTokens instead (output alone, no separate input
	// margin needed - see that function's own doc comment) keeps the real
	// total much closer to what actually fits.
	NCtx uint32
	// MaxConcurrent is the number of concurrent generations this Worker
	// serves at once, sharing the one loaded model via multi-sequence
	// batching (llamacpp.Scheduler) rather than a mutex serializing every
	// call - see LLMGenerate. Must be >= 1; New defaults it to 2 - see
	// contextNCtx's own doc comment for why this is the real dial that
	// controls the shared KV pool's total VRAM cost (perSlotNCtx *
	// MaxConcurrent): a real, observed "failed to allocate buffer for kv
	// cache" at MaxConcurrent=4 (a ~142k-token pool) on this app's own GPU
	// budget, once combined with the primed system prompts and whatever TTS
	// clone model shares the same process, is what motivated lowering this
	// - not something to raise again without checking that math first. This
	// still leaves multiple genuinely-concurrent slots for poolLLM's other
	// kinds (characterization/direction/music-scoring) - KindSpeakerAttribution
	// specifically is additionally serialized to one in-flight task at a
	// time regardless (see jobs.Manager.globalAttributionSlotDependency), a
	// scheduling choice independent of this VRAM-driven slot count.
	MaxConcurrent int
	// SystemPrompts is every distinct system-prompt string this Worker's
	// caller will ever pass to LLMGenerate, decoded once each into its own
	// permanently-resident sequence at load time (see load) so later
	// LLMGenerate calls sharing one of these prompts can skip redecoding it
	// - the multi-slot generalization of what speakerattr.Client used to do
	// lazily, one Context per systemPrompt, via its own primedFor/
	// tryPrimed. Fixed and known up front here (unlike the old lazy
	// version) because priming has to happen before the Scheduler's own
	// goroutine starts owning the Context (see load) - decoding a new
	// prefix later, from an arbitrary caller's goroutine, would race the
	// Scheduler's own concurrent Decode calls on the same Context. A
	// systemPrompt not in this list simply never gets the reuse speedup
	// (LLMGenerate falls back to decoding it fresh every call) - never a
	// wrong answer, just a slower one, same as any other primed-reuse
	// verification failure.
	SystemPrompts []string
	// IdleUnloadAfter, if > 0, frees the loaded model (and every primed
	// prefix) once LLMGenerate hasn't been called for this long, reloading
	// (and re-priming) lazily on the next call - see idleLoop. 0 disables
	// idle unloading (the model, once loaded, stays resident for the rest
	// of the process's life). Deliberately idle-timeout based, not eager on
	// every switch away from LLM work: an eager version would thrash the
	// same way an earlier, since-removed internal/jobs.Manager mechanism
	// that force-canceled/evicted work on every pool switch was observed to
	// thrash in practice (see internal/jobs' own urgentElsewhereQueued doc
	// comment) - frequent, short LLM calls interleaved with sustained TTS
	// generation (e.g. lazy per-character voice provisioning during a big
	// book's bulk generation) would otherwise pay a full multi-GB reload on
	// nearly every call.
	IdleUnloadAfter time.Duration
	// BeforeLoad, if set, is called immediately before this Worker loads its
	// own GGUF model (first use, or the first call after an idle/explicit
	// unload) - wired by cmd/ttsworker/main.go to internal/audioworker.
	// Worker.UnloadCloneModels, so the LLM's own load never has to
	// momentarily coexist in VRAM with whatever clone models happen to be
	// resident, the same "unload before loading" reasoning as audioworker's
	// own evictOtherClonesLocked/Design - this box's GPU has no headroom to
	// spare for both a clone model and the LLM at once (see backend/
	// CLAUDE.md). UnloadCloneModels, not UnloadAll, deliberately leaves the
	// shared aligner resident - see that method's own doc comment. Optional
	// and nil-checked since cmd/benchattr (this package's other caller)
	// runs with no audioworker.Worker to unload.
	BeforeLoad func()

	// NUBatch, FlashAttn and KVType pass straight through to
	// llamacpp.ContextParams (NUBatch, FlashAttn, TypeK/TypeV); their zero
	// values keep llama.cpp's own defaults.
	NUBatch   uint32
	FlashAttn llamacpp.FlashAttnMode
	KVType    llamacpp.KVType

	// PrimeAsState keeps each primed system prompt as a host-side KV
	// snapshot (llamacpp.Context.SaveSeq) restored into a slot on admission,
	// instead of a permanently-resident sequence CopySeq'd from. The Context
	// is then non-unified (one KV stream per slot, NCtx cells each) and holds
	// no primed sequences at all. In a unified Context every call's attention
	// spans the whole used cell range - including every resident primed
	// prefix (~10K tokens for speakerattr's prompts) - which measured as ~2x
	// slower prompt processing on real attribution batches.
	PrimeAsState bool
}

func (c Config) withDefaults() Config {
	if c.MaxConcurrent < 1 {
		c.MaxConcurrent = 2
	}
	return c
}

// totals accumulates every finished LLMGenerate call's llamacpp.GenStats -
// see Worker.Totals.
type totals struct {
	mu sync.Mutex
	t  Totals
}

// Totals is the sum of llamacpp.GenStats over every LLMGenerate call this
// Worker has finished - cmd/benchattr's prefill-vs-decode breakdown.
type Totals struct {
	Calls                                 int
	ReusedTokens, PromptTokens, GenTokens int
	Queued, Prefill, Decode               time.Duration
}

// RoundStats returns the loaded model's Scheduler round totals (see
// llamacpp.RoundStats); zero if no model is loaded.
func (w *Worker) RoundStats() llamacpp.RoundStats {
	w.loadGate.RLock()
	defer w.loadGate.RUnlock()
	if w.sched == nil {
		return llamacpp.RoundStats{}
	}
	return w.sched.RoundStats()
}

// Totals returns the running sum of every finished call's stats.
func (w *Worker) Totals() Totals {
	w.totals.mu.Lock()
	defer w.totals.mu.Unlock()
	return w.totals.t
}

func (w *Worker) addTotals(st llamacpp.GenStats) {
	w.totals.mu.Lock()
	defer w.totals.mu.Unlock()
	t := &w.totals.t
	t.Calls++
	t.ReusedTokens += st.ReusedTokens
	t.PromptTokens += st.PromptTokens
	t.GenTokens += st.GenTokens
	t.Queued += st.Queued
	t.Prefill += st.Prefill
	t.Decode += st.Decode
}

// primed is one already-decoded, permanently-resident system-prompt prefix
// that a generation slot copies from (llamacpp.Context.CopySeq) instead of
// redecoding - see Config.SystemPrompts and load.
type primed struct {
	seqID        int32
	prefixTokens []llamacpp.Token
	state        []byte // PrimeAsState's snapshot; nil when seqID holds the prefix
}

// Worker embeds a GGUF model in-process, lazily on first LLMGenerate call
// (or the first call after an idle unload), and serves up to
// Config.MaxConcurrent concurrent generations against it via multi-sequence
// batching (llamacpp.Scheduler) instead of serializing every call behind a
// mutex the way speakerattr.Client used to.
//
// loadGate is a sync.RWMutex, the same shape internal/ttsworker.Manager's
// own restartGate already uses for an analogous reason: every LLMGenerate
// call takes an RLock for its full round trip (many concurrent "readers" -
// up to MaxConcurrent real generations at once, all able to proceed
// together since RLock never blocks another RLock), while loading or
// unloading the model takes the exclusive Lock (a rare "writer"), which
// naturally waits for every in-flight generation to finish first and blocks
// new calls until the swap completes.
type Worker struct {
	cfg Config

	loadGate sync.RWMutex
	model    *llamacpp.Model
	ctx      *llamacpp.Context
	sched    *llamacpp.Scheduler
	primed   map[string]*primed

	lastUsed atomic.Int64 // UnixNano of the most recent LLMGenerate call's start or finish; 0 = never used
	// inFlight counts LLMGenerate calls currently running (or waiting on
	// load/the scheduler) - idleLoop never unloads while it's nonzero.
	// lastUsed alone wasn't enough: it only marked a call's *start*, so one
	// long generation (several minutes) looked "idle" after IdleUnloadAfter,
	// and the resulting Unload then sat on loadGate.Lock waiting for that
	// call to finish - blocking every new call's RLock behind it meanwhile.
	inFlight atomic.Int64

	reqCounter atomic.Int64 // monotonic id for LLMGenerate's own debug logging - see LLMGenerate

	totals totals

	idleStop chan struct{}
	idleDone chan struct{}
}

var backendInitOnce sync.Once

// New creates a Worker. The model itself is not loaded until the first
// LLMGenerate call - see Worker's own doc comment.
func New(cfg Config) *Worker {
	cfg = cfg.withDefaults()
	w := &Worker{cfg: cfg}
	if cfg.IdleUnloadAfter > 0 {
		w.idleStop = make(chan struct{})
		w.idleDone = make(chan struct{})
		go w.idleLoop()
	}
	return w
}

// Loaded reports whether the model is currently resident - for GET /health.
func (w *Worker) Loaded() bool {
	w.loadGate.RLock()
	defer w.loadGate.RUnlock()
	return w.model != nil
}

// Close releases the loaded model, if any, and stops the idle-unload loop.
// Safe to call on a Worker that was never used.
func (w *Worker) Close() {
	if w.idleStop != nil {
		close(w.idleStop)
		<-w.idleDone
	}
	w.Unload()
}

// Unload frees the loaded model, its Context, and every primed prefix,
// reclaiming that VRAM/RAM immediately - the model reloads (and re-primes)
// lazily on the next LLMGenerate call. A no-op if nothing is loaded. This is
// also what Config.IdleUnloadAfter's own background sweep calls, and what a
// caller wanting deterministic "free this now, e.g. before a big TTS run"
// behavior can call directly instead of waiting for the idle timer.
func (w *Worker) Unload() {
	w.loadGate.Lock()
	defer w.loadGate.Unlock()
	w.unloadLocked()
}

func (w *Worker) unloadLocked() {
	if w.model == nil {
		return
	}
	w.sched.Close()
	w.ctx.Close()
	w.model.Close()
	w.model, w.ctx, w.sched, w.primed = nil, nil, nil, nil
}

// unloadIfIdle is idleLoop's own Unload, re-checking inFlight/lastUsed
// once it actually holds loadGate - a call that started between
// idleLoop's lock-free check and here has already bumped inFlight (before
// ever taking its own RLock), so it's never unloaded out from under.
func (w *Worker) unloadIfIdle() {
	w.loadGate.Lock()
	defer w.loadGate.Unlock()
	if w.model == nil || w.inFlight.Load() > 0 {
		return
	}
	if time.Since(time.Unix(0, w.lastUsed.Load())) < w.cfg.IdleUnloadAfter {
		return
	}
	log.Printf("llmworker: unloading model after %s idle", w.cfg.IdleUnloadAfter)
	w.unloadLocked()
}

// idleLoop periodically checks whether the model has sat unused past
// Config.IdleUnloadAfter and, if so, unloads it - see Config.
// IdleUnloadAfter's own doc comment for why this is timeout-based rather
// than triggered eagerly on every switch away from LLM work.
func (w *Worker) idleLoop() {
	defer close(w.idleDone)
	const pollInterval = 15 * time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-w.idleStop:
			return
		case <-ticker.C:
			if !w.Loaded() {
				continue
			}
			if w.inFlight.Load() > 0 {
				continue
			}
			lastNano := w.lastUsed.Load()
			if lastNano == 0 {
				continue
			}
			if time.Since(time.Unix(0, lastNano)) >= w.cfg.IdleUnloadAfter {
				w.unloadIfIdle()
			}
		}
	}
}

// load loads the model, creates its Context (sized for MaxConcurrent
// generation slots plus one dedicated sequence per Config.SystemPrompts
// entry), primes every configured system prompt into its own sequence, and
// starts the Scheduler - all before any other goroutine can reach the
// Context, which is what makes the priming decodes here safe to run
// directly rather than through the Scheduler itself (see Config.
// SystemPrompts's own doc comment).
func (w *Worker) load() error {
	w.loadGate.Lock()
	defer w.loadGate.Unlock()
	if w.model != nil {
		return nil // someone else already loaded it while we waited for the lock
	}
	if w.cfg.ModelPath == "" {
		return fmt.Errorf("llmworker: no model configured")
	}

	if w.cfg.BeforeLoad != nil {
		w.cfg.BeforeLoad()
	}

	backendInitOnce.Do(llamacpp.BackendInit)
	model, err := llamacpp.LoadModel(w.cfg.ModelPath, llamacpp.ModelParams{NGPULayers: w.cfg.NGPULayers})
	if err != nil {
		return fmt.Errorf("llmworker: load model %s: %w", w.cfg.ModelPath, err)
	}

	nSeqMax := uint32(w.cfg.MaxConcurrent) + uint32(len(w.cfg.SystemPrompts))
	residentPrimes := w.cfg.SystemPrompts
	if w.cfg.PrimeAsState {
		nSeqMax = uint32(w.cfg.MaxConcurrent)
		residentPrimes = nil
	}

	ctxNCtx, err := contextNCtx(model, w.cfg.NCtx, uint32(w.cfg.MaxConcurrent), residentPrimes)
	if err != nil {
		model.Close()
		return fmt.Errorf("llmworker: size context: %w", err)
	}
	log.Printf("llmworker: sizing KV cache to %d tokens (perSlot=%d x MaxConcurrent=%d + resident primed prefixes, nSeqMax=%d, primeAsState=%v)", ctxNCtx, w.cfg.NCtx, w.cfg.MaxConcurrent, nSeqMax, w.cfg.PrimeAsState)

	lctx, err := model.NewContext(llamacpp.ContextParams{
		NCtx:    ctxNCtx,
		NSeqMax: nSeqMax,
		// Unified is required for CopySeq's cheap partial-prefix path - see
		// ContextParams.KVUnified's own doc comment. PrimeAsState restores
		// snapshots instead, so each slot gets its own stream.
		KVUnified: !w.cfg.PrimeAsState,
		NUBatch:   w.cfg.NUBatch,
		FlashAttn: w.cfg.FlashAttn,
		TypeK:     w.cfg.KVType,
		TypeV:     w.cfg.KVType,
	})
	if err != nil {
		model.Close()
		return fmt.Errorf("llmworker: create context: %w", err)
	}

	primedMap := make(map[string]*primed, len(w.cfg.SystemPrompts))
	stateBytes := 0
	for i, sp := range w.cfg.SystemPrompts {
		seqID := int32(w.cfg.MaxConcurrent + i)
		if w.cfg.PrimeAsState {
			seqID = 0 // decoded, snapshotted, then wiped - see below
		}
		p, err := primePrefix(model, lctx, sp, seqID)
		if err == nil && w.cfg.PrimeAsState {
			p.state = lctx.SaveSeq(seqID)
			lctx.TrimSequence(seqID, 0)
			if p.state == nil {
				err = fmt.Errorf("empty KV snapshot")
			}
			stateBytes += len(p.state)
		}
		if err != nil {
			log.Printf("llmworker: could not prime system prompt %d/%d, it will be decoded fresh every call: %v", i+1, len(w.cfg.SystemPrompts), err)
			continue
		}
		primedMap[sp] = p
	}
	if w.cfg.PrimeAsState {
		log.Printf("llmworker: %d primed prefix snapshots, %d MiB", len(primedMap), stateBytes>>20)
	}

	w.model = model
	w.ctx = lctx
	w.sched = llamacpp.NewScheduler(lctx, w.cfg.MaxConcurrent)
	w.primed = primedMap
	return nil
}

// renderPrefixTokens renders sp the same way primePrefix decodes it (a
// system-turn-only chat-template prefix, addAssistant=false) and returns
// its exact token count, without needing a Context - contextNCtx uses this
// to size one before any Context exists at all; primePrefix re-renders (a
// second, cheap tokenize - no repeated work worth caching) since it needs
// the actual token slice, not just a count.
func renderPrefixTokens(model *llamacpp.Model, systemPrompt string) ([]llamacpp.Token, error) {
	prefixRendered, err := model.Vocab().ApplyChatTemplate(model.ChatTemplate(""), []llamacpp.ChatMessage{
		{Role: "system", Content: systemPrompt},
	}, false)
	if err != nil {
		return nil, fmt.Errorf("render system-prompt prefix: %w", err)
	}
	return model.Vocab().Tokenize(prefixRendered, true, true)
}

// contextNCtx computes the real llamacpp.ContextParams.NCtx to request for
// a KVUnified context serving maxConcurrent generation slots plus one
// permanently-primed sequence per systemPrompts entry - see Config.NCtx's
// own doc comment for why this can't just be perSlotNCtx itself, and for
// why perSlotNCtx defaults to speakerattr.MaxOutputTokens (an output-only
// ceiling) rather than something already budgeting input+output combined.
// Every primed sequence's own fixed cost is measured exactly (tokenizing
// here needs only model.Vocab(), available before any Context exists), so
// the shared pool pays for what priming actually costs rather than a
// second guess at it; perSlotNCtx (0 substituted the same way
// Model.NewContext's own doc comment says NCtx's zero value already means,
// model.NCtxTrain()) is reserved on top for each of maxConcurrent slots.
func contextNCtx(model *llamacpp.Model, perSlotNCtx uint32, maxConcurrent uint32, systemPrompts []string) (uint32, error) {
	if perSlotNCtx == 0 {
		if t := model.NCtxTrain(); t > 0 {
			perSlotNCtx = uint32(t)
		}
	}

	total := maxConcurrent * perSlotNCtx
	for _, sp := range systemPrompts {
		tokens, err := renderPrefixTokens(model, sp)
		if err != nil {
			return 0, fmt.Errorf("measure primed prefix cost: %w", err)
		}
		total += uint32(len(tokens))
	}
	return total, nil
}

// primePrefix renders sp as a system-turn chat-template prefix (no user
// turn, no assistant turn - addAssistant=false) and decodes it into seqID,
// mirroring speakerattr's old primedFor: the rendered prefix must be
// exactly the leading portion of a later full (system+user, addAssistant
// =true) rendering for reuse to be valid, which LLMGenerate's own
// token-for-token check (tryPrimed) verifies on every call rather than
// assuming it here.
func primePrefix(model *llamacpp.Model, lctx *llamacpp.Context, systemPrompt string, seqID int32) (*primed, error) {
	prefixRendered, err := model.Vocab().ApplyChatTemplate(model.ChatTemplate(""), []llamacpp.ChatMessage{
		{Role: "system", Content: systemPrompt},
	}, false)
	if err != nil {
		return nil, fmt.Errorf("render system-prompt prefix: %w", err)
	}
	prefixTokens, err := model.Vocab().Tokenize(prefixRendered, true, true)
	if err != nil {
		return nil, fmt.Errorf("tokenize system-prompt prefix: %w", err)
	}
	n, err := lctx.DecodePromptSeq(prefixRendered, seqID)
	if err != nil {
		return nil, fmt.Errorf("decode system-prompt prefix: %w", err)
	}
	if int(n) != len(prefixTokens) {
		// Tokenize and DecodePromptSeq each tokenize prefixRendered
		// independently - they must always agree since it's the same text
		// through the same tokenizer, so a mismatch means something is
		// wrong enough not to trust at all (mirrors speakerattr's original
		// primedFor's own identical check).
		return nil, fmt.Errorf("decoded prefix token count %d != tokenized count %d", n, len(prefixTokens))
	}
	return &primed{seqID: seqID, prefixTokens: prefixTokens}, nil
}

// tryPrimed looks up systemPrompt's primed prefix, if any, and verifies
// fullPrompt's own tokenization genuinely still starts with it,
// token-for-token, before handing back the (startPos, primedSeq) pair
// LLMGenerate passes to the Scheduler. Returns ok=false (never an error)
// whenever reuse can't be confirmed safe - no primed entry for this exact
// systemPrompt string, or a verification mismatch - each logged once per
// occurrence so a systematic mismatch is visible, but always as a fallback
// to decoding the full prompt fresh, never a failure of the call itself.
// Caller must hold w.loadGate (for read).
func (w *Worker) tryPrimed(model *llamacpp.Model, systemPrompt, fullPrompt string) (startPos int32, primedSeq int32, state []byte, ok bool) {
	p, found := w.primed[systemPrompt]
	if !found {
		return 0, 0, nil, false
	}

	fullTokens, err := model.Vocab().Tokenize(fullPrompt, true, true)
	if err != nil {
		log.Printf("llmworker: could not tokenize prompt to verify primed-prefix reuse, decoding fresh for this call: %v", err)
		return 0, 0, nil, false
	}
	if len(fullTokens) <= len(p.prefixTokens) {
		log.Printf("llmworker: full prompt tokenized shorter than the primed prefix alone, decoding fresh for this call")
		return 0, 0, nil, false
	}
	for i, t := range p.prefixTokens {
		if fullTokens[i] != t {
			log.Printf("llmworker: chat-template rendering no longer matches the primed system-prompt prefix at token %d, decoding fresh for this call", i)
			return 0, 0, nil, false
		}
	}
	return int32(len(p.prefixTokens)), p.seqID, p.state, true
}

// LLMGenerate renders systemPrompt/userPrompt as one chat completion and
// generates a reply, reusing systemPrompt's own primed prefix (see
// Config.SystemPrompts/tryPrimed) when available. Unlike the old
// speakerattr.Client.generate, this does not serialize concurrent callers
// behind a mutex - up to Config.MaxConcurrent calls run genuinely
// concurrently, multiplexed by the Scheduler's own multi-sequence batching
// onto the one loaded model.
func (w *Worker) LLMGenerate(ctx context.Context, systemPrompt, userPrompt string, temp float32, maxTokens int) (out string, err error) {
	w.inFlight.Add(1)
	w.lastUsed.Store(time.Now().UnixNano())
	defer func() {
		w.lastUsed.Store(time.Now().UnixNano())
		w.inFlight.Add(-1)
	}()

	// Debug instrumentation for diagnosing calls that take far longer than
	// expected (a real, observed problem: some batches through the live
	// pipeline take minutes despite an isolated reproduction of the exact
	// same prompt finishing in seconds) - reqID correlates this call's own
	// start/end log lines with each other and with whatever's happening
	// concurrently in the rest of the log. Logs the full prompt text,
	// deliberately untruncated, since a truncated prompt is exactly what's
	// useless when the goal is reproducing a specific slow/stuck call
	// outside the live pipeline.
	reqID := w.reqCounter.Add(1)
	start := time.Now()
	var stats llamacpp.GenStats
	log.Printf("llmworker: [req %d] starting generate (temp=%.2f maxTokens=%d)\n--- system prompt ---\n%s\n--- user prompt ---\n%s\n--- end prompt ---", reqID, temp, maxTokens, systemPrompt, userPrompt)
	defer func() {
		if err != nil {
			log.Printf("llmworker: [req %d] failed after %s: %v", reqID, time.Since(start).Round(time.Millisecond), err)
		} else {
			log.Printf("llmworker: [req %d] finished after %s (%d bytes out; prompt %d tok + %d reused, prefill %s; gen %d tok, decode %s; queued %s)\n--- output ---\n%s\n--- end output ---",
				reqID, time.Since(start).Round(time.Millisecond), len(out),
				stats.PromptTokens, stats.ReusedTokens, stats.Prefill.Round(time.Millisecond),
				stats.GenTokens, stats.Decode.Round(time.Millisecond), stats.Queued.Round(time.Millisecond), out)
		}
	}()

	if !w.Loaded() {
		if err := w.load(); err != nil {
			return "", err
		}
	}

	w.loadGate.RLock()
	defer w.loadGate.RUnlock()
	if w.model == nil {
		// Unloaded again between the check above and here (idle sweep, or
		// a concurrent Unload call) - rare; the caller's own retry (if any)
		// will simply reload it.
		return "", fmt.Errorf("llmworker: model not loaded")
	}
	model, sched := w.model, w.sched

	fullPrompt, err := model.Vocab().ApplyChatTemplate(model.ChatTemplate(""), []llamacpp.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, true)
	if err != nil {
		return "", fmt.Errorf("llmworker: apply chat template: %w", err)
	}

	// Not closed here - llamacpp.Scheduler.Generate can return well before
	// the Scheduler is actually done sampling from this (ctx cancelled
	// while still queued, or already admitted into an active slot; see
	// GenRequest.Sampler's own doc comment) - closing it on our own right
	// after Generate returns used to race the Scheduler's own still-running
	// goroutine calling Sample() on this exact same, now-freed sampler, a
	// real segfault (llama_sampler_sample on a freed/nil handle) confirmed
	// live via ttsworker crash dumps. The Scheduler now owns closing it.
	sampler := llamacpp.NewSampler(llamacpp.SamplerParams{Temp: temp, TopK: 40, TopP: 0.9})

	req := llamacpp.GenRequest{Prompt: fullPrompt, Sampler: sampler, MaxTokens: maxTokens, Stats: &stats}
	if startPos, primedSeq, state, ok := w.tryPrimed(model, systemPrompt, fullPrompt); ok {
		req.StartPos = startPos
		req.PrimedSeq = primedSeq
		req.PrefixState = state
	}

	out, err = sched.Generate(ctx, req)
	if err != nil {
		return "", err
	}
	w.addTotals(stats)
	out = stripThinking(out)
	return out, nil
}

// stripThinking removes a leading <think>...</think> (or <thinking>...
// </thinking>) block some models emit unconditionally before their actual
// answer - see speakerattr's original copy of this function for the full
// reasoning (Qwen3's hybrid-thinking chat template, and why an unstripped
// trace would corrupt a caller like CharacterizeVoice that uses the result
// verbatim). Kept here, not in speakerattr, since it's about what a raw
// model reply looks like, not about any particular caller's use of it.
func stripThinking(s string) string {
	s = strings.TrimSpace(s)
	for _, tag := range []string{"think", "thinking"} {
		open := "<" + tag + ">"
		closeTag := "</" + tag + ">"
		if !strings.HasPrefix(s, open) {
			continue
		}
		end := strings.Index(s, closeTag)
		if end == -1 {
			return ""
		}
		return strings.TrimSpace(s[end+len(closeTag):])
	}
	return s
}
