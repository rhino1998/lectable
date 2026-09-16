// Package store persists the library (books, chapters, paragraphs, and
// per-paragraph, per-voice TTS audio status) in DuckDB.
//
// Referential integrity (deleting a book's chapters/paragraphs) is handled
// explicitly in application code rather than via FK ON DELETE CASCADE, to
// stay on the simplest, most portable subset of DuckDB's SQL support.
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

const schema = `
CREATE TABLE IF NOT EXISTS books (
	id TEXT PRIMARY KEY,
	title TEXT NOT NULL,
	author TEXT NOT NULL DEFAULT '',
	language TEXT NOT NULL DEFAULT '',
	cover_ext TEXT NOT NULL DEFAULT '',
	series_name TEXT NOT NULL DEFAULT '',
	series_index DOUBLE NOT NULL DEFAULT 0,
	added_at BIGINT NOT NULL,
	voice_preset_id TEXT NOT NULL DEFAULT 'velvet-narrator',
	voice_instruct TEXT NOT NULL DEFAULT '',
	voice_language TEXT NOT NULL DEFAULT 'Auto',
	voice_seed INTEGER NOT NULL DEFAULT 1006, -- resolved from voice_preset_id at voice-selection time; see httpapi.resolveVoiceSeed - matches voices.Presets' velvet-narrator entry
	-- Whether/how a character's own narration voice can override this
	-- book's own - see store.CharacterVoiceMode's own doc comment for the
	-- four possible values ('narrator'/'assigned'/'instruct_unassigned'/
	-- 'instruct_all'). Defaults to 'narrator' so an existing book's
	-- narration never changes until a reader explicitly opts into
	-- something else.
	character_voice_mode TEXT NOT NULL DEFAULT 'narrator',
	-- Gates jobs.Manager's speechDirectionDependency: while true, a
	-- paragraph clone task for this book waits on its chapter reaching
	-- store.ChapterStateTagged (direction-tagging) before generating -
	-- see that function's own doc comment. Defaults false (generate
	-- immediately, don't wait) rather than true, since waiting only ever
	-- matters for the Higgs clone model to begin with and most books never
	-- touch direction tagging at all - a reader opts in per book instead
	-- of every book paying an LLM-pass delay before its first audio.
	-- Flipping this false -> true invalidates the book's already-generated
	-- audio (see httpapi.handleUpdateVoice) since that audio was produced
	-- with no guarantee its chapter was ever actually tagged first.
	speech_direction BOOLEAN NOT NULL DEFAULT false,
	-- Reader-facing book-wide background-music toggle - see
	-- store.Book.MusicEnabled's own doc comment. Book-wide, not
	-- per-chapter: a chapter's own music_regions rows and passes.music
	-- fact (chapters.passes) still exist per-chapter regardless (scoring/
	-- generation are inherently chapter-scoped work), but whether the
	-- feature is on at all is one setting for the whole book, the same
	-- shape speech_direction above already uses. Off by default.
	music_enabled BOOLEAN NOT NULL DEFAULT false,
	pos_chapter_idx INTEGER NOT NULL DEFAULT 0,
	pos_paragraph_idx INTEGER NOT NULL DEFAULT 0,
	pos_seconds DOUBLE NOT NULL DEFAULT 0,
	-- Seconds-per-character measured from a one-off throwaway sample
	-- cloned through voices.FastPresetID (PocketTTS) right after upload -
	-- see jobs.Manager.EnqueueLengthEstimate/Store.SetLengthEstimate. 0
	-- until that sample finishes. Used by httpapi.estimateTotalSeconds as
	-- a real, generated-audio-calibrated fallback for the book's length
	-- estimate until the book's own actually-selected voice has real ready
	-- audio of its own to calibrate from (at which point that voice's own
	-- pace wins instead, being the more accurate of the two).
	estimate_sec_per_char DOUBLE NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS chapters (
	id TEXT PRIMARY KEY,
	book_id TEXT NOT NULL,
	idx INTEGER NOT NULL,
	title TEXT NOT NULL,
	-- This chapter's own pipeline progress, one independent boolean field
	-- per pass, JSON-encoded (store.Passes, marshaled/unmarshaled the same
	-- way tts_tags/describes_characters/pronunciation below already are) -
	-- see store.Chapter.Passes (models.go) for the full "why independent
	-- fields instead of one flat, forward-only state" reasoning (a single
	-- ChapterState enum - itself replacing a plain attributed bool plus a
	-- per-clone-model directed map - used to live here; replaced because
	-- attribution and direction-tagging are genuinely independent facts
	-- about a chapter, not sequential stages of one pipeline, and a single
	-- enum couldn't represent "tagged but never attributed" at all, which
	-- a manual "Tag directions" click can produce). Each field is a
	-- persisted fact, not derived from paragraph state (paragraphs.speaker
	-- counts, paragraphs.tts_tags presence) - a chapter with no dialogue at
	-- all can legitimately finish attribution with some/all of its
	-- narration paragraphs still carrying "" (a model that drops a
	-- paragraph's own JSON entry, or a blank-text paragraph
	-- indistinguishable from the "Lines:" blank-line paragraph-break marker
	-- itself - see speakerattr.attributeBatch), and most paragraphs
	-- legitimately get no delivery tag at all (see directionSystemPrompt's
	-- own "expect this short" instruction) - re-deriving "was this chapter
	-- actually processed" from either would misreport a chapter that
	-- genuinely finished a pass but happened to change nothing as "never
	-- run". Set by httpapi.attributeChapter/directChapter (Store.
	-- SetChapterAttributed/SetChapterDirected, each a read-modify-write
	-- that only touches its own field(s) - see their own doc comments)
	-- once a run completes over the *whole* chapter in one uninterrupted
	-- pass, never on a pause or a genuine failure partway through.
	-- attribution/description are cleared together by
	-- Store.ClearBookSpeakers when a chapter's own attribution specifically
	-- is cleared; direction is never touched by that, since it's now a
	-- fully independent field instead of entangled in the same value.
	-- A native JSON column (see Passes' own Scan/Value methods), not TEXT -
	-- the same choice paragraphs.describes_characters/tts_tags/pronunciation
	-- below now make too, after confirming DuckDB's own MAP and VARIANT
	-- types both refuse to bind as a query parameter through this driver
	-- (empirically confirmed against both go-duckdb and the official
	-- duckdb-go), while JSON - really just a validated VARCHAR under the
	-- hood - binds a plain Go string with an ordinary bound parameter
	-- exactly like this column's own predecessor did.
	passes JSON NOT NULL DEFAULT '{}',
	UNIQUE(book_id, idx)
);

CREATE TABLE IF NOT EXISTS paragraphs (
	id TEXT PRIMARY KEY,
	chapter_id TEXT NOT NULL,
	idx INTEGER NOT NULL,
	position INTEGER NOT NULL DEFAULT 0,
	content TEXT NOT NULL,
	-- '' (never attributed) or 'Narrator' both narrate in the book's own
	-- voice; anything else is a character name - see Paragraph.Speaker and
	-- internal/narration.Resolver.
	speaker TEXT NOT NULL DEFAULT '',
	-- Set at import time for a segment split out of a mixed narration/
	-- dialogue source paragraph - see Paragraph.Inline.
	inline BOOLEAN NOT NULL DEFAULT false,
	-- Set at import time - see Paragraph.IsQuote.
	is_quote BOOLEAN NOT NULL DEFAULT false,
	-- Set by internal/speakerattr.Client.ScareQuoteChapter or a reader's
	-- own manual annotation - see Paragraph.ScareQuote. Always false when
	-- is_quote is false.
	scare_quote BOOLEAN NOT NULL DEFAULT false,
	-- Raw JSON array of character names this paragraph's narration
	-- physically/personality-describes (see internal/speakerattr's
	-- AttributionResult.Describes and Store.SetParagraphDescriptions) -
	-- '[]' (the common case: most paragraphs describe no one) until a
	-- description-tagging attribution run sets it. Only ever meaningfully
	-- non-empty on a narration paragraph (is_quote = false) -
	-- httpapi.attributeChapter enforces that deterministically, the same
	-- way it already does for speaker.
	describes_characters JSON NOT NULL DEFAULT '[]', -- native JSON, not TEXT - see chapters.passes' own doc comment for why
	-- Raw JSON object mapping a clone_model id (e.g. "audiocpp-higgs-4b")
	-- to that model's own ParagraphDirection (see models.go) - two
	-- independently-produced, fully-annotated variants of this paragraph's
	-- own content column, one per internal/speakerattr LLM pass
	-- (Client.DirectChapter for sentence-level emotion/style/prosody tags,
	-- Client.TagSfx for inline sfx/pause tags - see ParagraphDirection's
	-- own doc comment). Keyed by clone_model, not stored flat, because the
	-- tag vocabulary is specific to one model family (Higgs's own
	-- tokenizer vocabulary means nothing to Qwen3's) and a paragraph's
	-- resolved clone model can differ across books/characters (see
	-- character_voices below) - tagging under one model never clobbers
	-- another model's own previously-tagged value. '{}' (no annotation for
	-- any model) until a tagging run sets one. Applied only when
	-- generating this paragraph's audio (jobs.Manager.generate, via
	-- Paragraph.ResolveGenerationText) - never during forced alignment/
	-- word-timing, which always uses this row's own plain content
	-- untouched.
	tts_tags JSON NOT NULL DEFAULT '{}', -- native JSON, not TEXT - see chapters.passes' own doc comment for why
	-- Raw JSON array of pronounce.Substitution (this paragraph's own
	-- resolved pronunciation fixes - see Store.SetParagraphPronunciation
	-- and Paragraph.ResolveGenerationText/Pronunciation). Unlike tts_tags,
	-- NOT keyed by clone_model: a pronunciation fix ("Dr." -> "Doctor") is
	-- plain word substitution, independent of which TTS model narrates
	-- it, unlike a delivery tag's own model-specific control-token
	-- vocabulary. '[]' (no ambiguous abbreviation found in this
	-- paragraph, the overwhelmingly common case) until
	-- internal/speakerattr.Client.ResolvePronunciation sets it. Applied
	-- only at generation time, same as tts_tags - never during forced
	-- alignment/word-timing, which always uses this row's own plain
	-- content untouched.
	pronunciation JSON NOT NULL DEFAULT '[]', -- native JSON, not TEXT - see chapters.passes' own doc comment for why
	-- Raw JSON array of pronounce.Substitution, same shape/column-purpose
	-- as pronunciation above but from a different, non-LLM source: epub
	-- structural markup (<strong>/<b>/<em>/<i>), resolved once at import
	-- time (internal/epub's extractEmphasis) rather than by a later
	-- background pass - see Paragraph.Emphasis/ResolveGenerationText.
	-- '[]' (no emphasis markup found in this paragraph, the common case).
	emphasis JSON NOT NULL DEFAULT '[]', -- native JSON, not TEXT - see chapters.passes' own doc comment for why
	UNIQUE(chapter_id, idx)
);

-- A speaking character, discovered by internal/speakerattr or created
-- directly, scoped by its scope column (see store.SeriesScope) rather
-- than to one book - a character recurring across a series shares one row
-- (identity + summary) across every book in it, regardless of narrator/
-- clone model. summary is the LLM's characterization of how they should
-- sound (see store.Character.Summary); ref_line is its paired reference
-- passage (see store.Character.RefLine), written together in the same
-- speakerattr.Client.CharacterizeVoice call so the reference clip actually
-- performs the voice summary describes instead of reading a flat, unrelated
-- passage. Voice *assignment* lives in character_voices below, not here -
-- see that table's comment.
CREATE TABLE IF NOT EXISTS characters (
	id TEXT PRIMARY KEY,
	scope TEXT NOT NULL,
	name TEXT NOT NULL,
	summary TEXT NOT NULL DEFAULT '',
	ref_line TEXT NOT NULL DEFAULT '',
	is_role BOOLEAN NOT NULL DEFAULT false,
	created_at BIGINT NOT NULL,
	UNIQUE(scope, name)
);

-- A character's assigned voice, one row per (character, clone_model) -
-- separate from characters' identity/summary because a voice_preset
-- clones through one specific clone_model (voice_presets.clone_model) and
-- isn't portable to another; the same character can end up with a
-- different auto-assigned preset per model (see httpapi.provisionCharacterVoice),
-- while still sharing one identity/summary across all of them (see the
-- characters table comment above). '' voice_preset_id rows are never
-- stored - Store.SetCharacterVoice deletes the row instead of writing one
-- with an empty preset id.
CREATE TABLE IF NOT EXISTS character_voices (
	character_id TEXT NOT NULL,
	clone_model TEXT NOT NULL,
	voice_preset_id TEXT NOT NULL,
	PRIMARY KEY (character_id, clone_model)
);

CREATE TABLE IF NOT EXISTS images (
	id TEXT PRIMARY KEY,
	chapter_id TEXT NOT NULL,
	position INTEGER NOT NULL,
	ext TEXT NOT NULL
);

-- A scene/section break (epub.BlockBreak) - position-only, same shape as
-- images minus the file bytes. Never a paragraph: nothing to narrate.
CREATE TABLE IF NOT EXISTS breaks (
	id TEXT PRIMARY KEY,
	chapter_id TEXT NOT NULL,
	position INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_breaks_chapter ON breaks(chapter_id);

-- A bookmark is a paragraph the reader flagged, with an optional note.
-- One per paragraph (UNIQUE) - "bookmark this paragraph, optionally with a
-- note" is a single toggleable action, not bookmark-then-separately-annotate.
CREATE TABLE IF NOT EXISTS bookmarks (
	id TEXT PRIMARY KEY,
	paragraph_id TEXT NOT NULL UNIQUE,
	note TEXT NOT NULL DEFAULT '',
	created_at BIGINT NOT NULL
);

-- One row per (paragraph, voice) that generation has touched. A book can
-- switch its current voice freely; each voice's generated audio stays
-- cached here under its own voice_id rather than being discarded, so
-- switching back to a previously-used voice is instant.
CREATE TABLE IF NOT EXISTS paragraph_audio (
	paragraph_id TEXT NOT NULL,
	voice_id TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'pending',
	error TEXT NOT NULL DEFAULT '',
	duration_seconds DOUBLE NOT NULL DEFAULT 0,
	-- Raw JSON [{text,start,end}] from tts-service's POST
	-- /align, filled in by a follow-up update once alignment finishes
	-- (see jobs.Manager.alignParagraph) - '[]' until then. Real word
	-- timing for karaoke-highlight/click-to-seek instead of guessing from
	-- character position within the text.
	word_timings TEXT NOT NULL DEFAULT '[]',
	-- pointer_offset/pointer_seconds together mean "this paragraph has no
	-- audio file of its own - its real audio is pointer_offset paragraphs
	-- earlier (by Idx, same chapter+voice), starting pointer_seconds into
	-- that paragraph's own clip" - see jobs.Manager's scare-quote merge
	-- group (store.Paragraph.ScareQuote) and AudioState.PointerOffset's own
	-- doc comment. 0/0 (the default, and the only ever value for a
	-- paragraph with its own real file) means no pointer at all.
	pointer_offset INTEGER NOT NULL DEFAULT 0,
	pointer_seconds DOUBLE NOT NULL DEFAULT 0,
	PRIMARY KEY (paragraph_id, voice_id)
);

-- One row per paragraph that has ever had a sound effect (Stable Audio's
-- own SFX checkpoint, see backend/CLAUDE.md's "SFX sound effects"
-- section) prompt set or generated - most paragraphs have none at all,
-- so (unlike paragraph_audio, which every paragraph eventually gets a row in once
-- narration is generated) a missing row here is the overwhelmingly common
-- case, not an edge case. paragraph_id alone as the primary key (not
-- paired with a voice_id) - a sound effect isn't narration, so it doesn't
-- vary by which clone_model currently voices this paragraph. No FK
-- constraint, same reasoning this file's own package doc comment gives
-- for paragraph_audio/paragraphs generally: referential integrity is
-- handled in application code (Store.DeleteBook/DeleteChapterAudio-style
-- explicit cleanup), not enforced here.
CREATE TABLE IF NOT EXISTS sfx (
	paragraph_id TEXT PRIMARY KEY,
	-- Reader-entered text-to-audio prompt - source-of-prompt automation
	-- (an LLM pass, mirroring speakerattr.Client.TagSfx) is a deferred
	-- design decision, not yet built - see Store.SFXState.
	prompt TEXT NOT NULL DEFAULT '',
	-- One of AudioGenerating/AudioReady/AudioError, or '' before any
	-- generation has ever run - there's no "pending" state here:
	-- generation is dispatched through jobs.Manager's ordinary
	-- KindSFXGeneration task (see EnqueueSFXGeneration), never
	-- auto-triggered the way a fresh paragraph's own narration is.
	status TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	duration_seconds DOUBLE NOT NULL DEFAULT 0,
	-- Which word (a 0-based index into this paragraph's own forced-
	-- alignment paragraph_audio.word_timings, NOT a byte/rune offset into
	-- paragraphs.content) playback should start the sound effect at, once
	-- ready. Defaults to 0 (the paragraph's very first word - i.e. "as
	-- soon as this paragraph starts playing").
	trigger_word INTEGER NOT NULL DEFAULT 0
);

-- One row per chapter tone region, as judged by internal/speakerattr.
-- Client.ScoreMusic - see store.MusicRegion's own doc comment for the full
-- generation pipeline this feeds. speakerattr itself only ever judges a
-- region's own start_idx (see MusicRegionResult) - end_idx is still
-- stored, for cheap reads (every real caller lists a whole chapter's
-- regions at once and wants each one's real paragraph range without an
-- extra computation pass), but it's Store.AppendMusicRegions that fills it
-- in, from sibling start_idx values, at write time - never trusted
-- directly from the LLM. A chapter's regions are contiguous and non-
-- overlapping by construction this way, covering every paragraph exactly
-- once, in idx order (idx here, not to be confused with start_idx/end_idx,
-- which are paragraph indices).
CREATE TABLE IF NOT EXISTS music_regions (
	id TEXT PRIMARY KEY,
	chapter_id TEXT NOT NULL,
	idx INTEGER NOT NULL,
	start_idx INTEGER NOT NULL,
	end_idx INTEGER NOT NULL,
	mood TEXT NOT NULL DEFAULT '',
	prompt TEXT NOT NULL DEFAULT '',
	-- 'cut' or 'continuation' - see store.MusicTransition's own doc comment.
	transition TEXT NOT NULL DEFAULT 'cut',
	-- One of AudioPending/AudioGenerating/AudioReady/AudioError - the same
	-- vocabulary paragraph_audio/sfx already use.
	status TEXT NOT NULL DEFAULT 'pending',
	error TEXT NOT NULL DEFAULT '',
	duration_seconds DOUBLE NOT NULL DEFAULT 0,
	UNIQUE(chapter_id, idx)
);

CREATE INDEX IF NOT EXISTS idx_music_regions_chapter ON music_regions(chapter_id);

-- User-created, reusable narrator voices, distinct from the curated
-- presets compiled into internal/voices - those are never stored here,
-- only proxied. Creating/editing one triggers the ttsworker process to
-- render (via internal/voicerefs) and cache a stable reference clip for it.
CREATE TABLE IF NOT EXISTS voice_presets (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	instruct TEXT NOT NULL,
	ref_text TEXT NOT NULL,
	seed INTEGER NOT NULL,
	speed_multiplier DOUBLE NOT NULL DEFAULT 1.0,
	created_at BIGINT NOT NULL,
	-- Which clone model this preset clones through, e.g.
	-- "audiocpp-qwen3-0.6b" or "audiocpp-higgs-4b" (see
	-- internal/audioworker's cloneModelFamilies) - "" defers to the
	-- worker's own configured default rather than pinning one, but new
	-- presets are created with an explicit value (see
	-- httpapi.handleCreateCustomVoicePreset) so a book's narrator model
	-- doesn't silently change if the worker's default is later changed.
	clone_model TEXT NOT NULL DEFAULT 'audiocpp-higgs-4b',
	-- Which VoiceDesign engine renders this preset's reference clip, e.g.
	-- "qwen3_tts" or "breeze_tts" (see internal/audioworker's
	-- designEngines) - same "" defers / new presets get an explicit value
	-- shape as clone_model above (voices.DefaultDesignModel).
	design_model TEXT NOT NULL DEFAULT 'breeze_tts'
);

-- Singleton (always exactly the id=0 row - see Open) holding the voice new
-- books are created with, settable from the Voices page rather than only
-- per-book from the reader.
CREATE TABLE IF NOT EXISTS default_voice (
	id INTEGER PRIMARY KEY,
	preset_id TEXT NOT NULL DEFAULT 'velvet-narrator',
	instruct TEXT NOT NULL DEFAULT '',
	language TEXT NOT NULL DEFAULT 'Auto',
	seed INTEGER NOT NULL DEFAULT 1006
);

CREATE INDEX IF NOT EXISTS idx_chapters_book ON chapters(book_id);
CREATE INDEX IF NOT EXISTS idx_paragraphs_chapter ON paragraphs(chapter_id);
CREATE INDEX IF NOT EXISTS idx_images_chapter ON images(chapter_id);
CREATE INDEX IF NOT EXISTS idx_paragraph_audio_paragraph ON paragraph_audio(paragraph_id);
CREATE INDEX IF NOT EXISTS idx_bookmarks_paragraph ON bookmarks(paragraph_id);
CREATE INDEX IF NOT EXISTS idx_characters_scope ON characters(scope);
CREATE INDEX IF NOT EXISTS idx_character_voices_character ON character_voices(character_id);
CREATE INDEX IF NOT EXISTS idx_paragraphs_speaker ON paragraphs(speaker);
`

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("duckdb", path)
	if err != nil {
		return nil, err
	}
	// DuckDB is single-writer; one connection keeps that explicit and avoids
	// any cross-connection lock contention. This app has no need for write
	// concurrency.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrateCharactersRefLine(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateVoicePresetsDesignModel(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateParagraphsEmphasis(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateCharactersIsRole(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateParagraphsScareQuote(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateParagraphAudioPointer(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`INSERT INTO default_voice (id) SELECT 0 WHERE NOT EXISTS (SELECT 1 FROM default_voice)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("seed default_voice: %w", err)
	}
	return &Store{db: db}, nil
}

