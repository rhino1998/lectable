package audioworker

import (
	"log"
	"os"
	"strconv"
)

// One entry per clone_model id the Go backend can ask for (see
// backend/internal/voices for where these ids are surfaced to callers) -
// mirrors tts-service/clone_backends/__init__.py's BACKENDS registry
// exactly, now that audio.cpp is called in-process via audiocpp-go instead
// of over HTTP to a Python process.
type cloneFamily struct {
	// ggml family name audio.cpp's registry loads by.
	family string
	// Path to this family's cloning GGUF.
	modelPath string
	// Session-level options - widens audio.cpp's own internal voice-prompt/
	// reference cache (keyed by hash of reference audio + text) beyond its
	// 1-slot default, since switching between presets on one warm session
	// is the common case here, not the exception.
	sessionOptions map[string]string
	// Request-level options applied to every generate() call for this
	// family, before any caller-supplied options.
	defaultRequestOptions map[string]string
	// refTextOption is the request-option key this family reads the
	// reference clip's transcript from - "reference_text" for every family
	// except pocket_tts, which looks for "voice_clone_text" instead (see
	// pocket_tts's own apply_generation_options). Empty defaults to
	// "reference_text" in Generate.
	refTextOption string
	// noLanguageOption: true if this family's own request-option contract
	// doesn't include "language" at all - breeze_tts validates every
	// request option strictly against its model spec and throws
	// "unknown BreezeTTS request option: language" if it's set, unlike
	// qwen3_tts/higgs_audio_tts/pocket_tts, which silently accept it (it
	// auto-detects zh/en from the text itself instead). When true, Generate
	// skips languageOption entirely rather than passing it through
	// audiocpp.Request.SetText.
	noLanguageOption bool
	// noReference: true for a family with no voice-cloning/reference-audio
	// capability at all (its own model_specs "tasks" list has no "clone" -
	// soprano_tts today) - a single fixed built-in voice, every call
	// rendering identically regardless of caller. Generate skips
	// SetVoiceAudio/refTextOption entirely rather than sending a reference
	// clip the family has no request option to accept at all. The caller
	// (jobs.Manager.generate, via voicerefs) still resends whatever
	// reference-clip bytes it has on hand for this clone_model, same as
	// every other family - they're simply ignored here. See
	// backend/internal/voices' own NoCloneModels for the cmd/server-side
	// half of this (voicerefs.Regenerate skips VoiceDesign for these
	// clone_models, since there's no design-capable engine to render a
	// meaningful reference clip from in the first place).
	noReference bool
	// instructOption is the request-option key this family reads a
	// clone-time style instruction from, alongside a reference clip in the
	// very same call - "instruction" for breeze_tts, which is the only
	// family in this registry whose own model_specs accepts both a voice
	// reference *and* an instruction together (confirmed directly against
	// model_specs/breeze_tts.json's request options and its own "Voice
	// cloning" CLI example, which passes --voice-ref/--reference-text
	// alongside --request-option instruction=... in one call) - every
	// other family here keeps cloning and instruction-driven design as
	// mutually exclusive checkpoints/tasks (qwen3_tts's Base clone
	// checkpoint has no instruct option at all; its own VoiceDesign
	// checkpoint takes an instruct but no reference). Empty (every family
	// but breeze) means Generate never sends GenerateRequest.Instruct for
	// this family at all, even if a caller sets one - mirrors noReference's
	// own "caller may resend something this family can't use" shape.
	instructOption string
	// instructGuidanceScale, when non-empty, is a "guidance_scale" request
	// option applied only alongside instructOption - i.e. only on a call
	// that actually sends a non-empty Instruct (see Worker.Generate) -
	// never on a plain clone call with no instruction. Lets a family whose
	// own default guidance strength is too weak to make an attached
	// instruction actually audible turn it up specifically for that case,
	// without changing every other call this family already serves. Empty
	// (every family but breeze) is a no-op - see breezeCloneGuidanceScale's
	// own doc comment.
	instructGuidanceScale string
	// poolSizeOverride, if > 0, replaces Config.ClonePoolSize as this
	// family's own session-pool cap (see loadedClone.poolSize/Worker.
	// checkoutSession) - a heavier family can need a lower concurrency cap
	// than the process-wide default even though sessions are now created
	// lazily (see getCloneModel): each Higgs session still keeps its own
	// resident KV-cache/decode-graph state once actually created, so 4
	// concurrent Higgs sessions (the global default, sized for lighter
	// families) is more simultaneous VRAM than this box's GPU has headroom
	// for once a clone/design/LLM model is also resident - see the
	// "audiocpp-higgs" entry below and backend/CLAUDE.md.
	poolSizeOverride int
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// higgsMaxTokens: Higgs's own built-in max_tokens default (2048) can run out
// mid-paragraph on longer or pause-heavy text ("Higgs TTS generation reached
// max_tokens (N) before EOC for this text chunk" - observed live against
// real book paragraphs, not a hypothetical), since the framework's own
// long-text chunker (session.cpp's chunk_text_request, ~1024 characters per
// chunk by default) applies this budget fresh to *each* internal chunk
// independently, and a character count that size still isn't a tight bound
// on how many AR tokens a chunk actually takes to render. A stored
// paragraph here can be arbitrarily long (see internal/epub - there's no
// app-side length cap; audio.cpp's own chunker is relied on to subdivide it
// into however many chunks it needs, each independently budgeted at this
// same value), so this value is chosen for one chunk's own needs, not
// scaled to any particular paragraph length.
//
// This was raised to 4096 then 8192 early on (still not always enough - a
// character voiced with a slow, deliberate instructed delivery needs
// meaningfully more generated frames per word than a normal pace does,
// confirmed live against a real, comparatively short ~650-char/119-word
// paragraph that still ran out even at 8192) before a live A/B sweep
// (effective RTF measured against real queue throughput, book "Jake's
// Magical Market 3", varying this alongside higgsClonePoolSize/
// cloneCacheSlots) found the opposite direction actually wins: 1024 nearly
// halved effective RTF versus 8192 (0.48 vs 0.995 at pool size 1) with zero
// overflow/OOM/restart events in the process - a stalled/runaway decode
// (backend/CLAUDE.md's own documented "grinds on far longer than the text
// warrants" failure mode) burns real wall-clock time and RSS in proportion
// to whatever ceiling it grinds toward, so a *smaller* ceiling fails faster
// and cheaper on the rare paragraph that needs it, while an ordinary
// paragraph practically never brushes this budget in the first place
// (audio.cpp's own per-chunk chars-to-frames ratio at a normal pace stays
// well under even 1024). jobs.Manager.generateClone's own text-bisection
// fallback (splitting an overflowing paragraph at a sentence boundary and
// generating each half separately, concatenating the results) is what
// actually recovers from an overflow at this now-tighter budget, so a
// paragraph that needs more than 1024 tokens for one chunk still finishes,
// just via one extra round trip instead of failing outright.
//
// Deliberately never raised above this default for an individual request:
// any value requested above whatever this session's own default is
// reproducibly segfaulted audiocpp_session_run deep inside audio.cpp
// itself when tried (confirmed via three separate crash traces, all at the
// exact same native call site, one of them with the raised value set as
// the *original* session default from a cold start - ruling out a
// stale-CUDA-graph-shape explanation for why a mid-session change
// specifically would be unsafe, the value itself is unsafe however it's
// set) - rather than returning a graceful "still not enough" error the way
// this default does.
var higgsMaxTokens = envOr("LECTABLE_AUDIOCPP_HIGGS_MAX_TOKENS", "1024")

// higgsClonePoolSize overrides Config.ClonePoolSize's process-wide default
// (4, sized for lighter clone families) down to 1 for Higgs specifically -
// see cloneFamily.poolSizeOverride's own doc comment: each Higgs session
// keeps a genuinely heavy resident KV-cache/decode-graph, and this box's
// 24GB GPU (an AMD RX 7900 XTX, per ttsworker's own ggml_cuda_init startup
// log - not otherwise documented in backend/CLAUDE.md) has no spare VRAM
// headroom for several of those at once on top of whatever design/align/LLM
// model also happens to be loaded. 2 concurrent sessions' own combined
// resident footprint pushes past the card's own VRAM ceiling, which doesn't
// just risk an outright allocation failure (see the Gioni incident below) -
// short of that, it spills the overflow into system RAM (GTT/unified
// memory), which is real, working memory but at system memory bandwidth
// instead of the GPU's own, so *every* concurrent session pays a bandwidth
// stall on top of its own compute cost, not just the one that happened to
// land in the slower tier. That's why 2 sessions never actually delivered
// proportionally more throughput than 1 in practice (see the sweep below) -
// the intuitive "more concurrency = more throughput" case doesn't hold once
// the working set no longer fits in VRAM at all.
//
// Was 2 originally (headroom for a second concurrent session, still well
// short of the process-wide default of 4), briefly dropped to 1 during the
// Gioni incident (backend/CLAUDE.md's own account of it) while an oversized
// reference clip was the live cause of every Higgs prefill allocation
// failing outright, then raised back to 2 once the real fix (capping
// reference-clip length - see voicerefs.maxRefClipSeconds/speakerattr.
// maxRefLineWords) addressed the actual VRAM cause. Settled back at 1 after
// a live A/B sweep (effective RTF against real queue throughput, alongside
// higgsMaxTokens/cloneCacheSlots) found pool size barely matters once
// higgsMaxTokens dropped to 1024: 1 session measured 0.48 effective RTF
// versus 2 sessions' own 0.50-0.56 at the same token budget (a first,
// pool-size-1 measurement of 1.12 was a false signal - contaminated by a
// separate, since-fixed bug in preprocessVoiceProvisionPhase re-dispatching
// voice-provision tasks for already-provisioned characters, not the pool
// size itself) - not worth the VRAM-bandwidth-stall risk above for
// concurrency this box's GPU can't actually back with real headroom.
// Revisit upward again only on hardware with enough VRAM to hold 2+
// sessions' own combined working set without spilling over.
var higgsClonePoolSize = envIntOr("LECTABLE_AUDIOCPP_HIGGS_CLONE_POOL_SIZE", 1)

// qwen3ClonePoolSize/omnivoiceClonePoolSize/breezeClonePoolSize are the same
// per-family override as higgsClonePoolSize above, sized per family rather
// than left at Config.ClonePoolSize's process-wide default (4): all three
// get a smaller-but-still-concurrent pool of 2 - breeze_tts (the heaviest
// of the three, see its own model_specs) was capped at 1 until profiled,
// raised to 2 to match the other two now that its own design-engine
// sibling (designPoolSize, same default) has been profiled live at 2
// without incident - still worth watching for the same VRAM-bandwidth-
// spillover risk higgsClonePoolSize's own doc comment documents (this
// box's GPU has no spare headroom to lose to that for real, sustained
// concurrent load), just not yet observed for breeze specifically.
var qwen3ClonePoolSize = envIntOr("LECTABLE_AUDIOCPP_QWEN3_CLONE_POOL_SIZE", 2)
var omnivoiceClonePoolSize = envIntOr("LECTABLE_AUDIOCPP_OMNIVOICE_CLONE_POOL_SIZE", 2)
var breezeClonePoolSize = envIntOr("LECTABLE_AUDIOCPP_BREEZE_CLONE_POOL_SIZE", 2)

// breezeCloneGuidanceScale is breeze_tts's own classifier-free guidance-
// scale request option (model_specs/breeze_tts.json, default 1.0),
// overridden here the same way designEngines["breeze_tts"]'s own
// defaultRequestOptions already raises it for VoiceDesign - "for stronger
// adherence to the design instruction" there; here, for stronger adherence
// to a clone-time instruction (cloneFamily.instructOption - see
// store.Book.InstructCharacterVoices) alongside the reference clip.
// Deliberately wired in as cloneFamily.instructGuidanceScale (applied only
// when a call actually sends a non-empty Instruct - see Worker.Generate),
// not cloneFamily.defaultRequestOptions (applied to every call for the
// family regardless): guidance_scale is a general guidance strength, not
// an instruction-only weight, so bumping it unconditionally would also
// change plain audiocpp-breeze-tts cloning with no instruction at all - an
// existing, separate use case (any book/preset on this clone model, with
// or without InstructCharacterVoices) this change has no reason to touch.
// "4" matches the design engine's own value as a starting point - not yet
// A/B'd live against a plain instructed clone the way that value was for
// VoiceDesign specifically.
var breezeCloneGuidanceScale = envOr("LECTABLE_AUDIOCPP_BREEZE_CLONE_GUIDANCE_SCALE", "4")

// designPoolSize is the most independent audiocpp sessions any one loaded
// design engine (loadedDesign, worker.go) will ever create, lazily -
// Config.ClonePoolSize's own counterpart for Worker.Design, now that it
// reuses a warm, cached model+session pool instead of loading a fresh
// model and closing it again on every single call. See design.go's own
// doc comment for why that used to be true (a rare, one-off render not
// worth holding a second multi-GB model resident for) and
// jobs.maxDesignInFlight's own doc comment for the real crash a burst of
// concurrent Design calls caused under that old shape - concurrent calls
// each racing registry.LoadModel (a corrupted jump inside
// audiocpp_model_load, confirmed live). loadMu (worker.go) already
// serializes the actual LoadModel call process-wide independently of this
// pool, so this pool's own job is narrower than it used to need to be:
// letting *already-loaded* concurrent Design calls actually run
// concurrently via independent Session handles, the same reasoning
// higgsClonePoolSize's own doc comment gives for Generate. Should match
// jobs.maxDesignInFlight - keep the two in step the same way
// higgsClonePoolSize/internal/jobs.maxInFlight already need to be.
var designPoolSize = envIntOr("LECTABLE_AUDIOCPP_DESIGN_POOL_SIZE", 2)

// cloneCacheSlots bounds each family's own prepared-reference-audio/voice-
// prompt cache (audio.cpp's own per-family "*_cache_slots" session option -
// qwen3_tts.voice_prompt_cache_slots, higgs_audio_tts.reference_cache_slots,
// pocket_tts.voice_state_cache_slots, breeze_tts.reference_cache_slots),
// keyed by hash of (reference audio, reference text) so switching between
// presets on one warm session reuses an already-prepared entry instead of
// recomputing it. Raised well past every family's own tiny built-in
// default (1, or 4 for some other audio.cpp families - see their own
// model_specs) since this app routinely has far more than a handful of
// distinct presets/characters active in one session (a book's whole
// speaker roster can run into the dozens) - each cache miss beyond the
// slot count evicts the least-recently-used entry and pays for a fresh
// prepare on next use, not a correctness issue, just avoidable latency.
// Each slot holds a small prepared representation (an encoded reference,
// not the raw audio or a model-sized allocation), so this is a modest,
// bounded memory cost even at this size - not remotely comparable to
// resident model VRAM.
var cloneCacheSlots = envOr("LECTABLE_AUDIOCPP_CACHE_SLOTS", "32")

// cloneModelFamilies maps a public clone_model id (what a voice preset
// actually stores/sends) to the family key in cloneFamilies below.
var cloneModelFamilies = map[string]string{
	"audiocpp-qwen3-0.6b":  "audiocpp-qwen3",
	"audiocpp-higgs-4b":    "audiocpp-higgs",
	"audiocpp-pocket-100m": "audiocpp-pocket",
	"audiocpp-breeze-tts":  "audiocpp-breeze",
	"audiocpp-omnivoice":   "audiocpp-omnivoice",
	"audiocpp-soprano":     "audiocpp-soprano",
}

var cloneFamilies = map[string]cloneFamily{
	"audiocpp-qwen3": {
		family: "qwen3_tts",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_QWEN3_MODEL_PATH",
			"/home/rhino/audio.cpp/models/Qwen3-TTS-12Hz-0.6B-Base-GGUF/qwen3-tts-12hz-0.6b-base-q8_0.gguf",
		),
		sessionOptions:   map[string]string{"qwen3_tts.voice_prompt_cache_slots": cloneCacheSlots},
		poolSizeOverride: qwen3ClonePoolSize,
	},
	"audiocpp-higgs": {
		family: "higgs_audio_tts",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_HIGGS_MODEL_PATH",
			"/home/rhino/audio.cpp/models/Higgs-Audio-v3-TTS-4B-GGUF/higgs-audio-v3-tts-4b-q8_0.gguf",
		),
		sessionOptions: map[string]string{
			"higgs_audio_tts.reference_cache_slots": cloneCacheSlots,
		},
		defaultRequestOptions: map[string]string{"max_tokens": higgsMaxTokens},
		poolSizeOverride:      higgsClonePoolSize,
	},
	// PocketTTS (Kyutai's 100M-parameter package, see audio.cpp's
	// model_specs/pocket_tts.json) - by far the fastest family this worker
	// loads (audio.cpp's own README benchmarks put it at 30-48x realtime,
	// vs. Higgs/Qwen3's much heavier decode graphs), which is exactly why
	// internal/voices' FastPresetID clones through this family instead of
	// the book's own (usually much slower) chosen engine: a throwaway
	// length-estimate sample only needs a plausible chars/sec ratio, not
	// this book's actual narrator, so there's no reason to pay for a slow
	// engine's real decode just to measure that.
	"audiocpp-pocket": {
		family: "pocket_tts",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_POCKET_MODEL_PATH",
			"/home/rhino/audio.cpp/models/PocketTTS-GGUF/english/pocket-tts-english-q8_0.gguf",
		),
		sessionOptions: map[string]string{"pocket_tts.voice_state_cache_slots": cloneCacheSlots},
		// English/German/Italian/Portuguese/Spanish are separate model
		// packages (see pocket_tts.json's own "packages" list), not a
		// per-request language switch - the default download above is the
		// English package, so this is fixed at load time rather than
		// exposed as a per-call option the way qwen3_tts/higgs_audio_tts's
		// own language passthrough is.
		refTextOption: "voice_clone_text",
	},
	// BreezeTTS 2 (audio-cpp/audio.cpp-gguf's "breeze_tts" family) - a
	// prompt-audio cloning engine like qwen3_tts/higgs_audio_tts, wired in
	// as an additional clone_model option alongside them. Its own
	// "reference_text" request-option name already matches this package's
	// default (refTextOption left empty below), unlike pocket_tts above.
	"audiocpp-breeze": {
		family: "breeze_tts",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_BREEZE_MODEL_PATH",
			"/home/rhino/audio.cpp/models/Breeze-TTS-2-GGUF/breeze-tts-2-q8_0.gguf",
		),
		sessionOptions:   map[string]string{"breeze_tts.reference_cache_slots": cloneCacheSlots},
		noLanguageOption: true,
		poolSizeOverride: breezeClonePoolSize,
		// See instructOption's own doc comment - breeze_tts is the one
		// family here whose clone task also accepts a style instruction
		// alongside the reference clip in the same call.
		instructOption: "instruction",
		// See breezeCloneGuidanceScale's own doc comment.
		instructGuidanceScale: breezeCloneGuidanceScale,
	},
	// OmniVoice (k2-fsa, audio-cpp/audio.cpp-gguf's "omnivoice" family) -
	// a massively multilingual (600+ languages) zero-shot cloning/design
	// engine. Its own "reference_text" request-option name already
	// matches this package's default (refTextOption left empty below),
	// and it has no dedicated *_cache_slots session option at all (see
	// its own docs/models/omnivoice.md's Options table) - nothing to set
	// here beyond the model path.
	"audiocpp-omnivoice": {
		family: "omnivoice",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_OMNIVOICE_MODEL_PATH",
			"/home/rhino/audio.cpp/models/OmniVoice-GGUF/omnivoice-q8_0.gguf",
		),
		poolSizeOverride: omnivoiceClonePoolSize,
	},
	// SopranoTTS (WalkingCat/Soprano-1.1-80M-GGUF, audio.cpp's "soprano_tts"
	// family) - an ultra-lightweight (~80M) English-only engine with a
	// single fixed built-in voice: no clone/design capability at all (its
	// own model_specs/soprano_tts.json "tasks" list is just ["tts"]), so
	// every call renders identically regardless of caller - see noReference
	// below. Benchmarked (audiocpp_cli --metrics, hip backend, RX 7900 XTX)
	// at RTF 0.10 (~10x realtime) on a 168-char sentence - slower than
	// audiocpp-pocket's RTF 0.07 (~15x) but still well clear of
	// audiocpp-higgs's RTF 0.27 (~3.7x). Rejects a "language" request
	// option outright ("unknown Soprano request option: language",
	// confirmed live) despite being single-language anyway - noLanguageOption
	// set for the same reason as breeze_tts above. No dedicated
	// *_cache_slots session option (see soprano_tts.json's own "options" -
	// nothing to widen here, same as omnivoice above).
	"audiocpp-soprano": {
		family: "soprano_tts",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_SOPRANO_MODEL_PATH",
			"/home/rhino/audio.cpp/models/Soprano-1.1-80M-GGUF/soprano-1.1-80m-q8_0.gguf",
		),
		noLanguageOption: true,
		noReference:      true,
	},
}

