package store

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/rhino1998/lectable/backend/internal/deliverytags"
	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

const (
	AudioPending    = "pending"
	AudioGenerating = "generating"
	AudioReady      = "ready"
	AudioError      = "error"
)

type Book struct {
	ID            string
	Title         string
	Author        string
	Language      string
	CoverExt      string
	SeriesName    string  // "" if not part of a series
	SeriesIndex   float64 // 0 if unknown; can be fractional (e.g. a 2.5 novella)
	AddedAt       int64
	VoicePresetID string
	VoiceInstruct string
	VoiceLanguage string
	VoiceSeed     int // 0 = none resolved (falls back to tts-service's own default); see UpdateVoice
	// CloneModel is which clone model narrates this book - every voice in
	// it, narrator and characters alike. A property of the book, never of
	// a voice preset; see the books.clone_model schema comment.
	CloneModel string
	// CharacterVoiceMode controls whether/how a character's own narration
	// voice can override the book's own - see CharacterVoiceMode's own doc
	// comment for the four possible values. MultiVoice/
	// InstructCharacterVoices/InstructAllCharacterVoices below are thin
	// derived-boolean methods over this one field, kept so most existing
	// call sites read almost exactly as they did back when those were
	// three independent stored columns (multi_voice/
	// instruct_character_voices, plus a never-shipped third) - replaced by
	// this single enum because those three booleans only ever actually
	// produced these four distinct, mutually exclusive behaviors in
	// combination anyway.
	CharacterVoiceMode CharacterVoiceMode
	// SpeechDirection gates jobs.Manager's speechDirectionDependency -
	// see the schema's own doc comment on speech_direction for why
	// this defaults false and what flipping it on does to already-
	// generated audio.
	SpeechDirection bool

	// StrictOrder makes a multi-voice book's narration wait for speaker
	// attribution: a chapter's paragraphs are only queued once the chapter
	// is attributed (jobs.Manager.strictBlocked), so each line is rendered
	// once, in its speaker's voice - at the price of a reader who jumps
	// ahead waiting for attribution to reach them. Off (the default) is
	// reader-first: narrate now with what is known, and re-render the
	// lines attribution later gives to a character.
	StrictOrder bool
	// SettingRequired makes the passes that read the book's setting profile
	// fail when the setting pass does, rather than run without one - see
	// jobs.Manager.bookSettingDependency. Off by default.
	SettingRequired bool
	PosChapterIdx   int
	PosParagraphIdx int
	PosSeconds      float64
	// LastReadAt is when the reading position was last saved (unix
	// seconds), 0 for a book never opened.
	LastReadAt int64
	// EstimateSecPerChar is jobs.Manager.EnqueueLengthEstimate's own
	// PocketTTS-calibrated seconds-per-character measurement - see the
	// schema's own doc comment on estimate_sec_per_char.
	EstimateSecPerChar float64
	// MusicEnabled is the reader-facing background-music toggle - book-
	// wide, not per-chapter (unlike attribution/direction, which a reader
	// triggers chapter by chapter): scoring/generation are still inherently
	// chapter-scoped work (see MusicRegion/Passes.Ambience), but whether the
	// feature runs at all is one setting for the whole book, set via the
	// same PUT .../voice endpoint SpeechDirection above uses. Off by
	// default, so uploading or reading a book never triggers Stable Audio
	// work on its own. Flipping it on doesn't eagerly score every chapter
	// at once - jobs.Manager.MaybeAdvanceChapterMusic only ever advances a
	// chapter that's actually been asked to score (see httpapi's
	// handleRunChapterAmbience, triggered explicitly per chapter - the
	// Speakers page's own per-row/bulk buttons, or the reader lazily
	// triggering the chapter it's currently on). Flipping it off doesn't
	// delete anything already scored/generated - MaybeAdvanceChapterMusic
	// just stops dispatching further region generation until it's back on.
	MusicEnabled bool
}

// CharacterVoiceMode is store.Book.CharacterVoiceMode's own type - one of
// four mutually exclusive settings for whether/how a character's own
// narration voice can override the book's:
type CharacterVoiceMode string

