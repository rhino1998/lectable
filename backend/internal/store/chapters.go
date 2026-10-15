package store

import (
	"database/sql"
	"strconv"
	"strings"
)

// SpeakerVoice pairs a paragraph's speaker attribution with its own
// resolved voice_id, letting ListChapterSummaries/ChapterNarration count
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
//
// SpeakerIndex 0 is the speaker's own voice. A row with SpeakerIndex n > 0
// is the voice of that speaker's lines numbered n (Paragraph.SpeakerIndex -
// several unnamed people sharing a role); a numbered line with no such row
// takes the speaker's own voice.
//
// FromIdx and ToIdx limit a row to the chapters from index FromIdx up to
// but not including ToIdx - a character whose voice changes over the book
// (CharacterStage) has one row per stretch. A zero ToIdx is no limit.
type SpeakerVoice struct {
	Speaker      string
	SpeakerIndex int
	VoiceID      string
	FromIdx      int
	ToIdx        int
}

// MaxSpeakerIndex is the highest Paragraph.SpeakerIndex ever stored (see
// speakerattr.MaxRoleIndex) - how many numbered voices a role can need.
const MaxSpeakerIndex = 6

// speakerVoices answers which voice a line is read in, from a book's
// voice and its SpeakerVoice overrides - SpeakerVoice's rules, applied in
// Go to rows grouped by speaker (readyByVoice) rather than as a join.
// The join had the overrides in the statement: five bound values apiece
// ran a series of a few thousand characters past SQLite's variable limit,
// and passing them as one JSON value left the planner to choose between
// an index and a scan of them all per paragraph.
type speakerVoices struct {
	bookVoice string
	by        map[speakerKey][]SpeakerVoice
}

type speakerKey struct {
	speaker string
	index   int
}

func newSpeakerVoices(bookVoice string, overrides []SpeakerVoice) speakerVoices {
	v := speakerVoices{bookVoice: bookVoice, by: make(map[speakerKey][]SpeakerVoice, len(overrides))}
	for _, ov := range overrides {
		k := speakerKey{ov.Speaker, ov.SpeakerIndex}
		v.by[k] = append(v.by[k], ov)
	}
	return v
}

// reads reports whether voiceID is the voice of speaker's lines numbered
// index in the chapter at chapterIdx.
func (v speakerVoices) reads(speaker string, index, chapterIdx int, voiceID string) bool {
	in := func(index int) (found, match bool) {
		for _, ov := range v.by[speakerKey{speaker, index}] {
			if chapterIdx < ov.FromIdx || (ov.ToIdx != 0 && chapterIdx >= ov.ToIdx) {
				continue
			}
			found = true
			match = match || ov.VoiceID == voiceID
		}
		return found, match
	}
	if index > 0 {
		if found, match := in(index); found {
			return match
		}
	}
	if found, match := in(0); found {
		return match
	}
	return voiceID == v.bookVoice
}

// readyGroup is the ready audio of one chapter's paragraphs that share a
// speaker and were generated under one voice.
type readyGroup struct {
	chapterID    string
	chapterIdx   int
	speaker      string
	speakerIndex int
	voiceID      string
	n            int
	chars        int64
	seconds      float64
}