// designEngine bundles everything Design (design.go) needs to render a
// fresh reference clip via one VoiceDesign-capable family - see
// designEngines/activeDesignEngine below for why more than one now exists.
type designEngine struct {
	// ggml family name audio.cpp's registry loads by.
	family string
	// Path to this family's VoiceDesign GGUF.
	modelPath string
	// Session-level options, as cloneFamily.sessionOptions.
	sessionOptions map[string]string
	// Request-level options applied to every Design() call for this
	// engine, before instruct/seed - e.g. breeze_tts's guidance_scale.
	defaultRequestOptions map[string]string
	// instructOption is the request-option key this engine reads the
	// design instruction from - "instruct" for qwen3_tts, "instruction"
	// for breeze_tts/omnivoice. Empty defaults to "instruct" in Design.
	instructOption string
	// noLanguageOption: see cloneFamily's own doc comment - same issue,
	// same fix, on the design path.
	noLanguageOption bool
	// designTask is the audiocpp session task token Design opens - "vdes"
	// for every engine so far except omnivoice, whose own session.cpp
	// hard-rejects anything but "tts" (`OmniVoice only supports
	// VoiceTaskKind::Tts`, confirmed directly against its source): a
	// design vs. clone call there is distinguished purely by whether a
	// voice reference is attached to the request, not by task kind at
	// all - see cloneFamily's own getCloneModel call, which already opens
	// every family's cloning session as "tts" for the same reason. Empty
	// defaults to "vdes" in Design.
	designTask string
}

