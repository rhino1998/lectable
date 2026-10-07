package store

import (
	"database/sql"
	"time"
)

// RecapKind is what a BookRecap covers.
type RecapKind string

const (
	// RecapPosition recaps the book itself, up to a reading position.
	RecapPosition RecapKind = "position"
	// RecapSeries recaps the book before this one in its series, for a
	// reader at this book's start.
	RecapSeries RecapKind = "series"
)

// BookRecap is a book's "previously on" recap (httpapi.prepareRecap) - one
// per book, replaced whenever a new one is written.
type BookRecap struct {
	BookID string
	Kind   RecapKind
	// ChapterIdx/ParagraphIdx is the reading position it was written for:
	// it covers nothing at or after that paragraph. ThroughIdx is the last
	// paragraph of that chapter a reader can be at and still be offered it.
	ChapterIdx   int
	ParagraphIdx int
	ThroughIdx   int
	// SourceBookID is the recapped book for RecapSeries, "" otherwise.
	SourceBookID string
	// Title is what the recap is of, as spoken before it ("Brightblade").
	Title   string
	Content string
	// Error is why writing it failed; Content is "" then.
	Error string
	// AudioVoice identifies the narrator voice its clip was spoken in
	// (voice id and clone model), "" when it has no clip.
	AudioVoice   string
	AudioSeconds float64
	// Segments is the clip's layout as JSON (httpapi's recapSegmentDTO
	// list): the stretches it was spoken in, with their words' timings.
	// "[]" with no clip. A string, so a BookRecap stays comparable.
	Segments string
	// ReadAt is the book's LastReadAt when the recap was written - when the
	// reader stopped where it picks up.
	ReadAt    int64
	UpdatedAt int64
}

// Covers reports whether r was written for a reader now at
// chapterIdx/paragraphIdx.
func (r *BookRecap) Covers(chapterIdx, paragraphIdx int) bool {
	return r != nil && r.ChapterIdx == chapterIdx && r.ParagraphIdx <= paragraphIdx && paragraphIdx <= r.ThroughIdx
}

const bookRecapColumns = `book_id, kind, chapter_idx, paragraph_idx, through_idx, source_book_id, title, content, error, audio_voice, audio_seconds, CAST(segments AS VARCHAR), read_at, updated_at`

// GetBookRecap returns bookID's recap, nil if it has none.
func (s *SQLiteStore) GetBookRecap(bookID string) (*BookRecap, error) {
	var r BookRecap
	err := s.db.QueryRow(`SELECT `+bookRecapColumns+` FROM book_recaps WHERE book_id = ?`, bookID).Scan(
		&r.BookID, &r.Kind, &r.ChapterIdx, &r.ParagraphIdx, &r.ThroughIdx, &r.SourceBookID, &r.Title, &r.Content, &r.Error, &r.AudioVoice, &r.AudioSeconds, &r.Segments, &r.ReadAt, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// SetBookRecap stores r as its book's recap, replacing any existing one,
// and returns the UpdatedAt it stamped.
func (s *SQLiteStore) SetBookRecap(r BookRecap) (int64, error) {
	now := time.Now().Unix()
	if r.Segments == "" {
		r.Segments = "[]"
	}
	_, err := s.db.Exec(`INSERT INTO book_recaps (book_id, kind, chapter_idx, paragraph_idx, through_idx, source_book_id, title, content, error,
			audio_voice, audio_seconds, segments, read_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (book_id) DO UPDATE SET kind = excluded.kind, chapter_idx = excluded.chapter_idx, paragraph_idx = excluded.paragraph_idx,
			through_idx = excluded.through_idx, source_book_id = excluded.source_book_id, title = excluded.title, content = excluded.content,
			error = excluded.error, audio_voice = excluded.audio_voice, audio_seconds = excluded.audio_seconds, segments = excluded.segments,
			read_at = excluded.read_at, updated_at = excluded.updated_at`,
		r.BookID, string(r.Kind), r.ChapterIdx, r.ParagraphIdx, r.ThroughIdx, r.SourceBookID, r.Title, r.Content, r.Error, r.AudioVoice, r.AudioSeconds, r.Segments, r.ReadAt, now)
	return now, err
}

// SetBookRecapAudio records the clip spoken for bookID's recap, and its
// layout (BookRecap.Segments) - only while the recap is still the one
// written at updatedAt, so a clip that finishes after its recap was
// replaced isn't credited to the new one. Reports whether it was recorded.
func (s *SQLiteStore) SetBookRecapAudio(bookID string, updatedAt int64, voice string, seconds float64, segments string) (bool, error) {
	if segments == "" {
		segments = "[]"
	}
	res, err := s.db.Exec(`UPDATE book_recaps SET audio_voice = ?, audio_seconds = ?, segments = ? WHERE book_id = ? AND updated_at = ?`, voice, seconds, segments, bookID, updatedAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteBookRecap removes bookID's recap.
func (s *SQLiteStore) DeleteBookRecap(bookID string) error {
	_, err := s.db.Exec(`DELETE FROM book_recaps WHERE book_id = ?`, bookID)
	return err
}

// ChapterExtent is one chapter's size and whether it has summary-pass
// notes - what planning a recap reads of a book (ChapterExtents).
type ChapterExtent struct {
	ID         string
	Idx        int
	Title      string
	Paragraphs int
	Chars      int
	Summarized bool
}

// ChapterExtents returns bookID's chapters in order, each with its
// paragraph count, text length and whether it has been summarized.
func (s *SQLiteStore) ChapterExtents(bookID string) ([]ChapterExtent, error) {
	rows, err := s.db.Query(`SELECT c.id, c.idx, c.title, COUNT(p.id), COALESCE(SUM(length(p.content)), 0),
			EXISTS (SELECT 1 FROM chapter_summaries cs WHERE cs.chapter_id = c.id)
		FROM chapters c LEFT JOIN paragraphs p ON p.chapter_id = c.id
		WHERE c.book_id = ? GROUP BY c.id ORDER BY c.idx ASC`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChapterExtent
	for rows.Next() {
		var e ChapterExtent
		if err := rows.Scan(&e.ID, &e.Idx, &e.Title, &e.Paragraphs, &e.Chars, &e.Summarized); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RecapNever is the RecapAfterMinutes value that never offers a recap.
const RecapNever = -1

// RecapAfterMinutes is how long a reader must have been away from a book
// before the clients offer its recap unasked: 0 every time it is opened,
// RecapNever never. Library-wide.
func (s *SQLiteStore) RecapAfterMinutes() (int, error) {
	var minutes int
	err := s.db.QueryRow(`SELECT recap_after_minutes FROM default_voice WHERE id = 0`).Scan(&minutes)
	return minutes, err
}

// SetRecapAfterMinutes sets RecapAfterMinutes.
func (s *SQLiteStore) SetRecapAfterMinutes(minutes int) error {
	_, err := s.db.Exec(`UPDATE default_voice SET recap_after_minutes = ? WHERE id = 0`, minutes)
	return err
}
