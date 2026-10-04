package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ChapterNotes is one chapter's summary-pass notes
// (speakerattr.Client.SummarizeChapter, a jobs.KindSummary task via
// httpapi.summarizeChapter): what happens, its scenes, and its cast.
// Stored as JSON in chapter_summaries.summary.
type ChapterNotes struct {
	Summary string         `json:"summary"`
	Scenes  []SummaryScene `json:"scenes"`
	Cast    []CastMember   `json:"cast"`
	// Edited notes were written or corrected by the reader (PUT .../notes);
	// the summary pass leaves them alone unless told to overwrite them.
	Edited bool `json:"edited,omitempty"`
}

// SummaryScene is one scene of a chapter, running from StartIdx to the next
// scene's StartIdx (or the chapter's end).
type SummaryScene struct {
	StartIdx int      `json:"startIdx"`
	Location string   `json:"location"`
	Time     string   `json:"time"`
	Present  []string `json:"present"`
}

// CastMember is one character present in a chapter.
type CastMember struct {
	Name     string   `json:"name"`
	Role     bool     `json:"role"`
	Pronouns string   `json:"pronouns"`
	Refs     []string `json:"refs"`
}

// GetChapterNotes returns chapterID's summary-pass notes, nil if it has none.
func (s *SQLiteStore) GetChapterNotes(chapterID string) (*ChapterNotes, error) {
	var raw string
	err := s.db.QueryRow(`SELECT CAST(summary AS VARCHAR) FROM chapter_summaries WHERE chapter_id = ?`, chapterID).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out ChapterNotes
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("decode chapter summary: %w", err)
	}
	return &out, nil
}

// SetChapterNotes stores chapterID's summary-pass notes, replacing any existing one,
// and marks its passes.summary.
func (s *SQLiteStore) SetChapterNotes(chapterID string, notes ChapterNotes) error {
	raw, err := json.Marshal(notes)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.Exec(`INSERT INTO chapter_summaries (chapter_id, summary, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (chapter_id) DO UPDATE SET summary = excluded.summary, updated_at = excluded.updated_at`, chapterID, string(raw), time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE chapters SET passes = json_patch(passes, '{"summary": true}') WHERE id = ?`, chapterID); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearBookSummaries deletes the summaries of bookID's chapters with idx >=
// fromIdx and unsets their passes.summary - the summary pass's Reset.
// Returns how many were deleted.
func (s *SQLiteStore) ClearBookSummaries(bookID string, fromIdx int) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	res, err := tx.Exec(`DELETE FROM chapter_summaries WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ? AND idx >= ?)`, bookID, fromIdx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(`UPDATE chapters SET passes = json_patch(passes, '{"summary": null}') WHERE book_id = ? AND idx >= ?`, bookID, fromIdx); err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}