const (
	// CharacterVoiceModeNarrator: every paragraph narrates in the book's
	// own single voice, regardless of its attributed Speaker or any
	// character's assigned voice - the original, single-voice-per-book
	// behavior. Default for every book, so speaker attribution and
	// character-voice assignment can happen at any time without changing
	// how a book actually narrates until a reader explicitly opts into
	// something else. Frontend label: "Narrator only".
	CharacterVoiceModeNarrator CharacterVoiceMode = "narrator"
	// CharacterVoiceModeAssigned: a character with a voice explicitly
	// assigned (PUT /api/books/{id}/characters/{id}/voice) narrates in
	// that voice; an unassigned character still falls back to the book's
	// own. Frontend label: "Custom character voices".
	CharacterVoiceModeAssigned CharacterVoiceMode = "assigned"
	// CharacterVoiceModeInstructUnassigned: like Assigned, except a
	// character with *no* voice explicitly assigned resolves to the
	// book's own narrator voice - same reference clip, same preset -
	// styled by their own characterization (store.Character.Summary) sent
	// as a clone-time instruction, instead of internal/jobs lazily
	// auto-provisioning them an entirely separate, independently
	// VoiceDesign-rendered voice (see httpapi.provisionCharacterVoiceAttempt
	// and narration.ResolvedVoice.CloneInstruct). Only breeze_tts (the one
	// clone family that honors an instruction alongside a reference clip
	// in the same call - internal/audioworker's cloneFamily.
	// instructOption) actually does anything different here; any other
	// resolved clone model behaves exactly like Assigned. An explicitly
	// assigned character is unaffected either way. Frontend label: "Style
	// unassigned characters with the narrator's voice".
	CharacterVoiceModeInstructUnassigned CharacterVoiceMode = "instruct_unassigned"
	// CharacterVoiceModeInstructAll: every real character speaker resolves
	// to the book's own narrator voice styled by their own
	// characterization, unconditionally - even one with an explicit voice
	// assignment. That assignment isn't deleted, just ignored for
	// resolution while this mode is active; switching to Assigned or
	// InstructUnassigned restores it exactly as it was. Same breeze_tts/
	// already-characterized gating as InstructUnassigned. Frontend label:
	// "Style all characters with the narrator's voice".
	CharacterVoiceModeInstructAll CharacterVoiceMode = "instruct_all"
)

// DefaultCharacterVoiceMode is what a brand-new book starts with.
const DefaultCharacterVoiceMode = CharacterVoiceModeNarrator

// MultiVoice reports whether b has per-character narration voices turned
// on at all (any CharacterVoiceMode but CharacterVoiceModeNarrator) -
// internal/narration.Resolver.ForParagraph's own top-level gate, and
// httpapi/jobs' shared "is this book's speaker table/roster actually
// meaningful for narration" check.
func (b Book) MultiVoice() bool {
	return b.CharacterVoiceMode != CharacterVoiceModeNarrator
}

// InstructCharacterVoices reports whether b is specifically in
// CharacterVoiceModeInstructUnassigned - see that constant's own doc
// comment.
func (b Book) InstructCharacterVoices() bool {
	return b.CharacterVoiceMode == CharacterVoiceModeInstructUnassigned
}

// InstructAllCharacterVoices reports whether b is specifically in
// CharacterVoiceModeInstructAll - see that constant's own doc comment.
func (b Book) InstructAllCharacterVoices() bool {
	return b.CharacterVoiceMode == CharacterVoiceModeInstructAll
}

// VoicePreset is a user-created, reusable narrator voice: an instruct
// string, seed, and reference text, cloned into a stable reference clip via
// the ttsworker process (see internal/voicerefs) rather than the built-in
// curated presets (those live in internal/voices and are just compiled-in,
// never stored here).
type VoicePreset struct {
	ID       string
	Name     string
	Instruct string
	RefText  string
	Seed     int
	// Applied to the rendered reference clip (time-stretch) before it's
	// cloned from - see internal/voicerefs/internal/wsola. 1.0 = no change.
	SpeedMultiplier float64
	CreatedAt       int64
	// Which VoiceDesign engine renders this preset's reference clip (e.g.
	// "qwen3_tts", "breeze_tts" - see backend/internal/audioworker's
	// designEngines). "" defers to the worker's own process-wide default,
	// but a new preset is created with an explicit value (voices.
	// DefaultDesignModel, currently "breeze_tts" - see
	// httpapi.handleCreateCustomVoicePreset) so it doesn't silently change
	// if that default is later reconfigured.
	DesignModel string
}