// designEngines: qwen3_tts's own VoiceDesign checkpoint was the sole engine
// here since this worker's design.go was first written - any engine capable
// of sampling "a voice matching this instruct+seed" produces an equally
// valid clip for another family to clone the resulting timbre from, so
// there was no reason to maintain more than one. breeze_tts was wired in
// as an alternate and is now the default (see designEngineID below) -
// qwen3_tts stays registered/selectable to fall back to it. Selection is
// env-var controlled only, for now, not yet exposed as a per-preset choice.
var designEngines = map[string]designEngine{
	"qwen3_tts": {
		family: "qwen3_tts",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_QWEN3_DESIGN_MODEL_PATH",
			"/home/rhino/audio.cpp/models/Qwen3-TTS-12Hz-1.7B-VoiceDesign-GGUF/qwen3-tts-12hz-1.7b-voicedesign-q8_0.gguf",
		),
		// weight_type left at its own "native" default (this checkpoint's
		// own q8_0 quantization) - an earlier attempt forced f32 here as a
		// fix for "audiocpp_session_run: runtime error: Qwen3 sampler has
		// no finite logits" crashes seen during voice provisioning, but
		// that's been reverted; revisit (narrow to specific presets, or a
		// different weight_type) if those crashes resurface.
	},
	"breeze_tts": {
		family: "breeze_tts",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_BREEZE_DESIGN_MODEL_PATH",
			"/home/rhino/audio.cpp/models/Breeze-TTS-2-GGUF/breeze-tts-2-q8_0.gguf",
		),
		// guidance_scale: breeze_tts's own classifier-free guidance-scale
		// request option (default 1.0 per its model spec) - raised well
		// above default for stronger adherence to the design instruction.
		defaultRequestOptions: map[string]string{"guidance_scale": "4"},
		instructOption:        "instruction",
		noLanguageOption:      true,
	},
	// OmniVoice - see cloneFamilies' own "audiocpp-omnivoice" entry for
	// the family generally. designTask "tts": OmniVoice's session.cpp
	// throws outright for any task kind other than Tts, including "vdes"
	// (VoiceDesign) - a design call there is just a "tts" request with no
	// voice reference attached and an "instruction" option set instead
	// (see instructOption below), the same shape its own cloning call
	// uses with a reference attached (see getCloneModel's own "tts"
	// session, shared by every family for exactly this reason).
	"omnivoice": {
		family: "omnivoice",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_OMNIVOICE_MODEL_PATH",
			"/home/rhino/audio.cpp/models/OmniVoice-GGUF/omnivoice-q8_0.gguf",
		),
		instructOption: "instruction",
		designTask:     "tts",
	},
}

