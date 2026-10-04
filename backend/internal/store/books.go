package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
)

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
func (s *SQLiteStore) CreateBook(title, author, language string, coverExt string, seriesName string, seriesIndex float64, chapters []ChapterInput) (bookID string, images []CreatedImage, err error) {
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
		`INSERT INTO books (id, title, author, language, cover_ext, series_name, series_index, added_at, voice_preset_id, voice_instruct, voice_language, voice_seed, clone_model)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		bookID, title, author, language, coverExt, seriesName, seriesIndex, time.Now().Unix(),
		defaultVoice.PresetID, defaultVoice.Instruct, defaultVoice.Language, defaultVoice.Seed, defaultVoice.CloneModel,
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

		chImages, err := insertChapterBlocks(tx, chapterID, ch.Blocks)
		if err != nil {
			return "", nil, err
		}
		images = append(images, chImages...)
	}

	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return bookID, images, nil
}

// insertChapterBlocks inserts one chapter's parsed blocks (paragraphs, image
// placeholders, scene breaks) under chapterID - CreateBook's per-chapter
// body, shared with ReplaceChapterContent. Returned images are in block
// order, for the caller to write each one's bytes to disk under its ID.
func insertChapterBlocks(tx *notifyTx, chapterID string, blocks []BlockInput) (images []CreatedImage, err error) {
	paragraphIdx := 0
	for position, block := range blocks {
		switch block.Kind {
		case BlockImage:
			imageID := NewID()
			_, err = tx.Exec(
				`INSERT INTO images (id, chapter_id, position, ext) VALUES (?, ?, ?, ?)`,
				imageID, chapterID, position, block.Ext,
			)
			if err != nil {
				return nil, fmt.Errorf("insert image: %w", err)
			}
			images = append(images, CreatedImage{ChapterID: chapterID, ImageID: imageID, Ext: block.Ext})
		case BlockBreak:
			_, err = tx.Exec(
				`INSERT INTO breaks (id, chapter_id, position) VALUES (?, ?, ?)`,
				NewID(), chapterID, position,
			)
			if err != nil {
				return nil, fmt.Errorf("insert break: %w", err)
			}
		default:
			emphasis := block.Emphasis
			if emphasis == nil {
				emphasis = []pronounce.Substitution{}
			}
			encodedEmphasis, err := PronunciationList(emphasis).Value()
			if err != nil {
				return nil, fmt.Errorf("encode emphasis: %w", err)
			}
			_, err = tx.Exec(
				`INSERT INTO paragraphs (id, chapter_id, idx, position, content, inline, is_quote, emphasis) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				NewID(), chapterID, paragraphIdx, position, block.Text, block.Inline, block.IsQuote, encodedEmphasis,
			)
			if err != nil {
				return nil, fmt.Errorf("insert paragraph: %w", err)
			}
			paragraphIdx++
		}
	}
	return images, nil
}