// Passes is a chapter's own book-preprocess pipeline progress
// (chapters.passes, stored as a JSON object - the same "flexible,
// independent facts" shape tts_tags/pronunciation
// already use their own JSON columns for, rather than one forward-only
// ChapterState enum this replaced). A struct, not a map, on the Go side so
// call sites get a typed field (ch.Passes.Direction) instead of a
// string-keyed lookup a typo could silently miss; the column itself
// stays a flexible JSON object so a future pass can be added as just
// another field with no schema change.
type Passes struct {
	// Attribution is set once speaker attribution
	// (internal/speakerattr.Client.AttributeChapter, via
	// httpapi.attributeChapter) has completed over the whole chapter at
	// least once, in one uninterrupted pass (possibly across several
	// paused/resumed continuations - never set on a pause or a genuine
	// failure partway through, only a full completion).
	Attribution bool `json:"attribution"`
	// ScareQuote is set once scare-quote tagging
	// (internal/speakerattr.Client.ScareQuoteChapter, a jobs.KindScareQuote
	// task - see httpapi.scareQuoteChapterForJob) has completed over the
	// whole chapter, persisting Store.SetParagraphScareQuotes. Both
	// attribution waits on this
	// (jobs.Manager.scareQuoteDependency).
	ScareQuote bool `json:"scareQuote"`
	// Direction is set once emotion labeling
	// (internal/speakerattr.Client.EmotionChapter, via
	// httpapi.directChapter - any clone model) has completed over the whole
	// chapter at least once, the same "full pass, not a pause/failure" contract as
	// Attribution. Fully independent of Attribution - a
	// reader can trigger "Tag directions" directly on a chapter that was
	// never attributed via the per-chapter button, bypassing
	// runBookPipeline's own attribute-then-tag ordering entirely, and this
	// struct (unlike the old single ChapterState enum it replaced)
	// represents that combination just fine.
	Direction bool `json:"direction"`
	// Pronunciation is set once pronunciation resolution
	// (internal/speakerattr.Client.ResolvePronunciation, a
	// jobs.KindPronunciation task via httpapi.pronounceChapter) has
	// completed a full pass over the chapter - Direction's own contract,
	// but its own pass.
	Pronunciation bool `json:"pronunciation"`
	// Ambience is set once the ambience pass's music sub-pass
	// (internal/speakerattr.Client.ScoreMusic, via httpapi.ambienceChapter)
	// has completed a full, uninterrupted pass over this chapter (never on
	// a pause or a genuine failure partway through) - Direction's own
	// "full pass, not a pause/failure" contract, and just as independent of
	// Attribution/Direction as Direction itself is: a reader
	// can trigger the ambience pass for a chapter regardless of whether
	// it's ever been attributed/direction-tagged. Triggered by
	// httpapi.handleRunChapterAmbience (the Speakers page's per-chapter
	// button/bulk actions, book preprocessing's own ambience phase, or the
	// reader lazily running whichever chapter it's currently on) -
	// deliberately NOT gated on store.Book.MusicEnabled: that toggle only
	// controls whether a scored region's audio actually gets generated
	// (jobs.Manager.MaybeAdvanceChapterMusic), not whether scoring itself
	// can run (see Book.MusicEnabled's own doc comment). Stored as
	// "music" until the pass was renamed (and backfilled by hand).
	Ambience bool `json:"ambience"`
	// Lighting is set once the ambience pass's lighting sub-pass
	// (internal/speakerattr.Client.DescribeLighting, via
	// httpapi.lightChapter) has given every one of the chapter's music
	// regions a MusicRegion.Lighting cue. Runs after the music sub-pass in
	// the same task (it lights the regions scoring produced), but is its
	// own flag so a chapter scored before lighting existed - Ambience set,
	// Lighting not - gets just the lighting sub-pass on its next ambience
	// run, without re-scoring its music.
	Lighting bool `json:"lighting"`
	// Summary is set once the summary pass (internal/speakerattr.Client.
	// SummarizeChapter, a jobs.KindSummary task via httpapi.
	// summarizeChapter) has stored the chapter's ChapterNotes. Speaker
	// attribution and the ambience pass wait on it (jobs.Manager.
	// summaryDependency) to read its cast and scenes.
	Summary bool `json:"summary"`
}

// AmbienceDone reports whether the whole ambience pass - music scoring and
// its lighting sub-pass - has finished for the chapter.
func (p Passes) AmbienceDone() bool { return p.Ambience && p.Lighting }

// scanJSON is the shared implementation behind every JSON column's own
// Scan method in this package (Passes, Lighting,
// AliasList, PronunciationList): the column holds JSON text, decoded into
// dst. SQL NULL leaves dst at its zero value.
func scanJSON(v any, dst any) error {
	var data []byte
	switch t := v.(type) {
	case nil:
		return nil // dst already holds its own zero value
	case string:
		data = []byte(t)
	case []byte:
		data = t
	default:
		return fmt.Errorf("decode JSON column: unexpected %T", v)
	}
	if len(data) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("decode JSON column: %w", err)
	}
	return nil
}

// jsonValue is scanJSON's Value-side counterpart, shared by the same set
// of types' own driver.Valuer implementations.
func jsonValue(v any) (driver.Value, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(data), nil
}

// Scan implements sql.Scanner so a chapters.passes row (a native JSON
// column) can be read straight into a Passes value via rows.Scan(&p) -
// see scanJSON's own doc comment for why this bridge is necessary at all.
func (p *Passes) Scan(v any) error { return scanJSON(v, p) }

// Value implements driver.Valuer so a Passes value can be passed directly
// as a query argument (e.g. `db.Exec(update, p, chapterID)`) instead of
// every call site marshaling it to a string by hand first.
func (p Passes) Value() (driver.Value, error) { return jsonValue(p) }