// designEngineID selects which designEngines entry activeDesignEngine
// resolves to - "breeze_tts" (the default, as of its own guidance_scale=4
// tuning above) or "qwen3_tts" (the original engine, still selectable to
// fall back to it). Falls back to "qwen3_tts" for an unrecognized value.
var designEngineID = envOr("LECTABLE_AUDIOCPP_DESIGN_ENGINE", "breeze_tts")

var activeDesignEngine = func() designEngine {
	if e, ok := designEngines[designEngineID]; ok {
		return e
	}
	log.Printf("audioworker: unknown LECTABLE_AUDIOCPP_DESIGN_ENGINE=%q, falling back to qwen3_tts", designEngineID)
	return designEngines["qwen3_tts"]
}()

// resolveDesignEngine picks which designEngines entry Design (design.go)
// actually uses for one call: designModel, when non-empty, lets a caller
// pin a specific preset to one engine (e.g. internal/voices' own
// velvet-narrator, kept on qwen3_tts - see voices.Preset.DesignModel's own
// doc comment) regardless of activeDesignEngine's own process-wide
// default; "" (most presets) defers to that default. An unrecognized,
// non-empty designModel also falls back to it, logged the same way an
// unrecognized LECTABLE_AUDIOCPP_DESIGN_ENGINE does.
func resolveDesignEngine(designModel string) designEngine {
	if designModel == "" {
		return activeDesignEngine
	}
	if e, ok := designEngines[designModel]; ok {
		return e
	}
	log.Printf("audioworker: unknown design model %q, falling back to process default", designModel)
	return activeDesignEngine
}

