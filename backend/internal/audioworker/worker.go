// Package audioworker is the only package in this module that imports
// audiocpp-go (a cgo binding around audio.cpp's C ABI, see
// ../../../audiocpp-go). It runs exclusively inside the ttsworker binary
// (cmd/ttsworker) - never inside cmd/server, which must stay free of any
// dependency on libaudiocpp.so so it can be recycled independently (a
// leak inside audio.cpp's own native code, confirmed via heaptrack against
// a real workload, is what the worker/watchdog split in
// internal/ttsworker exists to contain). See backend/CLAUDE.md.
package audioworker

import (
	"container/list"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rhino1998/lectable/audiocpp-go/audiocpp"
)

// loadedClone bundles one family's loaded Model with a small pool of warm,
// reused Sessions - mirrors tts-service/clone_backends/audiocpp.py's
// _Loaded, widened from a single Session to a pool (see Config.
// ClonePoolSize) because audiocpp.h's own ABI contract says "handles are
// not thread-safe individually [but] separate handles may be used
// concurrently from separate threads": a single shared Session forced
// every concurrent Generate call to queue behind one global lock around
// Session.Run, even though the actual CPU-bound cost on that path (audio.cpp
// unconditionally tearing down and rebuilding Higgs's prefill graph on
// every single call - see backend/CLAUDE.md) has nothing to do with the one
// physical GPU and everything to do with each call's own independent,
// single-threaded host-side graph construction. Separate Session handles
// let that construction work - and each session's own independent
// generator state (prefill_graph_/decode_graph_/ar_kv_cache_) - genuinely
// overlap across goroutines instead of serializing.
type loadedClone struct {
	model *audiocpp.Model
	fam   cloneFamily // kept to lazily create more sessions later - see Worker.checkoutSession

	// poolSize caps how many sessions this clone_model will ever create -
	// see Config.ClonePoolSize. created counts how many exist so far
	// (guarded by Worker.cloneMu, same as every other newCloneSessionLocked
	// caller): sessions are created lazily, one at a time, the first time a
	// concurrent Generate call actually needs one beyond whatever's already
	// idle in avail - not all at once when the clone_model loads - so a
	// clone_model that only ever sees one Generate call at a time (or none
	// at all, for a merely eagerly-*loaded* default clone_model sitting
	// idle - see Config.DefaultCloneModel) never pays for sessions 2..N.
	// See Worker.checkoutSession.
	poolSize int
	created  int
	// sessions is every session ever created for this clone_model, used
	// only by close() to release every one of them; avail is the subset
	// currently idle, checked out/in by Generate (including a session that
	// just overflowed max_tokens - see Generate's own isMaxTokensOverflow
	// handling in generate.go, which keeps it warm rather than evicting).
	sessions []*audiocpp.Session
	avail    chan *audiocpp.Session

	// inFlight counts callers currently between a successful
	// Worker.getCloneModel(this clone_model) and the matching
	// Worker.releaseCloneModel - guarded by Worker.cloneMu, same as every
	// other field here. This has to span a whole Generate call, not just
	// its checkoutSession/avail window: audiocpp.h documents that "handles
	// are not thread-safe individually", so closing a session (or its
	// backing model, before checkoutSession has even created one) while
	// another goroutine is using it - Session.Run(), or the window between
	// getCloneModel returning and checkoutSession's own newCloneSessionLocked
	// call - is a use-after-free, not just a bookkeeping error. Both
	// evictToCapLocked and UnloadCloneModels skip any loadedClone with
	// inFlight > 0 rather than closing it out from under an active caller.
	inFlight int
}

func (lc *loadedClone) close() {
	for _, s := range lc.sessions {
		s.Close()
	}
	lc.model.Close()
}

// loadedDesign is loadedClone's counterpart for a VoiceDesign engine
// (design.go's Design) - same warm-model/pooled-session shape, same
// audiocpp.h thread-safety reasoning (loadedClone's own doc comment), and
// the same lazy per-call session creation up to poolSize (see
// Worker.checkoutDesignSession, mirroring checkoutSession). Unlike
// loadedClone, there's no LRU/over-cap eviction *among* design engines -
// only a handful exist (designEngines) and this app only ever wants one
// resident at a time in practice (Config.DesignModel/resolveDesignEngine
// picks one process-wide default, with per-preset overrides rare) - so
// Worker.getDesignModel just evicts every *other* loaded design engine
// before loading a new one, the same "only one kind of multi-GB model
// resident at a time" rule getCloneModel/getDesignModel already enforce
// against each other (clone vs. design).
type loadedDesign struct {
	model  *audiocpp.Model
	engine designEngine

	// poolSize/created/sessions/avail/inFlight mirror loadedClone's own
	// fields exactly - see its doc comments for what each does and why.
	poolSize int
	created  int
	sessions []*audiocpp.Session
	avail    chan *audiocpp.Session
	inFlight int
}

func (ld *loadedDesign) close() {
	for _, s := range ld.sessions {
		s.Close()
	}
	ld.model.Close()
}