type Chapter struct {
	ID     string
	BookID string
	Idx    int
	Title  string
	// Passes is this chapter's own book-preprocess pipeline progress - see
	// Passes' own doc comment. httpapi's own JSON DTOs still expose
	// attributed/directed separately, derived from this struct, so the API
	// surface existing frontend code depends on is unchanged.
	Passes Passes
}

// MusicRegion is one contiguous span of a chapter's own paragraphs sharing
// one background-music tone, as judged by internal/speakerattr.Client.
// ScoreMusic - every paragraph in [StartIdx, EndIdx] (inclusive, by
// store.Paragraph.Idx) gets this region's own generated clip playing under
// it. ScoreMusic only ever decides a region's StartIdx - EndIdx is filled in
// separately, by Store.AppendMusicRegions (see EndIdx's own doc comment) -
// so a chapter's regions are contiguous and non-overlapping by
// construction, covering every paragraph exactly once.
//
// Generation (internal/musicgen.GenerateRegion, dispatched via
// jobs.Manager's KindMusicGeneration) waits for every paragraph in the
// whole chapter to have real, ready narration audio first (see
// jobs.Manager.MaybeAdvanceChapterMusic) - a region's own
// TargetDurationSeconds (computed at dispatch time, not
// stored) is the sum of those paragraphs' own real narration durations, so
// the generated clip actually matches how long the region plays under
// for, whatever that turns out to be (no artificial minimum - see
// musicgen.GenerateRegion's own doc comment; it does pad every clip by a
// fixed crossfadePaddingSeconds to cover the clients' boundary crossfades) - and, for a Transition of
// "continuation", for the immediately preceding region (by Idx) to have
// already finished generating, so its own clip is available to seed this
// one's first chunk from (see internal/musicgen's own package doc comment
// for what "seed" means here and why cross-region stitching/crossfading
// is deliberately NOT done server-side - this app's chosen delivery is
// per-region clips, mixed live client-side).
type MusicRegion struct {
	ID        string
	ChapterID string
	Idx       int // this region's ordinal within the chapter, 0-based
	StartIdx  int // first paragraph Idx covered, inclusive - the only boundary ScoreMusic/AppendMusicRegions ever actually stores
	// EndIdx is the last paragraph Idx this region covers, inclusive - a
	// real, stored column (for cheap reads - every real caller lists a
	// whole chapter's regions at once), but never itself scored/trusted
	// from the LLM (see speakerattr.MusicRegionResult, which carries no
	// end of its own at all) - Store.AppendMusicRegions is the only thing
	// that ever computes and writes it, from sibling StartIdx values (the
	// next region's own StartIdx minus one; or the chapter's own last
	// paragraph Idx for the chapter's last region - see AppendMusicRegions'
	// own doc comment for how it handles that provisionally, before a
	// possible later batch). A region "persists until the next region"
	// rather than the LLM declaring its own end, both because that's one
	// less thing speakerattr.Client.ScoreMusic's own batches can get wrong
	// relative to each other and because it means a chapter's regions can
	// never have a gap or overlap in the first place, by construction,
	// rather than by validation.
	EndIdx int
	Mood   string
	Prompt string
	// Ambience is the Stable Audio prompt for the region's ambient
	// soundscape layer (speakerattr.MusicRegionResult.Ambience), rendered
	// separately and mixed under the music - "" means music only.
	Ambience   string
	Transition MusicTransition
	// Lighting is the region's room-lighting cue from the ambience pass's
	// lighting sub-pass (Passes.Lighting) - the zero value (IsSet false)
	// until that sub-pass has reached this region. The backend only
	// stores it; clients drive the actual lights (Philips Hue) themselves.
	Lighting Lighting
	// Status is one of AudioPending/AudioGenerating/AudioReady/AudioError -
	// the same four-state vocabulary paragraph/SFX audio already uses,
	// reused here rather than inventing a parallel one.
	Status          string
	Error           string
	DurationSeconds float64
}

// MusicTransition is MusicRegion.Transition's own type - see
// internal/speakerattr's ScoreMusic and internal/musicgen's own package
// doc comment for what each value drives.
type MusicTransition string

const (
	// MusicTransitionCut: this region's music should feel like a hard cut
	// from whatever played before it (a new scene, a chapter break, an
	// abrupt tonal reversal) - generated unseeded. Always the value for a
	// chapter's very first region, regardless of what ScoreMusic itself
	// returns for it (there's nothing to continue from).
	MusicTransitionCut MusicTransition = "cut"
	// MusicTransitionContinuation: this region's music should evolve
	// smoothly out of the previous region's own mood - generated with its
	// first chunk seeded from the previous region's own clip. Playback
	// crossfades every region switch the same way regardless of this value.
	MusicTransitionContinuation MusicTransition = "continuation"
)

