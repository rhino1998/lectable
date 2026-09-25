package audioworker

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
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
	// sessionCallers, if > 1, lets that many Generate calls share each
	// pooled session concurrently instead of checking it out exclusively -
	// for a family whose session batches concurrent runs itself (Higgs with
	// higgs_audio_tts.decode_batch_size: its scheduler thread decodes every
	// in-flight paragraph in one step). See Worker.checkoutSession.
	sessionCallers int
	// languageTag, if set, maps languageOption's own output ("Auto" or a
	// lowercased display name like "english") plus the text being spoken
	// onto the language tag this family actually expects - fireredtts3
	// wants its tokenizer's own capitalized tags ("English"), firered_audio
	// ISO-style codes ("en"/"zh"), and both default to Chinese when given
	// nothing usable. nil passes languageOption's value through unchanged.
	languageTag func(language, text string) string
	// noRefText: true for a family that conditions on reference audio alone
	// and has no transcript request option at all (auk - its spec validates
	// request options strictly, so sending "reference_text" would throw).
	// Generate still sends the reference clip itself.
	noRefText bool
	// wrapText, if set, rewrites the text sent to the model - auk's clone
	// path has no voice-description option to pair with a reference, so its
	// text has to carry the complete upstream zero-shot instruction instead
	// of just the words to speak (see aukCloneText).
	wrapText func(text string) string
	// estimateDuration: true for a fixed-duration flow family (auk) that
	// renders exactly as many seconds as requested rather than stopping on
	// its own - Generate derives "duration_sec" from the reference clip's
	// own pace (seconds per character of its transcript) projected onto the
	// text being spoken; see estimateCloneDuration.
	estimateDuration bool
	// temperature: true if this family's own session reads a plain
	// "temperature" request option (its sampling temperature), so Generate
	// may pass a caller's per-call override through - read directly from
	// each family's session source, since most model specs don't list their
	// options. False for flow/diffusion families with no such knob
	// (fireredtts3, zipvoice, auk), omnivoice (separate class/position
	// temperatures), and firered_audio (its "temperature" only applies to
	// the understanding/ASR path).
	temperature bool
	// refAsPromptAudio: for voxcpm1/voxcpm2, which take two separate
	// reference inputs - the speaker reference (timbre only, what
	// SetVoiceAudio attaches) and a "prompt audio" clip the model continues
	// from, which is the only thing its reference transcript is paired with
	// (audio.cpp's voxcpm2 audiovae.cpp encode_prompt_audio). Upstream's
	// "ultimate clone" sends the same clip as both; when true, Generate also
	// attaches the reference clip as the request's input audio whenever a
	// transcript is available. Off by default (voxCPMPromptAudio): in
	// practice the continuation leaked the tail end of the reference clip
	// into the start of the generated paragraph, so the default is
	// timbre-only cloning, which never continues from anything.
	refAsPromptAudio bool
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// modelsDir is where every default GGUF path below lives - audio.cpp's own
// models/ directory, by default in a checkout at ~/audio.cpp.
var modelsDir = envOr("LECTABLE_AUDIOCPP_MODELS_DIR", homePath("audio.cpp", "models"))

// homePath joins elem onto the current user's home directory, falling
// back to a path relative to the working directory if it can't be found.
func homePath(elem ...string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(elem...)
	}
	return filepath.Join(append([]string{home}, elem...)...)
}

// modelPath resolves rel (e.g. "Breeze-TTS-2-GGUF/breeze-tts-2-q8_0.gguf")
// against modelsDir.
func modelPath(rel string) string {
	return filepath.Join(modelsDir, rel)
}