// auxEngine is the shared pooled-session state for one "auxiliary"
// generation-only model family (ACE-Step/Music, Stable Audio - Music/SFX/
// Medium, ...) - loadedDesign's own counterpart, generalized over a
// family key instead of hand-duplicated per family. Every one of these
// has exactly one instance (no keyed-by-preset map the way loadedClone
// needs, and no "several engines but only one resident" case the way
// loadedDesign's designEngines has), the same warm-model/pooled-session
// shape, and the same full mutual exclusion against every clone/design
// model *and* every other aux engine (this app's GPU never holds more
// than one heavy model family resident at a time) - see Worker.getAux/
// unloadOtherAux. One generic type/method set backs all of them rather
// than a hand-copied loadedFoley/loadedAceStep/... per family (which is
// exactly how this started - see git history - before a third and fourth
// family made the duplication worth collapsing); the same "don't
// hand-duplicate, generalize over a key" approach internal/jobs' own
// Kind/kindPool already uses for an analogous problem.
type auxEngine struct {
	// key/family/modelPath/task/poolSize are fixed at registration
	// (registerAuxEngines, called once from New) and never change
	// afterward, so they're safe to read without holding mu.
	key       string // Worker.auxEngines' own map key and every log line's identifier, e.g. "ace_step"
	family    string // audio.cpp family name for ModelConfig.FamilyHint, e.g. "ace_step"
	modelPath string
	task      string // audio.cpp session task, e.g. "gen"
	poolSize  int

	mu       sync.Mutex // guards every field below
	model    *audiocpp.Model
	created  int
	sessions []*audiocpp.Session
	avail    chan *audiocpp.Session
	inFlight int
}

// close releases model/sessions and resets e back to its own unloaded
// state (model nil, avail re-created) so a later getAux reloads cleanly -
// callers must already hold e.mu.
func (e *auxEngine) close() {
	for _, s := range e.sessions {
		s.Close()
	}
	if e.model != nil {
		e.model.Close()
	}
	e.model = nil
	e.sessions = nil
	e.created = 0
	e.avail = make(chan *audiocpp.Session, e.poolSize)
}

// Config controls how a Worker's clone models are loaded/evicted.
type Config struct {
	// DefaultCloneModel is the clone_model every request without an
	// explicit one falls back to - eagerly loaded by New, and never
	// evicted by MaxExtraClones (though it is still freed, like everything
	// else, by an idle-triggered UnloadAll - see IdleUnloadAfter).
	DefaultCloneModel string
	MaxExtraClones    int
	// ClonePoolSize is the most independent audiocpp Session handles each
	// loaded clone_model will ever create, lazily, as concurrent Generate
	// calls for it actually need them - see loadedClone's own doc comment
	// for why more than one is the whole point, and Worker.checkoutSession
	// for the lazy-creation logic itself. Should match (or exceed) however
	// many Generate calls the caller actually dispatches concurrently for
	// one clone_model (internal/jobs' own maxInFlight, today 4) - a smaller
	// pool just means callers beyond the pool size queue for a session the
	// same way every caller used to queue for the single shared one, only
	// less often. <= 0 is treated as 1 (today's pre-pooling behavior).
	ClonePoolSize int
	// IdleUnloadAfter, if > 0, frees every currently-loaded clone model
	// (including DefaultCloneModel) once neither Generate, Design, nor
	// Align has been called for this long - see idleLoop. Reloaded lazily
	// on the next call, same as any other cache miss. 0 disables idle
	// unloading. See internal/llmworker.Config.IdleUnloadAfter's own doc
	// comment for why this is timeout-based, not triggered on every single
	// switch away from TTS work - the same thrashing risk applies here,
	// now that llmworker's own model shares this same process/VRAM budget.
	IdleUnloadAfter time.Duration
}

// fcfsMutex is a mutual-exclusion lock that grants access in exactly the
// order Lock was called (first-come-first-served), unlike sync.Mutex:
// under contention a plain sync.Mutex lets a goroutine that calls Lock
// later "barge" ahead of one that's already been waiting (Go's own
// starvation-mode fairness only kicks in once a waiter has been blocked
// over 1ms - see sync.Mutex's own doc comment - so under light/bursty
// contention it's not actually ordered at all). Worker.runMu needs real
// FCFS, not just "eventually fair": internal/jobs' own poolGeneration
// dispatches up to maxInFlight requests concurrently in a specific
// priority order (tier, then book/chapter/paragraph position - see
// comparePosition), and every one of them ends up contending on this same
// lock once they reach Session.Run - a bare sync.Mutex could let a later,
// lower-priority request's Run jump ahead of an earlier, higher-priority
// one that arrived first, silently undoing the queue's own ordering at
// the very last step.
type fcfsMutex struct {
	mu      sync.Mutex
	waiters []chan struct{}
	held    bool
}

func (f *fcfsMutex) Lock() {
	f.mu.Lock()
	if !f.held {
		f.held = true
		f.mu.Unlock()
		return
	}
	ch := make(chan struct{})
	f.waiters = append(f.waiters, ch)
	f.mu.Unlock()
	<-ch
}

func (f *fcfsMutex) Unlock() {
	f.mu.Lock()
	if len(f.waiters) == 0 {
		f.held = false
		f.mu.Unlock()
		return
	}
	next := f.waiters[0]
	f.waiters = f.waiters[1:]
	f.mu.Unlock()
	close(next) // hands off directly to next - held stays true throughout
}