// readyByVoice lists bookID's ready audio as readyGroups, of every voice -
// the caller keeps the ones each speaker is read in (speakerVoices.reads).
// only and onlyArgs are an optional further condition on chapters c. It
// starts from paragraph_audio and looks each row's speaker up in
// idx_paragraphs_stats, so a paragraph with no ready audio is never read.
func (s *SQLiteStore) readyByVoice(bookID, only string, onlyArgs []any) ([]readyGroup, error) {
	rows, err := s.db.Query(`
		SELECT c.id, c.idx, p.speaker, p.speaker_index, pa.voice_id,
			COUNT(*), COALESCE(SUM(LENGTH(p.content)), 0), COALESCE(SUM(pa.duration_seconds), 0)
		FROM chapters c
		JOIN paragraph_audio pa ON pa.chapter_id = c.id AND pa.status = 'ready'
		JOIN paragraphs p INDEXED BY idx_paragraphs_stats ON p.chapter_id = c.id AND p.id = pa.paragraph_id
		WHERE c.book_id = ?`+only+`
		GROUP BY c.id, p.speaker, p.speaker_index, pa.voice_id`, append([]any{bookID}, onlyArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []readyGroup
	for rows.Next() {
		var g readyGroup
		if err := rows.Scan(&g.chapterID, &g.chapterIdx, &g.speaker, &g.speakerIndex, &g.voiceID, &g.n, &g.chars, &g.seconds); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListChapterSummaries reports, per chapter, how many of its paragraphs
// have ready audio *for voiceID specifically* (or, for a paragraph whose
// speaker appears in overrides, for that speaker's own resolved voice
// instead - see SpeakerVoice) - a chapter fully generated under one voice
// shows as not-yet-generated when a different voice is selected, since
// that voice's audio doesn't exist yet.
func (s *SQLiteStore) ListChapterSummaries(bookID, voiceID string, overrides []SpeakerVoice) ([]ChapterSummary, error) {
	// With overrides, the ready subquery counts nothing (no row has the
	// empty voice) and the count comes from readyByVoice below.
	countVoice := voiceID
	if len(overrides) > 0 {
		countVoice = ""
	}
	rows, err := s.db.Query(`
		SELECT c.id, c.book_id, c.idx, c.title, c.passes, c.matter, c.matter_by,
			(SELECT COUNT(*) FROM paragraphs p WHERE p.chapter_id = c.id) AS total,
			(SELECT COUNT(*) FROM paragraph_audio pa WHERE pa.chapter_id = c.id AND pa.voice_id = ? AND pa.status = 'ready') AS ready
		FROM chapters c
		WHERE c.book_id = ?
		ORDER BY c.idx ASC`, countVoice, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChapterSummary
	for rows.Next() {
		var cs ChapterSummary
		if err := rows.Scan(&cs.ID, &cs.BookID, &cs.Idx, &cs.Title, &cs.Passes, &cs.Matter, &cs.MatterBy, &cs.ParagraphCount, &cs.ReadyCount); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	if err := rows.Err(); err != nil || len(overrides) == 0 {
		return out, err
	}
	groups, err := s.readyByVoice(bookID, "", nil)
	if err != nil {
		return nil, err
	}
	voices := newSpeakerVoices(voiceID, overrides)
	ready := map[string]int{}
	for _, g := range groups {
		if voices.reads(g.speaker, g.speakerIndex, g.chapterIdx, g.voiceID) {
			ready[g.chapterID] += g.n
		}
	}
	for i := range out {
		out[i].ReadyCount = ready[out[i].ID]
	}
	return out, nil
}

// ChapterNarration is one chapter's ChapterSummary plus its share of the
// book's NarrationStats - everything a book listing shows about narration
// progress, per chapter, so a caller can keep the chapters that didn't
// change and re-read only the ones that did (see Cached.ChapterNarration).
type ChapterNarration struct {
	ChapterSummary
	NarrationStats
}

// SumNarration splits a book's chapters into its chapter summaries and
// whole-book NarrationStats, which leave out front and back matter
// (Chapter.Matter) - never narrated in the background, so counting it
// would leave every book short of fully generated.
func SumNarration(chapters []ChapterNarration) ([]ChapterSummary, NarrationStats) {
	var stats NarrationStats
	summaries := make([]ChapterSummary, len(chapters))
	for i, c := range chapters {
		summaries[i] = c.ChapterSummary
		if c.Matter {
			continue
		}
		stats.TotalChars += c.TotalChars
		stats.TotalWords += c.TotalWords
		stats.ReadyChars += c.ReadyChars
		stats.ReadySeconds += c.ReadySeconds
	}
	return summaries, stats
}

// ChapterNarration is ListChapterSummaries with each chapter's narration
// stats alongside: its text length, and the length and duration of what's
// ready under voiceID (or, per speaker, under overrides - see SpeakerVoice).
// Stats are voice-scoped because narration pace is voice-specific: another
// voice's generated seconds would miscalibrate the estimate for this one.
// A non-empty chapterIDs limits the result to those chapters.
//
// Neither form visits a paragraph that has no ready audio: a chapter's
// text totals come from its chapter_text row (kept by triggers), and what's
// ready is counted from paragraph_audio - with overrides, by readyByVoice,
// which looks up each ready row's speaker in idx_paragraphs_stats. Joining every paragraph to
// its audio instead took ~5s over a 2.5M-paragraph library that had 200k
// ready clips.
func (s *SQLiteStore) ChapterNarration(bookID, voiceID string, overrides []SpeakerVoice, chapterIDs []string) ([]ChapterNarration, error) {
	only := ""
	onlyArgs := make([]any, len(chapterIDs))
	if len(chapterIDs) > 0 {
		only = " AND c.id IN (?" + strings.Repeat(", ?", len(chapterIDs)-1) + ")"
		for i, id := range chapterIDs {
			onlyArgs[i] = id
		}
	}

	// One voice for the whole book: ready audio is summed straight off
	// paragraph_audio's own chapter_id/chars (idx_paragraph_audio_chapter).
	// With overrides that join matches nothing (no row has the empty
	// voice) and what's ready comes from readyByVoice below.
	countVoice := voiceID
	if len(overrides) > 0 {
		countVoice = ""
	}
	rows, err := s.db.Query(`
		SELECT c.id, c.book_id, c.idx, c.title, c.passes, c.matter, c.matter_by,
			COALESCE(t.paragraphs, 0), COALESCE(t.chars, 0), COALESCE(t.words, 0),
			COUNT(pa.paragraph_id),
			COALESCE(SUM(pa.chars), 0),
			COALESCE(SUM(pa.duration_seconds), 0)
		FROM chapters c
		LEFT JOIN chapter_text t ON t.chapter_id = c.id
		LEFT JOIN paragraph_audio pa ON pa.chapter_id = c.id AND pa.voice_id = ? AND pa.status = 'ready'
		WHERE c.book_id = ?`+only+`
		GROUP BY c.id, c.book_id, c.idx, c.title, c.passes, c.matter, c.matter_by
		ORDER BY c.idx ASC`, append([]any{countVoice, bookID}, onlyArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChapterNarration
	for rows.Next() {
		var cn ChapterNarration
		if err := rows.Scan(&cn.ID, &cn.BookID, &cn.Idx, &cn.Title, &cn.Passes, &cn.Matter, &cn.MatterBy,
			&cn.ParagraphCount, &cn.TotalChars, &cn.TotalWords, &cn.ReadyCount, &cn.ReadyChars, &cn.ReadySeconds); err != nil {
			return nil, err
		}
		out = append(out, cn)
	}
	if err := rows.Err(); err != nil || len(overrides) == 0 {
		return out, err
	}
	groups, err := s.readyByVoice(bookID, only, onlyArgs)
	if err != nil {
		return nil, err
	}
	voices := newSpeakerVoices(voiceID, overrides)
	byID := make(map[string]*ChapterNarration, len(out))
	for i := range out {
		byID[out[i].ID] = &out[i]
	}
	for _, g := range groups {
		cn := byID[g.chapterID]
		if cn == nil || !voices.reads(g.speaker, g.speakerIndex, g.chapterIdx, g.voiceID) {
			continue
		}
		cn.ReadyCount += g.n
		cn.ReadyChars += g.chars
		cn.ReadySeconds += g.seconds
	}
	return out, nil
}

// ChapterFingerprints gives, for every chapter of bookID, an opaque string
// that changes whenever anything ChapterNarration reports for it could:
// the chapter's own row, its paragraphs' text or speakers (chapter_text's
// rev), or its paragraph_audio rows (count, duration, length and rowid
// sums per voice and status - one pass over idx_paragraph_audio_chapter).
// Cached.ChapterNarration compares these to find the chapters to re-read.
func (s *SQLiteStore) ChapterFingerprints(bookID string) (map[string]string, error) {
	prints := map[string][]byte{}
	chapters, err := s.db.Query(`
		SELECT c.id, c.idx, c.title, CAST(c.passes AS TEXT) || c.matter, COALESCE(t.rev, 0)
		FROM chapters c
		LEFT JOIN chapter_text t ON t.chapter_id = c.id
		WHERE c.book_id = ?`, bookID)
	if err != nil {
		return nil, err
	}
	defer chapters.Close()
	for chapters.Next() {
		var id, title, passes string
		var idx, rev int64
		if err := chapters.Scan(&id, &idx, &title, &passes, &rev); err != nil {
			return nil, err
		}
		b := strconv.AppendInt(nil, idx, 10)
		b = append(append(append(b, 0), title...), 0)
		b = append(append(b, passes...), 0)
		prints[id] = append(strconv.AppendInt(b, rev, 10), '\n')
	}
	if err := chapters.Err(); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(`
		SELECT pa.chapter_id, pa.voice_id, pa.status, COUNT(*), SUM(pa.duration_seconds), SUM(pa.chars), SUM(pa.rowid)
		FROM paragraph_audio pa
		WHERE pa.chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)
		GROUP BY pa.chapter_id, pa.voice_id, pa.status`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var chapterID, voiceID, status string
		var n, chars, rowids int64
		var seconds float64
		if err := rows.Scan(&chapterID, &voiceID, &status, &n, &seconds, &chars, &rowids); err != nil {
			return nil, err
		}
		b := prints[chapterID]
		b = append(append(append(append(b, voiceID...), 0), status...), 0)
		b = strconv.AppendInt(b, n, 10)
		b = strconv.AppendFloat(append(b, 0), seconds, 'g', -1, 64)
		b = strconv.AppendInt(append(b, 0), chars, 10)
		b = strconv.AppendInt(append(b, 0), rowids, 10)
		prints[chapterID] = append(b, '\n')
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(prints))
	for id, b := range prints {
		out[id] = string(b)
	}
	return out, nil
}

func (s *SQLiteStore) GetChapterByID(id string) (*Chapter, error) {
	var c Chapter
	err := s.db.QueryRow(`SELECT id, book_id, idx, title, passes, matter, matter_by FROM chapters WHERE id = ?`, id).
		Scan(&c.ID, &c.BookID, &c.Idx, &c.Title, &c.Passes, &c.Matter, &c.MatterBy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// CountChapters reports how many chapters bookID has.
func (s *SQLiteStore) CountChapters(bookID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM chapters WHERE book_id = ?`, bookID).Scan(&n)
	return n, err
}

func (s *SQLiteStore) GetChapterByIdx(bookID string, idx int) (*Chapter, error) {
	var c Chapter
	err := s.db.QueryRow(`SELECT id, book_id, idx, title, passes, matter, matter_by FROM chapters WHERE book_id = ? AND idx = ?`, bookID, idx).
		Scan(&c.ID, &c.BookID, &c.Idx, &c.Title, &c.Passes, &c.Matter, &c.MatterBy)
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
func (s *SQLiteStore) ListParagraphsRaw(chapterID string) ([]Paragraph, error) {
	rows, err := s.db.Query(`SELECT id, chapter_id, idx, position, content, speaker, speaker_index, inline, is_quote, scare_quote, emotion, pronunciation, emphasis, source_speaker, label, table_pos, direction FROM paragraphs WHERE chapter_id = ? ORDER BY idx ASC`, chapterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Paragraph
	for rows.Next() {
		var p Paragraph
		if err := rows.Scan(&p.ID, &p.ChapterID, &p.Idx, &p.Position, &p.Text, &p.Speaker, &p.SpeakerIndex, &p.Inline, &p.IsQuote, &p.ScareQuote, &p.Emotion, &p.Pronunciation, &p.Emphasis, &p.SourceSpeaker, &p.Label, &p.Table, &p.Direction); err != nil {
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
func (s *SQLiteStore) ListParagraphsRawForBook(bookID string) ([]Paragraph, error) {
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
func (s *SQLiteStore) GetParagraph(id string) (*Paragraph, error) {
	var p Paragraph
	err := s.db.QueryRow(`SELECT id, chapter_id, idx, position, content, speaker, speaker_index, inline, is_quote, scare_quote, emotion, pronunciation, emphasis, source_speaker, label, table_pos, direction FROM paragraphs WHERE id = ?`, id).
		Scan(&p.ID, &p.ChapterID, &p.Idx, &p.Position, &p.Text, &p.Speaker, &p.SpeakerIndex, &p.Inline, &p.IsQuote, &p.ScareQuote, &p.Emotion, &p.Pronunciation, &p.Emphasis, &p.SourceSpeaker, &p.Label, &p.Table, &p.Direction)
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
func (s *SQLiteStore) GetParagraphIDByIdx(chapterID string, idx int) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM paragraphs WHERE chapter_id = ? AND idx = ?`, chapterID, idx).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// ListImages returns a chapter's inline images ordered by their position
// among the chapter's content blocks.
func (s *SQLiteStore) ListImages(chapterID string) ([]Image, error) {
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
func (s *SQLiteStore) ListBreaks(chapterID string) ([]Break, error) {
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

func (s *SQLiteStore) GetImage(id string) (*Image, error) {
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