func envIntOr(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envFloatOr(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
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

// pocketClonePoolSize pins PocketTTS at 4 concurrent sessions explicitly
// rather than inheriting Config.ClonePoolSize's process-wide default (also
// 4 today) - a ~100M-parameter model whose sessions are cheap enough that
// 4 at once is no VRAM concern. (internal/jobs' maxInFlight is now 8, for
// Higgs batching; PocketTTS calls beyond 4 just wait for a free session.)
var pocketClonePoolSize = envIntOr("LECTABLE_AUDIOCPP_POCKET_CLONE_POOL_SIZE", 4)

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

// higgsReferenceKVSlots is higgs_audio_tts.reference_kv_slots: how many
// reference clips' prompt-prefix KV rows the Higgs session keeps resident
// on the GPU, so a call cloning from a recently used reference (preset or
// emotion variant) restores its prefix on-device (~1ms) instead of
// re-prefilling the whole reference (~400ms for a 20s clip). Each slot
// costs ~147KB of VRAM per prefix step (~60MB for a median 12s reference,
// ~90MB at the 20s voicerefs.maxRefClipSeconds cap). 16 covers a
// chapter's narrator, its common emotion variants and main characters;
// internal/jobs' compareClone groups background work by reference so
// fewer slots still hit. Needs the local audio.cpp patch adding this
// option (backend/CLAUDE.md) - an unpatched libaudiocpp.so rejects it.
var higgsReferenceKVSlots = envOr("LECTABLE_AUDIOCPP_HIGGS_REFERENCE_KV_SLOTS", "16")

// higgsDecodeBatch is higgs_audio_tts.decode_batch_size: how many paragraphs
// the one Higgs session decodes together per step (continuous batching,
// another local audio.cpp patch). Decode is memory-bandwidth bound - every
// step streams all ~4GB of weights for one frame - so a step decoding 4
// paragraphs' frames costs little more than one decoding 1. Matches
// internal/jobs' maxInFlight (8), which is what actually keeps this many
// paragraphs in flight at once; the session also gets this many concurrent
// callers (cloneFamily.sessionCallers). 1 turns batching off. Each slot
// reserves a 2048-step KV region (~300MB), so 8 slots is ~2.4GB of VRAM.
// Measured on 120 paragraphs: throughput RTF 0.301 unbatched, 0.108 at 4,
// 0.094 at 8.
var higgsDecodeBatch = envIntOr("LECTABLE_AUDIOCPP_HIGGS_DECODE_BATCH", 8)

// higgsFrameBudgetMultiplier is higgs_audio_tts.decode_frame_budget_
// multiplier: a chunk stops (failing "...before EOC", which generateClone's
// bisection then recovers from) once it has decoded this many times the
// frames its reference voice's own pace predicts for the text, floored at
// 250 frames (10s) inside audio.cpp. audio.cpp's own default (4.0) never
// fired in practice - every one of ~570 logged runaways ran the full
// max_tokens (1024 frames, ~10s of GPU) first, since 4x the pace of any
// paragraph over ~150 characters is already past 1024. Measured against
// 190 successful clips, no clip used more than 70% of a 2.5x/250-frame
// budget (under 50% for anything over ~30 characters).
var higgsFrameBudgetMultiplier = envOr("LECTABLE_AUDIOCPP_HIGGS_FRAME_BUDGET_MULTIPLIER", "2.5")

// cloneModelFamilies maps a public clone_model id (what a voice preset
// actually stores/sends) to the family key in cloneFamilies below.
var cloneModelFamilies = map[string]string{
	"audiocpp-qwen3-0.6b":  "audiocpp-qwen3",
	"audiocpp-higgs-4b":    "audiocpp-higgs",
	"audiocpp-pocket-100m": "audiocpp-pocket",
	"audiocpp-breeze-tts":  "audiocpp-breeze",
	"audiocpp-omnivoice":   "audiocpp-omnivoice",
	"audiocpp-soprano":     "audiocpp-soprano",
	"audiocpp-fireredtts3": "audiocpp-fireredtts3",
	"audiocpp-firered":     "audiocpp-firered",
	"audiocpp-auk":         "audiocpp-auk",
	"audiocpp-auk-flash":   "audiocpp-auk-flash",
	"audiocpp-moss-local":  "audiocpp-moss-local",
	"audiocpp-moss-nano":   "audiocpp-moss-nano",
	"audiocpp-voxcpm1":     "audiocpp-voxcpm1",
	"audiocpp-voxcpm2":     "audiocpp-voxcpm2",
	"audiocpp-zipvoice":    "audiocpp-zipvoice",
}

// mossLocalPoolSize: not yet profiled under concurrent load on this box -
// starts at 1, same reasoning as fireRedTTS3PoolSize above.
var mossLocalPoolSize = envIntOr("LECTABLE_AUDIOCPP_MOSS_LOCAL_CLONE_POOL_SIZE", 1)
var mossNanoPoolSize = envIntOr("LECTABLE_AUDIOCPP_MOSS_NANO_CLONE_POOL_SIZE", 1)
var voxCPM1PoolSize = envIntOr("LECTABLE_AUDIOCPP_VOXCPM1_CLONE_POOL_SIZE", 1)
var voxCPM2PoolSize = envIntOr("LECTABLE_AUDIOCPP_VOXCPM2_CLONE_POOL_SIZE", 1)

// voxCPM1CacheSlots/voxCPM2CacheSlots are each family's own
// prompt_cache_slots session option (audio.cpp default 1 - caches the
// encoded prompt/reference audio), falling back to cloneCacheSlots' shared
// value when unset - see mossLocalCacheSlots.
var voxCPM1CacheSlots = envOr("LECTABLE_AUDIOCPP_VOXCPM1_CACHE_SLOTS", cloneCacheSlots)
var voxCPM2CacheSlots = envOr("LECTABLE_AUDIOCPP_VOXCPM2_CACHE_SLOTS", cloneCacheSlots)

// voxCPMEncoderSampleCapacity sizes both VoxCPM families' AudioVAE
// encoder input buffer (voxcpm1/voxcpm2.audiovae_encoder_sample_capacity),
// the longest reference clip either can encode - audio.cpp's default
// 240000 samples is only 15s at the encoder's 16kHz, shorter than the 20s
// voicerefs.maxRefClipSeconds allows, and every clone call against a
// longer clip failed outright ("AudioVAE encoder sample capacity
// exceeded"). 480000 (30s) covers that cap with headroom and divides
// evenly by the encoder's stride, which audio.cpp requires. The whole
// buffer is encoded on every call regardless of clip length, so this is
// kept no larger than needed.
var voxCPMEncoderSampleCapacity = envOr("LECTABLE_AUDIOCPP_VOXCPM_ENCODER_SAMPLE_CAPACITY", "480000")

// voxCPMPromptAudio turns on VoxCPM's "ultimate clone" mode for both
// families (cloneFamily.refAsPromptAudio) - off by default because it
// leaked the end of the reference clip into generated audio; set true to
// trade that risk for its closer voice match.
var voxCPMPromptAudio = envOr("LECTABLE_AUDIOCPP_VOXCPM_PROMPT_AUDIO", "false") == "true"

// voxCPM2ModelPath is shared by cloneFamilies' "audiocpp-voxcpm2" and
// designEngines' "voxcpm2" - one GGUF covers cloning and voice design.
var voxCPM2ModelPath = envOr(
	"LECTABLE_AUDIOCPP_VOXCPM2_MODEL_PATH",
	modelPath("VoxCPM2-GGUF/voxcpm2-q8_0.gguf"),
)
var zipVoicePoolSize = envIntOr("LECTABLE_AUDIOCPP_ZIPVOICE_CLONE_POOL_SIZE", 1)

// espeakLibraryPath/espeakDataPath locate the eSpeak NG shared library and
// its voice data, which zipvoice loads at runtime to phonemize English text
// (this box's audio.cpp build doesn't statically link it -
// AUDIOCPP_STATIC_ESPEAK=OFF - and no system espeak-ng is installed). The
// defaults point at a copy unpacked from the espeakng_loader PyPI wheel
// into the models directory; point both at a system install instead
// (e.g. /usr/lib/x86_64-linux-gnu/libespeak-ng.so.1 and
// /usr/lib/x86_64-linux-gnu/espeak-ng-data) if one exists.
var espeakLibraryPath = envOr("LECTABLE_AUDIOCPP_ESPEAK_LIBRARY_PATH", modelPath("espeak-ng/libespeak-ng.so"))
var espeakDataPath = envOr("LECTABLE_AUDIOCPP_ESPEAK_DATA_PATH", modelPath("espeak-ng/espeak-ng-data"))

// mossLocalCacheSlots is moss_tts_local.reference_cache_slots (audio.cpp
// default 1) - its own env var so it can be tuned independently, falling
// back to cloneCacheSlots' shared value when unset. Each slot holds one
// reference clip's already-encoded audio codes, reused whenever the same
// (reference audio, transcript) pair comes back on a warm session.
var mossLocalCacheSlots = envOr("LECTABLE_AUDIOCPP_MOSS_LOCAL_CACHE_SLOTS", cloneCacheSlots)

// mossLanguage maps a language onto MOSS-TTS's prompt-template language
// slot, which takes free text ("- Language:\n<value>") rather than a fixed
// tag set - "Auto" leaves it empty so the model detects the language
// itself, anything else is capitalized to read as a language name.
func mossLanguage(language, _ string) string {
	if language == "" || strings.EqualFold(language, "auto") {
		return "Auto"
	}
	return strings.ToUpper(language[:1]) + strings.ToLower(language[1:])
}

// fireRedTTS3PoolSize/fireRedAudioPoolSize/aukPoolSize: each of these
// families is new here and not yet profiled under concurrent load on this
// box, so all start at 1 - the same conservative starting point
// higgsClonePoolSize's own doc comment argues for on a GPU with no spare
// VRAM headroom. FireRedAudio especially: its q8_0 package alone is ~14GB.
var fireRedTTS3PoolSize = envIntOr("LECTABLE_AUDIOCPP_FIREREDTTS3_CLONE_POOL_SIZE", 1)
var fireRedAudioPoolSize = envIntOr("LECTABLE_AUDIOCPP_FIRERED_AUDIO_CLONE_POOL_SIZE", 1)
var aukPoolSize = envIntOr("LECTABLE_AUDIOCPP_AUK_CLONE_POOL_SIZE", 1)

// fireRedTTS3ModelPath: the Instruct package, not Base - Instruct covers
// both cloning (template_name=instruct_tts) and VoiceDesign
// (template_name=voice_design) from one GGUF, and accepts the "tts" task
// every clone session here is opened with (getCloneModel), whereas Base
// only accepts the "clon" task and can't design at all (see
// docs/models/fireredtts3.md in the local audio.cpp checkout).
var fireRedTTS3ModelPath = envOr(
	"LECTABLE_AUDIOCPP_FIREREDTTS3_MODEL_PATH",
	modelPath("FireRedTTS3-Instruct-GGUF/fireredtts3-instruct-q8_0.gguf"),
)

var fireRedAudioModelPath = envOr(
	"LECTABLE_AUDIOCPP_FIRERED_AUDIO_MODEL_PATH",
	modelPath("FireRedAudio-GGUF/firered-audio-q8_0.gguf"),
)

// aukModelDir: AuK loads from a directory holding its component GGUFs plus
// config/tokenizer sidecars (audio-cpp/AuK-Base-and-Flash-GGUF), not a
// single file - see docs/community_models/auk.md in the local audio.cpp
// checkout. Base and Flash share this one directory; which generator is
// used is a session option (aukSessionOptions).
var aukModelDir = envOr("LECTABLE_AUDIOCPP_AUK_MODEL_DIR", modelPath("AuK-Base-and-Flash-GGUF"))

// aukQwenGGUF/aukBaseGGUF/aukFlashGGUF pick AuK's component files within
// aukModelDir. Defaults are q8_0 throughout rather than audio.cpp's own
// defaults (BF16 Qwen, F32 generator), which would need far more VRAM.
var aukQwenGGUF = envOr("LECTABLE_AUDIOCPP_AUK_QWEN_GGUF", "qwen2.5-omni-3b-q8_0.gguf")
var aukBaseGGUF = envOr("LECTABLE_AUDIOCPP_AUK_BASE_GGUF", "auk-base-q8_0.gguf")
var aukFlashGGUF = envOr("LECTABLE_AUDIOCPP_AUK_FLASH_GGUF", "auk-flash-q8_0.gguf")

func aukSessionOptions(flash bool) map[string]string {
	if flash {
		return map[string]string{"auk.variant": "flash", "auk.model_gguf": aukFlashGGUF, "auk.qwen_gguf": aukQwenGGUF}
	}
	return map[string]string{"auk.variant": "base", "auk.model_gguf": aukBaseGGUF, "auk.qwen_gguf": aukQwenGGUF}
}

// aukCharsPerSecond is the speaking pace auk's output duration is sized
// from whenever there's no reference clip to measure one from (VoiceDesign,
// or a clone call whose reference has no transcript) - auk renders exactly
// the duration it's asked for, so too low a value drags speech out and too
// high a value rushes it. ~14 chars/sec is an ordinary audiobook pace.
var aukCharsPerSecond = envFloatOr("LECTABLE_AUDIOCPP_AUK_CHARS_PER_SEC", 14)

// aukCloneText wraps text in AuK's upstream zero-shot-cloning instruction
// (Tencent-Hunyuan/AuK's COOKBOOK.md) - without an "instruct" option,
// audio.cpp's auk session passes text through verbatim as the model's
// whole instruction, so the words alone would not be read as speech to
// render in the reference voice.
func aukCloneText(text string) string {
	return `Say the following with the same voice: "` + text + `"`
}

// fireRedTTS3Language maps a language onto one of fireredtts3's own
// capitalized language tags. "Auto" (or nothing) guesses Chinese vs.
// English from the text itself - fireredtts3 has no auto-detection of its
// own and would otherwise default to Chinese normalization for English
// text.
func fireRedTTS3Language(language, text string) string {
	if language == "" || strings.EqualFold(language, "auto") {
		if hasCJK(text) {
			return "Chinese"
		}
		return "English"
	}
	return strings.ToUpper(language[:1]) + strings.ToLower(language[1:])
}

// fireRedAudioLanguage maps a language onto firered_audio's own tag, which
// it only uses to decide whether reference transcript and text are joined
// with a space ("en") or not ("zh") - so every language written without
// spaces between words maps to "zh", everything else to "en".
func fireRedAudioLanguage(language, text string) string {
	switch strings.ToLower(language) {
	case "chinese", "japanese", "cantonese", "zh", "ja":
		return "zh"
	case "", "auto":
		if hasCJK(text) {
			return "zh"
		}
	}
	return "en"
}

func hasCJK(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) {
			return true
		}
	}
	return false
}

