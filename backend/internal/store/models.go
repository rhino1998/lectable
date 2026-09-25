package store

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"

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
	PosChapterIdx   int
	PosParagraphIdx int
	PosSeconds      float64
	// EstimateSecPerChar is jobs.Manager.EnqueueLengthEstimate's own
	// PocketTTS-calibrated seconds-per-character measurement - see the
	// schema's own doc comment on estimate_sec_per_char.
	EstimateSecPerChar float64
	// MusicEnabled is the reader-facing background-music toggle - book-
	// wide, not per-chapter (unlike attribution/direction, which a reader
	// triggers chapter by chapter): scoring/generation are still inherently
	// chapter-scoped work (see MusicRegion/Passes.Music), but whether the
	// feature runs at all is one setting for the whole book, set via the
	// same PUT .../voice endpoint SpeechDirection above uses. Off by
	// default, so uploading or reading a book never triggers Stable Audio
	// work on its own. Flipping it on doesn't eagerly score every chapter
	// at once - jobs.Manager.MaybeAdvanceChapterMusic only ever advances a
	// chapter that's actually been asked to score (see httpapi's
	// handleScoreChapterMusic, triggered explicitly per chapter - the
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
// independent facts" shape tts_tags/describes_characters/pronunciation
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
	// Description is set once description tagging
	// (internal/speakerattr.Client.DescribeChapter, a jobs.KindDescription
	// task - see httpapi.describeChapterForJob) has completed over the
	// whole chapter, persisting which characters each narration paragraph
	// describes (Store.SetParagraphDescriptions). Its own task, dependent
	// on ScareQuote but not on Attribution.
	Description bool `json:"description"`
	// ScareQuote is set once scare-quote tagging
	// (internal/speakerattr.Client.ScareQuoteChapter, a jobs.KindScareQuote
	// task - see httpapi.scareQuoteChapterForJob) has completed over the
	// whole chapter, persisting Store.SetParagraphScareQuotes. Both
	// attribution and description tagging wait on this
	// (jobs.Manager.scareQuoteDependency).
	ScareQuote bool `json:"scareQuote"`
	// Direction is set once emotion labeling
	// (internal/speakerattr.Client.EmotionChapter, via
	// httpapi.directChapter - any clone model) has completed over the whole
	// chapter at least once, the same "full pass, not a pause/failure" contract as
	// Attribution. Fully independent of Attribution/Description - a
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
	// Music is set once internal/speakerattr.Client.ScoreMusic has
	// completed a full, uninterrupted pass over this chapter (never on a
	// pause or a genuine failure partway through) - Direction's own "full
	// pass, not a pause/failure" contract, and just as independent of
	// Attribution/Description/Direction as Direction itself is: a reader
	// can trigger background-music scoring for a chapter regardless of
	// whether it's ever been attributed/direction-tagged. Triggered by
	// httpapi.handleScoreChapterMusic (the Speakers page's per-chapter
	// button/bulk actions, book preprocessing's own music phase, or the
	// reader lazily scoring whichever chapter it's currently on) -
	// deliberately NOT gated on store.Book.MusicEnabled: that toggle only
	// controls whether a scored region's audio actually gets generated
	// (jobs.Manager.MaybeAdvanceChapterMusic), not whether scoring itself
	// can run (see Book.MusicEnabled's own doc comment).
	Music bool `json:"music"`
}