// Worker owns one audiocpp.Registry (shared across every loaded model) plus
// the set of clone-model and design-engine instances currently resident.
// Actual GPU/CPU work (Session.Run) is no longer serialized process-wide
// for either - see loadedClone/loadedDesign's own doc comments - except for
// Align, which still uses singleSessionRunMu below since it never pools
// sessions of its own.
type Worker struct {
	registry *audiocpp.Registry
	cfg      Config

	// singleSessionRunMu serializes Session.Run for align.go's single
	// shared aligner session, the one remaining call site with only ever
	// one live session handle to run at a time. Generate/Design no longer
	// use this: see loadedClone/loadedDesign's own doc comments for why
	// each now checks a session out of its own pool instead.
	singleSessionRunMu sync.Mutex

	// loadMu serializes every call into registry.LoadModel, regardless of
	// which of the three call sites makes it (getCloneModel below,
	// getDesignModel below, alignSession in align.go) - audio.cpp's own
	// audiocpp_model_load is not safe to call concurrently against one
	// shared *audiocpp.Registry. cloneMu/alignMu below still separately
	// guard their own loaded-state bookkeeping/dedup (so two concurrent
	// requests for the same clone model, or two for the aligner, only
	// actually load once) exactly as before - this is a narrower,
	// additional lock underneath all three, held only around the actual
	// native LoadModel call itself (see loadModel), not a replacement for
	// either. Confirmed necessary live: concurrent Design calls (e.g.
	// several queued voice-provision tasks dispatching together once
	// internal/jobs' own poolGeneration/poolLLM mutual exclusion let them
	// through at once) crashed the whole worker process - a corrupted
	// jump inside audiocpp_model_load (rip pointed at a garbage address)
	// - with no lock here to prevent two goroutines from calling into the
	// registry at the same instant.
	loadMu sync.Mutex

	cloneMu           sync.Mutex // guards loaded/lru/lruElem below
	loaded            map[string]*loadedClone
	lru               *list.List // front = most-recently-used
	lruElem           map[string]*list.Element
	defaultCloneModel string
	maxExtraClones    int

	// designMu guards loadedDesigns below - deliberately its own mutex,
	// never held nested with cloneMu (see getCloneModel/getDesignModel,
	// both of which fully release one before acquiring the other) so
	// there's no lock-ordering hazard between the two "evict the other
	// kind before loading my own" steps each performs. Keyed by
	// designEngine.family (e.g. "qwen3_tts", "breeze_tts") - the same
	// stable identifier resolveDesignEngine's own designEngines map uses.
	designMu      sync.Mutex
	loadedDesigns map[string]*loadedDesign

	// auxEngines holds every registered auxEngine (ACE-Step/Music, Stable
	// Audio - Music/SFX/Medium, ...), keyed by its own short key - built
	// once in New via registerAuxEngines and never
	// mutated afterward (no entries added/removed), so no registry-level
	// lock is needed to read the map itself; each entry's own mu guards
	// its mutable state. See auxEngine's own doc comment for why this
	// replaced a hand-duplicated loadedFoley/loadedAceStep/... field pair
	// per family.
	auxEngines map[string]*auxEngine

	alignMu       sync.Mutex // guards alignModel/alignSessionH/alignErr below
	alignModel    *audiocpp.Model
	alignSessionH *audiocpp.Session
	alignErr      error

	lastUsed atomic.Int64 // UnixNano of the most recent Generate/Design/Align call; 0 = never used

	idleStop chan struct{}
	idleDone chan struct{}
}

// New creates a Worker and eagerly loads cfg.DefaultCloneModel (the one
// every request without an explicit clone_model falls back to, and the one
// never evicted by cfg.MaxExtraClones).
func New(cfg Config) (*Worker, error) {
	registry, err := audiocpp.NewRegistry("")
	if err != nil {
		return nil, fmt.Errorf("create registry: %w", err)
	}
	w := &Worker{
		registry:          registry,
		cfg:               cfg,
		loaded:            make(map[string]*loadedClone),
		lru:               list.New(),
		lruElem:           make(map[string]*list.Element),
		defaultCloneModel: cfg.DefaultCloneModel,
		maxExtraClones:    cfg.MaxExtraClones,
		loadedDesigns:     make(map[string]*loadedDesign),
		auxEngines:        registerAuxEngines(),
	}
	lc, err := w.getCloneModel(cfg.DefaultCloneModel)
	if err != nil {
		registry.Close()
		return nil, fmt.Errorf("load default clone model %s: %w", cfg.DefaultCloneModel, err)
	}
	// This eager load isn't an in-flight caller (no Generate is actually
	// using it yet) - release immediately so it starts life at inFlight
	// == 0 like any other idle loaded clone_model, same as every other
	// getCloneModel caller does once it's done. See loadedClone.inFlight's
	// own doc comment.
	w.releaseCloneModel(lc)
	if cfg.IdleUnloadAfter > 0 {
		w.idleStop = make(chan struct{})
		w.idleDone = make(chan struct{})
		go w.idleLoop()
	}
	return w, nil
}

// touch records that a request actually used this Worker just now - see
// IdleUnloadAfter/idleLoop.
func (w *Worker) touch() {
	w.lastUsed.Store(time.Now().UnixNano())
}

