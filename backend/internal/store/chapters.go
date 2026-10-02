package store

import (
	"database/sql"
	"strings"
)

// SpeakerVoice pairs a paragraph's speaker attribution with its own
// resolved voice_id, letting ListChapterSummaries/BookNarrationStats count
// readiness per-paragraph-voice instead of assuming every paragraph in the
// book shares its one book-level voice - true only when Book.MultiVoice is
// off or no character has an assigned voice yet. store can't resolve this
// itself (internal/narration, which owns that resolution chain - built-in
// vs. custom presets, CharacterVoiceMode fallbacks, per-clone-model
// assignment - already imports store, so the reverse would cycle); the
// caller (httpapi) resolves it once per character and passes the result
// in. A paragraph whose speaker isn't in the list (empty, "Narrator", or
// simply unmatched) falls back to the book's own voiceID, same as before
// this existed. Empty overrides (the common, non-multi-voice case) skips
// the extra JOIN below entirely and costs nothing over the old query.
type SpeakerVoice struct {
	Speaker string
	VoiceID string
}

// speakerVoiceValues builds a `VALUES (?, ?), (?, ?), ...` fragment plus
// its matching args, mirroring bookVoiceValues below but keyed by speaker
// name instead of book id.
func speakerVoiceValues(overrides []SpeakerVoice) (fragment string, args []any) {
	rows := make([]string, len(overrides))
	args = make([]any, 0, len(overrides)*2)
	for i, ov := range overrides {
		rows[i] = "(?, ?)"
		args = append(args, ov.Speaker, ov.VoiceID)
	}
	return strings.Join(rows, ", "), args
}

