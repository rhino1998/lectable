-- The library's schema as it stood when migrations were introduced. Every
-- statement is IF NOT EXISTS, so applying this to a library created before
-- then changes nothing and only records it as version 1.
--
-- Never edit a migration once a library has applied it: add a new file.

-- +goose Up
-- +goose StatementBegin
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
	-- Which clone model narrates this book (every voice in it - narrator
	-- and characters alike), e.g. "audiocpp-pocket-100m" or
	-- "audiocpp-higgs-4b" (see internal/audioworker's cloneModelFamilies).
	-- A property of the book, not of any voice preset: a preset is only a
	-- reference clip recipe, cloneable through any model. Starts as
	-- default_voice.clone_model when the book is created (see CreateBook);
	-- changing it invalidates the book's generated audio (see
	-- httpapi.handleUpdateVoice).
	clone_model TEXT NOT NULL DEFAULT 'audiocpp-higgs-4b',
	-- Whether/how a character's own narration voice can override this
	-- book's own - see store.CharacterVoiceMode's own doc comment for the
	-- four possible values ('narrator'/'assigned'/'instruct_unassigned'/
	-- 'instruct_all'). Defaults to 'narrator' so an existing book's
	-- narration never changes until a reader explicitly opts into
	-- something else.
	character_voice_mode TEXT NOT NULL DEFAULT 'narrator',
	-- Gates jobs.Manager's speechDirectionDependency: while true, a
	-- paragraph clone task for this book waits on its chapter reaching
	-- passes.direction (emotion labeling) before generating - see that
	-- function's own doc comment. Defaults false (generate immediately,
	-- don't wait) - a reader opts in per book instead of every book
	-- paying an LLM-pass delay before its first audio.
	-- Flipping this false -> true invalidates the book's already-generated
	-- audio (see httpapi.handleUpdateVoice) since that audio was produced
	-- with no guarantee its chapter was ever actually tagged first.
	speech_direction BOOLEAN NOT NULL DEFAULT false,
	-- Reader-facing book-wide background-music toggle - see
	-- store.Book.MusicEnabled's own doc comment. Book-wide, not
	-- per-chapter: a chapter's own music_regions rows and passes.ambience
	-- fact (chapters.passes) still exist per-chapter regardless (scoring/
	-- generation are inherently chapter-scoped work), but whether the
	-- feature is on at all is one setting for the whole book, the same
	-- shape speech_direction above already uses. Off by default.
	music_enabled BOOLEAN NOT NULL DEFAULT false,
	-- See store.Book.StrictOrder / SettingRequired. Both off by default.
	strict_order BOOLEAN NOT NULL DEFAULT false,
	setting_required BOOLEAN NOT NULL DEFAULT false,
	pos_chapter_idx INTEGER NOT NULL DEFAULT 0,
	pos_paragraph_idx INTEGER NOT NULL DEFAULT 0,
	pos_seconds DOUBLE NOT NULL DEFAULT 0,
	-- When the reading position was last saved (unix seconds, 0 = never
	-- opened) - what the libraries' "Continue reading" entry picks by.
	last_read_at BIGINT NOT NULL DEFAULT 0,
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

-- One chapter's summary-pass notes (store.ChapterNotes, JSON) - see
-- speakerattr.Client.SummarizeChapter. No PRIMARY KEY (SetChapterNotes
-- deletes then inserts), like book_settings.
CREATE TABLE IF NOT EXISTS chapter_summaries (
	chapter_id TEXT NOT NULL,
	summary JSON NOT NULL,
	updated_at BIGINT NOT NULL
);

-- One book's setting profile (store.BookSetting, JSON) - see
-- speakerattr.Client.DescribeSetting. No PRIMARY KEY (SetBookSetting
-- deletes then inserts), like sync_tree_nodes.
CREATE TABLE IF NOT EXISTS book_settings (
	book_id TEXT NOT NULL,
	setting JSON NOT NULL,
	source TEXT NOT NULL,
	updated_at BIGINT NOT NULL
);