// aceStepModelPath/aceStepPoolSize configure the single ACE-Step music
// engine (acestep.go/loadedAceStep) - see docs/models/ace_step.md in the
// local audio.cpp checkout. Turbo (the guidance-distilled, fastest DiT
// variant) q8_0 by default - Base/XL are meaningfully larger and slower,
// and this app only ever drives ACE-Step's text2music route (see
// Worker.Music's own doc comment), which has no need for the extra
// editing-route fidelity a bigger DiT buys. Pool size defaults to 1, same
// reasoning as every other family here - a multi-step diffusion call, not
// a single autoregressive decode, on hardware with no VRAM headroom to
// spare.
var aceStepModelPath = envOr(
	"LECTABLE_AUDIOCPP_ACE_STEP_MODEL_PATH",
	"/home/rhino/audio.cpp/models/ACE-Step1.5-GGUF/turbo/ace-step-1.5-turbo-q8_0.gguf",
)
var aceStepPoolSize = envIntOr("LECTABLE_AUDIOCPP_ACE_STEP_POOL_SIZE", 1)

// stableAudioMusicModelPath/stableAudioMusicPoolSize configure the
// "stable_audio_music" auxEngine (Stable Audio 3, registered by
// registerAuxEngines in worker.go) - see docs/models/stable_audio.md in
// the local audio.cpp checkout. Small/Music by default (not Medium - see
// Worker.StableAudioMusic's own doc comment for why only the plain
// text-to-audio path is wired at all), q8_0.
//
// Pool size 1, not 2: confirmed live (two SIGSEGV crashes, both inside
// audiocpp.(*Session).Run -> audiocpp_session_run, both right after a
// fresh stable_audio load, both under concurrent Stable Audio traffic -
// see server.log) that running 2 pooled sessions concurrently against
// this family segfaults ttsworker - almost certainly the same general
// class of native audio.cpp bug this repo already has one confirmed,
// root-caused, locally-patched instance of for a completely different
// mechanism (higgs_audio_tts's HIP-graph-cache eviction, see
// backend/CLAUDE.md's "ttsworker / audioworker / llmworker" section) -
// but this one hasn't been root-caused yet, just contained. Reverted from
// 2 (which let two concurrent Stable Audio calls run without one queuing
// behind the other) back to every other family's own default of 1 to
// unblock; raise it again only once the concurrency bug itself is found
// and fixed upstream in /home/rhino/audio.cpp.
var stableAudioMusicModelPath = envOr(
	"LECTABLE_AUDIOCPP_STABLE_AUDIO_MUSIC_MODEL_PATH",
	"/home/rhino/audio.cpp/models/Stable-Audio-3-Small-Music-GGUF/stable-audio-3-small-music-q8_0.gguf",
)
var stableAudioMusicPoolSize = envIntOr("LECTABLE_AUDIOCPP_STABLE_AUDIO_MUSIC_POOL_SIZE", 1)