// scanJSON is the shared implementation behind every native-JSON column's
// own Scan method in this package (Passes, DescribesCharacterList,
// ParagraphTagMap, PronunciationList): this driver decodes a JSON column
// into a plain Go value (map[string]any/[]any for an object/array, nil for
// SQL NULL) rather than handing back the original raw text (confirmed
// empirically against both go-duckdb and the official duckdb-go: scanning
// a JSON column into *string/*[]byte fails outright, only `any` works), so
// every one of these types bridges that decoded value into its own typed
// shape via one encoding/json round trip, instead of every call site
// doing that bridging by hand.
func scanJSON(v any, dst any) error {
	if v == nil {
		return nil // dst already holds its own zero value
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("re-marshal scanned JSON value: %w", err)
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
// it. Only StartIdx is ever actually scored by the LLM, per region -
// EndIdx is filled in separately, by Store.AppendMusicRegions rather than
// speakerattr (see EndIdx's own doc comment) - so a chapter's regions are
// contiguous and non-overlapping by construction, covering every
// paragraph exactly once, rather than something ScoreMusic/
// parseMusicBoundaries have to separately validate against gaps/overlaps the
// way an explicit LLM-supplied end once required.
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

type ChapterSummary struct {
	Chapter
	ParagraphCount int
	ReadyCount     int
}

// DescribesCharacterList implements sql.Scanner/driver.Valuer over
// paragraphs.describes_characters (a native JSON column) - see scanJSON's
// own doc comment for why. A named type only because Go requires one to
// attach methods to at all; assignable to/from a plain []string anywhere
// in this codebase with no explicit conversion needed, since both share
// the same underlying (unnamed) type - see the Go spec's assignability
// rule for why that's enough.
type DescribesCharacterList []string

func (d *DescribesCharacterList) Scan(v any) error            { return scanJSON(v, d) }
func (d DescribesCharacterList) Value() (driver.Value, error) { return jsonValue(d) }

// AliasList implements sql.Scanner/driver.Valuer over characters.aliases -
// see DescribesCharacterList's own doc comment.
type AliasList []string

func (a *AliasList) Scan(v any) error            { return scanJSON(v, a) }
func (a AliasList) Value() (driver.Value, error) { return jsonValue(a) }

// PronunciationList implements sql.Scanner/driver.Valuer over
// paragraphs.pronunciation - see DescribesCharacterList's own doc comment.
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
	// Speaker/DescribesCharacters. jobs.Manager's generation dispatch
	// treats a flagged paragraph, and every paragraph reachable from it by
	// following Inline in either direction (the same run
	// internal/epub.splitQuoteSegments originally split out of one source
	// paragraph), as one merged TTS call rather than one call per
	// paragraph - see jobs.Manager.mergeGroup - so the scare-quoted phrase
	// is voiced in the same breath as the narration around it instead of
	// landing as its own oddly-isolated clip.
	ScareQuote bool
	// DescribesCharacters is which characters (if any) this paragraph's
	// narration physically/personality-describes - decoded from the
	// paragraphs.describes_characters column (see
	// Store.SetParagraphDescriptions/internal/speakerattr's "Describe"
	// stage). Always IsQuote == false when non-empty: only narration is
	// ever tagged as a description, never a quoted line. nil until a
	// Describe run has tagged this paragraph.
	DescribesCharacters DescribesCharacterList
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
}

// EffectiveEmotion is the emotion this paragraph actually generates with:
// Emotion for real spoken dialogue (IsQuote and not a ScareQuote) when it
// names a known emotion, emotions.Neutral otherwise. Narration always
// stays neutral - the narrator keeps one consistent delivery - and so does
// a stale id no longer in emotions.All.
func (p Paragraph) EffectiveEmotion() string {
	if !p.IsQuote || p.ScareQuote || !emotions.Valid(p.Emotion) {
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
// persist or invalidate. Never used for forced alignment/word timing,
// which always uses p.Text untouched.
func (p Paragraph) ResolveGenerationText(cloneModel string) string {
	var insertions []deliverytags.Insertion
	if cloneModel == voices.HiggsCloneModel {
		insertions = deliverytags.PauseInsertions(p.Text)
	}
	if len(insertions) == 0 && len(p.Pronunciation) == 0 && len(p.Emphasis) == 0 {
		return p.Text
	}
	return pronounce.Apply(p.Text, insertions, pronounce.Compose(p.Text, p.Pronunciation, p.Emphasis))
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
