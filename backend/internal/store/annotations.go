package store

import (
	"fmt"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
)

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
// across bookIDs, evenly spread over all of them in the given book order
// (then chapter/paragraph order within each; see spreadSample) and
// preferring lines of at least minSampleQuoteWords words - gathered for
// speakerattr.Client.CharacterizeVoice. bookIDs is typically
// ListSeriesBooks' output in series order; the explicit ORDER BY CASE
// keeps that order rather than however the database would order an IN (...)
// match. One query per character, fetched fresh when their own job runs,
// since their lines can still be attributed after the job was queued.
func (s *SQLiteStore) QuotesForBooks(bookIDs []string, characterName string, limit int) ([]QuoteContext, error) {
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

	rows, err := s.db.Query(`
		SELECT p.content, COALESCE(next.content, '')
		FROM paragraphs p
		JOIN chapters c ON c.id = p.chapter_id
		LEFT JOIN paragraphs next
			ON next.chapter_id = p.chapter_id AND next.idx = p.idx + 1
			AND next.inline
			AND (next.speaker = '' OR next.speaker = 'Narrator')
		WHERE c.book_id IN (`+bookPlaceholders+`) AND p.speaker = ?
		ORDER BY CASE c.book_id `+orderCases.String()+`END, c.idx ASC, p.idx ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var all, long []QuoteContext
	for rows.Next() {
		var qc QuoteContext
		if err := rows.Scan(&qc.Text, &qc.Context); err != nil {
			return nil, err
		}
		all = append(all, qc)
		if len(strings.Fields(qc.Text)) >= minSampleQuoteWords {
			long = append(long, qc)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(long) >= limit {
		all = long
	}
	return spreadSample(all, limit), nil
}

// SpokenLine is one of a character's lines, with what it answers.
type SpokenLine struct {
	Text string
	// Asked is the dialogue line before it, when someone else spoke it
	// within lookupReplyReach paragraphs - "" otherwise.
	Asked string
}

// SpokenLinesForBooks returns every line characterName speaks across
// bookIDs, in the given book order (then chapter/paragraph order), each
// with the line it answers - what speakerattr.StatedAges and IdiomCounts
// read. A character states their own age in a line that never names
// them, usually because someone asked.
func (s *SQLiteStore) SpokenLinesForBooks(bookIDs []string, characterName string) ([]SpokenLine, error) {
	var out []SpokenLine
	for _, bookID := range bookIDs {
		rows, err := s.db.Query(`
			SELECT p.content, COALESCE(a.content, '')
			FROM paragraphs p
			JOIN chapters c ON c.id = p.chapter_id
			LEFT JOIN paragraphs a ON a.speaker != p.speaker AND a.id = (
				SELECT q.id FROM paragraphs q
				WHERE q.chapter_id = p.chapter_id AND q.idx < p.idx AND q.idx >= p.idx - ? AND q.is_quote
				ORDER BY q.idx DESC LIMIT 1)
			WHERE c.book_id = ? AND p.speaker = ?
			ORDER BY c.idx ASC, p.idx ASC`, lookupReplyReach, bookID, characterName)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var l SpokenLine
			if err := rows.Scan(&l.Text, &l.Asked); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, l)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SpeechTagsForBooks returns the narration that follows each of
// characterName's own lines in the same source paragraph (QuoteContext's
// Context, for every line rather than a sample) across bookIDs, in the
// given book order - what speakerattr.VoiceClues searches for how the
// narration says they sound. A first-person narrator's own tags ("I said,
// a roughness to my voice") never name them, so a search by name misses
// these.
func (s *SQLiteStore) SpeechTagsForBooks(bookIDs []string, characterName string) ([]string, error) {
	var out []string
	for _, bookID := range bookIDs {
		rows, err := s.db.Query(`
			SELECT next.content
			FROM paragraphs p
			JOIN chapters c ON c.id = p.chapter_id
			JOIN paragraphs next
				ON next.chapter_id = p.chapter_id AND next.idx = p.idx + 1
				AND next.inline
				AND (next.speaker = '' OR next.speaker = 'Narrator')
			WHERE c.book_id = ? AND p.speaker = ?
			ORDER BY c.idx ASC, p.idx ASC`, bookID, characterName)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var text string
			if err := rows.Scan(&text); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, text)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CoSpeakersForBooks returns up to limit other speakers who speak in the
// most of the chapters characterName speaks in, across bookIDs, most shared
// chapters first - the cast characterization keeps a voice distinguishable
// from. Narrator and Unknown are left out.
func (s *SQLiteStore) CoSpeakersForBooks(bookIDs []string, characterName string, limit int) ([]string, error) {
	if len(bookIDs) == 0 {
		return nil, nil
	}
	bookPlaceholders := strings.TrimSuffix(strings.Repeat("?,", len(bookIDs)), ",")
	args := make([]any, 0, len(bookIDs)+3)
	for _, id := range bookIDs {
		args = append(args, id)
	}
	args = append(args, characterName, characterName, limit)
	rows, err := s.db.Query(`
		WITH shared AS (
			SELECT DISTINCT p.chapter_id
			FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id
			WHERE c.book_id IN (`+bookPlaceholders+`) AND p.speaker = ?
		)
		SELECT p.speaker
		FROM paragraphs p JOIN shared ON shared.chapter_id = p.chapter_id
		WHERE p.speaker NOT IN ('', 'Narrator', 'Unknown', ?)
		GROUP BY p.speaker
		ORDER BY count(DISTINCT p.chapter_id) DESC, count(*) DESC, p.speaker
		LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// minSampleQuoteWords is the shortest line QuotesForBooks prefers: "Yes."
// or "What?" says nothing about how someone talks, so shorter lines are
// only used when a character has too few longer ones to fill the sample.
const minSampleQuoteWords = 4

// spreadSample returns up to n of items evenly spread across all of them,
// in order, always keeping the first. A character's first n lines are
// usually one scene - Spire's Spite's lead (8000 lines) was characterized
// from his first 20, all from his introduction as a captured gang thug.
func spreadSample[T any](items []T, n int) []T {
	if len(items) <= n {
		return items
	}
	out := make([]T, n)
	for i := range out {
		out[i] = items[i*len(items)/n]
	}
	return out
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
	// SpeakerIndex is the line's Paragraph.SpeakerIndex.
	SpeakerIndex int
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
func (s *SQLiteStore) ParagraphsForSpeaker(bookIDs []string, name string) ([]SpeakerAppearance, error) {
	var out []SpeakerAppearance
	for _, bookID := range bookIDs {
		query := `
			SELECT b.id, b.title, c.idx, c.title, p.idx, p.id, p.content, COALESCE(p.speaker_index, 0)
			FROM paragraphs p
			JOIN chapters c ON c.id = p.chapter_id
			JOIN books b ON b.id = c.book_id
			WHERE c.book_id = ? AND p.speaker = ?
			ORDER BY c.idx ASC, p.idx ASC`
		args := []any{bookID, name}
		if name == "Narrator" {
			query = `
				SELECT b.id, b.title, c.idx, c.title, p.idx, p.id, p.content, COALESCE(p.speaker_index, 0)
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
			if err := rows.Scan(&a.BookID, &a.BookTitle, &a.ChapterIdx, &a.ChapterTitle, &a.ParagraphIdx, &a.ParagraphID, &a.Text, &a.SpeakerIndex); err != nil {
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
// ids in between. Every line written loses its speaker number - see
// SetParagraphSpeakersIndexed.
func (s *SQLiteStore) SetParagraphSpeakers(chapterID string, bySpeakerIdx map[int]string) error {
	return s.SetParagraphSpeakersIndexed(chapterID, bySpeakerIdx, nil)
}

// SetParagraphSpeakersIndexed is SetParagraphSpeakers plus each line's
// speaker number (Paragraph.SpeakerIndex), keyed like bySpeakerIdx; a line
// with no entry in indexes gets 0.
func (s *SQLiteStore) SetParagraphSpeakersIndexed(chapterID string, bySpeakerIdx map[int]string, indexes map[int]int) error {
	if len(bySpeakerIdx) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tx.scope = s.chapterBook(chapterID)
	for idx, speaker := range bySpeakerIdx {
		if _, err := tx.Exec(`UPDATE paragraphs SET speaker = ?, speaker_index = ? WHERE chapter_id = ? AND idx = ?`, speaker, indexes[idx], chapterID, idx); err != nil {
			return fmt.Errorf("set speaker for paragraph idx %d: %w", idx, err)
		}
	}
	return tx.Commit()
}

// SetParagraphScareQuotes records, for a chapter's paragraphs, which ones
// are scare quotes (see internal/speakerattr.Client.ScareQuoteChapter and
// Paragraph.ScareQuote) - SetParagraphSpeakers' own shape, but a plain
// bool column rather than JSON since there's nothing to encode. A paragraph
// present in byScareQuoteIdx with a false value explicitly clears a
// previously-set flag (e.g. a reader's own manual "not a scare quote after
// all" correction) - unlike SetParagraphDescriptions, which only ever
// receives entries worth setting, this can carry either direction.
//
// Also rewrites speaker to match: setting the flag forces "Narrator" (a
// scare quote is part of the narration, so whatever attribution assigned
// it as if it were dialogue no longer applies - and scare-quote merging
// needs it narration-voiced, see jobs.Manager.scareQuoteMergeGroup);
// clearing it turns a "Narrator" speaker back into "" (unattributed) so
// the next attribution run picks the now-real dialogue line up again,
// since "Narrator" is never valid for dialogue. Any other speaker is left
// alone on clear.
func (s *SQLiteStore) SetParagraphScareQuotes(chapterID string, byIdx map[int]bool) error {
	if len(byIdx) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tx.scope = s.chapterBook(chapterID)
	for idx, scareQuote := range byIdx {
		if _, err := tx.Exec(`UPDATE paragraphs SET scare_quote = ?,
			speaker = CASE WHEN ? THEN 'Narrator' WHEN speaker = 'Narrator' THEN '' ELSE speaker END
			WHERE chapter_id = ? AND idx = ?`, scareQuote, scareQuote, chapterID, idx); err != nil {
			return fmt.Errorf("set scare_quote for paragraph idx %d: %w", idx, err)
		}
	}
	return tx.Commit()
}

// SetParagraphQuote sets one paragraph's is_quote - a reader's fix for a
// line the parser got wrong (a multi-paragraph speech's unclosed “ reads as
// narration), without re-importing the chapter. Either way the line starts
// over: marked dialogue is "Unknown" until attributed, marked narration has
// no speaker; both drop the scare-quote flag and emotion,
// which only fit the other kind. Reports whether anything changed.
func (s *SQLiteStore) SetParagraphQuote(chapterID string, idx int, quote bool) (bool, error) {
	book := s.chapterBook(chapterID)
	speaker := ""
	if quote {
		speaker = "Unknown"
	}
	res, err := s.db.execScoped(book, `UPDATE paragraphs SET is_quote = ?, speaker = ?, source_speaker = '', speaker_index = 0, scare_quote = false,
		emotion = ''
		WHERE chapter_id = ? AND idx = ? AND is_quote <> ?`, quote, speaker, chapterID, idx, quote)
	if err != nil {
		return false, fmt.Errorf("set is_quote for paragraph idx %d: %w", idx, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ClearLegacyTags empties chapterID's legacy paragraphs.tts_tags (Higgs
// delivery tags from the LLM tag passes the emotion pass replaced) and
// returns the Idx of every paragraph that still had any - their existing
// audio was generated with those tags baked in, so the caller
// (httpapi.directChapter) invalidates it. Clearing makes that a one-time
// migration per chapter rather than something every re-run repeats.
func (s *SQLiteStore) ClearLegacyTags(chapterID string) ([]int, error) {
	book := s.chapterBook(chapterID)
	rows, err := s.db.Query(`SELECT idx FROM paragraphs WHERE chapter_id = ? AND tts_tags NOT IN ('{}', 'null', '')`, chapterID)
	if err != nil {
		return nil, err
	}
	var idxs []int
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			rows.Close()
			return nil, err
		}
		idxs = append(idxs, idx)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(idxs) == 0 {
		return nil, nil
	}
	if _, err := s.db.execScoped(book, `UPDATE paragraphs SET tts_tags = '{}' WHERE chapter_id = ? AND tts_tags NOT IN ('{}', 'null', '')`, chapterID); err != nil {
		return nil, err
	}
	return idxs, nil
}

// SetParagraphEmotions records, for a chapter's paragraphs keyed by Idx,
// each one's emotion (an internal/emotions id, or "" to clear it back to
// neutral) - the emotion pass's own counterpart of
// SetParagraphPronunciation. A paragraph absent from byIdx is left
// untouched.
func (s *SQLiteStore) SetParagraphEmotions(chapterID string, byIdx map[int]string) error {
	if len(byIdx) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tx.scope = s.chapterBook(chapterID)
	for idx, emotion := range byIdx {
		if _, err := tx.Exec(`UPDATE paragraphs SET emotion = ? WHERE chapter_id = ? AND idx = ?`, emotion, chapterID, idx); err != nil {
			return fmt.Errorf("set emotion for paragraph idx %d: %w", idx, err)
		}
	}
	return tx.Commit()
}

// SetParagraphPronunciation records, for a chapter's paragraphs, the
// pronunciation substitutions resolved by
// internal/speakerattr.Client.ResolvePronunciation (see the
// paragraphs.pronunciation column's own doc comment in Open). This isn't
// clone-model-keyed - a pronunciation fix is plain word substitution, independent of
// which TTS model narrates it (see Paragraph.ResolveGenerationText) - so
// each call is a plain overwrite rather than a read-modify-write into a
// per-model map: a paragraph absent from byIdx is left untouched, and one
// present is fully replaced (a nil/empty slice clears it), matching
// ResolvePronunciation's own idempotent full-chapter-refresh semantics -
// the same "safe to redo" contract attribution/direction-tagging already
// have.
func (s *SQLiteStore) SetParagraphPronunciation(chapterID string, byIdx map[int][]pronounce.Substitution) error {
	if len(byIdx) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tx.scope = s.chapterBook(chapterID)
	for idx, subs := range byIdx {
		if subs == nil {
			subs = []pronounce.Substitution{}
		}
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
// that happens to share the same (wrong) speaker. The line's speaker
// number (Paragraph.SpeakerIndex) is cleared: it belonged to the old value.
func (s *SQLiteStore) SetParagraphSpeaker(paragraphID, speaker string) error {
	book := s.paragraphBook(paragraphID)
	_, err := s.db.execScoped(book, `UPDATE paragraphs SET speaker = ?, speaker_index = 0 WHERE id = ?`, speaker, paragraphID)
	return err
}

// ClearBookSpeakers resets every paragraph's speaker attribution back to
// "" (unattributed), and clears Passes.Attribution
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
func (s *SQLiteStore) ClearBookSpeakers(bookID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(
		`UPDATE paragraphs SET speaker = source_speaker, speaker_index = 0 WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`,
		bookID,
	); err != nil {
		return err
	}
	// Clears PassAttribution/PassScareQuote for every
	// chapter of bookID, unconditionally - unlike the old state-enum
	// version this replaced (which only reset a chapter exactly at
	// ChapterStateAttributed, leaving ChapterStateTagged alone specifically
	// to avoid also losing its direction-tagging progress), there's no such
	// risk here: PassDirection is a fully independent field now, so
	// clearing the other three never touches it regardless of whether it's
	// true, false, or absent. json_patch's own RFC 7396 semantics
	// delete a key when the patch sets it to null - see
	// SetChapterAttributed's own doc comment for why this is a single
	// statement rather than a Go-side read-modify-write loop.
	if _, err := tx.Exec(
		`UPDATE chapters SET passes = json_patch(passes, '{"attribution": null, "scareQuote": null}') WHERE book_id = ?`,
		bookID,
	); err != nil {
		return err
	}
	return tx.Commit()
}