// Lighting is one music region's room-lighting cue (music_regions.lighting,
// a native JSON column - NULL until the lighting sub-pass reaches the
// region). Deliberately device-agnostic: an sRGB color plus a brightness
// and a named effect, which a client maps onto whatever lights it drives.
type Lighting struct {
	// Description is the lighting sub-pass's own short description of the
	// light ("dim flickering candlelight in a stone cellar") - display only.
	Description string `json:"description,omitempty"`
	// Color is the base color, "#rrggbb".
	Color string `json:"color"`
	// Accent is a second "#rrggbb" color the effect moves toward (a
	// shimmer's other color, a lightning flash's tint, a siren's second
	// color) - "" for none.
	Accent string `json:"accent,omitempty"`
	// Brightness is 1-100, a fraction of the client's own brightness cap
	// rather than an absolute level.
	Brightness int `json:"brightness"`
	// Effect is how the light moves over time - see LightEffect.
	Effect LightEffect `json:"effect"`
}

// IsSet reports whether the lighting sub-pass has filled l in.
func (l Lighting) IsSet() bool { return l.Color != "" }

// Scan implements sql.Scanner for the music_regions.lighting JSON column -
// see scanJSON. NULL scans to the zero value.
func (l *Lighting) Scan(v any) error { return scanJSON(v, l) }

// Value implements driver.Valuer, storing the zero value as NULL.
func (l Lighting) Value() (driver.Value, error) {
	if !l.IsSet() {
		return nil, nil
	}
	return jsonValue(l)
}

// LightEffect is Lighting.Effect's own type. Clients animate these
// themselves (see frontend/src/lighting and android's playback/lighting) -
// the backend never talks to a light.
type LightEffect string

const (
	// LightEffectSteady holds the color.
	LightEffectSteady LightEffect = "steady"
	// LightEffectFlicker jitters brightness around the base like a flame -
	// firelight, candles, torches.
	LightEffectFlicker LightEffect = "flicker"
	// LightEffectPulse breathes slowly up and down - dread, a heartbeat,
	// magic.
	LightEffectPulse LightEffect = "pulse"
	// LightEffectLightning holds the base with occasional brief flashes of
	// the accent - a thunderstorm.
	LightEffectLightning LightEffect = "lightning"
	// LightEffectShimmer drifts slowly between Color and Accent - water,
	// aurora, moonlight through leaves.
	LightEffectShimmer LightEffect = "shimmer"
	// LightEffectFlash blinks on a 2s cycle - a siren or alarm: between
	// Color and Accent (police red and blue), or with no Accent between
	// Color and near dark.
	LightEffectFlash LightEffect = "flash"
)

// LightEffects lists every LightEffect, in display order.
var LightEffects = []LightEffect{LightEffectSteady, LightEffectFlicker, LightEffectPulse, LightEffectLightning, LightEffectShimmer, LightEffectFlash}

type ChapterSummary struct {
	Chapter
	ParagraphCount int
	ReadyCount     int
}

// AliasList implements sql.Scanner/driver.Valuer over characters.aliases -
// see scanJSON's own doc comment.
type AliasList []string

func (a *AliasList) Scan(v any) error            { return scanJSON(v, a) }
func (a AliasList) Value() (driver.Value, error) { return jsonValue(a) }

// PronunciationList implements sql.Scanner/driver.Valuer over
// paragraphs.pronunciation - see scanJSON's own doc comment.
type PronunciationList []pronounce.Substitution

func (p *PronunciationList) Scan(v any) error            { return scanJSON(v, p) }
func (p PronunciationList) Value() (driver.Value, error) { return jsonValue(p) }