var cloneFamilies = map[string]cloneFamily{
	"audiocpp-qwen3": {
		temperature: true,
		family:      "qwen3_tts",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_QWEN3_MODEL_PATH",
			modelPath("Qwen3-TTS-12Hz-0.6B-Base-GGUF/qwen3-tts-12hz-0.6b-base-q8_0.gguf"),
		),
		sessionOptions:   map[string]string{"qwen3_tts.voice_prompt_cache_slots": cloneCacheSlots},
		poolSizeOverride: qwen3ClonePoolSize,
	},
	"audiocpp-higgs": {
		temperature: true,
		family:      "higgs_audio_tts",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_HIGGS_MODEL_PATH",
			modelPath("Higgs-Audio-v3-TTS-4B-GGUF/higgs-audio-v3-tts-4b-q8_0.gguf"),
		),
		sessionOptions: map[string]string{
			"higgs_audio_tts.reference_cache_slots":          cloneCacheSlots,
			"higgs_audio_tts.reference_kv_slots":             higgsReferenceKVSlots,
			"higgs_audio_tts.decode_batch_size":              strconv.Itoa(higgsDecodeBatch),
			"higgs_audio_tts.decode_frame_budget_multiplier": higgsFrameBudgetMultiplier,
		},
		defaultRequestOptions: map[string]string{"max_tokens": higgsMaxTokens},
		poolSizeOverride:      higgsClonePoolSize,
		sessionCallers:        higgsDecodeBatch,
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
		temperature: true,
		family:      "pocket_tts",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_POCKET_MODEL_PATH",
			modelPath("PocketTTS-GGUF/english/pocket-tts-english-q8_0.gguf"),
		),
		sessionOptions: map[string]string{"pocket_tts.voice_state_cache_slots": cloneCacheSlots},
		// English/German/Italian/Portuguese/Spanish are separate model
		// packages (see pocket_tts.json's own "packages" list), not a
		// per-request language switch - the default download above is the
		// English package, so this is fixed at load time rather than
		// exposed as a per-call option the way qwen3_tts/higgs_audio_tts's
		// own language passthrough is.
		refTextOption:    "voice_clone_text",
		poolSizeOverride: pocketClonePoolSize,
	},
	// BreezeTTS 2 (audio-cpp/audio.cpp-gguf's "breeze_tts" family) - a
	// prompt-audio cloning engine like qwen3_tts/higgs_audio_tts, wired in
	// as an additional clone_model option alongside them. Its own
	// "reference_text" request-option name already matches this package's
	// default (refTextOption left empty below), unlike pocket_tts above.
	"audiocpp-breeze": {
		temperature: true,
		family:      "breeze_tts",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_BREEZE_MODEL_PATH",
			modelPath("Breeze-TTS-2-GGUF/breeze-tts-2-q8_0.gguf"),
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
			modelPath("OmniVoice-GGUF/omnivoice-q8_0.gguf"),
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
		temperature: true,
		family:      "soprano_tts",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_SOPRANO_MODEL_PATH",
			modelPath("Soprano-1.1-80M-GGUF/soprano-1.1-80m-q8_0.gguf"),
		),
		noLanguageOption: true,
		noReference:      true,
	},
	// FireRedTTS3 (FireRedTeam, Apache-2.0) - the Instruct package, which
	// clones via template_name=instruct_tts on a plain "tts" session (see
	// fireRedTTS3ModelPath's own doc comment for why not Base). Its clone
	// template reads the reference transcript from "reference_text", this
	// package's default. Language is its own capitalized tag, defaulting to
	// Chinese if unset - see fireRedTTS3Language.
	"audiocpp-fireredtts3": {
		family:                "fireredtts3",
		modelPath:             fireRedTTS3ModelPath,
		sessionOptions:        map[string]string{"fireredtts3.reference_cache_slots": cloneCacheSlots},
		defaultRequestOptions: map[string]string{"template_name": "instruct_tts"},
		languageTag:           fireRedTTS3Language,
		poolSizeOverride:      fireRedTTS3PoolSize,
	},
	// FireRedAudio (FireRedTeam, Apache-2.0) - one multimodal GGUF covering
	// ASR/understanding as well as cloning (template_name=tts_clone, used
	// here) and VoiceDesign (see designEngines' own "firered_audio" entry).
	"audiocpp-firered": {
		family:                "firered_audio",
		modelPath:             fireRedAudioModelPath,
		sessionOptions:        map[string]string{"firered_audio.reference_cache_slots": cloneCacheSlots},
		defaultRequestOptions: map[string]string{"template_name": "tts_clone"},
		languageTag:           fireRedAudioLanguage,
		poolSizeOverride:      fireRedAudioPoolSize,
	},
	// AuK / AuK-Flash (Tencent, MIT) - an experimental flow-matching family
	// conditioned on reference audio alone (no transcript - noRefText), with
	// a fixed output duration it never stops short of on its own
	// (estimateDuration) and a text prompt that must carry the full upstream
	// cloning instruction (wrapText). Its spec lists no "language" option at
	// all, so none is sent. Flash is the same family and directory with a
	// fixed 4-step schedule instead of Base's 32 - much faster, at some
	// quality cost.
	//
	// audio.cpp's auk session currently refuses every backend but CUDA
	// ("AuK native session currently requires CUDA" - HIP is a separate
	// backend type there), so on this box's AMD GPU (LECTABLE_AUDIOCPP_
	// BACKEND=hip) every call fails at session creation until that check is
	// relaxed upstream. Wired in anyway for CUDA hosts.
	"audiocpp-auk": {
		family:           "auk",
		modelPath:        aukModelDir,
		sessionOptions:   aukSessionOptions(false),
		noLanguageOption: true,
		noRefText:        true,
		wrapText:         aukCloneText,
		estimateDuration: true,
		poolSizeOverride: aukPoolSize,
	},
	// MOSS-TTS-Local v1.5 (OpenMOSS, Apache-2.0) - one self-contained GGUF
	// (audio tokenizer included). Clones whenever a reference clip is
	// attached, on the same "tts" session every family here uses; reads its
	// transcript from "reference_text", this package's default. Chunks long
	// text itself (2048 chars per chunk by default).
	"audiocpp-moss-local": {
		temperature: true,
		family:      "moss_tts_local",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_MOSS_LOCAL_MODEL_PATH",
			modelPath("MOSS-TTS-Local-v1.5-GGUF/moss-tts-local-v1.5-q8_0.gguf"),
		),
		sessionOptions:   map[string]string{"moss_tts_local.reference_cache_slots": mossLocalCacheSlots},
		languageTag:      mossLanguage,
		poolSizeOverride: mossLocalPoolSize,
	},
	// MOSS-TTS-Nano 100M (OpenMOSS, Apache-2.0) - MOSS-TTS-Local's small
	// sibling, one ~190MB GGUF. Same reference_text/clone-by-attachment
	// shape, but its prompt has no language slot at all (the "language"
	// option SetText sends is simply never read) and no reference-cache
	// session option to widen.
	"audiocpp-moss-nano": {
		temperature: true,
		family:      "moss_tts_nano",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_MOSS_NANO_MODEL_PATH",
			modelPath("MOSS-TTS-Nano-100M-GGUF/moss-tts-nano-100m-q8_0.gguf"),
		),
		poolSizeOverride: mossNanoPoolSize,
	},
	// ZipVoice-Distill (k2-fsa, Apache-2.0; audio.cpp community port of
	// davidxifeng/zipvoice-gguf) - a non-autoregressive flow-matching cloner
	// (8 Euler steps), English and Chinese. Needs the reference transcript
	// ("reference_text", this package's default) and eSpeak NG for English
	// phonemization (espeakLibraryPath/espeakDataPath). Output length comes
	// from its own duration predictor, scaled from the reference clip's pace.
	"audiocpp-zipvoice": {
		family: "zipvoice",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_ZIPVOICE_MODEL_PATH",
			modelPath("ZipVoice-Distill-GGUF/zipvoice-distill-q8_0.gguf"),
		),
		sessionOptions: map[string]string{
			"zipvoice.espeak_library_path": espeakLibraryPath,
			"zipvoice.espeak_data_path":    espeakDataPath,
		},
		poolSizeOverride: zipVoicePoolSize,
	},
	// VoxCPM2 (OpenBMB, Apache-2.0) - MiniCPM backbone + diffusion
	// AudioVAE, cloning and voice design from one GGUF (see designEngines'
	// own "voxcpm2" entry). Clones from the reference clip's timbre by
	// default (see refAsPromptAudio for the optional "ultimate clone" mode);
	// auto-detects language (its prompt has no language slot, and the
	// "language" option SetText sends is never read). Chunks long text
	// itself (2048 chars by default, continuing each chunk from the last).
	"audiocpp-voxcpm2": {
		family:    "voxcpm2",
		modelPath: voxCPM2ModelPath,
		sessionOptions: map[string]string{
			"voxcpm2.prompt_cache_slots":               voxCPM2CacheSlots,
			"voxcpm2.audiovae_encoder_sample_capacity": voxCPMEncoderSampleCapacity,
		},
		noLanguageOption: true,
		refAsPromptAudio: voxCPMPromptAudio,
		poolSizeOverride: voxCPM2PoolSize,
	},
	// VoxCPM1 0.5B (OpenBMB, Apache-2.0) - clone-only predecessor, run by
	// the same audio.cpp runtime as voxcpm2. Its spec validates request
	// options strictly and lists no "language" (noLanguageOption).
	"audiocpp-voxcpm1": {
		family: "voxcpm1",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_VOXCPM1_MODEL_PATH",
			modelPath("VoxCPM1-GGUF/voxcpm-0.5b-q8_0-audiovae-f16.gguf"),
		),
		sessionOptions: map[string]string{
			"voxcpm1.prompt_cache_slots":               voxCPM1CacheSlots,
			"voxcpm1.audiovae_encoder_sample_capacity": voxCPMEncoderSampleCapacity,
		},
		noLanguageOption: true,
		refAsPromptAudio: voxCPMPromptAudio,
		poolSizeOverride: voxCPM1PoolSize,
	},
	"audiocpp-auk-flash": {
		family:           "auk",
		modelPath:        aukModelDir,
		sessionOptions:   aukSessionOptions(true),
		noLanguageOption: true,
		noRefText:        true,
		wrapText:         aukCloneText,
		estimateDuration: true,
		poolSizeOverride: aukPoolSize,
	},
}