// ReplaceChapterContent resets one chapter to freshly parsed content - the
// store half of re-importing a single chapter from its source epub
// (httpapi's handleReimportChapter), so a parser fix can be applied to the
// chapters that need it without deleting the whole book and losing every
// other chapter's attribution, manual fixes, and generated audio.
//
// Keeps the chapter's own row (ID, idx), so anything keyed on it - job
// dedup keys, audio/image directory paths, the book's reading position -
// still points at the same chapter. Everything derived from the old
// paragraphs is dropped: paragraph rows (speakers, emotions, pronunciation,
// scare-quote flags), their narration/SFX audio rows, music regions, image
// placeholders, scene breaks, the chapter's cached sync-tree node, and
// every pass flag, so the preprocess pipeline redoes this chapter from
// scratch. The character roster is untouched - it's series-wide, and a
// character left with no lines is harmless.
//
// Bookmarks survive, re-pointed at whichever new paragraph now has the old
// one's idx (clamped to the chapter's new last paragraph) - the text there
// may differ slightly, but losing a reader's bookmark outright is worse.
//
// Returns the old image IDs/exts (for the caller to delete from disk) and
// the newly created ones (for the caller to write).
func (s *SQLiteStore) ReplaceChapterContent(chapterID, title string, blocks []BlockInput) (oldImages, newImages []CreatedImage, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()

	var bookID string
	var chapterIdx int
	if err := tx.QueryRow(`SELECT book_id, idx FROM chapters WHERE id = ?`, chapterID).Scan(&bookID, &chapterIdx); err != nil {
		return nil, nil, fmt.Errorf("look up chapter: %w", err)
	}

	type savedBookmark struct {
		id, note  string
		createdAt int64
		idx       int
	}
	var bookmarks []savedBookmark
	rows, err := tx.Query(`SELECT b.id, b.note, b.created_at, p.idx FROM bookmarks b JOIN paragraphs p ON p.id = b.paragraph_id WHERE p.chapter_id = ? ORDER BY p.idx`, chapterID)
	if err != nil {
		return nil, nil, fmt.Errorf("list bookmarks: %w", err)
	}
	for rows.Next() {
		var b savedBookmark
		if err := rows.Scan(&b.id, &b.note, &b.createdAt, &b.idx); err != nil {
			rows.Close()
			return nil, nil, err
		}
		bookmarks = append(bookmarks, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	rows, err = tx.Query(`SELECT id, ext FROM images WHERE chapter_id = ?`, chapterID)
	if err != nil {
		return nil, nil, fmt.Errorf("list images: %w", err)
	}
	for rows.Next() {
		img := CreatedImage{ChapterID: chapterID}
		if err := rows.Scan(&img.ImageID, &img.Ext); err != nil {
			rows.Close()
			return nil, nil, err
		}
		oldImages = append(oldImages, img)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	for _, stmt := range []struct{ what, sql string }{
		{"bookmarks", `DELETE FROM bookmarks WHERE paragraph_id IN (SELECT id FROM paragraphs WHERE chapter_id = ?)`},
		{"paragraph edits", `DELETE FROM paragraph_edits WHERE paragraph_id IN (SELECT id FROM paragraphs WHERE chapter_id = ?)`},
		{"paragraph audio", `DELETE FROM paragraph_audio WHERE paragraph_id IN (SELECT id FROM paragraphs WHERE chapter_id = ?)`},
		{"sfx", `DELETE FROM sfx WHERE paragraph_id IN (SELECT id FROM paragraphs WHERE chapter_id = ?)`},
		{"paragraphs", `DELETE FROM paragraphs WHERE chapter_id = ?`},
		{"images", `DELETE FROM images WHERE chapter_id = ?`},
		{"breaks", `DELETE FROM breaks WHERE chapter_id = ?`},
		{"music regions", `DELETE FROM music_regions WHERE chapter_id = ?`},
		{"summary", `DELETE FROM chapter_summaries WHERE chapter_id = ?`},
	} {
		if _, err := tx.Exec(stmt.sql, chapterID); err != nil {
			return nil, nil, fmt.Errorf("delete %s: %w", stmt.what, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM sync_tree_nodes WHERE book_id = ? AND chapter_idx = ?`, bookID, chapterIdx); err != nil {
		return nil, nil, fmt.Errorf("delete sync tree node: %w", err)
	}
	if _, err := tx.Exec(`UPDATE chapters SET title = ?, passes = '{}' WHERE id = ?`, title, chapterID); err != nil {
		return nil, nil, fmt.Errorf("reset chapter: %w", err)
	}

	newImages, err = insertChapterBlocks(tx, chapterID, blocks)
	if err != nil {
		return nil, nil, err
	}

	if len(bookmarks) > 0 {
		var newIDs []string
		rows, err := tx.Query(`SELECT id FROM paragraphs WHERE chapter_id = ? ORDER BY idx`, chapterID)
		if err != nil {
			return nil, nil, fmt.Errorf("list new paragraphs: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, nil, err
			}
			newIDs = append(newIDs, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		taken := map[string]bool{}
		for _, b := range bookmarks {
			if len(newIDs) == 0 {
				break
			}
			pid := newIDs[min(b.idx, len(newIDs)-1)]
			if taken[pid] {
				continue // bookmarks.paragraph_id is UNIQUE - two old bookmarks clamped onto one paragraph keep the first
			}
			taken[pid] = true
			if _, err := tx.Exec(`INSERT INTO bookmarks (id, paragraph_id, note, created_at) VALUES (?, ?, ?, ?)`, b.id, pid, b.note, b.createdAt); err != nil {
				return nil, nil, fmt.Errorf("restore bookmark: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return oldImages, newImages, nil
}

func (s *SQLiteStore) ListBooks() ([]Book, error) {
	rows, err := s.db.Query(`SELECT id, title, author, language, cover_ext, series_name, series_index, added_at,
		voice_preset_id, voice_instruct, voice_language, voice_seed, clone_model, character_voice_mode, speech_direction, music_enabled,
		pos_chapter_idx, pos_paragraph_idx, pos_seconds, last_read_at, estimate_sec_per_char
		FROM books ORDER BY added_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Book
	for rows.Next() {
		var b Book
		if err := rows.Scan(&b.ID, &b.Title, &b.Author, &b.Language, &b.CoverExt, &b.SeriesName, &b.SeriesIndex, &b.AddedAt,
			&b.VoicePresetID, &b.VoiceInstruct, &b.VoiceLanguage, &b.VoiceSeed, &b.CloneModel, &b.CharacterVoiceMode, &b.SpeechDirection, &b.MusicEnabled,
			&b.PosChapterIdx, &b.PosParagraphIdx, &b.PosSeconds, &b.LastReadAt, &b.EstimateSecPerChar); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) GetBook(id string) (*Book, error) {
	var b Book
	err := s.db.QueryRow(`SELECT id, title, author, language, cover_ext, series_name, series_index, added_at,
		voice_preset_id, voice_instruct, voice_language, voice_seed, clone_model, character_voice_mode, speech_direction, music_enabled,
		pos_chapter_idx, pos_paragraph_idx, pos_seconds, last_read_at, estimate_sec_per_char
		FROM books WHERE id = ?`, id).Scan(
		&b.ID, &b.Title, &b.Author, &b.Language, &b.CoverExt, &b.SeriesName, &b.SeriesIndex, &b.AddedAt,
		&b.VoicePresetID, &b.VoiceInstruct, &b.VoiceLanguage, &b.VoiceSeed, &b.CloneModel, &b.CharacterVoiceMode, &b.SpeechDirection, &b.MusicEnabled,
		&b.PosChapterIdx, &b.PosParagraphIdx, &b.PosSeconds, &b.LastReadAt, &b.EstimateSecPerChar)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *SQLiteStore) DeleteBook(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM bookmarks WHERE paragraph_id IN (SELECT id FROM paragraphs WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ?))`, id); err != nil {
		return fmt.Errorf("delete bookmarks: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM paragraph_edits WHERE paragraph_id IN (SELECT id FROM paragraphs WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ?))`, id); err != nil {
		return fmt.Errorf("delete paragraph edits: %w", err)
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
	if _, err := tx.Exec(`DELETE FROM chapter_summaries WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, id); err != nil {
		return fmt.Errorf("delete chapter summaries: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM chapters WHERE book_id = ?`, id); err != nil {
		return fmt.Errorf("delete chapters: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM sync_tree_nodes WHERE book_id = ?`, id); err != nil {
		return fmt.Errorf("delete sync tree: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM book_settings WHERE book_id = ?`, id); err != nil {
		return fmt.Errorf("delete book setting: %w", err)
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
// cloneModel sets Book.CloneModel, which every voice in the book (narrator
// and characters alike) clones through.
// characterVoiceMode sets store.Book.CharacterVoiceMode - see its own doc
// comment for the four possible values - independent of presetID/instruct/
// language/seed, which remain the fallback voice regardless of mode.
func (s *SQLiteStore) UpdateVoice(bookID, presetID, instruct, language string, seed int, cloneModel string, characterVoiceMode CharacterVoiceMode, speechDirection, musicEnabled bool) error {
	_, err := s.db.Exec(
		`UPDATE books SET voice_preset_id = ?, voice_instruct = ?, voice_language = ?, voice_seed = ?, clone_model = ?, character_voice_mode = ?, speech_direction = ?, music_enabled = ? WHERE id = ?`,
		presetID, instruct, language, seed, cloneModel, characterVoiceMode, speechDirection, musicEnabled, bookID,
	)
	return err
}

// SetLengthEstimate stores bookID's own PocketTTS-calibrated seconds-per-
// character measurement - see Book.EstimateSecPerChar's own doc comment.
func (s *SQLiteStore) SetLengthEstimate(bookID string, secPerChar float64) error {
	_, err := s.db.Exec(`UPDATE books SET estimate_sec_per_char = ? WHERE id = ?`, secPerChar, bookID)
	return err
}

// UpdatePosition reports its write to OnChange as PositionTable rather than
// "books" - see PositionTable. It also stamps the book as just read
// (Book.LastReadAt).
func (s *SQLiteStore) UpdatePosition(bookID string, chapterIdx, paragraphIdx int, seconds float64) error {
	_, err := s.db.execAs(PositionTable,
		`UPDATE books SET pos_chapter_idx = ?, pos_paragraph_idx = ?, pos_seconds = ?, last_read_at = ? WHERE id = ?`,
		chapterIdx, paragraphIdx, seconds, time.Now().Unix(), bookID,
	)
	return err
}

// ListSeriesBooks returns every book sharing seriesName (non-empty), in
// series order - used to gather a recurring character's dialogue across a
// whole series (see QuotesForBooks) and to keep attribution/voice casting
// consistent throughout it.
func (s *SQLiteStore) ListSeriesBooks(seriesName string) ([]Book, error) {
	rows, err := s.db.Query(`SELECT id, title, author, language, cover_ext, series_name, series_index, added_at,
		voice_preset_id, voice_instruct, voice_language, voice_seed, clone_model, character_voice_mode, speech_direction,
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
			&b.VoicePresetID, &b.VoiceInstruct, &b.VoiceLanguage, &b.VoiceSeed, &b.CloneModel, &b.CharacterVoiceMode, &b.SpeechDirection,
			&b.PosChapterIdx, &b.PosParagraphIdx, &b.PosSeconds, &b.EstimateSecPerChar); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