// idleLoop periodically checks whether every clone model has sat unused
// past cfg.IdleUnloadAfter and, if so, unloads all of them - see Config.
// IdleUnloadAfter's own doc comment for why.
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
			lastNano := w.lastUsed.Load()
			if lastNano == 0 {
				continue
			}
			if time.Since(time.Unix(0, lastNano)) >= w.cfg.IdleUnloadAfter {
				log.Printf("audioworker: unloading all clone models after %s idle", w.cfg.IdleUnloadAfter)
				w.UnloadAll()
			}
		}
	}
}

// Close stops the idle-unload loop, if running, and releases every
// currently-loaded model. Safe to call even if IdleUnloadAfter was never
// configured.
func (w *Worker) Close() {
	if w.idleStop != nil {
		close(w.idleStop)
		<-w.idleDone
	}
	w.UnloadAll()
}

// UnloadAll closes every currently-loaded clone model (including
// DefaultCloneModel - unlike evictExcessLocked, which never touches it),
// every currently-loaded design engine, and the aligner model/session, if
// loaded, freeing their VRAM/RAM immediately. Each reloads lazily on its
// next actual use, same as any other cache miss (getCloneModel/
// getDesignModel/alignSession). See Config.IdleUnloadAfter, whose own
// background sweep is UnloadAll's main caller - or call it directly for
// deterministic "free this now" behavior, e.g. right before a caller knows
// a long LLM-only stretch of work is starting. Callers that only want to
// free room for another *clone* model (llmworker's own BeforeLoad hook)
// should call UnloadCloneModels instead - see its own doc comment for why
// the aligner deliberately isn't swept up in those; getDesignModel calls
// UnloadCloneModels directly for the same reason, and getCloneModel calls
// UnloadDesignModels in the other direction.
func (w *Worker) UnloadAll() {
	w.UnloadCloneModels()
	w.UnloadDesignModels()
	w.UnloadAuxEngines()
	w.unloadAligner()
}

// UnloadCloneModels closes every currently-loaded clone model (including
// DefaultCloneModel), freeing their VRAM/RAM immediately, but leaves the
// aligner - if loaded - untouched. One forced-alignment call runs after
// essentially every paragraph any clone model (the Higgs pool included)
// generates (see internal/jobs.Manager.alignParagraph), far more often than
// Design or an LLM call happens - so unlike a clone model, which is fine to
// evict and lazily reload, the aligner is meant to stay one single,
// persistent, shared instance for as long as this process lives: reused
// across every concurrent Higgs-pool session's own alignment calls rather
// than being torn down and rebuilt by an unrelated model swap. Also matters
// for correctness, not just efficiency - Align's own session.Run happens
// outside alignMu (only guarded by singleSessionRunMu), so tearing the
// session down concurrently with an in-flight Align call would be a
// use-after-free on audio.cpp's own handle; simply never doing that from
// this path removes the race rather than needing to synchronize around it.
// Callers wanting the aligner freed too still have UnloadAll (idle timeout,
// the explicit /unload endpoint).
func (w *Worker) UnloadCloneModels() {
	w.cloneMu.Lock()
	for id, lc := range w.loaded {
		if lc.inFlight > 0 {
			// A Generate call is actively using this clone_model right
			// now - same use-after-free hazard evictToCapLocked guards
			// against (see loadedClone.inFlight's own doc comment), reachable
			// here too since Design (this method's own caller) and
			// Generate both dispatch through internal/jobs' shared
			// poolGeneration and can run concurrently. Leave it loaded;
			// it'll be picked up by a later Unload call once idle.
			continue
		}
		lc.close()
		delete(w.loaded, id)
		if el, ok := w.lruElem[id]; ok {
			w.lru.Remove(el)
			delete(w.lruElem, id)
		}
	}
	w.cloneMu.Unlock()
}

// UnloadDesignModels closes every currently-loaded design engine, freeing
// their VRAM/RAM immediately - loadedDesign's own counterpart to
// UnloadCloneModels, same "skip anything still inFlight, it'll be picked
// up once idle" logic and same reasoning for leaving the aligner alone.
// Called from getCloneModel before loading a new clone_model (the "only
// one kind resident at a time" rule, in the direction getDesignModel's own
// call to UnloadCloneModels already enforced before this pool existed),
// and from UnloadAll/the idle-unload sweep.
func (w *Worker) UnloadDesignModels() {
	w.designMu.Lock()
	for id, ld := range w.loadedDesigns {
		if ld.inFlight > 0 {
			continue
		}
		ld.close()
		delete(w.loadedDesigns, id)
	}
	w.designMu.Unlock()
}

func (w *Worker) unloadAligner() {
	w.alignMu.Lock()
	if w.alignSessionH != nil {
		w.alignSessionH.Close()
	}
	if w.alignModel != nil {
		w.alignModel.Close()
	}
	w.alignSessionH = nil
	w.alignModel = nil
	w.alignErr = nil
	w.alignMu.Unlock()
}

// loadModel calls registry.LoadModel under loadMu - see that field's own
// doc comment for why every LoadModel call in this package must go through
// here rather than calling w.registry.LoadModel directly.
func (w *Worker) loadModel(modelPath string, config audiocpp.ModelConfig, options *audiocpp.Options) (*audiocpp.Model, error) {
	w.loadMu.Lock()
	defer w.loadMu.Unlock()
	return w.registry.LoadModel(modelPath, config, options)
}

