// Package voices holds the curated narrator voice presets for the
// VoiceDesign model - a direct port of tts-service/voices.py, now that the
// backend is the sole owner of everything except the disposable ttsworker
// process (see backend/CLAUDE.md). These are never stored in the DB (see
// store.go's voice_presets table comment): they're compiled-in, proxied
// to callers exactly as tts-service used to proxy them over HTTP.
//
// Each preset is a natural-language style instruction fed to the
// VoiceDesign task, plus a fixed Seed. VoiceDesign samples a voice matching
// the instruct text rather than cloning a fixed speaker, so an unseeded
// call can realize a slightly different individual timbre each time even
// with identical instruct text. Pinning Seed per preset makes every
// paragraph generated with that preset sample the same voice realization,
// which is what keeps a book's narrator consistent across paragraphs.
//
// Even seeded, VoiceDesign can still drift: the sampled voice depends on
// the seeded RNG *and* the input text, so different paragraph text can walk
// the sampling trajectory to a noticeably different individual timbre
// despite an identical seed. For real cross-paragraph stability, the
// backend additionally clones each preset from a single reference clip
// (see internal/voicerefs) instead of re-sampling VoiceDesign per
// paragraph. Instruct+Seed+RefText here are what bootstrap that one
// reference clip: Instruct+Seed pick the voice (via VoiceDesign), and
// RefText is what's actually spoken in it - it defaults to DefaultRefText
// but can be overridden per preset (custom presets only; built-ins here
// all use the default).
package voices

// DefaultRefText is phonetically-balanced (a long-standing favorite in
// voice-cloning reference-recording guides, extended here) so the one
// reference clip every preset clones from actually samples most English
// phonemes, rather than whatever narrow set a short throwaway line happens
// to contain. Also long enough to clear 5 seconds even after a 2x
// speed_multiplier and a fast VoiceDesign narration pace.
const DefaultRefText = "The beige hue on the waters of the loch impressed all, including the " +
	"French queen, before she heard that symphony again, just as young " +
	"Arthur wanted. It was a pleasant, quiet journey through the misty " +
	"hills, past the old stone bridge where the shepherd once played his " +
	"haunting melody, and everyone agreed the view alone was worth the " +
	"long, winding drive."

// Preset is one curated built-in narrator voice.
type Preset struct {
	ID       string
	Name     string
	Instruct string
	Seed     int
	// RefText: what's actually spoken in the reference clip. Every built-in
	// preset here uses DefaultRefText; the field exists (rather than a bare
	// constant) so Preset has the same shape a custom preset's row does.
	RefText string
	// SpeedMultiplier is applied to the rendered reference clip (time-
	// stretch, pitch preserved) before it's used to build the clone - not a
	// TTS generation parameter, since VoiceDesign/cloning have no native
	// speed control. >1.0 speeds the reference up (biasing the cloned
	// narrator's pace faster), <1.0 slows it down.
	SpeedMultiplier float64
	// DesignModel overrides which VoiceDesign engine renders this preset's
	// reference clip (see backend/internal/audioworker's designEngines -
	// "qwen3_tts" or "breeze_tts", kept as plain literal strings here
	// rather than a shared import, same reasoning as the clone_model ids
	// below: this package is linked into cmd/server, which must
	// never import audioworker/audiocpp-go). "" (every built-in preset
	// except velvet-narrator) defers to the worker's own process-wide
	// default (LECTABLE_AUDIOCPP_DESIGN_ENGINE, currently breeze_tts).
	// velvet-narrator is pinned to DesignModelQwen3 explicitly - its
	// instruct was tuned against qwen3_tts's own VoiceDesign output, and a
	// breeze_tts render of the same instruct sounds different enough that
	// it shouldn't silently drift just because the process-wide default
	// changed.
	DesignModel string
}

// DesignModel values - see Preset.DesignModel's own doc comment for why
// these are plain literals, not a shared import with audioworker.
const (
	DesignModelQwen3  = "qwen3_tts"
	DesignModelBreeze = "breeze_tts"
)

// DefaultDesignModel is what a brand-new custom voice preset is created
// with when the request doesn't specify one - always breeze_tts,
// regardless of whether the new preset's instruct/seed/refText happen to
// match an existing preset's own recipe (its "derivation source" - see
// httpapi.findDerivationSource): a new voice never silently inherits
// another preset's design model just because it reuses that preset's
// already-rendered reference clip.
const DefaultDesignModel = DesignModelBreeze