// stableAudioSFXModelPath/stableAudioSFXPoolSize configure the
// "stable_audio_sfx" auxEngine - the Small/SFX package, same family/task
// as "stable_audio_music" above (both are literally the audio.cpp
// "stable_audio" family - see docs/models/stable_audio.md's own "same gen
// task, intended for SFX prompts instead of music prompts" framing) but a
// genuinely different checkpoint, so it gets its own auxEngine entry/
// model slot rather than being a request-time switch on one shared engine.
//
// Pool size 1 - see stableAudioMusicPoolSize's own doc comment: the
// confirmed-live concurrent-session SIGSEGV is in the shared
// "stable_audio" family's own native session/Run path, not anything
// specific to which checkpoint is loaded, so every stable_audio auxEngine
// gets the same reduced pool size until it's root-caused.
var stableAudioSFXModelPath = envOr(
	"LECTABLE_AUDIOCPP_STABLE_AUDIO_SFX_MODEL_PATH",
	"/home/rhino/audio.cpp/models/Stable-Audio-3-Small-SFX-GGUF/stable-audio-3-small-sfx-q8_0.gguf",
)
var stableAudioSFXPoolSize = envIntOr("LECTABLE_AUDIOCPP_STABLE_AUDIO_SFX_POOL_SIZE", 1)