// getCloneModel returns cloneModel's loaded instance, loading (and
// registering in the LRU) it on first use. Mirrors tts-service's
// _get_clone_model.
func (w *Worker) getCloneModel(cloneModel string) (*loadedClone, error) {
	w.cloneMu.Lock()
	if lc, ok := w.loaded[cloneModel]; ok {
		w.touchLRULocked(cloneModel)
		lc.inFlight++
		w.cloneMu.Unlock()
		return lc, nil
	}
	w.cloneMu.Unlock()

	familyKey, ok := cloneModelFamilies[cloneModel]
	if !ok {
		return nil, fmt.Errorf("unsupported clone_model %q", cloneModel)
	}
	fam := cloneFamilies[familyKey]

	// Cache miss - about to pay for this clone_model's own multi-GB
	// weights, so free room for it first: every resident design engine and
	// every aux engine (ACE-Step/Stable Audio - each its own mutex,
	// released above and never held nested with cloneMu - see designMu's
	// own doc comment and getDesignModel/getAux's mirrored step in the
	// other direction), then over-cap clone models
	// below. Best-effort/racy against a concurrent getDesignModel/getAux
	// doing the same thing in reverse - no worse than the eviction here
	// already was before this method tracked design engines at all, just
	// VRAM hygiene, not a correctness requirement.
	w.UnloadDesignModels()
	w.UnloadAuxEngines()

	w.cloneMu.Lock()
	defer w.cloneMu.Unlock()
	if lc, ok := w.loaded[cloneModel]; ok {
		// Lost the race while cloneMu was released above - another caller
		// already loaded it.
		w.touchLRULocked(cloneModel)
		lc.inFlight++
		return lc, nil
	}

	// Free room for this not-yet-loaded clone_model before paying for its
	// own weights, not after - see evictBeforeLoadLocked's own doc comment.
	w.evictBeforeLoadLocked(cloneModel)

	log.Printf("loading %s (%s, clone_model=%s) on %s for stable preset cloning ...", fam.family, fam.modelPath, cloneModel, backendName)
	model, err := w.loadModel(fam.modelPath, audiocpp.ModelConfig{FamilyHint: fam.family}, nil)
	if err != nil {
		return nil, fmt.Errorf("load model: %w", err)
	}

	poolSize := w.cfg.ClonePoolSize
	if fam.poolSizeOverride > 0 {
		poolSize = fam.poolSizeOverride
	}
	if poolSize < 1 {
		poolSize = 1
	}

	// No sessions are created here - see loadedClone.poolSize/created and
	// Worker.checkoutSession. Loading a clone_model (e.g. Config.
	// DefaultCloneModel's own eager load in New) must not by itself spin up
	// its whole session pool: a session isn't just cheap bookkeeping, it's
	// its own warm generator state, so a clone_model that's loaded but
	// idle - or only ever sees one Generate call in flight at a time -
	// should only ever hold the sessions it's actually using concurrently,
	// not ClonePoolSize of them.
	lc := &loadedClone{model: model, fam: fam, poolSize: poolSize, avail: make(chan *audiocpp.Session, poolSize)}
	lc.inFlight++
	w.loaded[cloneModel] = lc
	w.touchLRULocked(cloneModel)
	log.Printf("%s loaded (session pool created lazily, up to %d sessions).", fam.family, poolSize)
	w.evictExcessLocked()
	return lc, nil
}

// releaseCloneModel drops the reservation getCloneModel placed on lc via
// its own inFlight field, letting eviction/UnloadCloneModels consider
// closing it again once inFlight reaches 0. Callers must call this exactly
// once for every getCloneModel call that returned lc successfully, only
// once they're completely finished with it - after returning any
// checked-out session to lc.avail, not right after checkoutSession - see
// loadedClone.inFlight's own doc comment for why the reservation has to
// span the whole call.
func (w *Worker) releaseCloneModel(lc *loadedClone) {
	w.cloneMu.Lock()
	lc.inFlight--
	w.cloneMu.Unlock()
}

// checkoutSession returns one of lc's pooled sessions for Generate to run a
// request on, creating a fresh one lazily - only once every session created
// so far is already checked out, and only up to lc.poolSize - rather than
// the pool having been fully created upfront when the clone_model loaded.
// Mirrors newCloneSessionLocked's own "must hold w.cloneMu" contract for the
// actual creation, same as its other call site (getCloneModel).
func (w *Worker) checkoutSession(lc *loadedClone) (*audiocpp.Session, error) {
	select {
	case s := <-lc.avail:
		return s, nil
	default:
	}

	w.cloneMu.Lock()
	if lc.created < lc.poolSize {
		session, err := w.newCloneSessionLocked(lc.model, lc.fam)
		if err != nil {
			w.cloneMu.Unlock()
			return nil, fmt.Errorf("create session %d/%d: %w", lc.created+1, lc.poolSize, err)
		}
		lc.sessions = append(lc.sessions, session)
		lc.created++
		w.cloneMu.Unlock()
		return session, nil
	}
	w.cloneMu.Unlock()

	// Pool already at full size and every session is checked out - the same
	// backpressure a fully-eager pool gave once warmed up.
	return <-lc.avail, nil
}