type Paragraph struct {
	ID              string
	ChapterID       string
	Idx             int // sequential text-block index; drives TTS and playback position
	Position        int // ordinal among ALL blocks (text+image) in the chapter, for display order
	Text            string
	AudioStatus     string
	AudioError      string
	DurationSeconds float64
	// WordTimings is raw JSON (paragraph_audio.word_timings) - a
	// [{text,start,end}] array from tts-service's POST /align,
	// or "[]" before alignment has run. Passed straight through to the
	// frontend (httpapi.paragraphDTO) without being unmarshaled on this
	// side; nothing in Go needs to inspect individual words.
	WordTimings string
	// Speaker is "" (never attributed) or "Narrator" - both narrate in the
	// book's own voice - or a character name, set by internal/speakerattr
	// via httpapi's attribute-speakers endpoint (or manually). A non-empty,
	// non-"Narrator" name only actually changes this paragraph's narration
	// voice once that name is registered in the characters table AND
	// assigned a voice - see internal/narration.Resolver.
	Speaker string
	// SpeakerIndex tells apart several unnamed people who share Speaker's
	// role label (store.Character.IsRole) in one conversation: attribution
	// answers "Guard 1", "Guard 2" (speakerattr.Client.AttributeChapter's
	// indexes) and stores Speaker "Guard" with the number here. 0 is a
	// line with no number. The numbers are slots, not identities - "Guard
	// 2" in the next scene is someone else - and mean nothing on a line
	// whose Speaker isn't a role. Every write of Speaker sets it too.
	SpeakerIndex int
	// SourceSpeaker is who the book itself says speaks this line - a
	// play's speaker label, read at import (internal/epub's play pass).
	// Speaker starts as it; attribution takes it as given rather than
	// asking (httpapi's runAttribution) and resetting attribution puts
	// it back. "" in prose, and on a play's narration.
	SourceSpeaker string
	// Label is text shown before the paragraph but never spoken or
	// aligned: a play's speaker label as the book sets it ("THESEUS.")
	// with any direction on how the line is delivered ("[Calling.]").
	Label string
	// Inline marks this paragraph as a split-out continuation of the
	// immediately preceding paragraph, rather than the start of a new
	// visual one - set at import time when a source paragraph mixing
	// narration and quoted dialogue is split into separately-attributable/
	// voiceable segments (see internal/epub's splitQuoteSegments). Always
	// false for a paragraph that starts a (possibly single-segment) visual
	// paragraph. Mainly a display hint for the frontend - it doesn't affect
	// attribution or playback ordering, which still operate on Idx/Position
	// exactly as for any other paragraph - but jobs.Manager's generation
	// dispatch does read it (alongside ScareQuote) to reconstruct a scare
	// quote's own merge group; see ScareQuote's own doc comment.
	Inline bool
	// IsQuote marks this paragraph as an actual quoted-dialogue span (see
	// internal/epub's Block.IsQuote/splitQuoteSegments), as opposed to a
	// narration/description segment. httpapi.attributeChapter refuses to
	// persist a character-name Speaker on a paragraph where this is
	// false, regardless of what internal/speakerattr's model decided -
	// narration *about* a character isn't that character speaking.
	IsQuote bool
	// ScareQuote marks an IsQuote paragraph as NOT actually spoken dialogue
	// despite looking like it structurally - quotation marks used for
	// sarcasm/skepticism, mentioning a term rather than uttering it, or
	// quoting a written source (see internal/speakerattr.Client.
	// ScareQuoteChapter, and store.Store.SetParagraphScareQuotes, which
	// this is decoded from) - set either by that pass or by a reader's own
	// manual annotation. Always false for a non-IsQuote paragraph -
	// httpapi enforces this the same deterministic way it already does for
	// Speaker. jobs.Manager's generation dispatch
	// treats a flagged paragraph, and every paragraph reachable from it by
	// following Inline in either direction (the same run
	// internal/epub.splitQuoteSegments originally split out of one source
	// paragraph), as one merged TTS call rather than one call per
	// paragraph - see jobs.Manager.mergeGroup - so the scare-quoted phrase
	// is voiced in the same breath as the narration around it instead of
	// landing as its own oddly-isolated clip.
	ScareQuote bool
	// Emotion is this paragraph's delivery emotion (an internal/emotions
	// id), or emotions.Neutral ("") - set by the emotion pass
	// (internal/speakerattr.Client.EmotionChapter, via
	// httpapi.directChapter) or a reader's manual override. Only ever
	// honored for real dialogue - see EffectiveEmotion.
	Emotion string
	// Pronunciation is this paragraph's own resolved pronunciation
	// substitutions (internal/speakerattr.Client.ResolvePronunciation),
	// decoded from the paragraphs.pronunciation column (see
	// Store.SetParagraphPronunciation and ResolveGenerationText). Not
	// clone_model-keyed - a pronunciation fix is plain word
	// substitution, independent of which TTS model narrates it. nil/empty
	// (the common case) means no ambiguous abbreviation was found in this
	// paragraph at all.
	Pronunciation PronunciationList
	// Emphasis is this paragraph's own structural emphasis substitutions -
	// epub markup (<strong>/<b> respelled in upper case, <em>/<i> wrapped
	// in "scare quotes") resolved once at import time
	// (internal/epub.extractEmphasis), decoded from the
	// paragraphs.emphasis column. Same shape and generation-time
	// treatment as Pronunciation (see ResolveGenerationText, which merges
	// both into one combined substitution list) but from a structural,
	// non-LLM source computed at a different time - kept in its own
	// column rather than folded into Pronunciation so re-running
	// pronunciation resolution can never clobber it, and vice versa.
	// nil/empty (the common case) means no emphasis markup was found in
	// this paragraph's own source HTML.
	Emphasis PronunciationList
	// Table places this paragraph in a source <table>: one row's cells
	// joined into Text (epub.CellSeparator), or one paragraph of a cell
	// holding several - set at import (internal/epub's table.go), the zero
	// value outside a table. The clients lay a table's paragraphs out as
	// a grid; ResolveGenerationText reads a row as sentences, each cell
	// introduced by its column heading.
	Table TablePos
}