// Presets are the curated built-in narrator voices, in display order.
var Presets = []Preset{
	{
		ID:              "calm-male",
		Name:            "Calm Male Narrator",
		Instruct:        "Speak as a calm, warm middle-aged male narrator reading a novel aloud. Measured pace, clear diction, neutral accent, gentle and steady tone.",
		Seed:            1001,
		RefText:         DefaultRefText,
		SpeedMultiplier: 1.0,
	},
	{
		ID:              "warm-female",
		Name:            "Warm Female Narrator",
		Instruct:        "Speak as a warm, friendly adult female narrator reading a novel aloud. Even pace, clear articulation, neutral accent, inviting and expressive tone.",
		Seed:            1002,
		RefText:         DefaultRefText,
		SpeedMultiplier: 1.0,
	},
	{
		ID:              "crisp-british",
		Name:            "Crisp British Narrator",
		Instruct:        "Speak with a crisp, articulate British accent, as an experienced audiobook narrator. Precise diction, dry and composed delivery, moderate pace.",
		Seed:            1003,
		RefText:         DefaultRefText,
		SpeedMultiplier: 1.0,
	},
	{
		ID:              "deep-dramatic",
		Name:            "Deep Dramatic Narrator",
		Instruct:        "Speak in a deep, resonant, dramatic voice suited for fantasy or thriller narration. Slightly slower pace, weighty delivery, subtle tension in the tone.",
		Seed:            1004,
		RefText:         DefaultRefText,
		SpeedMultiplier: 1.0,
	},
	{
		ID:              "bright-energetic",
		Name:            "Bright Energetic Narrator",
		Instruct:        "Speak in a bright, energetic, youthful voice, well suited for lighthearted fiction. Slightly faster pace, upbeat and engaging tone, clear enunciation.",
		Seed:            1005,
		RefText:         DefaultRefText,
		SpeedMultiplier: 1.0,
	},
	{
		ID:              "velvet-narrator",
		Name:            "Velvet Narrator",
		Instruct:        "Speak as a British male narrator with a deep, resonant bass-baritone voice. Warm, rich timbre with a gravelly weight beneath it, an unhurried, measured pace, and calm, commanding gravitas - the reassuring authority of a seasoned documentary narrator, warm enough to feel intimate even at its most weighty.",
		Seed:            1006,
		RefText:         DefaultRefText,
		SpeedMultiplier: 1.0,
		DesignModel:     DesignModelQwen3,
	},
	{
		ID:              "documentary-narrator",
		Name:            "Documentary Narrator",
		Instruct:        "Speak as a male narrator with a slight British accent and a deep, resonant voice. Warm, rich timbre with the reassuring authority of a seasoned documentary narrator, warm enough to feel intimate even at its most weighty. Include elements of David Attenborough and Morgan Freeman",
		Seed:            1008,
		RefText:         DefaultRefText,
		SpeedMultiplier: 1.0,
	},
	{
		ID:              FastPresetID,
		Name:            "Fast Narrator",
		Instruct:        "Speak as a plain, neutral adult narrator reading a novel aloud. Even pace, clear diction, unremarkable accent, calm and unhurried delivery.",
		Seed:            1007,
		RefText:         DefaultRefText,
		SpeedMultiplier: 1.0,
	},
}

// FastPresetID names the built-in preset internal/jobs.Manager's own
// length-estimate sample (EnqueueLengthEstimate) clones from, always
// through FastCloneModel regardless of any book's own clone model. Also a
// perfectly normal, selectable narrator preset like any other.
var FastPresetID = "fast-narrator"

// Clone model ids (see backend/internal/audioworker's cloneModelFamilies),
// kept as plain literals here rather than a shared import - this package
// is linked into cmd/server, which must never import audioworker/
// audiocpp-go (see backend/CLAUDE.md's hard invariant).
//
// Which clone model narrates is a property of the *book* (store.Book.
// CloneModel), never of a voice preset: a preset is only a reference clip
// recipe (instruct/seed/refText/designModel), and the same clip can be
// cloned through any clone model.
const (
	// PocketCloneModel is PocketTTS - by far the fastest family the worker
	// loads (see audioworker/families.go's "audiocpp-pocket" doc comment).
	PocketCloneModel = "audiocpp-pocket-100m"
	// HiggsCloneModel is Higgs Audio - the only family whose delivery-tag
	// vocabulary speech direction (httpapi's direction tagging,
	// store.Book.SpeechDirection) targets.
	HiggsCloneModel = "audiocpp-higgs-4b"
	// SopranoCloneModel is a single fixed built-in voice with no cloning
	// at all - the worker ignores any reference clip sent with it.
	SopranoCloneModel = "audiocpp-soprano"
)

// FastCloneModel is what EnqueueLengthEstimate's throwaway sample clones
// through: a real, generated-audio chars/sec ratio without paying for a
// slow engine's full decode just to measure it.
const FastCloneModel = PocketCloneModel

// PresetsByID indexes Presets by id.
var PresetsByID = func() map[string]Preset {
	m := make(map[string]Preset, len(Presets))
	for _, p := range Presets {
		m[p.ID] = p
	}
	return m
}()

// DefaultPresetID is the preset a new book (or the process default voice)
// starts with.
var DefaultPresetID = "velvet-narrator"

// DefaultCloneModel is the factory default for store.DefaultVoice.
// CloneModel (the clone model new books start with, settable from the
// Voices page), the fallback for a book whose own clone model is somehow
// blank, and what ttsworker eagerly loads at startup.
const DefaultCloneModel = HiggsCloneModel

// InstructedCloneModel is the one clone_model whose family accepts a
// clone-time style instruction alongside its reference clip in the same
// call ("instructed voice cloning" - see backend/internal/audioworker's
// cloneFamily.instructOption) - internal/narration.Resolver checks against
// this to decide whether store.Book.InstructCharacterVoices actually does
// anything for a given book, since it's a no-op under any other clone
// model.
const InstructedCloneModel = "audiocpp-breeze-tts"

// DisplayLanguages mirrors clone_backends/audiocpp.py's DISPLAY_LANGUAGES -
// a human-readable subset of qwen3_tts's Model.Languages(), hardcoded here
// rather than queried from the (lazily-loaded) worker so this list is
// available even before any model has loaded. "Auto" is a real, separately
// accepted value handled by the caller (see handleVoiceLanguages), not
// included here.
var DisplayLanguages = []string{
	"Chinese", "English", "French", "German", "Italian", "Japanese",
	"Korean", "Portuguese", "Russian", "Spanish",
}