// migrateCharactersRefLine is a narrow, one-off exception to this
// package's usual "no migrations - wipe DATA_DIR" policy (see this
// package's own doc comment and backend/CLAUDE.md): ref_line was added to
// an already-shipped characters table, and unlike every other schema
// change so far, existing characterizations were worth preserving rather
// than discarding.
//
// DuckDB's ALTER TABLE ADD COLUMN rejects a NOT NULL constraint outright
// ("Parser Error: Adding columns with constraints not yet supported" -
// confirmed directly against this DuckDB version), but accepts a bare
// DEFAULT with no NOT NULL alongside it. That default is set here to
// voices.DefaultRefText specifically, not ” - every already-existing
// character's voice preset, if it has one, was already rendered from
// exactly that passage (the old, pre-ref_line httpapi.
// provisionCharacterVoice unconditionally used voices.DefaultRefText for
// every character), so backfilling ref_line to the same text keeps
// GetCharacter's own return value consistent with what that character's
// already-cached reference clip actually is, rather than backfilling to
// ” and leaning on httpapi.characterRefText's runtime fallback to paper
// over the gap. A brand-new character discovered after this migration
// still starts with whatever this column's now-current default is until
// it's actually characterized (see UpsertCharacter's own INSERT, which
// doesn't set ref_line explicitly) - inconsequential either way, since
// SetCharacterSummary always overwrites both Summary and RefLine together
// the first time a character is characterized, before ref_line is ever
// read.
//
// ADD COLUMN IF NOT EXISTS makes this safe to run unconditionally on
// every Open call - a no-op once the column exists, including on a
// brand-new database where the schema's own CREATE TABLE already defined
// it (with its own NOT NULL DEFAULT ”, not this one).
func migrateCharactersRefLine(db *sql.DB) error {
	escaped := strings.ReplaceAll(voices.DefaultRefText, "'", "''")
	if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE characters ADD COLUMN IF NOT EXISTS ref_line TEXT DEFAULT '%s'`, escaped)); err != nil {
		return fmt.Errorf("migrate characters.ref_line: %w", err)
	}
	return nil
}

// migrateVoicePresetsDesignModel is migrateCharactersRefLine's own shape
// (see its doc comment for the underlying DuckDB constraint/ADD COLUMN IF
// NOT EXISTS reasoning), applied to design_model on voice_presets: an
// already-existing preset row predating this column backfills to
// voices.DefaultDesignModel (matching the schema's own DEFAULT above)
// rather than "" - every already-existing preset's cached reference clip
// was rendered by whatever the worker's engine was at the time, which
// (until this feature) was always process-wide breeze_tts by default, so
// this keeps GetVoicePreset/ListVoicePresets consistent with that.
func migrateVoicePresetsDesignModel(db *sql.DB) error {
	if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE voice_presets ADD COLUMN IF NOT EXISTS design_model TEXT DEFAULT '%s'`, voices.DefaultDesignModel)); err != nil {
		return fmt.Errorf("migrate voice_presets.design_model: %w", err)
	}
	return nil
}

// migrateParagraphsEmphasis is migrateCharactersRefLine's own shape again,
// applied to emphasis on paragraphs: an already-existing paragraph row
// predating this column backfills to '[]' (no emphasis) rather than
// leaving it null - the epub structural markup that would have populated
// it was never captured for a book imported before this feature existed,
// so there's nothing to backfill it *to* beyond the same empty default a
// brand-new paragraph with no emphasis at all would get.
func migrateParagraphsEmphasis(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE paragraphs ADD COLUMN IF NOT EXISTS emphasis JSON DEFAULT '[]'`); err != nil {
		return fmt.Errorf("migrate paragraphs.emphasis: %w", err)
	}
	return nil
}

// migrateParagraphsScareQuote is migrateCharactersRefLine's own shape
// again, applied to scare_quote: an already-existing paragraph row
// predating this column backfills to false - a paragraph that's never
// been through internal/speakerattr.Client.ScareQuoteChapter isn't known
// to be a scare quote, the same "no signal to backfill from" default a
// brand-new one would get.
func migrateParagraphsScareQuote(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE paragraphs ADD COLUMN IF NOT EXISTS scare_quote BOOLEAN DEFAULT false`); err != nil {
		return fmt.Errorf("migrate paragraphs.scare_quote: %w", err)
	}
	return nil
}

// migrateParagraphAudioPointer is migrateCharactersRefLine's own shape
// again, applied to paragraph_audio.pointer_offset/pointer_seconds: an
// already-existing row predating these columns backfills to 0/0 (no
// pointer - the same "this paragraph has its own real file" default a
// brand-new row would get).
func migrateParagraphAudioPointer(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE paragraph_audio ADD COLUMN IF NOT EXISTS pointer_offset INTEGER DEFAULT 0`); err != nil {
		return fmt.Errorf("migrate paragraph_audio.pointer_offset: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE paragraph_audio ADD COLUMN IF NOT EXISTS pointer_seconds DOUBLE DEFAULT 0`); err != nil {
		return fmt.Errorf("migrate paragraph_audio.pointer_seconds: %w", err)
	}
	return nil
}

// migrateCharactersIsRole is migrateCharactersRefLine's own shape again,
// applied to is_role: an already-existing character row predating this
// column backfills to false (not a role) rather than leaving it null -
// exactly the same "no signal to backfill from" default a brand-new
// character discovered with no role judgment at all would get.
func migrateCharactersIsRole(db *sql.DB) error {
	if _, err := db.Exec(`ALTER TABLE characters ADD COLUMN IF NOT EXISTS is_role BOOLEAN DEFAULT false`); err != nil {
		return fmt.Errorf("migrate characters.is_role: %w", err)
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func NewID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RandomSeed picks a seed for a new custom voice preset - just needs to be
// some fixed positive int that anchors one realization of that preset's
// instruct-conditioned voice distribution; which one doesn't matter.
func RandomSeed() int {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return (int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])) & 0x7fffffff
}

type DefaultVoice struct {
	PresetID string
	Instruct string
	Language string
	Seed     int
}

func (s *Store) GetDefaultVoice() (DefaultVoice, error) {
	var v DefaultVoice
	err := s.db.QueryRow(`SELECT preset_id, instruct, language, seed FROM default_voice WHERE id = 0`).
		Scan(&v.PresetID, &v.Instruct, &v.Language, &v.Seed)
	return v, err
}

func (s *Store) SetDefaultVoice(presetID, instruct, language string, seed int) error {
	_, err := s.db.Exec(
		`UPDATE default_voice SET preset_id = ?, instruct = ?, language = ?, seed = ? WHERE id = 0`,
		presetID, instruct, language, seed,
	)
	return err
}

// VoiceID derives a stable identifier for a narration voice from its full
// config, so the same (preset, instruct, language) combination always
// lands on the same cached audio regardless of when or how it was set -
// and two different configs never collide.
func VoiceID(presetID, instruct, language string) string {
	h := sha256.Sum256([]byte(presetID + "\x00" + instruct + "\x00" + language))
	return hex.EncodeToString(h[:])[:16]
}

// BlockKind mirrors epub.BlockKind without importing that package here
// (store shouldn't depend on the epub parser).
type BlockKind string

const (
	BlockText  BlockKind = "text"
	BlockImage BlockKind = "image"
	// BlockBreak mirrors epub.BlockBreak - a scene/section break. Never
	// becomes a paragraph row (nothing to narrate or display beyond the
	// break itself) - CreateBook inserts it into `breaks` instead, the
	// same position-only shape `images` already uses for BlockImage.
	BlockBreak BlockKind = "break"
)

type BlockInput struct {
	Kind BlockKind
	Text string // BlockText
	Ext  string // BlockImage, e.g. ".jpg"
	// Inline: BlockText only - see Paragraph.Inline.
	Inline bool
	// IsQuote: BlockText only - see Paragraph.IsQuote.
	IsQuote bool
	// Emphasis: BlockText only - see Paragraph.Emphasis.
	Emphasis []pronounce.Substitution
}

type ChapterInput struct {
	Title  string
	Blocks []BlockInput
}

// CreatedImage identifies where one image block ended up in the database,
// so the caller (which has the actual image bytes, not the store) can
// write the file to disk using the same ID.
type CreatedImage struct {
	ChapterID string
	ImageID   string
	Ext       string
}