// designEngine bundles everything Design (design.go) needs to render a
// fresh reference clip via one VoiceDesign-capable family - see
// designEngines/activeDesignEngine below for why more than one now exists.
type designEngine struct {
	// id is this engine's own designEngines key - what loadedDesigns is
	// keyed by (not family: auk and auk_flash are the same family, loaded
	// with different session options).
	id string
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
	// design instruction from - "instruct" for qwen3_tts/auk, "instruction"
	// for breeze_tts/omnivoice/fireredtts3/firered_audio. Empty defaults to
	// "instruct" in Design.
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
	// languageTag: see cloneFamily's own doc comment - same mapping, on the
	// design path.
	languageTag func(language, text string) string
	// estimateDuration: true for auk, which can't render without an
	// explicit "duration_sec" when there's no reference clip - Design sizes
	// it from aukCharsPerSecond.
	estimateDuration bool
	// guidanceScale: true if this engine's own model spec has a
	// "guidance_scale" request option, so Design may pass a caller's
	// per-call override through (breeze_tts, fireredtts3, firered_audio,
	// auk). False for qwen3_tts/omnivoice (no such option - their specs
	// would reject it) and auk_flash (always runs without guidance).
	guidanceScale bool
	// temperature: see cloneFamily.temperature - true for qwen3_tts and
	// breeze_tts, the design engines whose sessions read "temperature".
	temperature bool
	// instructAsTextPrefix: true for voxcpm2, which has no instruction
	// request option - a voice description goes in parentheses at the very
	// start of the text itself ("(A warm, deep male voice)Hello...") -
	// so Design prepends it rather than setting instructOption.
	instructAsTextPrefix bool
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
		temperature: true,
		id:          "qwen3_tts",
		family:      "qwen3_tts",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_QWEN3_DESIGN_MODEL_PATH",
			modelPath("Qwen3-TTS-12Hz-1.7B-VoiceDesign-GGUF/qwen3-tts-12hz-1.7b-voicedesign-q8_0.gguf"),
		),
		// weight_type left at its own "native" default (this checkpoint's
		// own q8_0 quantization) - an earlier attempt forced f32 here as a
		// fix for "audiocpp_session_run: runtime error: Qwen3 sampler has
		// no finite logits" crashes seen during voice provisioning, but
		// that's been reverted; revisit (narrow to specific presets, or a
		// different weight_type) if those crashes resurface.
	},
	"breeze_tts": {
		temperature:   true,
		id:            "breeze_tts",
		family:        "breeze_tts",
		guidanceScale: true,
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_BREEZE_DESIGN_MODEL_PATH",
			modelPath("Breeze-TTS-2-GGUF/breeze-tts-2-q8_0.gguf"),
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
		id:     "omnivoice",
		family: "omnivoice",
		modelPath: envOr(
			"LECTABLE_AUDIOCPP_OMNIVOICE_MODEL_PATH",
			modelPath("OmniVoice-GGUF/omnivoice-q8_0.gguf"),
		),
		instructOption: "instruction",
		designTask:     "tts",
	},
	// FireRedTTS3 Instruct - the same GGUF as cloneFamilies' own
	// "audiocpp-fireredtts3" entry, on a "vdes" session with
	// template_name=voice_design and the description in "instruction".
	"fireredtts3": {
		id:                    "fireredtts3",
		guidanceScale:         true,
		family:                "fireredtts3",
		modelPath:             fireRedTTS3ModelPath,
		defaultRequestOptions: map[string]string{"template_name": "voice_design"},
		instructOption:        "instruction",
		languageTag:           fireRedTTS3Language,
	},
	// FireRedAudio - same GGUF as cloneFamilies' own "audiocpp-firered"
	// entry, same template/instruction shape as fireredtts3 above.
	"firered_audio": {
		id:                    "firered_audio",
		guidanceScale:         true,
		family:                "firered_audio",
		modelPath:             fireRedAudioModelPath,
		defaultRequestOptions: map[string]string{"template_name": "voice_design"},
		instructOption:        "instruction",
		languageTag:           fireRedAudioLanguage,
	},
	// AuK / AuK-Flash - design is auk's own "instruction TTS": a plain "tts"
	// request with no reference and the voice description in "instruct",
	// which audio.cpp wraps in AuK's upstream "Generate speech based on the
	// following description" template itself. Needs an explicit duration
	// (estimateDuration). Same CUDA-only caveat as cloneFamilies' own
	// "audiocpp-auk" entry.
	"auk": {
		id:               "auk",
		guidanceScale:    true,
		family:           "auk",
		modelPath:        aukModelDir,
		sessionOptions:   aukSessionOptions(false),
		instructOption:   "instruct",
		noLanguageOption: true,
		designTask:       "tts",
		estimateDuration: true,
	},
	// VoxCPM2 - the same GGUF as cloneFamilies' own "audiocpp-voxcpm2"
	// entry, on a plain "tts" session: design is just text prefixed with
	// the voice description in parentheses (instructAsTextPrefix), no
	// reference attached. Reads "guidance_scale" (default 2.0).
	"voxcpm2": {
		id:                   "voxcpm2",
		family:               "voxcpm2",
		modelPath:            voxCPM2ModelPath,
		designTask:           "tts",
		noLanguageOption:     true,
		instructAsTextPrefix: true,
		guidanceScale:        true,
	},
	"auk_flash": {
		id:               "auk_flash",
		family:           "auk",
		modelPath:        aukModelDir,
		sessionOptions:   aukSessionOptions(true),
		instructOption:   "instruct",
		noLanguageOption: true,
		designTask:       "tts",
		estimateDuration: true,
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
	modelPath("ACE-Step1.5-GGUF/turbo/ace-step-1.5-turbo-q8_0.gguf"),
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
// and fixed upstream in the local audio.cpp checkout.
var stableAudioMusicModelPath = envOr(
	"LECTABLE_AUDIOCPP_STABLE_AUDIO_MUSIC_MODEL_PATH",
	modelPath("Stable-Audio-3-Small-Music-GGUF/stable-audio-3-small-music-q8_0.gguf"),
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
	modelPath("Stable-Audio-3-Small-SFX-GGUF/stable-audio-3-small-sfx-q8_0.gguf"),
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
	modelPath("Stable-Audio-3-Medium-GGUF/stable-audio-3-medium-q8_0.gguf"),
)
var stableAudioMediumPoolSize = envIntOr("LECTABLE_AUDIOCPP_STABLE_AUDIO_MEDIUM_POOL_SIZE", 1)

var alignerModelPath = envOr(
	"LECTABLE_ALIGNER_MODEL_PATH",
	modelPath("Qwen3-ForcedAligner-0.6B-GGUF/qwen3-forced-aligner-0.6b-q8_0.gguf"),
)

// transcriberModelPath is the ASR model behind POST /transcribe (asr.go).
// Parakeet TDT: ~0.03 RTF on a 7900 XTX and punctuated, cased output.
var transcriberModelPath = envOr(
	"LECTABLE_TRANSCRIBER_MODEL_PATH",
	modelPath("Parakeet-TDT-0.6B-v3-GGUF/parakeet-tdt-0.6b-v3-q8_0.gguf"),
)

// backendName picks which of audio.cpp's own GPU backends to run on -
// "cuda"/"hip"/"vulkan"/"cpu"/"best".
var backendName = envOr("LECTABLE_AUDIOCPP_BACKEND", "hip")

// sessionThreads: CPU-side thread count audio.cpp's session_create wants
// even for a GPU backend (host-side pre/post-processing).
var sessionThreads = 4