-- A reader's manual corrections of machine-labeled paragraph fields
-- (store.ParagraphEdit): field is 'speaker', 'emotion' or 'scare_quote'
-- ('true'/'false'), original what the
-- pass had written before the first edit, value the reader's latest. One
-- row per (paragraph_id, field), kept by RecordParagraphEdit's
-- select-then-write, no PRIMARY KEY - like chapter_summaries.
CREATE TABLE IF NOT EXISTS paragraph_edits (
	paragraph_id TEXT NOT NULL,
	field TEXT NOT NULL,
	original TEXT NOT NULL,
	value TEXT NOT NULL,
	edited_at BIGINT NOT NULL
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
	-- counts, paragraphs.emotion presence) - a chapter with no dialogue at
	-- all can legitimately finish attribution with some/all of its
	-- narration paragraphs still carrying "" (a model that drops a
	-- paragraph's own JSON entry, or a blank-text paragraph
	-- indistinguishable from the "Lines:" blank-line paragraph-break marker
	-- itself - see speakerattr.attributeBatch), and most paragraphs
	-- legitimately get no emotion at all (neutral is the emotion pass's
	-- own default) - re-deriving "was this chapter
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
	-- Declared JSON (see Passes' own Scan/Value methods), like
	-- paragraphs.describes_characters/tts_tags/pronunciation below: SQLite
	-- stores it as text, and the declared type is what tells row export
	-- (SQLiteStore.rowJSON) to nest the value rather than quote it.
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
	-- Which of several unnamed people sharing a role label this line is
	-- (attribution's "Guard 2"), 0 for none - see Paragraph.SpeakerIndex.
	speaker_index INTEGER NOT NULL DEFAULT 0,
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
	-- Legacy: per-clone-model Higgs delivery tags from the LLM tagging
	-- passes the emotion pass replaced. Never applied any more; only
	-- cleared (Store.ClearLegacyTags) as each chapter is re-labeled, so
	-- audio generated with them gets invalidated once.
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
	-- This paragraph's delivery emotion (an internal/emotions id), or ''
	-- for neutral - see Paragraph.Emotion/EffectiveEmotion.
	emotion TEXT NOT NULL DEFAULT '',
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
	invalid BOOLEAN NOT NULL DEFAULT false,
	-- Other names this character goes by - see Character.Aliases.
	aliases JSON NOT NULL DEFAULT '[]',
	-- "he/him", "she/her", "they/them", "it/its", or '' - see
	-- Character.Pronouns.
	pronouns TEXT NOT NULL DEFAULT '',
	created_at BIGINT NOT NULL,
	UNIQUE(scope, name)
);

-- A character's assigned voice, one row per (character, clone_model) -
-- separate from characters' identity/summary because a voice cast for one
-- clone model (the book's own books.clone_model) isn't necessarily a good
-- fit under another; the same character can end up with a
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
-- Listener-authored pronunciation rules (store.LexiconEntry), scoped like
-- characters (SeriesScope). term_key is lower(term): one rule per word per
-- scope, whatever its case.
CREATE TABLE IF NOT EXISTS pronunciation_lexicon (
	id TEXT PRIMARY KEY,
	scope TEXT NOT NULL,
	term_key TEXT NOT NULL,
	term TEXT NOT NULL,
	replacement TEXT NOT NULL,
	case_sensitive BOOLEAN NOT NULL DEFAULT false,
	prefix BOOLEAN NOT NULL DEFAULT false,
	created_at BIGINT NOT NULL,
	UNIQUE (scope, term_key)
);

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
	-- Copies of the paragraph's own chapter_id and length(content), filled
	-- when the row is inserted (neither ever changes for a paragraph), so
	-- the single-voice aggregates and AudioRefs read idx_paragraph_audio_chapter
	-- alone instead of joining every row to its paragraph. '' / 0 on a row
	-- whose paragraph didn't exist at insert.
	chapter_id TEXT NOT NULL DEFAULT '',
	chars INTEGER NOT NULL DEFAULT 0,
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
	-- Ambient-soundscape prompt mixed under the music, '' for none - see
	-- store.MusicRegion.Ambience.
	ambience TEXT NOT NULL DEFAULT '',
	-- 'cut' or 'continuation' - see store.MusicTransition's own doc comment.
	transition TEXT NOT NULL DEFAULT 'cut',
	-- store.Lighting, NULL until the ambience pass's lighting sub-pass
	-- reaches this region.
	lighting JSON,
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
	-- Which VoiceDesign engine renders this preset's reference clip, e.g.
	-- "qwen3_tts" or "breeze_tts" (see internal/audioworker's
	-- designEngines) - "" defers to the worker's own configured default,
	-- but new presets are created with an explicit value
	-- (voices.DefaultDesignModel) so a preset's sound doesn't silently
	-- change if the worker's default is later changed. No clone model
	-- here: which model clones a preset's clip is the book's choice
	-- (books.clone_model), not the preset's.
	design_model TEXT NOT NULL DEFAULT 'breeze_tts'
);

-- Singleton (always exactly the id=0 row - see Open) holding what new books
-- are created with - voice, clone model and the per-book switches -
-- settable from the Voices page rather than only per book.
CREATE TABLE IF NOT EXISTS default_voice (
	id INTEGER PRIMARY KEY,
	preset_id TEXT NOT NULL DEFAULT 'velvet-narrator',
	instruct TEXT NOT NULL DEFAULT '',
	language TEXT NOT NULL DEFAULT 'Auto',
	seed INTEGER NOT NULL DEFAULT 1006,
	clone_model TEXT NOT NULL DEFAULT 'audiocpp-higgs-4b',
	-- The per-book switches a new book starts with (store.BookDefaults) -
	-- the books columns of the same names.
	character_voice_mode TEXT NOT NULL DEFAULT 'narrator',
	speech_direction BOOLEAN NOT NULL DEFAULT false,
	music_enabled BOOLEAN NOT NULL DEFAULT false,
	strict_order BOOLEAN NOT NULL DEFAULT false,
	setting_required BOOLEAN NOT NULL DEFAULT false
);

-- The offline-sync hash tree's stored chapter nodes (see
-- httpapi/manifest.go): the hash httpapi built for one chapter, keyed by
-- the fingerprints (SyncFingerprints) of the rows it was built from and
-- the build of the code that hashed it (version), so a later manifest
-- request can reuse it instead of rebuilding the chapter. A derived cache,
-- never a source of truth - any row may be deleted at any time. One row per
-- (book_id, chapter_idx), kept by PutSyncTreeNodes' update-then-insert
-- rather than a PRIMARY KEY: no index to go wrong on a table this small.
CREATE TABLE IF NOT EXISTS sync_tree_nodes (
	book_id TEXT NOT NULL,
	chapter_idx INTEGER NOT NULL,
	version TEXT NOT NULL,
	book_fp TEXT NOT NULL,
	chapter_fp TEXT NOT NULL,
	hash TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_chapters_book ON chapters(book_id);
CREATE INDEX IF NOT EXISTS idx_paragraphs_chapter ON paragraphs(chapter_id);
-- Covering indexes for the whole-book aggregates (narration stats, chapter
-- summaries, ready counts by speaker): everything those read per paragraph
-- and per audio row, so they never touch the wide table rows (content,
-- word_timings).
CREATE INDEX IF NOT EXISTS idx_paragraphs_stats ON paragraphs(chapter_id, id, speaker, speaker_index, length(content));
CREATE INDEX IF NOT EXISTS idx_paragraph_audio_stats ON paragraph_audio(paragraph_id, voice_id, status, duration_seconds);
CREATE INDEX IF NOT EXISTS idx_paragraph_audio_chapter ON paragraph_audio(chapter_id, voice_id, status, duration_seconds, chars);

-- A deleted paragraph takes its audio rows with it, whichever path deleted
-- it: paragraph_audio.chapter_id is only trustworthy while every row with
-- one still has its paragraph.
CREATE TRIGGER IF NOT EXISTS paragraphs_audio_delete AFTER DELETE ON paragraphs BEGIN
	DELETE FROM paragraph_audio WHERE paragraph_id = old.id;
END;
CREATE INDEX IF NOT EXISTS idx_images_chapter ON images(chapter_id);
CREATE INDEX IF NOT EXISTS idx_bookmarks_paragraph ON bookmarks(paragraph_id);
CREATE INDEX IF NOT EXISTS idx_paragraph_edits_paragraph ON paragraph_edits(paragraph_id);
CREATE INDEX IF NOT EXISTS idx_characters_scope ON characters(scope);
CREATE INDEX IF NOT EXISTS idx_character_voices_character ON character_voices(character_id);
CREATE INDEX IF NOT EXISTS idx_paragraphs_speaker ON paragraphs(speaker);

-- Full-text index over paragraphs.content for SearchParagraphs: trigrams,
-- so a query matches any substring of three or more characters,
-- case-insensitively. External-content (the text lives only in paragraphs,
-- joined by rowid) and kept current by the triggers below. Needs the
-- sqlite_fts5 build tag.
CREATE VIRTUAL TABLE IF NOT EXISTS paragraphs_fts USING fts5(content, content='paragraphs', tokenize='trigram');

CREATE TRIGGER IF NOT EXISTS paragraphs_fts_insert AFTER INSERT ON paragraphs BEGIN
	INSERT INTO paragraphs_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS paragraphs_fts_delete AFTER DELETE ON paragraphs BEGIN
	INSERT INTO paragraphs_fts(paragraphs_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS paragraphs_fts_update AFTER UPDATE OF content ON paragraphs BEGIN
	INSERT INTO paragraphs_fts(paragraphs_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
	INSERT INTO paragraphs_fts(rowid, content) VALUES (new.rowid, new.content);
END;
-- +goose StatementEnd