// CreateBook inserts a book together with all of its chapters, paragraphs,
// and image placeholders in a single transaction. Returned images are in
// the same order as the image blocks appeared across the input, so callers
// can zip them back up with the original image bytes.
func (s *Store) CreateBook(title, author, language string, coverExt string, seriesName string, seriesIndex float64, chapters []ChapterInput) (bookID string, images []CreatedImage, err error) {
	defaultVoice, err := s.GetDefaultVoice()
	if err != nil {
		return "", nil, fmt.Errorf("get default voice: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()

	bookID = NewID()
	_, err = tx.Exec(
		`INSERT INTO books (id, title, author, language, cover_ext, series_name, series_index, added_at, voice_preset_id, voice_instruct, voice_language, voice_seed)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		bookID, title, author, language, coverExt, seriesName, seriesIndex, time.Now().Unix(),
		defaultVoice.PresetID, defaultVoice.Instruct, defaultVoice.Language, defaultVoice.Seed,
	)
	if err != nil {
		return "", nil, fmt.Errorf("insert book: %w", err)
	}

	for ci, ch := range chapters {
		chapterID := NewID()
		_, err = tx.Exec(
			`INSERT INTO chapters (id, book_id, idx, title) VALUES (?, ?, ?, ?)`,
			chapterID, bookID, ci, ch.Title,
		)
		if err != nil {
			return "", nil, fmt.Errorf("insert chapter: %w", err)
		}

		paragraphIdx := 0
		for position, block := range ch.Blocks {
			switch block.Kind {
			case BlockImage:
				imageID := NewID()
				_, err = tx.Exec(
					`INSERT INTO images (id, chapter_id, position, ext) VALUES (?, ?, ?, ?)`,
					imageID, chapterID, position, block.Ext,
				)
				if err != nil {
					return "", nil, fmt.Errorf("insert image: %w", err)
				}
				images = append(images, CreatedImage{ChapterID: chapterID, ImageID: imageID, Ext: block.Ext})
			case BlockBreak:
				_, err = tx.Exec(
					`INSERT INTO breaks (id, chapter_id, position) VALUES (?, ?, ?)`,
					NewID(), chapterID, position,
				)
				if err != nil {
					return "", nil, fmt.Errorf("insert break: %w", err)
				}
			default:
				emphasis := block.Emphasis
				if emphasis == nil {
					emphasis = []pronounce.Substitution{}
				}
				// .Value() explicitly - see SetParagraphDescriptions' own
				// doc comment for why a bare PronunciationList can't be
				// passed directly.
				encodedEmphasis, err := PronunciationList(emphasis).Value()
				if err != nil {
					return "", nil, fmt.Errorf("encode emphasis: %w", err)
				}
				_, err = tx.Exec(
					`INSERT INTO paragraphs (id, chapter_id, idx, position, content, inline, is_quote, emphasis) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
					NewID(), chapterID, paragraphIdx, position, block.Text, block.Inline, block.IsQuote, encodedEmphasis,
				)
				if err != nil {
					return "", nil, fmt.Errorf("insert paragraph: %w", err)
				}
				paragraphIdx++
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return bookID, images, nil
}

func (s *Store) ListBooks() ([]Book, error) {
	rows, err := s.db.Query(`SELECT id, title, author, language, cover_ext, series_name, series_index, added_at,
		voice_preset_id, voice_instruct, voice_language, voice_seed, character_voice_mode, speech_direction, music_enabled,
		pos_chapter_idx, pos_paragraph_idx, pos_seconds, estimate_sec_per_char
		FROM books ORDER BY added_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Book
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.ID, &b.Title, &b.Author, &b.Language, &b.CoverExt, &b.SeriesName, &b.SeriesIndex, &b.AddedAt,
			&b.VoicePresetID, &b.VoiceInstruct, &b.VoiceLanguage, &b.VoiceSeed, &b.CharacterVoiceMode, &b.SpeechDirection, &b.MusicEnabled,
			&b.PosChapterIdx, &b.PosParagraphIdx, &b.PosSeconds, &b.EstimateSecPerChar); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) GetBook(id string) (*Book, error) {
	var b Book
	err := s.db.QueryRow(`SELECT id, title, author, language, cover_ext, series_name, series_index, added_at,
		voice_preset_id, voice_instruct, voice_language, voice_seed, character_voice_mode, speech_direction, music_enabled,
		pos_chapter_idx, pos_paragraph_idx, pos_seconds, estimate_sec_per_char
		FROM books WHERE id = ?`, id).Scan(
		&b.ID, &b.Title, &b.Author, &b.Language, &b.CoverExt, &b.SeriesName, &b.SeriesIndex, &b.AddedAt,
		&b.VoicePresetID, &b.VoiceInstruct, &b.VoiceLanguage, &b.VoiceSeed, &b.CharacterVoiceMode, &b.SpeechDirection, &b.MusicEnabled,
		&b.PosChapterIdx, &b.PosParagraphIdx, &b.PosSeconds, &b.EstimateSecPerChar)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) DeleteBook(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM bookmarks WHERE paragraph_id IN (SELECT id FROM paragraphs WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ?))`, id); err != nil {
		return fmt.Errorf("delete bookmarks: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM paragraphs WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, id); err != nil {
		return fmt.Errorf("delete paragraphs: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM images WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, id); err != nil {
		return fmt.Errorf("delete images: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM breaks WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, id); err != nil {
		return fmt.Errorf("delete breaks: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM chapters WHERE book_id = ?`, id); err != nil {
		return fmt.Errorf("delete chapters: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM books WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete book: %w", err)
	}
	return tx.Commit()
}

// UpdateVoice sets a book's narration voice. seed should be whichever
// preset (built-in or custom) presetID resolves to - 0 if presetID is
// empty (a fully custom instruct with no preset backing it), in which case
// tts-service falls back to its own unseeded default for that call.
// characterVoiceMode sets store.Book.CharacterVoiceMode - see its own doc
// comment for the four possible values - independent of presetID/instruct/
// language/seed, which remain the fallback voice regardless of mode.
func (s *Store) UpdateVoice(bookID, presetID, instruct, language string, seed int, characterVoiceMode CharacterVoiceMode, speechDirection, musicEnabled bool) error {
	_, err := s.db.Exec(
		`UPDATE books SET voice_preset_id = ?, voice_instruct = ?, voice_language = ?, voice_seed = ?, character_voice_mode = ?, speech_direction = ?, music_enabled = ? WHERE id = ?`,
		presetID, instruct, language, seed, characterVoiceMode, speechDirection, musicEnabled, bookID,
	)
	return err
}

// SetLengthEstimate stores bookID's own PocketTTS-calibrated seconds-per-
// character measurement - see Book.EstimateSecPerChar's own doc comment.
func (s *Store) SetLengthEstimate(bookID string, secPerChar float64) error {
	_, err := s.db.Exec(`UPDATE books SET estimate_sec_per_char = ? WHERE id = ?`, secPerChar, bookID)
	return err
}

func (s *Store) ListVoicePresets() ([]VoicePreset, error) {
	rows, err := s.db.Query(`SELECT id, name, instruct, ref_text, seed, speed_multiplier, created_at, clone_model, design_model FROM voice_presets ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []VoicePreset
	for rows.Next() {
		var p VoicePreset
		if err := rows.Scan(&p.ID, &p.Name, &p.Instruct, &p.RefText, &p.Seed, &p.SpeedMultiplier, &p.CreatedAt, &p.CloneModel, &p.DesignModel); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// VoicePresetGroups maps every custom voice preset currently assigned to a
// character (across any clone model - see character_voices) to a display
// label for the Voices page's grouping: the series name characters sharing
// that preset's owner are scoped under (see SeriesScope), or that lone
// book's title when there's no series. A preset never assigned to a
// character (a reader's own general-purpose narrator voice, not one
// auto-created for a speaker - see httpapi.provisionCharacterVoice) is
// simply absent from the returned map, letting the caller tell "this
// belongs to a series/book's speaker roster" apart from "just one of my
// voices". Scope strings are "series:<name>" or "book:<id>" (see
// SeriesScope); substr offsets below skip past those literal prefixes
// (DuckDB substr is 1-indexed).
func (s *Store) VoicePresetGroups() (map[string]string, error) {
	rows, err := s.db.Query(`
		SELECT cv.voice_preset_id,
			CASE WHEN c.scope LIKE 'series:%' THEN substr(c.scope, 8) ELSE b.title END AS label
		FROM character_voices cv
		JOIN characters c ON c.id = cv.character_id
		LEFT JOIN books b ON c.scope LIKE 'book:%' AND b.id = substr(c.scope, 6)
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var presetID string
		var label sql.NullString
		if err := rows.Scan(&presetID, &label); err != nil {
			return nil, err
		}
		if label.Valid && label.String != "" {
			out[presetID] = label.String
		}
	}
	return out, rows.Err()
}

func (s *Store) GetVoicePreset(id string) (*VoicePreset, error) {
	var p VoicePreset
	err := s.db.QueryRow(`SELECT id, name, instruct, ref_text, seed, speed_multiplier, created_at, clone_model, design_model FROM voice_presets WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Instruct, &p.RefText, &p.Seed, &p.SpeedMultiplier, &p.CreatedAt, &p.CloneModel, &p.DesignModel)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) CreateVoicePreset(name, instruct, refText string, seed int, speedMultiplier float64, cloneModel, designModel string) (VoicePreset, error) {
	p := VoicePreset{
		ID: NewID(), Name: name, Instruct: instruct, RefText: refText, Seed: seed,
		SpeedMultiplier: speedMultiplier, CreatedAt: time.Now().Unix(), CloneModel: cloneModel,
		DesignModel: designModel,
	}
	_, err := s.db.Exec(
		`INSERT INTO voice_presets (id, name, instruct, ref_text, seed, speed_multiplier, created_at, clone_model, design_model) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Instruct, p.RefText, p.Seed, p.SpeedMultiplier, p.CreatedAt, p.CloneModel, p.DesignModel,
	)
	if err != nil {
		return VoicePreset{}, err
	}
	return p, nil
}

func (s *Store) UpdateVoicePreset(id, name, instruct, refText string, seed int, speedMultiplier float64, cloneModel, designModel string) error {
	_, err := s.db.Exec(
		`UPDATE voice_presets SET name = ?, instruct = ?, ref_text = ?, seed = ?, speed_multiplier = ?, clone_model = ?, design_model = ? WHERE id = ?`,
		name, instruct, refText, seed, speedMultiplier, cloneModel, designModel, id,
	)
	return err
}

// UpdateVoicePresetSeed persists a bumped-on-retry seed (see
// jobs.task.runProvision's own doc comment) back onto an existing preset,
// touching only that one column - unlike UpdateVoicePreset's full-row
// replace, a caller here typically only has the bumped seed value in hand,
// not a freshly-read copy of every other field, and shouldn't have to fetch
// the row just to round-trip its own unchanged fields back through
// UpdateVoicePreset.
func (s *Store) UpdateVoicePresetSeed(id string, seed int) error {
	_, err := s.db.Exec(`UPDATE voice_presets SET seed = ? WHERE id = ?`, seed, id)
	return err
}

func (s *Store) DeleteVoicePreset(id string) error {
	_, err := s.db.Exec(`DELETE FROM voice_presets WHERE id = ?`, id)
	return err
}

func (s *Store) UpdatePosition(bookID string, chapterIdx, paragraphIdx int, seconds float64) error {
	_, err := s.db.Exec(
		`UPDATE books SET pos_chapter_idx = ?, pos_paragraph_idx = ?, pos_seconds = ? WHERE id = ?`,
		chapterIdx, paragraphIdx, seconds, bookID,
	)
	return err
}

// ListChapterSummaries reports, per chapter, how many of its paragraphs
// have ready audio *for voiceID specifically* - a chapter fully generated
// under one voice shows as not-yet-generated when a different voice is
// selected, since that voice's audio doesn't exist yet.
func (s *Store) ListChapterSummaries(bookID, voiceID string) ([]ChapterSummary, error) {
	rows, err := s.db.Query(`
		SELECT c.id, c.book_id, c.idx, c.title, c.passes,
			COUNT(p.id) AS total,
			COALESCE(SUM(CASE WHEN pa.status = 'ready' THEN 1 ELSE 0 END), 0) AS ready
		FROM chapters c
		LEFT JOIN paragraphs p ON p.chapter_id = c.id
		LEFT JOIN paragraph_audio pa ON pa.paragraph_id = p.id AND pa.voice_id = ?
		WHERE c.book_id = ?
		GROUP BY c.id, c.book_id, c.idx, c.title, c.passes
		ORDER BY c.idx ASC`, voiceID, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChapterSummary
	for rows.Next() {
		var cs ChapterSummary
		if err := rows.Scan(&cs.ID, &cs.BookID, &cs.Idx, &cs.Title, &cs.Passes, &cs.ParagraphCount, &cs.ReadyCount); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// BookVoice pairs a book with its currently-resolved voice_id, for the
// batch queries below - each book in a library listing can be narrated by
// a different voice, so a single shared voiceID (as ListChapterSummaries/
// BookNarrationStats take) doesn't work across more than one book at once.
type BookVoice struct {
	BookID  string
	VoiceID string
}

// bookVoiceValues builds a `VALUES (?, ?), (?, ?), ...` fragment plus its
// matching args, shared by the two batch queries below.
func bookVoiceValues(bookVoices []BookVoice) (fragment string, args []any) {
	rows := make([]string, len(bookVoices))
	args = make([]any, 0, len(bookVoices)*2)
	for i, bv := range bookVoices {
		rows[i] = "(?, ?)"
		args = append(args, bv.BookID, bv.VoiceID)
	}
	return strings.Join(rows, ", "), args
}

// ListChapterSummariesForBooks is ListChapterSummaries' batch counterpart:
// one query across every book in bookVoices instead of one query per book.
// Built for httpapi.handleListBooks, which used to call ListChapterSummaries
// once per book in the library - fine for one book, but 2*N serialized
// full-book scans (this, plus BookNarrationStatsForBooks's sibling) on
// this package's single shared connection (see this package's own doc
// comment) for a whole library, stalling every other request behind
// whichever scan is running. Joins against a small VALUES list of (book_id,
// voice_id) pairs instead, so the whole library costs one scan of
// chapters/paragraphs/paragraph_audio, not one scan per book. Returns a map
// keyed by book id, each chapter list already in idx order; a book with no
// chapters is simply absent.
func (s *Store) ListChapterSummariesForBooks(bookVoices []BookVoice) (map[string][]ChapterSummary, error) {
	out := make(map[string][]ChapterSummary, len(bookVoices))
	if len(bookVoices) == 0 {
		return out, nil
	}
	valuesFragment, args := bookVoiceValues(bookVoices)

	rows, err := s.db.Query(`
		WITH book_voice(book_id, voice_id) AS (VALUES `+valuesFragment+`)
		SELECT c.id, c.book_id, c.idx, c.title, c.passes,
			COUNT(p.id) AS total,
			COALESCE(SUM(CASE WHEN pa.status = 'ready' THEN 1 ELSE 0 END), 0) AS ready
		FROM chapters c
		JOIN book_voice bv ON bv.book_id = c.book_id
		LEFT JOIN paragraphs p ON p.chapter_id = c.id
		LEFT JOIN paragraph_audio pa ON pa.paragraph_id = p.id AND pa.voice_id = bv.voice_id
		GROUP BY c.id, c.book_id, c.idx, c.title, c.passes
		ORDER BY c.book_id, c.idx ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var cs ChapterSummary
		if err := rows.Scan(&cs.ID, &cs.BookID, &cs.Idx, &cs.Title, &cs.Passes, &cs.ParagraphCount, &cs.ReadyCount); err != nil {
			return nil, err
		}
		out[cs.BookID] = append(out[cs.BookID], cs)
	}
	return out, rows.Err()
}

// BookNarrationStatsForBooks is BookNarrationStats' batch counterpart - see
// ListChapterSummariesForBooks's doc comment for why. A book with no
// paragraphs is simply absent from the returned map; callers should treat
// that the same as a zero-value NarrationStats.
func (s *Store) BookNarrationStatsForBooks(bookVoices []BookVoice) (map[string]NarrationStats, error) {
	out := make(map[string]NarrationStats, len(bookVoices))
	if len(bookVoices) == 0 {
		return out, nil
	}
	valuesFragment, args := bookVoiceValues(bookVoices)

	rows, err := s.db.Query(`
		WITH book_voice(book_id, voice_id) AS (VALUES `+valuesFragment+`)
		SELECT c.book_id,
			COALESCE(SUM(LENGTH(p.content)), 0),
			COALESCE(SUM(CASE WHEN pa.status = 'ready' THEN LENGTH(p.content) ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN pa.status = 'ready' THEN pa.duration_seconds ELSE 0 END), 0)
		FROM chapters c
		JOIN book_voice bv ON bv.book_id = c.book_id
		JOIN paragraphs p ON p.chapter_id = c.id
		LEFT JOIN paragraph_audio pa ON pa.paragraph_id = p.id AND pa.voice_id = bv.voice_id
		GROUP BY c.book_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var bookID string
		var stats NarrationStats
		if err := rows.Scan(&bookID, &stats.TotalChars, &stats.ReadyChars, &stats.ReadySeconds); err != nil {
			return nil, err
		}
		out[bookID] = stats
	}
	return out, rows.Err()
}

func (s *Store) GetChapterByID(id string) (*Chapter, error) {
	var c Chapter
	err := s.db.QueryRow(`SELECT id, book_id, idx, title, passes FROM chapters WHERE id = ?`, id).
		Scan(&c.ID, &c.BookID, &c.Idx, &c.Title, &c.Passes)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) GetChapterByIdx(bookID string, idx int) (*Chapter, error) {
	var c Chapter
	err := s.db.QueryRow(`SELECT id, book_id, idx, title, passes FROM chapters WHERE book_id = ? AND idx = ?`, bookID, idx).
		Scan(&c.ID, &c.BookID, &c.Idx, &c.Title, &c.Passes)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// SetChapterMusicScored marks chapterID's own passes.music true, leaving
// every other Passes field untouched - set by httpapi.scoreChapterMusic
// once internal/speakerattr.Client.ScoreMusic completes a full,
// uninterrupted pass over the whole chapter, never on a pause or a genuine
// failure partway through. SetChapterAttributed/SetChapterDirected's own
// json_merge_patch shape (see SetChapterAttributed's own doc comment for
// why a single statement like this, not a Go-side read-modify-write).
func (s *Store) SetChapterMusicScored(chapterID string) error {
	_, err := s.db.Exec(
		`UPDATE chapters SET passes = json_merge_patch(passes, '{"music": true}') WHERE id = ?`,
		chapterID,
	)
	return err
}

// ClearMusicRegions deletes every music_regions row for chapterID - called
// once, up front, by httpapi.scoreChapterMusic before a (re)scoring run
// starts: a chapter's regions must always fully partition its paragraphs
// with no gaps/overlaps, so a partial old set can never usefully coexist
// with a fresh run's own output the way, say, tts_tags accumulates
// incrementally across reruns. Returns whatever existed before deleting
// (same "read before delete, return refs" shape
// DeleteParagraphAudioForSpeaker below already uses) so the caller can
// remove each region's own on-disk audio file - this package never
// touches the filesystem itself, see DeleteChapterAudio's own doc comment
// for that DB-delete/disk-delete split.
func (s *Store) ClearMusicRegions(chapterID string) ([]MusicRegion, error) {
	regions, err := s.ListMusicRegions(chapterID)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`DELETE FROM music_regions WHERE chapter_id = ?`, chapterID); err != nil {
		return nil, err
	}
	return regions, nil
}

// AppendMusicRegions inserts regions as the next contiguous run of a
// chapter's own music_regions.idx sequence, starting from whatever the
// chapter's current max idx + 1 is (0 if the chapter has none yet) - see
// ScoreMusic's own doc comment for why regions are persisted incrementally,
// batch by batch, rather than all at once at the very end. Assigns each
// inserted region a fresh ID and returns the persisted rows (with ID/Idx
// filled in) so the caller (httpapi.scoreChapterMusic) can react to newly
// available regions - e.g. kick maybeAdvanceChapterMusic - without a
// separate re-list.
//
// Computes and stores every inserted region's own EndIdx here - the only
// place that ever does - rather than deriving it fresh on every read
// (every real caller lists a whole chapter's regions at once, so storing
// it once at write time is cheaper than recomputing it on every list).
// Within this one batch, a region's EndIdx is simply its own next
// sibling's StartIdx minus one; regions[len(regions)-1] instead gets
// lastParagraphIdx (the chapter's own real last paragraph, which the
// caller already has in hand - see httpapi.scoreChapterMusic) as a
// provisional value, since whether anything comes after it depends on
// whether a *later* batch gets appended too. If one does, that later
// call's own first step fixes this batch's own last region back up to the
// real boundary, before inserting anything new - see the UPDATE below.
func (s *Store) AppendMusicRegions(chapterID string, regions []MusicRegionInput, lastParagraphIdx int) ([]MusicRegion, error) {
	if len(regions) == 0 {
		return nil, nil
	}
	var nextIdx int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(idx) + 1, 0) FROM music_regions WHERE chapter_id = ?`, chapterID).Scan(&nextIdx)
	if err != nil {
		return nil, err
	}
	if nextIdx > 0 {
		// This chapter already has at least one region from an earlier
		// batch - its own last one (idx = nextIdx-1) had its EndIdx set
		// provisionally to lastParagraphIdx, since at the time it was
		// inserted there was no way yet to know a following batch (this
		// one) would exist. Now that one does, it actually ends right
		// where this batch's own first region begins.
		if _, err := s.db.Exec(
			`UPDATE music_regions SET end_idx = ? WHERE chapter_id = ? AND idx = ?`,
			regions[0].StartIdx-1, chapterID, nextIdx-1,
		); err != nil {
			return nil, err
		}
	}
	out := make([]MusicRegion, 0, len(regions))
	for i, r := range regions {
		endIdx := lastParagraphIdx
		if i+1 < len(regions) {
			endIdx = regions[i+1].StartIdx - 1
		}
		region := MusicRegion{
			ID:         NewID(),
			ChapterID:  chapterID,
			Idx:        nextIdx + i,
			StartIdx:   r.StartIdx,
			EndIdx:     endIdx,
			Mood:       r.Mood,
			Prompt:     r.Prompt,
			Transition: r.Transition,
			Status:     AudioPending,
		}
		_, err := s.db.Exec(
			`INSERT INTO music_regions (id, chapter_id, idx, start_idx, end_idx, mood, prompt, transition, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			region.ID, region.ChapterID, region.Idx, region.StartIdx, region.EndIdx, region.Mood, region.Prompt, region.Transition, region.Status,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, region)
	}
	return out, nil
}

// MusicRegionInput is AppendMusicRegions' own insert shape - just the
// fields internal/speakerattr.Client.ScoreMusic actually decides (only a
// StartIdx, no end - see MusicRegion.EndIdx's own doc comment), leaving
// ID/Status (always freshly generated/AudioPending on insert) and
// DurationSeconds (only meaningful once generation finishes) out.
type MusicRegionInput struct {
	StartIdx   int
	Mood       string
	Prompt     string
	Transition MusicTransition
}

// ListMusicRegions returns chapterID's own music regions in idx order.
func (s *Store) ListMusicRegions(chapterID string) ([]MusicRegion, error) {
	rows, err := s.db.Query(`SELECT id, chapter_id, idx, start_idx, end_idx, mood, prompt, transition, status, error, duration_seconds FROM music_regions WHERE chapter_id = ? ORDER BY idx ASC`, chapterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MusicRegion
	for rows.Next() {
		var r MusicRegion
		if err := rows.Scan(&r.ID, &r.ChapterID, &r.Idx, &r.StartIdx, &r.EndIdx, &r.Mood, &r.Prompt, &r.Transition, &r.Status, &r.Error, &r.DurationSeconds); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetMusicRegion looks up one region by id - httpapi.handleGetMusicRegionAudio's
// own single-row lookup, not a whole chapter's list.
func (s *Store) GetMusicRegion(id string) (*MusicRegion, error) {
	var r MusicRegion
	err := s.db.QueryRow(`SELECT id, chapter_id, idx, start_idx, end_idx, mood, prompt, transition, status, error, duration_seconds FROM music_regions WHERE id = ?`, id).
		Scan(&r.ID, &r.ChapterID, &r.Idx, &r.StartIdx, &r.EndIdx, &r.Mood, &r.Prompt, &r.Transition, &r.Status, &r.Error, &r.DurationSeconds)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// SetMusicRegionGenerating/SetMusicRegionReady/SetMusicRegionError are
// music_regions' own status-transition writes - SetParagraphGenerating/
// SetParagraphReady/SetParagraphError's own counterparts (upsertSFXStatus
// below), just against this table instead.
func (s *Store) SetMusicRegionGenerating(id string) error {
	_, err := s.db.Exec(`UPDATE music_regions SET status = ?, error = '' WHERE id = ?`, AudioGenerating, id)
	return err
}

func (s *Store) SetMusicRegionReady(id string, durationSeconds float64) error {
	_, err := s.db.Exec(`UPDATE music_regions SET status = ?, error = '', duration_seconds = ? WHERE id = ?`, AudioReady, durationSeconds, id)
	return err
}

func (s *Store) SetMusicRegionError(id, message string) error {
	_, err := s.db.Exec(`UPDATE music_regions SET status = ?, error = ? WHERE id = ?`, AudioError, message, id)
	return err
}

// ResetMusicRegionAudio puts a single already-touched (ready, error, or
// even still-pending) region back to AudioPending - ResetParagraphAudio's
// own counterpart, for the reader's per-region "Generate"/"Regenerate"
// button (see httpapi.handleRegenerateMusicRegion) rather than
// ResetChapterMusicAudio's whole-chapter scope. Never touches this
// region's own scored mood/prompt/transition - only its generation state,
// so jobs.Manager.MaybeAdvanceChapterMusic picks it back up as the next
// eligible region to generate.
func (s *Store) ResetMusicRegionAudio(id string) error {
	_, err := s.db.Exec(`UPDATE music_regions SET status = ?, error = '', duration_seconds = 0 WHERE id = ?`, AudioPending, id)
	return err
}

// ResetChapterMusicAudio resets every one of chapterID's own music regions
// back to AudioPending (duration 0) while leaving their own scored mood/
// prompt/transition untouched - called whenever this chapter's narration
// audio itself gets invalidated (DeleteChapterAudio) since a region's
// target duration is derived from its paragraphs' own real narration
// durations (see store.MusicRegion's own doc comment) and so goes stale
// the moment that narration is deleted/regenerated. Never touches
// passes.music or deletes any row - re-scoring is not needed
// just because narration changed, only re-generating the audio once
// narration is ready again. Returns the regions as they stood before the
// reset (same reasoning as ClearMusicRegions) so the caller can remove
// each one's now-stale on-disk clip.
func (s *Store) ResetChapterMusicAudio(chapterID string) ([]MusicRegion, error) {
	regions, err := s.ListMusicRegions(chapterID)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`UPDATE music_regions SET status = ?, error = '', duration_seconds = 0 WHERE chapter_id = ?`, AudioPending, chapterID); err != nil {
		return nil, err
	}
	return regions, nil
}

// SetChapterAttributed marks chapterID's own PassAttribution/PassDescription/
// PassScareQuote true in one write (a single attributeChapter run always
// triggers all three together - see Passes.Description/Passes.ScareQuote's
// own doc comments), leaving PassDirection completely untouched either way.
// Set by httpapi.attributeChapter once a run finishes covering the whole
// chapter in one uninterrupted pass, never on a pause or a genuine failure
// partway through.
//
// Uses DuckDB's own json_merge_patch (RFC 7396) rather than a Go-side
// read-modify-write: merging the literal patch object directly into the
// stored value, in one statement, is both simpler and race-free (no
// separate SELECT to go stale between reading and writing) - a merge
// patch's own semantics are exactly "set these keys, leave every other key
// alone" already, precisely what every one of this package's chapters.
// passes writers wants. The patch itself is always one of the two literal
// strings below (no external/request-supplied text ever reaches this
// query), so it's inlined directly rather than bound as a parameter.
func (s *Store) SetChapterAttributed(chapterID string) error {
	_, err := s.db.Exec(
		`UPDATE chapters SET passes = json_merge_patch(passes, '{"attribution": true, "description": true, "scareQuote": true}') WHERE id = ?`,
		chapterID,
	)
	return err
}

// SetChapterDirected marks chapterID's own PassDirection true, leaving
// PassAttribution/PassDescription untouched - SetChapterAttributed's own
// sibling, set by httpapi.directChapter once a run finishes covering the
// whole chapter in one uninterrupted pass. See SetChapterAttributed's own
// doc comment for why this is a single json_merge_patch statement rather
// than a Go-side read-modify-write.
func (s *Store) SetChapterDirected(chapterID string) error {
	_, err := s.db.Exec(
		`UPDATE chapters SET passes = json_merge_patch(passes, '{"direction": true}') WHERE id = ?`,
		chapterID,
	)
	return err
}

// ListParagraphsRaw returns a chapter's paragraphs (structural fields plus
// Speaker) with no per-voice audio status - since a character's assigned
// voice can now override the book's own per paragraph (see
// internal/narration.Resolver), different paragraphs in the same chapter
// can resolve to different voice_ids, so there's no longer one single
// voice_id a JOIN here could filter on. Callers resolve each paragraph's
// applicable voice_id themselves and batch-fetch status via
// ParagraphAudioStatuses.
func (s *Store) ListParagraphsRaw(chapterID string) ([]Paragraph, error) {
	rows, err := s.db.Query(`SELECT id, chapter_id, idx, position, content, speaker, inline, is_quote, scare_quote, describes_characters, tts_tags, pronunciation, emphasis FROM paragraphs WHERE chapter_id = ? ORDER BY idx ASC`, chapterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Paragraph
	for rows.Next() {
		var p Paragraph
		if err := rows.Scan(&p.ID, &p.ChapterID, &p.Idx, &p.Position, &p.Text, &p.Speaker, &p.Inline, &p.IsQuote, &p.ScareQuote, &p.DescribesCharacters, &p.Tags, &p.Pronunciation, &p.Emphasis); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListParagraphsRawForBook returns every paragraph across all of bookID's
// chapters (structural fields plus Speaker), in book order - the
// whole-book counterpart to ListParagraphsRaw, used for book-wide
// aggregation (httpapi's character summary table) rather than one chapter
// at a time.
func (s *Store) ListParagraphsRawForBook(bookID string) ([]Paragraph, error) {
	rows, err := s.db.Query(`
		SELECT p.id, p.chapter_id, p.idx, p.position, p.content, p.speaker, p.inline, p.is_quote
		FROM paragraphs p
		JOIN chapters c ON c.id = p.chapter_id
		WHERE c.book_id = ?
		ORDER BY c.idx ASC, p.idx ASC`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Paragraph
	for rows.Next() {
		var p Paragraph
		if err := rows.Scan(&p.ID, &p.ChapterID, &p.Idx, &p.Position, &p.Text, &p.Speaker, &p.Inline, &p.IsQuote); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AudioState is one (paragraph, voice) pair's generation status, as looked
// up by ParagraphAudioStatuses.
type AudioState struct {
	Status          string
	Error           string
	DurationSeconds float64
	WordTimings     string
	// PointerOffset/PointerSeconds: see paragraph_audio.pointer_offset's
	// own doc comment. PointerOffset 0 (the common case) means this
	// paragraph has its own real audio file; a positive value means its
	// real audio actually lives on the paragraph PointerOffset positions
	// earlier by Idx (same chapter+voice), starting PointerSeconds into
	// that paragraph's own clip - httpapi resolves this into an audioUrl
	// pointing straight at that other paragraph rather than this one.
	PointerOffset  int
	PointerSeconds float64
}

// ParagraphAudioStatuses batch-looks-up audio state for paragraphIDs under
// one shared voiceID - the replacement for ListParagraphs' old chapter-wide
// JOIN now that a chapter's paragraphs don't all necessarily share one
// voice: callers group paragraph ids by their resolved voice_id first (see
// internal/narration.Resolver) and call this once per distinct voice_id
// actually in use, rather than once per paragraph. A paragraph absent from
// the returned map has never been touched by voiceID (equivalent to the
// old COALESCE(..., 'pending') default) - callers should treat that as
// AudioPending.
func (s *Store) ParagraphAudioStatuses(paragraphIDs []string, voiceID string) (map[string]AudioState, error) {
	out := make(map[string]AudioState, len(paragraphIDs))
	if len(paragraphIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(paragraphIDs)), ",")
	args := make([]any, 0, len(paragraphIDs)+1)
	for _, id := range paragraphIDs {
		args = append(args, id)
	}
	args = append(args, voiceID)

	rows, err := s.db.Query(
		`SELECT paragraph_id, status, error, duration_seconds, word_timings, pointer_offset, pointer_seconds
		 FROM paragraph_audio
		 WHERE paragraph_id IN (`+placeholders+`) AND voice_id = ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var a AudioState
		if err := rows.Scan(&id, &a.Status, &a.Error, &a.DurationSeconds, &a.WordTimings, &a.PointerOffset, &a.PointerSeconds); err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, rows.Err()
}

// CountReadyAudioForSpeaker counts bookID's paragraphs attributed to
// speaker (the "Narrator"/"" equivalence handled the same way
// ParagraphsForSpeaker's own does) whose voiceID audio is ready -
// httpapi.handleListSpeakers' own "X/Y generated" figure per row. Scoped
// via a JOIN on (book, speaker, voice) rather than
// ParagraphAudioStatuses' approach of one query placeholder per paragraph
// id, which becomes genuinely expensive - and, worse, holds the single
// shared DuckDB connection (see this package's own doc comment) for that
// whole query, stalling every other concurrent request app-wide until it
// returns - for a speaker with many thousands of paragraphs (the
// Narrator, on a long book).
func (s *Store) CountReadyAudioForSpeaker(bookID, speaker, voiceID string) (int, error) {
	query := `
		SELECT COUNT(*)
		FROM paragraph_audio pa
		JOIN paragraphs p ON p.id = pa.paragraph_id
		JOIN chapters c ON c.id = p.chapter_id
		WHERE c.book_id = ? AND pa.voice_id = ? AND pa.status = ? AND p.speaker = ?`
	args := []any{bookID, voiceID, AudioReady, speaker}
	if speaker == "Narrator" {
		query = `
			SELECT COUNT(*)
			FROM paragraph_audio pa
			JOIN paragraphs p ON p.id = pa.paragraph_id
			JOIN chapters c ON c.id = p.chapter_id
			WHERE c.book_id = ? AND pa.voice_id = ? AND pa.status = ? AND (p.speaker = 'Narrator' OR p.speaker = '')`
		args = []any{bookID, voiceID, AudioReady}
	}

	var count int
	err := s.db.QueryRow(query, args...).Scan(&count)
	return count, err
}

// GetParagraph is a structural, voice-agnostic lookup (id/chapter/position/
// text/speaker only) - used where the caller needs to know which chapter
// (and, via Speaker, which voice - see internal/narration.Resolver) a
// paragraph belongs to before it can even determine which voice applies.
func (s *Store) GetParagraph(id string) (*Paragraph, error) {
	var p Paragraph
	err := s.db.QueryRow(`SELECT id, chapter_id, idx, position, content, speaker, inline, is_quote, scare_quote, describes_characters, tts_tags, pronunciation, emphasis FROM paragraphs WHERE id = ?`, id).
		Scan(&p.ID, &p.ChapterID, &p.Idx, &p.Position, &p.Text, &p.Speaker, &p.Inline, &p.IsQuote, &p.ScareQuote, &p.DescribesCharacters, &p.Tags, &p.Pronunciation, &p.Emphasis)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListCharacters returns scope's (see SeriesScope) known characters in
// discovery order. Identity/summary only - see CharacterVoicesForModel for
// their (per-clone-model) voice assignments.
func (s *Store) ListCharacters(scope string) ([]Character, error) {
	rows, err := s.db.Query(`SELECT id, scope, name, summary, ref_line, is_role, created_at FROM characters WHERE scope = ? ORDER BY created_at ASC`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Character
	for rows.Next() {
		var c Character
		if err := rows.Scan(&c.ID, &c.Scope, &c.Name, &c.Summary, &c.RefLine, &c.IsRole, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCharacter is a by-id lookup (e.g. httpapi's .../characters/{id}/...
// routes, which address a character directly rather than by scope+name).
func (s *Store) GetCharacter(id string) (*Character, error) {
	var c Character
	err := s.db.QueryRow(`SELECT id, scope, name, summary, ref_line, is_role, created_at FROM characters WHERE id = ?`, id).
		Scan(&c.ID, &c.Scope, &c.Name, &c.Summary, &c.RefLine, &c.IsRole, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) GetCharacterByName(scope, name string) (*Character, error) {
	var c Character
	err := s.db.QueryRow(`SELECT id, scope, name, summary, ref_line, is_role, created_at FROM characters WHERE scope = ? AND name = ?`, scope, name).
		Scan(&c.ID, &c.Scope, &c.Name, &c.Summary, &c.RefLine, &c.IsRole, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// UpsertCharacter registers name as a character of scope if it isn't
// already known - called as speaker attribution (internal/speakerattr)
// discovers character names, so each one gets exactly one row (and so one
// consistent identity/summary) no matter how many chapters, or books in
// the same series (narrated by whichever clone models), it ends up
// attributed in. created reports whether this call actually inserted a
// new row, for callers that care; characterization itself no longer runs
// at discovery time at all - it's deferred until a character's voice is
// actually needed (see httpapi.provisionCharacterVoice,
// jobs.Manager.provisionMissingCharacterVoices).
//
// isRole is only ever applied on the initial insert - an existing row's
// own IsRole is never overwritten by a later call, even one that
// disagrees. Deliberately sticky-from-first-sight rather than
// last-write-wins: a role label ("Guard", "Thug") and a real individual's
// name are both just strings once discovered, and a later batch
// misjudging one as the other (in either direction) shouldn't silently
// reclassify an identity every other chapter/book has already been
// building dialogue/voice history against.
func (s *Store) UpsertCharacter(scope, name string, isRole bool) (character Character, created bool, err error) {
	existing, err := s.GetCharacterByName(scope, name)
	if err != nil {
		return Character{}, false, err
	}
	if existing != nil {
		return *existing, false, nil
	}
	c := Character{ID: NewID(), Scope: scope, Name: name, IsRole: isRole, CreatedAt: time.Now().Unix()}
	_, err = s.db.Exec(
		`INSERT INTO characters (id, scope, name, summary, is_role, created_at) VALUES (?, ?, ?, '', ?, ?)`,
		c.ID, c.Scope, c.Name, c.IsRole, c.CreatedAt,
	)
	if err != nil {
		return Character{}, false, err
	}
	return c, true, nil
}

// CharacterVoiceForModel returns id's assigned voice preset for cloneModel
// specifically, or "" if none has been assigned for that model yet (either
// never auto-assigned, or explicitly cleared - see SetCharacterVoice). A
// character can be assigned a different preset per clone model (a preset
// clones through one specific model and isn't portable to another - see
// store.Character's doc comment) - callers resolving a single paragraph's
// voice pass the *book's own* resolved clone model
// (internal/narration.Resolver.ForParagraph).
func (s *Store) CharacterVoiceForModel(characterID, cloneModel string) (string, error) {
	var presetID string
	err := s.db.QueryRow(`SELECT voice_preset_id FROM character_voices WHERE character_id = ? AND clone_model = ?`, characterID, cloneModel).Scan(&presetID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return presetID, err
}

// CharacterVoicesForModel batch-looks-up cloneModel's assigned voice
// preset for each of characterIDs - the multi-character counterpart of
// CharacterVoiceForModel, for callers resolving a whole chapter/book's
// worth of characters at once (httpapi's handleGetChapter/
// handleListSpeakers) rather than one at a time. A character id absent
// from the returned map has no voice assigned for cloneModel.
func (s *Store) CharacterVoicesForModel(characterIDs []string, cloneModel string) (map[string]string, error) {
	out := make(map[string]string, len(characterIDs))
	if len(characterIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(characterIDs)), ",")
	args := make([]any, 0, len(characterIDs)+1)
	for _, id := range characterIDs {
		args = append(args, id)
	}
	args = append(args, cloneModel)

	rows, err := s.db.Query(
		`SELECT character_id, voice_preset_id FROM character_voices WHERE character_id IN (`+placeholders+`) AND clone_model = ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id, presetID string
		if err := rows.Scan(&id, &presetID); err != nil {
			return nil, err
		}
		out[id] = presetID
	}
	return out, rows.Err()
}

// VoicePresetIDsForCharacter returns every voice_preset_id currently
// assigned to characterID, across every clone model it has one for - the
// "all models, one character" counterpart of CharacterVoiceForModel's
// "one model, one character" and CharacterVoicesForModel's "one model,
// many characters". Used to invalidate every one of a character's
// assigned voices after a re-characterization changes their Summary (see
// httpapi.handleCharacterizeSpeaker) - unlike an ordinary read path
// resolving narration for one specific book (which only ever cares about
// that book's own resolved clone model), invalidation has to reach every
// clone model this character has ever been assigned a voice under, not
// just whichever one happens to be in use right now.
func (s *Store) VoicePresetIDsForCharacter(characterID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT voice_preset_id FROM character_voices WHERE character_id = ?`, characterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var presetID string
		if err := rows.Scan(&presetID); err != nil {
			return nil, err
		}
		out = append(out, presetID)
	}
	return out, rows.Err()
}

// SetCharacterVoice assigns (or, with "", clears) a character's narration
// voice for cloneModel specifically - see internal/narration.Resolver for
// how this then overrides the book's own voice for paragraphs attributed
// to this character, when the book currently in use narrates through that
// same clone model.
func (s *Store) SetCharacterVoice(characterID, cloneModel, voicePresetID string) error {
	if voicePresetID == "" {
		_, err := s.db.Exec(`DELETE FROM character_voices WHERE character_id = ? AND clone_model = ?`, characterID, cloneModel)
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO character_voices (character_id, clone_model, voice_preset_id) VALUES (?, ?, ?)
		 ON CONFLICT (character_id, clone_model) DO UPDATE SET voice_preset_id = excluded.voice_preset_id`,
		characterID, cloneModel, voicePresetID,
	)
	return err
}

// SetCharacterSummary records the LLM's characterization of how a
// character should sound, and its paired reference passage (see
// store.Character.Summary/RefLine, speakerattr.Client.CharacterizeVoice) -
// written together, in one call, since CharacterizeVoice always produces
// both from the same generation. Independent of SetCharacterVoice since a
// manual voice reassignment (handleSetCharacterVoice) shouldn't touch
// either.
func (s *Store) SetCharacterSummary(id, summary, refLine string) error {
	_, err := s.db.Exec(`UPDATE characters SET summary = ?, ref_line = ? WHERE id = ?`, summary, refLine, id)
	return err
}

// DeleteCharacter removes id's character_voices rows and the characters
// row itself - manual cascade (no FK ON DELETE CASCADE - see this
// package's own doc comment). Deliberately leaves the voice_presets those
// character_voices rows may have pointed at alone: a preset is
// independently deletable and may still be referenced elsewhere (a
// book's own voice, the default voice, another character sharing it) -
// callers that also want those gone delete them explicitly themselves
// (see httpapi.handleDeleteBookSpeakerData).
func (s *Store) DeleteCharacter(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM character_voices WHERE character_id = ?`, id); err != nil {
		return fmt.Errorf("delete character voices: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM characters WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete character: %w", err)
	}
	return tx.Commit()
}

// ListSeriesBooks returns every book sharing seriesName (non-empty), in
// series order - used to gather a recurring character's dialogue across a
// whole series (see QuotesForBooks) and to keep attribution/voice casting
// consistent throughout it.
func (s *Store) ListSeriesBooks(seriesName string) ([]Book, error) {
	rows, err := s.db.Query(`SELECT id, title, author, language, cover_ext, series_name, series_index, added_at,
		voice_preset_id, voice_instruct, voice_language, voice_seed, character_voice_mode, speech_direction,
		pos_chapter_idx, pos_paragraph_idx, pos_seconds, estimate_sec_per_char
		FROM books WHERE series_name = ? ORDER BY series_index ASC, added_at ASC`, seriesName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Book
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.ID, &b.Title, &b.Author, &b.Language, &b.CoverExt, &b.SeriesName, &b.SeriesIndex, &b.AddedAt,
			&b.VoicePresetID, &b.VoiceInstruct, &b.VoiceLanguage, &b.VoiceSeed, &b.CharacterVoiceMode, &b.SpeechDirection,
			&b.PosChapterIdx, &b.PosParagraphIdx, &b.PosSeconds, &b.EstimateSecPerChar); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// QuoteContext is one of a character's own attributed dialogue lines, plus
// (when one immediately follows in the same chapter *and* is Inline - see
// below) the narration paragraph right after it - typically a "Name said"
// attribution tag. That tag is often the *only* place a character's own
// gender/age is actually established in the text, via the narrator's
// pronouns ("she asked", "his voice") - their own dialogue content usually
// doesn't reveal it at all, and speakerattr.Client.CharacterizeVoice
// deliberately never sees their name (specifically to avoid assuming
// gender from it - see its own system prompt), so without this it has no
// real signal for gender/age at all beyond guessing from register/diction.
// Context is "" when no narration paragraph immediately follows (chapter/
// book boundary, two quotes back to back with no tag between them, or the
// next stored paragraph is Inline=false - a genuinely new source
// paragraph that just happens to sit at idx+1, not this quote's own
// attribution tag; epub.splitQuoteSegments only flags a segment Inline
// when it's a later piece of the *same* original source paragraph the
// quote itself came from, so requiring it here is what keeps this from
// picking up unrelated narration that starts a fresh paragraph right
// after the quote).
type QuoteContext struct {
	Text    string
	Context string
}

// QuotesForBooks returns up to limit of characterName's own attributed
// paragraphs (plus surrounding narration context - see QuoteContext)
// across bookIDs, in the given book order (then chapter/paragraph order
// within each) - gathered for speakerattr.Client.CharacterizeVoice.
// bookIDs is typically ListSeriesBooks' output in series order, so if
// limit truncates, earlier-book (more established) dialogue is kept over
// later-book dialogue - preserved here via an explicit ORDER BY CASE
// ranking each book_id by its position in bookIDs, rather than however
// DuckDB would otherwise order an IN (...) match, since this is one single
// query across every book now (see this function's own doc history: it
// used to loop issuing one query per book, an O(len(bookIDs)) count that
// grows as a series gains more books - constant now, matching
// DescriptionsForBooks' own single-query shape) rather than a query per
// book. Still just one query per *character* - see this function's own
// callers (characterizeVoice) for why that per-character granularity
// itself stays as is: a character's own quotes/descriptions can still be
// discovered/attributed after this character's own job was queued, so a
// fetch shared across several characters up front (decided once, before
// any of their jobs actually run) risks working from stale data by the
// time a given character's own job executes - fetching fresh, per
// character, at that point is what stays correct, just no longer paying
// an extra query per book to do it.
func (s *Store) QuotesForBooks(bookIDs []string, characterName string, limit int) ([]QuoteContext, error) {
	if len(bookIDs) == 0 {
		return nil, nil
	}
	bookPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(bookIDs)), ",")
	var orderCases strings.Builder
	args := make([]any, 0, len(bookIDs)*2+3)
	for _, id := range bookIDs {
		args = append(args, id)
	}
	args = append(args, characterName)
	for i, id := range bookIDs {
		fmt.Fprintf(&orderCases, "WHEN ? THEN %d ", i)
		args = append(args, id)
	}
	args = append(args, limit)

	rows, err := s.db.Query(`
		SELECT p.content, COALESCE(next.content, '')
		FROM paragraphs p
		JOIN chapters c ON c.id = p.chapter_id
		LEFT JOIN paragraphs next
			ON next.chapter_id = p.chapter_id AND next.idx = p.idx + 1
			AND next.inline
			AND (next.speaker = '' OR next.speaker = 'Narrator')
		WHERE c.book_id IN (`+bookPlaceholders+`) AND p.speaker = ?
		ORDER BY CASE c.book_id `+orderCases.String()+`END, c.idx ASC, p.idx ASC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []QuoteContext
	for rows.Next() {
		var qc QuoteContext
		if err := rows.Scan(&qc.Text, &qc.Context); err != nil {
			return nil, err
		}
		out = append(out, qc)
	}
	return out, rows.Err()
}

// DescriptionContext is one paragraph of narration describing
// characterName's physical appearance or personality (see
// internal/speakerattr's AttributionResult.Describes and
// speakerattr.DescriptionContext, which this mirrors) - gathered for
// speakerattr.Client.CharacterizeVoice the same way QuotesForBooks gathers
// their own dialogue.
type DescriptionContext struct {
	Text string
}

// likeEscape escapes s's own LIKE metacharacters (%, _, and the escape
// character itself) so it can be embedded in a LIKE pattern and matched
// literally - without this, a character name that happens to contain one of
// these (e.g. an underscore) would silently match more than intended
// (DescriptionsForBooks' own needle wraps the result in unescaped `%`
// wildcards on either side, which must keep matching "anything", so only
// the name's own characters are escaped here).
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// DescriptionsForBooks returns up to limit paragraphs across bookIDs whose
// describes_characters (see SetParagraphDescriptions) includes
// characterName, in the given book order (then chapter/paragraph order
// within each) - the description-narration counterpart of QuotesForBooks,
// gathered for speakerattr.Client.CharacterizeVoice. Matching is a LIKE
// against the JSON-quoted name rather than unmarshaling describes_characters
// for every row in Go, the same raw-JSON-column convention
// paragraph_audio.word_timings already uses - the quoting keeps a match from
// crossing a name boundary (e.g. "Jo" cannot match inside "John") the way an
// unquoted substring search could.
func (s *Store) DescriptionsForBooks(bookIDs []string, characterName string, limit int) ([]DescriptionContext, error) {
	if len(bookIDs) == 0 {
		return nil, nil
	}
	needle := `%"` + likeEscape(characterName) + `"%`
	bookPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(bookIDs)), ",")
	var orderCases strings.Builder
	args := make([]any, 0, len(bookIDs)*2+3)
	for _, id := range bookIDs {
		args = append(args, id)
	}
	args = append(args, needle)
	for i, id := range bookIDs {
		fmt.Fprintf(&orderCases, "WHEN ? THEN %d ", i)
		args = append(args, id)
	}
	args = append(args, limit)

	rows, err := s.db.Query(`
		SELECT p.content
		FROM paragraphs p
		JOIN chapters c ON c.id = p.chapter_id
		WHERE c.book_id IN (`+bookPlaceholders+`) AND p.describes_characters LIKE ? ESCAPE '\'
		ORDER BY CASE c.book_id `+orderCases.String()+`END, c.idx ASC, p.idx ASC
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DescriptionContext
	for rows.Next() {
		var d DescriptionContext
		if err := rows.Scan(&d.Text); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SpeakerAppearance is one paragraph a character (or "Narrator") speaks,
// with enough book/chapter context to display and jump to it - the
// multi-book counterpart of SearchResult/Bookmark, since a character's
// scope can span an entire series (see SeriesScope).
type SpeakerAppearance struct {
	BookID       string
	BookTitle    string
	ChapterIdx   int
	ChapterTitle string
	ParagraphIdx int
	ParagraphID  string
	Text         string
}

// ParagraphsForSpeaker returns every paragraph attributed to name across
// bookIDs, in the given book order (then chapter/paragraph order within
// each) - a character's full "appears in" list, e.g. for a reader-facing
// "where does this character show up" view. bookIDs is typically
// ListSeriesBooks' output (series order) via httpapi.booksInScope,
// mirroring QuotesForBooks. name "Narrator" also matches paragraphs never
// explicitly attributed (speaker = ”), matching how every other
// narration-voice-resolution path in this app treats the two as
// equivalent (see internal/narration.Resolver). Backed by
// idx_paragraphs_speaker.
func (s *Store) ParagraphsForSpeaker(bookIDs []string, name string) ([]SpeakerAppearance, error) {
	var out []SpeakerAppearance
	for _, bookID := range bookIDs {
		query := `
			SELECT b.id, b.title, c.idx, c.title, p.idx, p.id, p.content
			FROM paragraphs p
			JOIN chapters c ON c.id = p.chapter_id
			JOIN books b ON b.id = c.book_id
			WHERE c.book_id = ? AND p.speaker = ?
			ORDER BY c.idx ASC, p.idx ASC`
		args := []any{bookID, name}
		if name == "Narrator" {
			query = `
				SELECT b.id, b.title, c.idx, c.title, p.idx, p.id, p.content
				FROM paragraphs p
				JOIN chapters c ON c.id = p.chapter_id
				JOIN books b ON b.id = c.book_id
				WHERE c.book_id = ? AND (p.speaker = 'Narrator' OR p.speaker = '')
				ORDER BY c.idx ASC, p.idx ASC`
			args = []any{bookID}
		}

		rows, err := s.db.Query(query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a SpeakerAppearance
			if err := rows.Scan(&a.BookID, &a.BookTitle, &a.ChapterIdx, &a.ChapterTitle, &a.ParagraphIdx, &a.ParagraphID, &a.Text); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, a)
		}
		closeErr := rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return out, nil
}

// ParagraphsDescribing returns every paragraph whose describes_characters
// includes name (see SetParagraphDescriptions), across bookIDs, in the
// given book order (then chapter/paragraph order within each) - the
// description-tagging counterpart of ParagraphsForSpeaker, backing a
// reader-facing "where is this character described" view the same way
// ParagraphsForSpeaker backs "where do they speak". A description
// paragraph's own persisted speaker is always "Narrator" (or "", the
// pre-attribution default) - httpapi.attributeChapter's is_quote gate on
// Describes guarantees a description can only ever land on a narration
// paragraph, never a quoted-dialogue one - so unlike ParagraphsForSpeaker,
// there's no separate "Narrator" special case to handle here. Matching
// uses the same JSON-quoted LIKE technique as DescriptionsForBooks, for
// the same reason (a match can't cross a name boundary).
func (s *Store) ParagraphsDescribing(bookIDs []string, name string) ([]SpeakerAppearance, error) {
	needle := `%"` + likeEscape(name) + `"%`
	var out []SpeakerAppearance
	for _, bookID := range bookIDs {
		rows, err := s.db.Query(`
			SELECT b.id, b.title, c.idx, c.title, p.idx, p.id, p.content
			FROM paragraphs p
			JOIN chapters c ON c.id = p.chapter_id
			JOIN books b ON b.id = c.book_id
			WHERE c.book_id = ? AND p.describes_characters LIKE ? ESCAPE '\'
			ORDER BY c.idx ASC, p.idx ASC`, bookID, needle)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a SpeakerAppearance
			if err := rows.Scan(&a.BookID, &a.BookTitle, &a.ChapterIdx, &a.ChapterTitle, &a.ParagraphIdx, &a.ParagraphID, &a.Text); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, a)
		}
		closeErr := rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return out, nil
}

// SetParagraphSpeakers records speaker-attribution results for a chapter's
// paragraphs, keyed by paragraph Idx (what internal/speakerattr's
// input/output both use) rather than id - the caller works from the same
// ListParagraphsRaw slice speakerattr was fed and never needs paragraph
// ids in between.
func (s *Store) SetParagraphSpeakers(chapterID string, bySpeakerIdx map[int]string) error {
	if len(bySpeakerIdx) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for idx, speaker := range bySpeakerIdx {
		if _, err := tx.Exec(`UPDATE paragraphs SET speaker = ? WHERE chapter_id = ? AND idx = ?`, speaker, chapterID, idx); err != nil {
			return fmt.Errorf("set speaker for paragraph idx %d: %w", idx, err)
		}
	}
	return tx.Commit()
}

// SetParagraphDescriptions records, for a chapter's paragraphs, which
// characters (if any) each paragraph's narration describes - the
// describes-tagging counterpart of SetParagraphSpeakers, keyed the same way
// (by paragraph Idx, from the same ListParagraphsRaw slice the caller
// already has). A paragraph with no entry in byDescribesIdx is left
// untouched, not reset to '[]' - an attribution run only reports paragraphs
// it actually found a description in; every other paragraph in the batch
// legitimately still describes no one, and rewriting it to '[]' would
// needlessly discard a description an earlier run (or manual edit) already
// recorded for it.
func (s *Store) SetParagraphDescriptions(chapterID string, byDescribesIdx map[int][]string) error {
	if len(byDescribesIdx) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for idx, names := range byDescribesIdx {
		// .Value() explicitly, not a bare DescribesCharacterList - this
		// driver's own JSON-alias bind path (bindJSON) special-cases a
		// native JSON column's parameter before ever checking
		// driver.Valuer, so passing the typed value directly fails
		// ("unsupported data type: JSON interface, need []byte, string or
		// nil" - confirmed empirically); Value() itself is still what
		// every one of these types' Scan/Value pair is for, just invoked
		// by hand here instead of by database/sql.
		encoded, err := DescribesCharacterList(names).Value()
		if err != nil {
			return fmt.Errorf("encode describes for paragraph idx %d: %w", idx, err)
		}
		if _, err := tx.Exec(`UPDATE paragraphs SET describes_characters = ? WHERE chapter_id = ? AND idx = ?`, encoded, chapterID, idx); err != nil {
			return fmt.Errorf("set describes for paragraph idx %d: %w", idx, err)
		}
	}
	return tx.Commit()
}

// SetParagraphScareQuotes records, for a chapter's paragraphs, which ones
// are scare quotes (see internal/speakerattr.Client.ScareQuoteChapter and
// Paragraph.ScareQuote) - SetParagraphDescriptions' own shape, but a plain
// bool column rather than JSON since there's nothing to encode. A paragraph
// present in byScareQuoteIdx with a false value explicitly clears a
// previously-set flag (e.g. a reader's own manual "not a scare quote after
// all" correction) - unlike SetParagraphDescriptions, which only ever
// receives entries worth setting, this can carry either direction.
func (s *Store) SetParagraphScareQuotes(chapterID string, byIdx map[int]bool) error {
	if len(byIdx) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for idx, scareQuote := range byIdx {
		if _, err := tx.Exec(`UPDATE paragraphs SET scare_quote = ? WHERE chapter_id = ? AND idx = ?`, scareQuote, chapterID, idx); err != nil {
			return fmt.Errorf("set scare_quote for paragraph idx %d: %w", idx, err)
		}
	}
	return tx.Commit()
}

// SetParagraphSentenceTags records, for a chapter's paragraphs,
// cloneModel's own sentence-level delivery annotation (see
// internal/speakerattr.Client.DirectChapter/validSentenceTags) - the
// speech-direction-tagging counterpart of SetParagraphSpeakers/
// SetParagraphDescriptions, keyed the same way (by paragraph Idx). Only
// ever touches each paragraph's own ParagraphDirection.SentenceText,
// leaving InlineText (set independently by SetParagraphInlineTags)
// untouched - see setParagraphDirectionField, the shared read-modify-write
// helper both of these are thin wrappers around.
func (s *Store) SetParagraphSentenceTags(chapterID, cloneModel string, byIdx map[int]string) error {
	return s.setParagraphDirectionField(chapterID, cloneModel, byIdx, func(d *ParagraphDirection, v string) { d.SentenceText = v })
}

// SetParagraphInlineTags is SetParagraphSentenceTags' own counterpart for
// cloneModel's positional annotation (internal/speakerattr.Client.TagSfx/
// validInlineTags) - only ever touches ParagraphDirection.InlineText,
// leaving SentenceText untouched.
func (s *Store) SetParagraphInlineTags(chapterID, cloneModel string, byIdx map[int]string) error {
	return s.setParagraphDirectionField(chapterID, cloneModel, byIdx, func(d *ParagraphDirection, v string) { d.InlineText = v })
}

// setParagraphDirectionField is the shared read-modify-write behind
// SetParagraphSentenceTags/SetParagraphInlineTags: for each paragraph in
// byIdx, decodes its current paragraphs.tts_tags, applies set to just
// cloneModel's own ParagraphDirection entry (leaving every other
// clone_model's, and whichever of SentenceText/InlineText set doesn't
// touch, exactly as they were), and writes the whole map back - so tagging
// under one clone model, or from one of the two LLM passes, never clobbers
// anything a different model or the other pass already recorded for this
// same paragraph. A paragraph absent from byIdx is left untouched
// entirely, same reasoning as SetParagraphDescriptions. An empty string
// value for a paragraph explicitly clears that field (rather than leaving
// a stale annotation from an earlier run) - if that empties cloneModel's
// whole ParagraphDirection (both fields now ""), the cloneModel key itself
// is dropped rather than kept as a pair of empty strings.
func (s *Store) setParagraphDirectionField(chapterID, cloneModel string, byIdx map[int]string, set func(d *ParagraphDirection, v string)) error {
	if len(byIdx) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for idx, v := range byIdx {
		var tags ParagraphTagMap
		err := tx.QueryRow(`SELECT tts_tags FROM paragraphs WHERE chapter_id = ? AND idx = ?`, chapterID, idx).Scan(&tags)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return fmt.Errorf("read tts_tags for paragraph idx %d: %w", idx, err)
		}
		if tags == nil {
			tags = ParagraphTagMap{}
		}
		d := tags[cloneModel]
		set(&d, v)
		if d.SentenceText == "" && d.InlineText == "" {
			delete(tags, cloneModel)
		} else {
			tags[cloneModel] = d
		}
		// .Value() explicitly - see SetParagraphDescriptions' own doc
		// comment for why a bare ParagraphTagMap can't be passed directly.
		encoded, err := tags.Value()
		if err != nil {
			return fmt.Errorf("encode tts_tags for paragraph idx %d: %w", idx, err)
		}
		if _, err := tx.Exec(`UPDATE paragraphs SET tts_tags = ? WHERE chapter_id = ? AND idx = ?`, encoded, chapterID, idx); err != nil {
			return fmt.Errorf("set tts_tags for paragraph idx %d: %w", idx, err)
		}
	}
	return tx.Commit()
}

// SetParagraphPronunciation records, for a chapter's paragraphs, the
// pronunciation substitutions resolved by
// internal/speakerattr.Client.ResolvePronunciation (see the
// paragraphs.pronunciation column's own doc comment in Open). Unlike
// SetParagraphSentenceTags/SetParagraphInlineTags, this isn't clone-model-
// keyed - a pronunciation fix is plain word substitution, independent of
// which TTS model narrates it (see Paragraph.ResolveGenerationText) - so
// each call is a plain overwrite rather than a read-modify-write into a
// per-model map: a paragraph absent from byIdx is left untouched, and one
// present is fully replaced (a nil/empty slice clears it), matching
// ResolvePronunciation's own idempotent full-chapter-refresh semantics -
// the same "safe to redo" contract attribution/direction-tagging already
// have.
func (s *Store) SetParagraphPronunciation(chapterID string, byIdx map[int][]pronounce.Substitution) error {
	if len(byIdx) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for idx, subs := range byIdx {
		if subs == nil {
			subs = []pronounce.Substitution{}
		}
		// .Value() explicitly - see SetParagraphDescriptions' own doc
		// comment for why a bare PronunciationList can't be passed
		// directly.
		encoded, err := PronunciationList(subs).Value()
		if err != nil {
			return fmt.Errorf("encode pronunciation for paragraph idx %d: %w", idx, err)
		}
		if _, err := tx.Exec(`UPDATE paragraphs SET pronunciation = ? WHERE chapter_id = ? AND idx = ?`, encoded, chapterID, idx); err != nil {
			return fmt.Errorf("set pronunciation for paragraph idx %d: %w", idx, err)
		}
	}
	return tx.Commit()
}

// SetParagraphSpeaker sets one paragraph's speaker attribution by its own
// id - the single-paragraph counterpart of SetParagraphSpeakers (a whole
// chapter's worth, keyed by idx) and ReassignCharacterSpeaker (every
// paragraph currently attributed to one name, across a whole book) - for
// correcting one individual misattributed line from the Speakers page's
// per-character "appearances" list without touching any other paragraph
// that happens to share the same (wrong) speaker.
func (s *Store) SetParagraphSpeaker(paragraphID, speaker string) error {
	_, err := s.db.Exec(`UPDATE paragraphs SET speaker = ? WHERE id = ?`, speaker, paragraphID)
	return err
}

// ClearBookSpeakers resets every paragraph's speaker attribution back to
// "" (unattributed), and clears Passes.Attribution/Passes.Description
// (chapters.passes) across every chapter of bookID - see
// httpapi.handleDeleteBookSpeakerData. Passes.Direction is deliberately
// left as-is rather than also cleared: this only clears attribution data
// (paragraphs.speaker), never a chapter's own delivery-tag data
// (paragraphs.tts_tags/pronunciation), so a directed chapter's own tagging
// is still genuinely intact and shouldn't be reported as needing to run
// again - the same asymmetry the old chapters.attributed/chapters.directed
// columns (and, later, the single ChapterState enum) already had, now
// expressed directly instead of by cases-within-one-flat-value.
// Deliberately scoped to just this book's own paragraphs.speaker/
// chapters.passes columns: the character roster, voice assignments, and
// voice presets it leaves untouched are shared series-wide (see
// store.SeriesScope) and may still be needed by other books in the same
// series.
func (s *Store) ClearBookSpeakers(bookID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE paragraphs SET speaker = '' WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`,
		bookID,
	); err != nil {
		return err
	}
	// Clears PassAttribution/PassDescription/PassScareQuote for every
	// chapter of bookID, unconditionally - unlike the old state-enum
	// version this replaced (which only reset a chapter exactly at
	// ChapterStateAttributed, leaving ChapterStateTagged alone specifically
	// to avoid also losing its direction-tagging progress), there's no such
	// risk here: PassDirection is a fully independent field now, so
	// clearing the other three never touches it regardless of whether it's
	// true, false, or absent. json_merge_patch's own RFC 7396 semantics
	// delete a key when the patch sets it to null - see
	// SetChapterAttributed's own doc comment for why this is a single
	// statement rather than a Go-side read-modify-write loop.
	if _, err := tx.Exec(
		`UPDATE chapters SET passes = json_merge_patch(passes, '{"attribution": null, "description": null, "scareQuote": null}') WHERE book_id = ?`,
		bookID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// ReassignCharacterSpeaker re-points every paragraph in bookID currently
// attributed to fromName (an exact match against paragraphs.speaker) at
// toName instead - used by httpapi.handleMergeCharacter to fold one
// character's lines into another's (or into "Narrator"/"Unknown", the two
// sentinel values - see handleMergeCharacter's own doc comment) and by
// handleDeleteCharacter (toName "Unknown" - the paragraphs were real
// quoted dialogue with a genuine speaker, just no longer this one, so
// "Unknown" is the same sentinel attribution itself falls back to for
// dialogue it can't confidently attribute; "" (Narrator) would be wrong -
// reserved for actual narration, never dialogue, see backend/CLAUDE.md's
// speakerattr section). Deliberately scoped to one book, not fromName's
// whole series scope, the same book-only split ClearBookSpeakers/
// handleDeleteBookSpeakerData use: deleting/merging a character's
// identity/voice reaches the whole series (the caller does that part
// separately, via DeleteCharacter), but no other book's paragraphs are
// touched here - a reader cleaning up a bad attribution on one book
// shouldn't silently re-attribute a sibling book they haven't looked at.
func (s *Store) ReassignCharacterSpeaker(bookID, fromName, toName string) error {
	_, err := s.db.Exec(
		`UPDATE paragraphs SET speaker = ? WHERE speaker = ? AND chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`,
		toName, fromName, bookID,
	)
	return err
}

// GetParagraphIDByIdx resolves a paragraph's stable id from its (chapter,
// idx) position - the public API addresses paragraphs this way (same as
// search results), so bookmark handlers need this to get to the id the
// bookmarks table actually keys on. Empty string, nil error if not found.
func (s *Store) GetParagraphIDByIdx(chapterID string, idx int) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM paragraphs WHERE chapter_id = ? AND idx = ?`, chapterID, idx).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// SearchResult is one paragraph matching a book search, with just enough
// chapter context to display and jump to it.
type SearchResult struct {
	ChapterIdx   int
	ChapterTitle string
	ParagraphIdx int
	Text         string
}

// escapeLike escapes ILIKE's own wildcard characters in user input so a
// search for e.g. "50%" or "under_stood" matches literally rather than
// being interpreted as a pattern.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// SearchParagraphs finds paragraphs in bookID whose text contains query
// (case-insensitive substring match), in book order, capped at limit
// matches.
func (s *Store) SearchParagraphs(bookID, query string, limit int) ([]SearchResult, error) {
	rows, err := s.db.Query(`
		SELECT c.idx, c.title, p.idx, p.content
		FROM paragraphs p
		JOIN chapters c ON c.id = p.chapter_id
		WHERE c.book_id = ? AND p.content ILIKE ? ESCAPE '\'
		ORDER BY c.idx ASC, p.idx ASC
		LIMIT ?`, bookID, "%"+escapeLike(query)+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.ChapterIdx, &r.ChapterTitle, &r.ParagraphIdx, &r.Text); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Bookmark is one flagged paragraph, with just enough chapter/paragraph
// context to display and jump to it (same shape as SearchResult) plus its
// own id and optional note.
type Bookmark struct {
	ID           string
	ChapterIdx   int
	ChapterTitle string
	ParagraphIdx int
	Text         string
	Note         string
	CreatedAt    int64
}

// UpsertBookmark flags paragraphID as bookmarked, or updates its note if it
// already is - bookmarking is a single toggleable action, so "bookmark
// this paragraph, optionally with a note" never needs a separate
// create-then-edit step. Returns the bookmark's id either way.
func (s *Store) UpsertBookmark(paragraphID, note string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM bookmarks WHERE paragraph_id = ?`, paragraphID).Scan(&id)
	if err == nil {
		_, err = s.db.Exec(`UPDATE bookmarks SET note = ? WHERE id = ?`, note, id)
		return id, err
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	id = NewID()
	_, err = s.db.Exec(
		`INSERT INTO bookmarks (id, paragraph_id, note, created_at) VALUES (?, ?, ?, ?)`,
		id, paragraphID, note, time.Now().UnixMilli(),
	)
	return id, err
}

// UpdateBookmarkNote edits an existing bookmark's note by its own id -
// UpsertBookmark's job is initial creation/toggling (addressed by
// paragraph_id); once a bookmark exists, further edits go by id instead.
func (s *Store) UpdateBookmarkNote(id, note string) error {
	_, err := s.db.Exec(`UPDATE bookmarks SET note = ? WHERE id = ?`, note, id)
	return err
}

func (s *Store) DeleteBookmark(id string) error {
	_, err := s.db.Exec(`DELETE FROM bookmarks WHERE id = ?`, id)
	return err
}

// ListBookmarks returns bookID's bookmarks in book order (not creation
// order), matching how search results are ordered - a bookmarks list reads
// most naturally as "these are the flagged spots as you'd encounter them",
// not as a most-recent-first log.
func (s *Store) ListBookmarks(bookID string) ([]Bookmark, error) {
	rows, err := s.db.Query(`
		SELECT bm.id, c.idx, c.title, p.idx, p.content, bm.note, bm.created_at
		FROM bookmarks bm
		JOIN paragraphs p ON p.id = bm.paragraph_id
		JOIN chapters c ON c.id = p.chapter_id
		WHERE c.book_id = ?
		ORDER BY c.idx ASC, p.idx ASC`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Bookmark
	for rows.Next() {
		var b Bookmark
		if err := rows.Scan(&b.ID, &b.ChapterIdx, &b.ChapterTitle, &b.ParagraphIdx, &b.Text, &b.Note, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetParagraphAudioStatus reports one paragraph's audio status for one
// voice specifically ("pending" if that voice has never touched it).
func (s *Store) GetParagraphAudioStatus(paragraphID, voiceID string) (string, error) {
	var status string
	err := s.db.QueryRow(`SELECT status FROM paragraph_audio WHERE paragraph_id = ? AND voice_id = ?`, paragraphID, voiceID).
		Scan(&status)
	if err == sql.ErrNoRows {
		return AudioPending, nil
	}
	return status, err
}

// GetParagraphAudioDuration is GetParagraphAudioStatus's own sibling for
// jobs.Manager.maybeAdvanceChapterMusic, which needs one paragraph's own
// real narration duration (not just its status) to size a music region's
// own target generation length - see store.MusicRegion's own doc comment.
// A single-row lookup, not the batch ParagraphAudioStatuses above: a
// region's own paragraph range is checked one at a time as narration
// completes, not all at once the way a chapter's full audio-status table
// is rendered.
func (s *Store) GetParagraphAudioDuration(paragraphID, voiceID string) (status string, durationSeconds float64, err error) {
	err = s.db.QueryRow(`SELECT status, duration_seconds FROM paragraph_audio WHERE paragraph_id = ? AND voice_id = ?`, paragraphID, voiceID).
		Scan(&status, &durationSeconds)
	if err == sql.ErrNoRows {
		return AudioPending, 0, nil
	}
	return status, durationSeconds, err
}

// ListImages returns a chapter's inline images ordered by their position
// among the chapter's content blocks.
func (s *Store) ListImages(chapterID string) ([]Image, error) {
	rows, err := s.db.Query(`SELECT id, chapter_id, position, ext FROM images WHERE chapter_id = ? ORDER BY position ASC`, chapterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Image
	for rows.Next() {
		var img Image
		if err := rows.Scan(&img.ID, &img.ChapterID, &img.Position, &img.Ext); err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, rows.Err()
}

// ListBreaks returns a chapter's scene breaks ordered by their position
// among the chapter's content blocks - ListImages' own doc comment,
// mirrored exactly.
func (s *Store) ListBreaks(chapterID string) ([]Break, error) {
	rows, err := s.db.Query(`SELECT id, chapter_id, position FROM breaks WHERE chapter_id = ? ORDER BY position ASC`, chapterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Break
	for rows.Next() {
		var b Break
		if err := rows.Scan(&b.ID, &b.ChapterID, &b.Position); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) GetImage(id string) (*Image, error) {
	var img Image
	err := s.db.QueryRow(`SELECT id, chapter_id, position, ext FROM images WHERE id = ?`, id).
		Scan(&img.ID, &img.ChapterID, &img.Position, &img.Ext)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &img, nil
}

// upsertParagraphAudio sets one (paragraph, voice) pair's audio state.
// DuckDB's ALTER-TABLE-with-constraints gaps earlier in this file were
// enough of a surprise that this deliberately avoids relying on ON
// CONFLICT support too: try the update first, and only insert if nothing
// existed to update.
// Always resets pointer_offset/pointer_seconds to 0 - every caller of this
// helper (SetParagraphGenerating/SetParagraphReady/SetParagraphError)
// represents a paragraph getting its own ordinary, independent audio state,
// never a scare-quote merge group's own pointer (that's exclusively
// SetParagraphReadyPointer's job) - a paragraph regenerated independently
// after previously being a pointer (jobs.Manager.generateIndependently's
// own fallback) must never leave a stale pointer behind once it has real
// audio of its own again.
func (s *Store) upsertParagraphAudio(paragraphID, voiceID, status, errMsg string, durationSeconds float64) error {
	res, err := s.db.Exec(
		`UPDATE paragraph_audio SET status = ?, error = ?, duration_seconds = ?, pointer_offset = 0, pointer_seconds = 0 WHERE paragraph_id = ? AND voice_id = ?`,
		status, errMsg, durationSeconds, paragraphID, voiceID,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	_, err = s.db.Exec(
		`INSERT INTO paragraph_audio (paragraph_id, voice_id, status, error, duration_seconds) VALUES (?, ?, ?, ?, ?)`,
		paragraphID, voiceID, status, errMsg, durationSeconds,
	)
	return err
}

func (s *Store) SetParagraphGenerating(id, voiceID string) error {
	return s.upsertParagraphAudio(id, voiceID, AudioGenerating, "", 0)
}

func (s *Store) SetParagraphReady(id, voiceID string, durationSeconds float64) error {
	return s.upsertParagraphAudio(id, voiceID, AudioReady, "", durationSeconds)
}

// SetParagraphReadyPointer marks id ready under voiceID with no audio file
// of its own - see AudioState.PointerOffset's own doc comment. Used only
// for a scare-quote merge group's own non-anchor members
// (jobs.Manager.handleMergedResult): rather than decoding/slicing/
// re-encoding a shared clip into a separate file per paragraph (a real,
// observed source of audible artifacts at the cut point - forced-alignment
// word boundaries are precise enough for karaoke-highlighting but not for
// actually splicing audio), every member but the anchor just records where
// its own content starts within the anchor's own untouched, never-edited
// recording. durationSeconds is still this paragraph's own logical share
// (next member's pointerSeconds minus this one's, or the anchor's own
// total duration minus this one's for the last member) - used for chapter/
// book listening-time estimates exactly as it is for a paragraph with its
// own file, even though no playback boundary is ever actually enforced
// against it (see usePlayback.ts, which plays straight through the whole
// shared clip and only uses these values to track which paragraph is
// "current" for highlighting/position-reporting).
func (s *Store) SetParagraphReadyPointer(id, voiceID string, pointerOffset int, pointerSeconds, durationSeconds float64) error {
	res, err := s.db.Exec(
		`UPDATE paragraph_audio SET status = ?, error = '', duration_seconds = ?, pointer_offset = ?, pointer_seconds = ? WHERE paragraph_id = ? AND voice_id = ?`,
		AudioReady, durationSeconds, pointerOffset, pointerSeconds, id, voiceID,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	_, err = s.db.Exec(
		`INSERT INTO paragraph_audio (paragraph_id, voice_id, status, duration_seconds, pointer_offset, pointer_seconds) VALUES (?, ?, ?, ?, ?, ?)`,
		id, voiceID, AudioReady, durationSeconds, pointerOffset, pointerSeconds,
	)
	return err
}

// SetParagraphWordTimings records alignment results for an already-ready
// paragraph - a follow-up call after SetParagraphReady, not part of the
// same upsert, since alignment runs as a separate best-effort step after
// audio is already generated and playable (see jobs.Manager.alignParagraph).
// A no-op if the (paragraph, voice) row doesn't exist yet, which shouldn't
// happen in practice (SetParagraphReady always runs first) but isn't worth
// erroring over if it somehow did.
func (s *Store) SetParagraphWordTimings(id, voiceID, wordTimingsJSON string) error {
	_, err := s.db.Exec(
		`UPDATE paragraph_audio SET word_timings = ? WHERE paragraph_id = ? AND voice_id = ?`,
		wordTimingsJSON, id, voiceID,
	)
	return err
}

func (s *Store) SetParagraphError(id, voiceID, message string) error {
	return s.upsertParagraphAudio(id, voiceID, AudioError, message, 0)
}

// ResetParagraphAudio puts an already-generated (ready or error) paragraph
// back to pending for voiceID, so the job queue picks it up again - for an
// explicit "regenerate this paragraph" request, not the normal pending ->
// generating -> ready path. Also clears word_timings (the old alignment
// belongs to audio that's about to be overwritten, and would otherwise
// linger and mismatch the new clip until re-alignment finishes) and any
// pointer_offset/pointer_seconds (whether this paragraph ends up its own
// real file or a pointer again is re-decided fresh by whatever regenerates
// it - see AudioState.PointerOffset's own doc comment - so a stale pointer
// value must never survive past this reset).
func (s *Store) ResetParagraphAudio(id, voiceID string) error {
	res, err := s.db.Exec(
		`UPDATE paragraph_audio SET status = ?, error = '', duration_seconds = 0, word_timings = '[]', pointer_offset = 0, pointer_seconds = 0 WHERE paragraph_id = ? AND voice_id = ?`,
		AudioPending, id, voiceID,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	_, err = s.db.Exec(
		`INSERT INTO paragraph_audio (paragraph_id, voice_id, status) VALUES (?, ?, ?)`,
		id, voiceID, AudioPending,
	)
	return err
}

// SFXState is one paragraph's own sound-effect (Stable Audio SFX) state, as
// looked up by ParagraphSFXStates/GetParagraphSFXState - AudioState's own
// counterpart for the sfx table, which (unlike paragraph_audio) has at
// most one row per paragraph, never keyed by voice - see that table's own
// doc comment.
type SFXState struct {
	Prompt          string
	Status          string
	Error           string
	DurationSeconds float64
	TriggerWord     int
}

// ParagraphSFXStates batch-looks-up sfx state for paragraphIDs -
// ParagraphAudioStatuses' own counterpart for the sfx table. A paragraph
// absent from the returned map has no sfx row at all (the overwhelmingly
// common case - most paragraphs never get one), equivalent to the zero
// SFXState{} (Status "").
func (s *Store) ParagraphSFXStates(paragraphIDs []string) (map[string]SFXState, error) {
	out := make(map[string]SFXState, len(paragraphIDs))
	if len(paragraphIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(paragraphIDs)), ",")
	args := make([]any, len(paragraphIDs))
	for i, id := range paragraphIDs {
		args[i] = id
	}

	rows, err := s.db.Query(
		`SELECT paragraph_id, prompt, status, error, duration_seconds, trigger_word FROM sfx WHERE paragraph_id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var st SFXState
		if err := rows.Scan(&id, &st.Prompt, &st.Status, &st.Error, &st.DurationSeconds, &st.TriggerWord); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// GetParagraphSFXState is ParagraphSFXStates' own single-paragraph
// counterpart, for the per-paragraph SFX handlers (chapters.go) that only
// ever need one row at a time. Returns the zero SFXState{}, not an error,
// for a paragraph with no sfx row yet - same "absent means never touched"
// convention ParagraphSFXStates' own map follows.
func (s *Store) GetParagraphSFXState(paragraphID string) (SFXState, error) {
	var st SFXState
	err := s.db.QueryRow(
		`SELECT prompt, status, error, duration_seconds, trigger_word FROM sfx WHERE paragraph_id = ?`, paragraphID,
	).Scan(&st.Prompt, &st.Status, &st.Error, &st.DurationSeconds, &st.TriggerWord)
	if err == sql.ErrNoRows {
		return SFXState{}, nil
	}
	return st, err
}

// SetParagraphSFXPrompt records id's own reader-entered sound-effect
// prompt, independent of status - saving a prompt doesn't by itself
// invalidate any already-generated clip (a reader may be drafting a prompt
// before generating at all); handleGenerateParagraphSFX is what actually
// triggers a new render and updates status. Insert-if-missing, same
// "UPDATE, check RowsAffected, INSERT on zero" pattern
// upsertParagraphAudio uses (see its own doc comment) - sfx rows are
// sparse, so a paragraph's very first prompt always needs to insert, not
// update.
func (s *Store) SetParagraphSFXPrompt(id, prompt string) error {
	res, err := s.db.Exec(`UPDATE sfx SET prompt = ? WHERE paragraph_id = ?`, prompt, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	_, err = s.db.Exec(`INSERT INTO sfx (paragraph_id, prompt) VALUES (?, ?)`, id, prompt)
	return err
}

// SetParagraphSFXTriggerWord records which word (a 0-based index into this
// paragraph's own forced-alignment word_timings) playback should start the
// sound effect at - see SFXState.TriggerWord. Same insert-if-missing
// shape as SetParagraphSFXPrompt.
func (s *Store) SetParagraphSFXTriggerWord(id string, wordIdx int) error {
	res, err := s.db.Exec(`UPDATE sfx SET trigger_word = ? WHERE paragraph_id = ?`, wordIdx, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	_, err = s.db.Exec(`INSERT INTO sfx (paragraph_id, trigger_word) VALUES (?, ?)`, id, wordIdx)
	return err
}

// upsertSFXStatus is the shared insert-if-missing helper behind
// SetParagraphSFXGenerating/SetParagraphSFXReady/SetParagraphSFXError -
// upsertParagraphAudio's own exact pattern, reused here for the same
// "sfx rows are sparse" reason SetParagraphSFXPrompt's own doc comment
// gives.
func (s *Store) upsertSFXStatus(paragraphID, status, errMsg string, durationSeconds float64) error {
	res, err := s.db.Exec(
		`UPDATE sfx SET status = ?, error = ?, duration_seconds = ? WHERE paragraph_id = ?`,
		status, errMsg, durationSeconds, paragraphID,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	_, err = s.db.Exec(
		`INSERT INTO sfx (paragraph_id, status, error, duration_seconds) VALUES (?, ?, ?, ?)`,
		paragraphID, status, errMsg, durationSeconds,
	)
	return err
}

func (s *Store) SetParagraphSFXGenerating(id string) error {
	return s.upsertSFXStatus(id, AudioGenerating, "", 0)
}

func (s *Store) SetParagraphSFXReady(id string, durationSeconds float64) error {
	return s.upsertSFXStatus(id, AudioReady, "", durationSeconds)
}

func (s *Store) SetParagraphSFXError(id, message string) error {
	return s.upsertSFXStatus(id, AudioError, message, 0)
}

// DeleteBookAudio drops every paragraph_audio row for bookID, across every
// voice_id it's ever been generated under (see that table's own doc
// comment on why more than one can exist per paragraph) - the "delete
// generated audio" button's DB half, for reclaiming space/starting a
// book's narration over without touching its text, chapters, or speaker
// attribution. httpapi.handleDeleteBookAudio pairs this with removing the
// book's own audio directory on disk (the actual .wav files) - this alone
// only clears the DB's own bookkeeping about them, the same split
// DeleteBook's own paragraph/file cleanup already follows.
func (s *Store) DeleteBookAudio(bookID string) error {
	_, err := s.db.Exec(
		`DELETE FROM paragraph_audio WHERE paragraph_id IN (
			SELECT id FROM paragraphs WHERE chapter_id IN (
				SELECT id FROM chapters WHERE book_id = ?
			)
		)`,
		bookID,
	)
	return err
}

// DeleteChapterAudio is DeleteBookAudio's single-chapter counterpart - the
// chapter-header "Clear generation" button's DB half, for resetting just
// one chapter's narration (across every voice_id it's ever been generated
// under) without touching the rest of the book. httpapi.handleDeleteChapterAudio
// pairs this with removing that chapter's own audio directory on disk,
// same split as the book-level version.
func (s *Store) DeleteChapterAudio(chapterID string) error {
	_, err := s.db.Exec(
		`DELETE FROM paragraph_audio WHERE paragraph_id IN (
			SELECT id FROM paragraphs WHERE chapter_id = ?
		)`,
		chapterID,
	)
	return err
}

// ResetAllChapterMusicAudioForBook resets every music region across every
// chapter of bookID back to AudioPending (duration 0) - DeleteBookAudio's
// own music counterpart. Pure DB, like ResetChapterMusicAudio - the
// caller (handleDeleteBookAudio) already removes this whole book's
// `audio/<bookID>` directory wholesale, which already covers every
// chapter's own music subdirectory (audiopath.MusicDir nests under it,
// the same way SFXDir/VoiceDir do), so there's no per-region file to
// return/remove here the way ClearMusicRegions/ResetChapterMusicAudio
// have to for their own single-chapter scope.
func (s *Store) ResetAllChapterMusicAudioForBook(bookID string) error {
	_, err := s.db.Exec(
		`UPDATE music_regions SET status = ?, error = '', duration_seconds = 0 WHERE chapter_id IN (
			SELECT id FROM chapters WHERE book_id = ?
		)`,
		AudioPending, bookID,
	)
	return err
}

// SpeakerAudioRef identifies one on-disk audio file
// (audiopath.ParagraphFile(dataDir, BookID, ChapterID, VoiceID, Idx)) that
// DeleteParagraphAudioForSpeaker's matching DB delete just orphaned -
// returned so the caller can remove it too, the same DB-delete/disk-delete
// pairing handleDeleteBookAudio/handleDeleteChapterAudio already use, just
// per-file here since a speaker's paragraphs are scattered through a
// chapter rather than filling it entirely (RemoveAll-ing a whole chapter's
// audio directory the way those two do would also wipe every other
// speaker's and the narrator's own lines in it).
type SpeakerAudioRef struct {
	BookID    string
	ChapterID string
	VoiceID   string
	Idx       int
}

// DeleteParagraphAudioForSpeaker deletes every paragraph_audio row - across
// every voice_id it's ever been generated under, not just whichever one
// currently resolves, same "every voice_id" scope DeleteBookAudio/
// DeleteChapterAudio already use - for paragraphs attributed to name,
// across bookIDs. This is the speaker-scoped counterpart of those two,
// used when a character's voice characterization changes (see
// httpapi.recharacterizeAndInvalidate) so their previously generated
// lines - narrated in a voice/characterization that no longer matches who
// they are - don't linger reachable (and wasting disk space) under their
// old voice_id. Reads every matching row before deleting anything so the
// caller has enough to remove the matching files, mirroring how
// ListParagraphsRaw's callers always fully drain a result set before this
// store's single DuckDB connection runs another query.
//
// name is compared with a plain equality, not ParagraphsForSpeaker's own
// "Narrator"/"" folding - this is only ever called with a real character's
// name (Narrator isn't a Character row and is never recharacterized), so
// that special case doesn't apply here.
func (s *Store) DeleteParagraphAudioForSpeaker(bookIDs []string, name string) ([]SpeakerAudioRef, error) {
	var out []SpeakerAudioRef
	for _, bookID := range bookIDs {
		rows, err := s.db.Query(`
			SELECT c.book_id, p.chapter_id, pa.voice_id, p.idx
			FROM paragraph_audio pa
			JOIN paragraphs p ON p.id = pa.paragraph_id
			JOIN chapters c ON c.id = p.chapter_id
			WHERE c.book_id = ? AND p.speaker = ?`, bookID, name)
		if err != nil {
			return out, err
		}
		var refs []SpeakerAudioRef
		for rows.Next() {
			var ref SpeakerAudioRef
			if err := rows.Scan(&ref.BookID, &ref.ChapterID, &ref.VoiceID, &ref.Idx); err != nil {
				rows.Close()
				return out, err
			}
			refs = append(refs, ref)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return out, err
		}
		rows.Close()

		if _, err := s.db.Exec(`
			DELETE FROM paragraph_audio WHERE paragraph_id IN (
				SELECT id FROM paragraphs WHERE chapter_id IN (
					SELECT id FROM chapters WHERE book_id = ?
				) AND speaker = ?
			)`, bookID, name); err != nil {
			return out, err
		}
		out = append(out, refs...)
	}
	return out, nil
}

// DeleteParagraphAudioByVoiceID deletes every paragraph_audio row within
// bookID matching voiceID exactly - the voice-preset-scoped counterpart of
// DeleteParagraphAudioForSpeaker (which filters by speaker name across
// every voice_id) for when the caller already knows the precise voice_id a
// change (e.g. re-rendering a custom preset's reference clip - see
// httpapi's "Regenerate" button on the Voices page) actually invalidates,
// rather than every voice_id a speaker identity has ever used. No speaker
// filter needed: voice_id already uniquely encodes (presetID, instruct,
// language) per store.VoiceID, so any paragraph whose audio was generated
// under this exact voice - book-level or character-level, whichever
// speaker it's attributed to - necessarily has this same voice_id, and
// nothing generated under a different voice can collide with it.
func (s *Store) DeleteParagraphAudioByVoiceID(bookID, voiceID string) ([]SpeakerAudioRef, error) {
	rows, err := s.db.Query(`
		SELECT c.book_id, p.chapter_id, pa.voice_id, p.idx
		FROM paragraph_audio pa
		JOIN paragraphs p ON p.id = pa.paragraph_id
		JOIN chapters c ON c.id = p.chapter_id
		WHERE c.book_id = ? AND pa.voice_id = ?`, bookID, voiceID)
	if err != nil {
		return nil, err
	}
	var refs []SpeakerAudioRef
	for rows.Next() {
		var ref SpeakerAudioRef
		if err := rows.Scan(&ref.BookID, &ref.ChapterID, &ref.VoiceID, &ref.Idx); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if _, err := s.db.Exec(`
		DELETE FROM paragraph_audio WHERE voice_id = ? AND paragraph_id IN (
			SELECT id FROM paragraphs WHERE chapter_id IN (
				SELECT id FROM chapters WHERE book_id = ?
			)
		)`, voiceID, bookID); err != nil {
		return refs, err
	}
	return refs, nil
}

// NarrationStats summarizes a book's text/audio so far, used to derive a
// self-calibrating narration-length estimate: once at least one paragraph
// has real generated audio, seconds-per-character for that book's voice is
// known and can be extrapolated across the rest of the (mostly ungenerated)
// text.
type NarrationStats struct {
	TotalChars   int64
	ReadyChars   int64
	ReadySeconds float64
}

// BookNarrationStats is scoped to voiceID because narration pace is
// voice-specific (different presets/instructs speak at different speeds) -
// mixing another voice's generated seconds in would miscalibrate the
// estimate for the one currently selected.
func (s *Store) BookNarrationStats(bookID, voiceID string) (NarrationStats, error) {
	var stats NarrationStats
	err := s.db.QueryRow(`
		SELECT
			COALESCE(SUM(LENGTH(p.content)), 0),
			COALESCE(SUM(CASE WHEN pa.status = 'ready' THEN LENGTH(p.content) ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN pa.status = 'ready' THEN pa.duration_seconds ELSE 0 END), 0)
		FROM paragraphs p
		JOIN chapters c ON c.id = p.chapter_id
		LEFT JOIN paragraph_audio pa ON pa.paragraph_id = p.id AND pa.voice_id = ?
		WHERE c.book_id = ?`, voiceID, bookID).
		Scan(&stats.TotalChars, &stats.ReadyChars, &stats.ReadySeconds)
	if err != nil {
		return NarrationStats{}, err
	}
	return stats, nil
}