// newCloneSessionLocked creates one fresh tts Session for fam on model.
// Callers must already hold w.cloneMu: audiocpp_session_create operates on
// model's own handle, which - per audiocpp.h's "handles are not
// thread-safe individually" - isn't safe to call concurrently against
// itself, even though the resulting Session handles are independent of
// each other once created.
func (w *Worker) newCloneSessionLocked(model *audiocpp.Model, fam cloneFamily) (*audiocpp.Session, error) {
	var opts *audiocpp.Options
	if len(fam.sessionOptions) > 0 {
		var err error
		opts, err = audiocpp.NewOptions(fam.sessionOptions)
		if err != nil {
			return nil, fmt.Errorf("build session options: %w", err)
		}
		defer opts.Close()
	}
	return model.Session("tts", "offline", audiocpp.BackendConfig{Backend: backendName, Threads: sessionThreads}, opts)
}

// getDesignModel returns engine's loaded instance, loading it on first use
// - loadedClone/getCloneModel's counterpart for a VoiceDesign engine (see
// loadedDesign's own doc comment). Keyed by engine.family.
func (w *Worker) getDesignModel(engine designEngine) (*loadedDesign, error) {
	key := engine.family

	w.designMu.Lock()
	if ld, ok := w.loadedDesigns[key]; ok {
		ld.inFlight++
		w.designMu.Unlock()
		return ld, nil
	}
	w.designMu.Unlock()

	// Cache miss - free room for this engine's own multi-GB weights: every
	// resident clone model (own mutex, released above - see cloneMu's own
	// "never held nested with designMu" rule, and getCloneModel's mirrored
	// step in the other direction), every aux engine, then every *other*
	// loaded design engine, since this app only ever wants one resident at
	// a time in practice (see loadedDesign's own doc comment).
	w.UnloadCloneModels()
	w.UnloadAuxEngines()
	w.evictOtherDesignEngines(key)

	w.designMu.Lock()
	defer w.designMu.Unlock()
	if ld, ok := w.loadedDesigns[key]; ok {
		// Lost the race while designMu was released above.
		ld.inFlight++
		return ld, nil
	}

	log.Printf("loading %s (%s) design engine on %s ...", engine.family, engine.modelPath, backendName)
	model, err := w.loadModel(engine.modelPath, audiocpp.ModelConfig{FamilyHint: engine.family}, nil)
	if err != nil {
		return nil, fmt.Errorf("load design model: %w", err)
	}

	poolSize := designPoolSize
	if poolSize < 1 {
		poolSize = 1
	}
	ld := &loadedDesign{model: model, engine: engine, poolSize: poolSize, avail: make(chan *audiocpp.Session, poolSize)}
	ld.inFlight++
	w.loadedDesigns[key] = ld
	log.Printf("%s design engine loaded (session pool created lazily, up to %d sessions).", engine.family, poolSize)
	return ld, nil
}

// evictOtherDesignEngines closes every loaded design engine except keep -
// see loadedDesign's own doc comment for why only one is ever meant to be
// resident at a time. Skips (leaves loaded) anything still inFlight, same
// use-after-free guard as UnloadCloneModels/UnloadDesignModels.
func (w *Worker) evictOtherDesignEngines(keep string) {
	w.designMu.Lock()
	for id, ld := range w.loadedDesigns {
		if id == keep || ld.inFlight > 0 {
			continue
		}
		ld.close()
		delete(w.loadedDesigns, id)
	}
	w.designMu.Unlock()
}

// releaseDesignModel drops the reservation getDesignModel placed on ld via
// its own inFlight field - see loadedClone.inFlight/releaseCloneModel's
// own doc comments for why this has to span the whole Design call, not
// just its own checkoutDesignSession/avail window.
func (w *Worker) releaseDesignModel(ld *loadedDesign) {
	w.designMu.Lock()
	ld.inFlight--
	w.designMu.Unlock()
}

// checkoutDesignSession returns one of ld's pooled sessions for Design to
// run a request on, creating a fresh one lazily - Worker.checkoutSession's
// own counterpart, same lazy-up-to-poolSize shape.
func (w *Worker) checkoutDesignSession(ld *loadedDesign) (*audiocpp.Session, error) {
	select {
	case s := <-ld.avail:
		return s, nil
	default:
	}

	w.designMu.Lock()
	if ld.created < ld.poolSize {
		session, err := w.newDesignSessionLocked(ld.model, ld.engine)
		if err != nil {
			w.designMu.Unlock()
			return nil, fmt.Errorf("create design session %d/%d: %w", ld.created+1, ld.poolSize, err)
		}
		ld.sessions = append(ld.sessions, session)
		ld.created++
		w.designMu.Unlock()
		return session, nil
	}
	w.designMu.Unlock()

	return <-ld.avail, nil
}

// newDesignSessionLocked creates one fresh session for engine on model.
// Callers must already hold w.designMu - newCloneSessionLocked's own
// doc comment covers why (audiocpp_session_create isn't safe to call
// concurrently against one model handle).
func (w *Worker) newDesignSessionLocked(model *audiocpp.Model, engine designEngine) (*audiocpp.Session, error) {
	var opts *audiocpp.Options
	if len(engine.sessionOptions) > 0 {
		var err error
		opts, err = audiocpp.NewOptions(engine.sessionOptions)
		if err != nil {
			return nil, fmt.Errorf("build session options: %w", err)
		}
		defer opts.Close()
	}
	designTask := engine.designTask
	if designTask == "" {
		designTask = "vdes"
	}
	return model.Session(designTask, "offline", audiocpp.BackendConfig{Backend: backendName, Threads: sessionThreads}, opts)
}