// Unvoiced reports whether p is a beat of silence rather than something
// said: nothing but an ellipsis ("…", "...", "“…”", ". . ."), or a table's
// heading row, whose headings are spoken with each row under it instead
// (TablePos.Heading). It gets no speaker and no emotion, and its clip is
// silence written directly (jobs.silentClip) rather than a TTS render.
func (p Paragraph) Unvoiced() bool {
	if p.Table.Heading {
		return true
	}
	dots := 0
	for _, r := range p.Text {
		switch {
		case r == '…':
			dots += 3
		case r == '.':
			dots++
		case unicode.IsSpace(r) || strings.ContainsRune("\"“”„«»'‘’", r):
		default:
			return false
		}
	}
	return dots >= 2
}

// EffectiveEmotion is the emotion this paragraph actually generates with:
// Emotion for real spoken dialogue (IsQuote and not a ScareQuote) when it
// names a known emotion, emotions.Neutral otherwise. Narration always
// stays neutral - the narrator keeps one consistent delivery - and so does
// a stale id no longer in emotions.All.
func (p Paragraph) EffectiveEmotion() string {
	if !p.IsQuote || p.ScareQuote || p.Unvoiced() || !emotions.Valid(p.Emotion) {
		return emotions.Neutral
	}
	return p.Emotion
}

// ResolveGenerationText returns p's actual generation-time text for
// cloneModel: p.Text with every Pronunciation and Emphasis substitution
// applied (pronounce.Compose - a fix inside an emphasized span is nested
// into it rather than dropped), plus - for Higgs only - a <|prosody:pause|> before each
// ellipsis/em dash (deliverytags.PauseInsertions), composed in one pass
// (pronounce.Apply - see its own doc comment for why a single combined
// pass keeps every offset meaningful). p.Text itself, unchanged, in the
// common case of nothing to apply. Pauses are computed here rather than
// stored: they're a pure function of the text, so there's nothing to
// persist or invalidate. Formatting characters (list bullets, box and
// rule glyphs - pronounce.FormattingSubstitutions) are stripped the same
// way, giving way to a pronunciation fix over the same span. A table row
// (Table) is read as sentences, each cell introduced by its column
// heading (TablePos.spoken). Never used for forced alignment/word timing,
// which always uses p.Text untouched. lexicon is the book's scope's
// listener-authored rules (ListLexicon), applied as EffectivePronunciation
// describes.
func (p Paragraph) ResolveGenerationText(cloneModel string, lexicon []pronounce.LexiconEntry) string {
	var insertions []deliverytags.Insertion
	if cloneModel == voices.HiggsCloneModel {
		insertions = deliverytags.PauseInsertions(p.Text)
	}
	labels, cellBreaks := p.Table.spoken(p.Text)
	insertions = append(insertions, labels...)
	pron := pronounce.MergeLexicon(pronounce.MergeLexicon(p.EffectivePronunciation(lexicon), cellBreaks), pronounce.FormattingSubstitutions(p.Text))
	if len(insertions) == 0 && len(pron) == 0 && len(p.Emphasis) == 0 {
		return p.Text
	}
	return pronounce.Apply(p.Text, insertions, pronounce.Compose(p.Text, pron, p.Emphasis))
}

// EffectivePronunciation is p.Pronunciation (the LLM pass's picks) merged
// with every whole-word match of lexicon's rules in p.Text - the rules win
// wherever the two overlap (pronounce.MergeLexicon).
func (p Paragraph) EffectivePronunciation(lexicon []pronounce.LexiconEntry) []pronounce.Substitution {
	return pronounce.MergeLexicon(pronounce.LexiconSubstitutions(p.Text, lexicon), p.Pronunciation)
}

