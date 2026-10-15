package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Edit fields recorded in paragraph_edits. Scare-quote values are
// "true"/"false".
const (
	EditSpeaker    = "speaker"
	EditEmotion    = "emotion"
	EditScareQuote = "scare_quote"
	// EditDirection values are a line's direction written into its text
	// (direction.Direction.Annotate), "" for none.
	EditDirection = "direction"
)

// ParagraphEdit is one reader correction of a machine-labeled paragraph
// field (speaker, emotion or scare-quote flag): what the pass had written before the first
// manual edit (Original) and what the reader set it to last (Value).
// Kept so attribution/emotion changes can be scored against the reader's
// corrections later, and so a re-run of either pass leaves a corrected line
// alone. Speaker values are stored as paragraphs.speaker holds them ("" is
// Narrator).
type ParagraphEdit struct {
	ParagraphID  string
	ChapterIdx   int
	ParagraphIdx int
	Field        string
	Original     string
	Value        string
	EditedAt     int64
	Text         string // the paragraph's content, for reading the edit back
}

// RecordParagraphEdit notes a manual change of paragraphID's field from
// before to after. The first edit keeps before as the machine's original;
// later edits only move Value. Setting a field back to its original drops
// the record - the machine had it right. A no-op edit records nothing.
func (s *SQLiteStore) RecordParagraphEdit(paragraphID, field, before, after string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var original string
	err = tx.QueryRow(`SELECT original FROM paragraph_edits WHERE paragraph_id = ? AND field = ?`, paragraphID, field).Scan(&original)
	switch {
	case err == nil:
		if original == after {
			_, err = tx.Exec(`DELETE FROM paragraph_edits WHERE paragraph_id = ? AND field = ?`, paragraphID, field)
		} else {
			_, err = tx.Exec(`UPDATE paragraph_edits SET value = ?, edited_at = ? WHERE paragraph_id = ? AND field = ?`, after, time.Now().Unix(), paragraphID, field)
		}
	case errors.Is(err, sql.ErrNoRows):
		if before == after {
			return nil
		}
		_, err = tx.Exec(`INSERT INTO paragraph_edits (paragraph_id, field, original, value, edited_at) VALUES (?, ?, ?, ?, ?)`, paragraphID, field, before, after, time.Now().Unix())
	}
	if err != nil {
		return fmt.Errorf("record %s edit: %w", field, err)
	}
	return tx.Commit()
}

// ParagraphEdits lists bookID's recorded edits of field ("" for every
// field), in reading order.
func (s *SQLiteStore) ParagraphEdits(bookID, field string) ([]ParagraphEdit, error) {
	rows, err := s.db.Query(`
		SELECT e.paragraph_id, c.idx, p.idx, e.field, e.original, e.value, e.edited_at, p.content
		FROM paragraph_edits e
		JOIN paragraphs p ON p.id = e.paragraph_id
		JOIN chapters c ON c.id = p.chapter_id
		WHERE c.book_id = ? AND (? = '' OR e.field = ?)
		ORDER BY c.idx, p.idx, e.field`, bookID, field, field)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ParagraphEdit
	for rows.Next() {
		var e ParagraphEdit
		if err := rows.Scan(&e.ParagraphID, &e.ChapterIdx, &e.ParagraphIdx, &e.Field, &e.Original, &e.Value, &e.EditedAt, &e.Text); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EditedParagraphIdx returns the idx of chapterID's paragraphs with a
// recorded manual edit of field - lines a re-run of that field's pass must
// leave alone.
func (s *SQLiteStore) EditedParagraphIdx(chapterID, field string) (map[int]bool, error) {
	rows, err := s.db.Query(`
		SELECT p.idx FROM paragraph_edits e JOIN paragraphs p ON p.id = e.paragraph_id
		WHERE p.chapter_id = ? AND e.field = ?`, chapterID, field)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			return nil, err
		}
		out[idx] = true
	}
	return out, rows.Err()
}