// registerAuxEngines builds every known auxEngine (see its own doc
// comment), called once from New. Adding a new generation-only family to
// this app is now just one entry here plus that family's own thin
// Worker.<Name> method (acestep.go/stableaudio.go) - no new pooling/
// eviction machinery to hand-write.
func registerAuxEngines() map[string]*auxEngine {
	specs := []*auxEngine{
		{key: "ace_step", family: "ace_step", modelPath: aceStepModelPath, task: "gen", poolSize: aceStepPoolSize},
		{key: "stable_audio_music", family: "stable_audio", modelPath: stableAudioMusicModelPath, task: "gen", poolSize: stableAudioMusicPoolSize},
		{key: "stable_audio_sfx", family: "stable_audio", modelPath: stableAudioSFXModelPath, task: "gen", poolSize: stableAudioSFXPoolSize},
		{key: "stable_audio_medium", family: "stable_audio", modelPath: stableAudioMediumModelPath, task: "gen", poolSize: stableAudioMediumPoolSize},
	}
	out := make(map[string]*auxEngine, len(specs))
	for _, e := range specs {
		if e.poolSize < 1 {
			e.poolSize = 1
		}
		e.avail = make(chan *audiocpp.Session, e.poolSize)
		out[e.key] = e
	}
	return out
}

// auxEngine looks up eng by key - eng.mu is always safe to lock and eng's
// own fixed fields (family/modelPath/task/poolSize) are safe to read
// without it once registerAuxEngines has run, since the map itself is
// never mutated after New (see Worker.auxEngines' own doc comment).
// Panics on an unregistered key - every caller in this package passes a
// compile-time-constant string matching a registerAuxEngines entry, so an
// unregistered key is a programming error, not a runtime condition to
// handle gracefully.
func (w *Worker) auxEngine(key string) *auxEngine {
	eng, ok := w.auxEngines[key]
	if !ok {
		panic("audioworker: unregistered aux engine " + key)
	}
	return eng
}

// getAux returns eng loaded and ready, loading it on first use -
// loadedClone/loadedDesign's own counterpart, generalized: evicts every
// clone model, every design engine, and every *other* aux engine first
// (this app's GPU never holds more than one heavy model family resident
// at once), then loads eng's own weights if still needed after that.
// Reserves eng via inFlight++ before returning (mirrors loadedClone.
// inFlight's own "spans the whole call, not just checkout" contract) -
// callers must call releaseAux exactly once when done.
func (w *Worker) getAux(eng *auxEngine) error {
	eng.mu.Lock()
	if eng.model != nil {
		eng.inFlight++
		eng.mu.Unlock()
		return nil
	}
	eng.mu.Unlock()

	w.UnloadCloneModels()
	w.UnloadDesignModels()
	w.unloadOtherAux(eng)

	eng.mu.Lock()
	defer eng.mu.Unlock()
	if eng.model != nil {
		// Lost the race while eng.mu was released above.
		eng.inFlight++
		return nil
	}

	log.Printf("loading %s (%s) on %s ...", eng.family, eng.modelPath, backendName)
	model, err := w.loadModel(eng.modelPath, audiocpp.ModelConfig{FamilyHint: eng.family}, nil)
	if err != nil {
		return fmt.Errorf("load %s model: %w", eng.family, err)
	}
	eng.model = model
	eng.inFlight++
	log.Printf("%s loaded (session pool created lazily, up to %d sessions).", eng.family, eng.poolSize)
	return nil
}

// releaseAux drops the reservation getAux placed via inFlight.
func (w *Worker) releaseAux(eng *auxEngine) {
	eng.mu.Lock()
	eng.inFlight--
	eng.mu.Unlock()
}

// checkoutAuxSession returns one of eng's pooled sessions, creating a
// fresh one lazily - Worker.checkoutSession's own counterpart, same
// lazy-up-to-poolSize shape.
func (w *Worker) checkoutAuxSession(eng *auxEngine) (*audiocpp.Session, error) {
	select {
	case s := <-eng.avail:
		return s, nil
	default:
	}

	eng.mu.Lock()
	if eng.created < eng.poolSize {
		session, err := eng.model.Session(eng.task, "offline", audiocpp.BackendConfig{Backend: backendName, Threads: sessionThreads}, nil)
		if err != nil {
			eng.mu.Unlock()
			return nil, fmt.Errorf("create %s session %d/%d: %w", eng.family, eng.created+1, eng.poolSize, err)
		}
		eng.sessions = append(eng.sessions, session)
		eng.created++
		eng.mu.Unlock()
		return session, nil
	}
	eng.mu.Unlock()

	return <-eng.avail, nil
}

// unloadAux closes eng's model, if loaded and not currently in flight
// (same use-after-free guard as UnloadCloneModels/UnloadDesignModels).
func (w *Worker) unloadAux(eng *auxEngine) {
	eng.mu.Lock()
	if eng.model != nil && eng.inFlight == 0 {
		eng.close()
	}
	eng.mu.Unlock()
}