// Character is a speaking character, discovered by speaker attribution
// (internal/speakerattr) or created directly, scoped by Scope (see
// SeriesScope) rather than to one book - a character recurring across a
// series shares one row (identity + Summary) across every book in it,
// *regardless* of which narrator voice/clone model each of those books
// uses. Voice *assignment* is a separate, per-clone-model concern - see
// CharacterVoiceForModel/CharacterVoicesForModel/SetCharacterVoice -
// keyed by the book's own clone model (Book.CloneModel), so a voice cast
// under one model doesn't carry over to another; a character can have a
// different assigned preset under "audiocpp-qwen3-0.6b" than under
// "audiocpp-higgs-4b", auto-created independently the first time each
// model encounters them (see httpapi.provisionCharacterVoice, called lazily -
// see jobs.Manager.provisionMissingCharacterVoices). Until a voice is
// assigned for the clone model currently in use, paragraphs attributed to
// this character still narrate in the book's own voice
// (internal/narration.Resolver).
type Character struct {
	ID    string
	Scope string
	Name  string
	// Summary is the LLM's natural-language characterization of how this
	// character should sound - gender, age, tone, speaking style - derived
	// from a sample of their own attributed dialogue
	// (speakerattr.Client.CharacterizeVoice) and used verbatim as each
	// auto-created voice preset's Instruct, shared across every clone
	// model's own preset for this character since it describes the
	// character, not the model. "" until that characterization has run
	// (e.g. this character's voice was first needed but they still had too
	// few quotes to characterize from, or the LLM call failed - see
	// httpapi.characterizeVoice). Kept even after voice assignment so it's
	// visible/reviewable rather than thrown away once consumed.
	Summary string
	// RefLine is a short generic reference passage paired with Summary,
	// written by the same speakerattr.Client.CharacterizeVoice call so its
	// sentence rhythm/pace actually matches the voice Summary describes -
	// used verbatim as that character's auto-created voice preset's
	// RefText (see httpapi.provisionCharacterVoice) in place of
	// voices.DefaultRefText, so the rendered reference clip gets to
	// perform the delivery Summary asks for rather than reading a flat,
	// unrelated passage regardless of how the character is meant to
	// sound. "" alongside an empty Summary (not yet characterized); falls
	// back to voices.DefaultRefText at the one call site that consumes it
	// as a defensive default, not because this should ever legitimately
	// be empty once Summary isn't.
	RefLine string
	// IsRole marks this identity as a generic role/type label (e.g.
	// "Guard", "Thug", "Merchant") rather than one consistent named
	// individual - set when speaker attribution
	// (internal/speakerattr.Client.AttributeChapter) first introduces the
	// name with its own "role" flag, since the narration gave a generic
	// function/type for whoever's speaking rather than a proper name.
	// Sticky from first sight: UpsertCharacter only ever sets this on the
	// row's initial insert, never flips it afterward - a later batch
	// misjudging an already-known individual as a role (or vice versa)
	// shouldn't silently reclassify them. Read by
	// speakerattr.Client.CharacterizeVoice's caller (httpapi.characterizeVoice)
	// to warn the casting prompt that the dialogue sample below may be
	// drawn from several different unnamed people who happened to share
	// this label, not one consistent personality.
	IsRole bool
	// Aliases are other names this character goes by ("Albert"/"Al" for
	// "Bert", "Nic" for "Jagged Nic") - listed in attribution's "Known
	// characters" prompt, folded back into Name when the model answers
	// with one, and used to read speech tags ("Albert said") - see
	// speakerattr.SpeakerNameResolver. Set from the Speakers page, and
	// added automatically when another character is merged into this one
	// (the merged-away name becomes an alias). Sorted.
	Aliases AliasList
	// Pronouns are the pronouns the text uses for this character - one of
	// "he/him", "she/her", "they/them", "it/its", or "" when never
	// established. Filled in by speaker attribution (the majority answer
	// across a chapter's lines, only while still "" - never overwriting
	// an existing value), editable from the Speakers page, and handed to
	// characterization as its gender signal
	// (speakerattr.Client.CharacterizeVoice).
	Pronouns string
	// Invalid marks this name as not a real speaker at all (a stray
	// pronoun, a vocative, a generic phrase the LLM keeps latching onto) -
	// set explicitly from the Speakers page or by an "Auto Split" of this
	// character (httpapi.handleReattributeSpeaker). The row is kept as a
	// tombstone rather than deleted precisely so the name *stays* known:
	// deleting it would just let the next attribution pass rediscover and
	// re-create it. Attribution/description passes withhold an invalid
	// name from "Known characters" and remap any result naming it to
	// "Unknown" (or drop it, for descriptions) instead of assigning it;
	// characterization/voice provisioning skip it. Paragraphs already
	// attributed to it are left alone - Auto Split is what redistributes
	// those.
	Invalid   bool
	CreatedAt int64
}

// SeriesScope is the key characters (and their voice assignments) are
// shared under: books sharing a non-empty SeriesName share one character
// roster - and so one consistent voice per recurring character - across
// the whole series; a book with no series gets its own scope keyed by its
// own id, so it keeps a private, book-only roster exactly as before series
// support existed.
func SeriesScope(book *Book) string {
	if book.SeriesName != "" {
		return "series:" + book.SeriesName
	}
	return "book:" + book.ID
}

// Image is an inline illustration found in a chapter's original markup. It
// has no audio of its own; Position places it among the chapter's
// paragraphs for display purposes only.
type Image struct {
	ID        string
	ChapterID string
	Position  int
	Ext       string // e.g. ".jpg", including the dot
}

// Break is a scene/section break (BlockBreak) - position-only, same shape
// as Image minus the file bytes.
type Break struct {
	ID        string
	ChapterID string
	Position  int
}