// ListChapterSummaries reports, per chapter, how many of its paragraphs
// have ready audio *for voiceID specifically* (or, for a paragraph whose
// speaker appears in overrides, for that speaker's own resolved voice
// instead - see SpeakerVoice) - a chapter fully generated under one voice
// shows as not-yet-generated when a different voice is selected, since
// that voice's audio doesn't exist yet.
func (s *DuckStore) ListChapterSummaries(bookID, voiceID string, overrides []SpeakerVoice) ([]ChapterSummary, error) {
	var rows *sql.Rows
	var err error
	if len(overrides) == 0 {
		rows, err = s.db.Query(`
			SELECT c.id, c.book_id, c.idx, c.title, c.passes,
				COUNT(p.id) AS total,
				COALESCE(SUM(CASE WHEN pa.status = 'ready' THEN 1 ELSE 0 END), 0) AS ready
			FROM chapters c
			LEFT JOIN paragraphs p ON p.chapter_id = c.id
			LEFT JOIN paragraph_audio pa ON pa.paragraph_id = p.id AND pa.voice_id = ?
			WHERE c.book_id = ?
			GROUP BY c.id, c.book_id, c.idx, c.title, c.passes
			ORDER BY c.idx ASC`, voiceID, bookID)
	} else {
		valuesFragment, valuesArgs := speakerVoiceValues(overrides)
		args := append(append([]any{}, valuesArgs...), voiceID, bookID)
		rows, err = s.db.Query(`
			WITH speaker_voice(speaker, voice_id) AS (VALUES `+valuesFragment+`)
			SELECT c.id, c.book_id, c.idx, c.title, c.passes,
				COUNT(p.id) AS total,
				COALESCE(SUM(CASE WHEN pa.status = 'ready' THEN 1 ELSE 0 END), 0) AS ready
			FROM chapters c
			LEFT JOIN paragraphs p ON p.chapter_id = c.id
			LEFT JOIN speaker_voice sv ON sv.speaker = p.speaker
			LEFT JOIN paragraph_audio pa ON pa.paragraph_id = p.id AND pa.voice_id = COALESCE(sv.voice_id, ?)
			WHERE c.book_id = ?
			GROUP BY c.id, c.book_id, c.idx, c.title, c.passes
			ORDER BY c.idx ASC`, args...)
	}
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

// BookSpeakerVoice is BookVoice's per-character counterpart for the batch
// queries below - SpeakerVoice plus a BookID, since speaker names aren't
// globally unique across an unrelated book/series.
type BookSpeakerVoice struct {
	BookID  string
	Speaker string
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

// bookSpeakerVoiceValues is bookVoiceValues' per-character counterpart,
// building a `VALUES (?, ?, ?), ...` fragment (book_id, speaker, voice_id)
// for the batch queries' own speaker_voice CTE.
func bookSpeakerVoiceValues(overrides []BookSpeakerVoice) (fragment string, args []any) {
	rows := make([]string, len(overrides))
	args = make([]any, 0, len(overrides)*3)
	for i, ov := range overrides {
		rows[i] = "(?, ?, ?)"
		args = append(args, ov.BookID, ov.Speaker, ov.VoiceID)
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
// overrides is BookSpeakerVoice's flattened form (see SpeakerVoice's own
// doc comment) - empty for a book with no multi-voice character
// assignments, so most libraries pay nothing extra over the old query.
func (s *DuckStore) ListChapterSummariesForBooks(bookVoices []BookVoice, overrides []BookSpeakerVoice) (map[string][]ChapterSummary, error) {
	out := make(map[string][]ChapterSummary, len(bookVoices))
	if len(bookVoices) == 0 {
		return out, nil
	}
	bvFragment, bvArgs := bookVoiceValues(bookVoices)

	var rows *sql.Rows
	var err error
	if len(overrides) == 0 {
		rows, err = s.db.Query(`
			WITH book_voice(book_id, voice_id) AS (VALUES `+bvFragment+`)
			SELECT c.id, c.book_id, c.idx, c.title, c.passes,
				COUNT(p.id) AS total,
				COALESCE(SUM(CASE WHEN pa.status = 'ready' THEN 1 ELSE 0 END), 0) AS ready
			FROM chapters c
			JOIN book_voice bv ON bv.book_id = c.book_id
			LEFT JOIN paragraphs p ON p.chapter_id = c.id
			LEFT JOIN paragraph_audio pa ON pa.paragraph_id = p.id AND pa.voice_id = bv.voice_id
			GROUP BY c.id, c.book_id, c.idx, c.title, c.passes
			ORDER BY c.book_id, c.idx ASC`, bvArgs...)
	} else {
		svFragment, svArgs := bookSpeakerVoiceValues(overrides)
		args := append(append([]any{}, bvArgs...), svArgs...)
		rows, err = s.db.Query(`
			WITH book_voice(book_id, voice_id) AS (VALUES `+bvFragment+`),
				speaker_voice(book_id, speaker, voice_id) AS (VALUES `+svFragment+`)
			SELECT c.id, c.book_id, c.idx, c.title, c.passes,
				COUNT(p.id) AS total,
				COALESCE(SUM(CASE WHEN pa.status = 'ready' THEN 1 ELSE 0 END), 0) AS ready
			FROM chapters c
			JOIN book_voice bv ON bv.book_id = c.book_id
			LEFT JOIN paragraphs p ON p.chapter_id = c.id
			LEFT JOIN speaker_voice sv ON sv.book_id = c.book_id AND sv.speaker = p.speaker
			LEFT JOIN paragraph_audio pa ON pa.paragraph_id = p.id AND pa.voice_id = COALESCE(sv.voice_id, bv.voice_id)
			GROUP BY c.id, c.book_id, c.idx, c.title, c.passes
			ORDER BY c.book_id, c.idx ASC`, args...)
	}
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

func (s *DuckStore) GetChapterByID(id string) (*Chapter, error) {
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

// CountChapters reports how many chapters bookID has.
func (s *DuckStore) CountChapters(bookID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM chapters WHERE book_id = ?`, bookID).Scan(&n)
	return n, err
}

func (s *DuckStore) GetChapterByIdx(bookID string, idx int) (*Chapter, error) {
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

// ListParagraphsRaw returns a chapter's paragraphs (structural fields plus
// Speaker) with no per-voice audio status - since a character's assigned
// voice can now override the book's own per paragraph (see
// internal/narration.Resolver), different paragraphs in the same chapter
// can resolve to different voice_ids, so there's no longer one single
// voice_id a JOIN here could filter on. Callers resolve each paragraph's
// applicable voice_id themselves and batch-fetch status via
// ParagraphAudioStatuses.
func (s *DuckStore) ListParagraphsRaw(chapterID string) ([]Paragraph, error) {
	rows, err := s.db.Query(`SELECT id, chapter_id, idx, position, content, speaker, speaker_index, inline, is_quote, scare_quote, describes_characters, emotion, pronunciation, emphasis FROM paragraphs WHERE chapter_id = ? ORDER BY idx ASC`, chapterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Paragraph
	for rows.Next() {
		var p Paragraph
		if err := rows.Scan(&p.ID, &p.ChapterID, &p.Idx, &p.Position, &p.Text, &p.Speaker, &p.SpeakerIndex, &p.Inline, &p.IsQuote, &p.ScareQuote, &p.DescribesCharacters, &p.Emotion, &p.Pronunciation, &p.Emphasis); err != nil {
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
func (s *DuckStore) ListParagraphsRawForBook(bookID string) ([]Paragraph, error) {
	rows, err := s.db.Query(`
		SELECT p.id, p.chapter_id, p.idx, p.position, p.content, p.speaker, p.speaker_index, p.inline, p.is_quote, p.scare_quote, p.emotion
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
		if err := rows.Scan(&p.ID, &p.ChapterID, &p.Idx, &p.Position, &p.Text, &p.Speaker, &p.SpeakerIndex, &p.Inline, &p.IsQuote, &p.ScareQuote, &p.Emotion); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetParagraph is a structural, voice-agnostic lookup (id/chapter/position/
// text/speaker only) - used where the caller needs to know which chapter
// (and, via Speaker, which voice - see internal/narration.Resolver) a
// paragraph belongs to before it can even determine which voice applies.
func (s *DuckStore) GetParagraph(id string) (*Paragraph, error) {
	var p Paragraph
	err := s.db.QueryRow(`SELECT id, chapter_id, idx, position, content, speaker, speaker_index, inline, is_quote, scare_quote, describes_characters, emotion, pronunciation, emphasis FROM paragraphs WHERE id = ?`, id).
		Scan(&p.ID, &p.ChapterID, &p.Idx, &p.Position, &p.Text, &p.Speaker, &p.SpeakerIndex, &p.Inline, &p.IsQuote, &p.ScareQuote, &p.DescribesCharacters, &p.Emotion, &p.Pronunciation, &p.Emphasis)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetParagraphIDByIdx resolves a paragraph's stable id from its (chapter,
// idx) position - the public API addresses paragraphs this way (same as
// search results), so bookmark handlers need this to get to the id the
// bookmarks table actually keys on. Empty string, nil error if not found.
func (s *DuckStore) GetParagraphIDByIdx(chapterID string, idx int) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM paragraphs WHERE chapter_id = ? AND idx = ?`, chapterID, idx).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// ListImages returns a chapter's inline images ordered by their position
// among the chapter's content blocks.
func (s *DuckStore) ListImages(chapterID string) ([]Image, error) {
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
func (s *DuckStore) ListBreaks(chapterID string) ([]Break, error) {
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

func (s *DuckStore) GetImage(id string) (*Image, error) {
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