// unloadOtherAux closes every registered aux engine except keep - called
// by getAux before loading its own weights, the same "evict every other
// kind first" rule getCloneModel/getDesignModel already follow against
// each other.
func (w *Worker) unloadOtherAux(keep *auxEngine) {
	for _, eng := range w.auxEngines {
		if eng == keep {
			continue
		}
		w.unloadAux(eng)
	}
}

// UnloadAuxEngines closes every registered aux engine - called by
// UnloadAll and by getCloneModel/getDesignModel before loading their own
// multi-GB weights.
func (w *Worker) UnloadAuxEngines() {
	for _, eng := range w.auxEngines {
		w.unloadAux(eng)
	}
}

func (w *Worker) touchLRULocked(cloneModel string) {
	if el, ok := w.lruElem[cloneModel]; ok {
		w.lru.MoveToFront(el)
		return
	}
	w.lruElem[cloneModel] = w.lru.PushFront(cloneModel)
}

// evictExcessLocked keeps w.loaded within maxExtraClones resident entries
// beyond defaultCloneModel (never evicted), evicting least-recently-used
// ones first - mirrors tts-service's _evict_excess_clone_models. Each
// loaded clone_model is backed by a multi-GB checkpoint, so holding an
// unbounded number of them resident is what previously risked exhausting
// VRAM/host RAM on a "compare backends" session.
func (w *Worker) evictExcessLocked() {
	cap := w.maxExtraClones
	if cap < 0 {
		cap = 0
	}
	w.evictToCapLocked(cap)
}

// evictBeforeLoadLocked frees up room for cloneModel - not yet in w.loaded -
// before its own (multi-GB) weights are actually loaded, rather than only
// evicting afterward the way evictExcessLocked's own post-load call still
// also does as a backstop. Loading a brand-new extra clone_model while
// every existing one is still resident needs VRAM for all of them at once
// for the duration of that load call, which can fail outright on hardware
// with no headroom to spare (this box's GPU has none - see
// backend/CLAUDE.md) - evicting first means the load itself only ever needs
// headroom for one clone_model beyond the steady-state cap, not one beyond
// however many were already resident. A no-op for defaultCloneModel, which
// evictExcessLocked never evicts anyway and so never counts against the cap.
func (w *Worker) evictBeforeLoadLocked(cloneModel string) {
	if cloneModel == w.defaultCloneModel {
		return
	}
	cap := w.maxExtraClones - 1
	if cap < 0 {
		cap = 0
	}
	w.evictToCapLocked(cap)
}

// evictToCapLocked is evictExcessLocked/evictBeforeLoadLocked's shared
// eviction loop, evicting least-recently-used non-default clone models
// until at most cap of them remain resident.
func (w *Worker) evictToCapLocked(cap int) {
	extraCount := 0
	for el := w.lru.Front(); el != nil; el = el.Next() {
		if el.Value.(string) != w.defaultCloneModel {
			extraCount++
		}
	}
	for extraCount > cap {
		var victimEl *list.Element
		for el := w.lru.Back(); el != nil; el = el.Prev() {
			name := el.Value.(string)
			if name == w.defaultCloneModel {
				continue
			}
			if w.loaded[name].inFlight > 0 {
				// A Generate call is actively using this clone_model
				// right now - closing it (or a session it's mid-Run on)
				// out from under that call would be a use-after-free.
				// See loadedClone.inFlight's own doc comment. Skip past
				// it and look for an idler LRU victim instead.
				continue
			}
			victimEl = el
			break
		}
		if victimEl == nil {
			// Every non-default clone_model beyond cap is currently busy
			// - nothing safe to evict right now rather than risk a
			// use-after-free. Left over-cap; the next eviction pass (the
			// next getCloneModel call) retries once something frees up.
			break
		}
		victim := victimEl.Value.(string)
		w.lru.Remove(victimEl)
		delete(w.lruElem, victim)
		lc := w.loaded[victim]
		delete(w.loaded, victim)
		extraCount--
		log.Printf("evicting clone model %s (least recently used, over max extra clone models=%d) ...", victim, cap)
		lc.close()
	}
}

// loadedCloneModelIDs reports which clone_models are currently resident, for
// GET /health.
func (w *Worker) loadedCloneModelIDs() []string {
	w.cloneMu.Lock()
	defer w.cloneMu.Unlock()
	out := make([]string, 0, len(w.loaded))
	for id := range w.loaded {
		out = append(out, id)
	}
	return out
}

// languageOption mirrors clone_backends/audiocpp.py's _language(): "Auto" (or
// empty) is a real, accepted value for qwen3_tts (Model.Languages() lists it
// verbatim); higgs_audio_tts "auto-handles supported languages" per its own
// docs, so the same passthrough is harmless. Anything else must be a
// family's own full language name, not an ISO code (e.g. "english", not
// "en") - audio.cpp's talker throws "unsupported language" otherwise.
func languageOption(language string) string {
	lang := strings.TrimSpace(language)
	if lang == "" {
		return "Auto"
	}
	if strings.EqualFold(lang, "auto") {
		return "Auto"
	}
	return strings.ToLower(lang)
}