// stableAudioMediumModelPath/stableAudioMediumPoolSize configure the
// "stable_audio_medium" auxEngine - the larger Stable Audio 3 Medium
// package, same family/task/request shape as "stable_audio_music" (see
// docs/models/stable_audio.md's own "same user-facing controls as the
// music path" framing for the Medium package), just a bigger DiT - so it
// gets its own auxEngine entry/model slot the same way stable_audio_sfx
// does, not a request-time switch on the Small/Music engine.
//
// Pool size 1 - this is the checkpoint the two confirmed-live SIGSEGV
// crashes actually happened against (both under concurrent
// /stable-audio-medium traffic); see stableAudioMusicPoolSize's own doc
// comment for why every stable_audio auxEngine gets the same reduction
// rather than just this one.
var stableAudioMediumModelPath = envOr(
	"LECTABLE_AUDIOCPP_STABLE_AUDIO_MEDIUM_MODEL_PATH",
	"/home/rhino/audio.cpp/models/Stable-Audio-3-Medium-GGUF/stable-audio-3-medium-q8_0.gguf",
)
var stableAudioMediumPoolSize = envIntOr("LECTABLE_AUDIOCPP_STABLE_AUDIO_MEDIUM_POOL_SIZE", 1)

var alignerModelPath = envOr(
	"LECTABLE_ALIGNER_MODEL_PATH",
	"/home/rhino/audio.cpp/models/Qwen3-ForcedAligner-0.6B-GGUF/qwen3-forced-aligner-0.6b-q8_0.gguf",
)

// backendName picks which of audio.cpp's own GPU backends to run on -
// "cuda"/"hip"/"vulkan"/"cpu"/"best".
var backendName = envOr("LECTABLE_AUDIOCPP_BACKEND", "hip")

// sessionThreads: CPU-side thread count audio.cpp's session_create wants
// even for a GPU backend (host-side pre/post-processing).
var sessionThreads = 4
