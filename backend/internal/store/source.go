package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// BookSource is where a book imported from a website came from, and how
// the last check for new chapters there went. A book from an uploaded
// epub has none.
type BookSource struct {
	// Kind names the site ("royalroad"), SourceID the work's id there
	// (a fiction's number, "vol-3").
	Kind     string
	SourceID string
	URL      string
	// CheckedAt is when the source was last checked (unix seconds), 0 if
	// never.
	CheckedAt int64
	// RemoteChapters is how many chapters the source offered at the last
	// check: the book's own plus any still to be fetched.
	RemoteChapters int
	// Error is why the last check stopped early, "" if it finished.
	Error string
	// Blocked is the title of the chapter the last check stopped at
	// because the source lists it but doesn't serve it yet, "" if none.
	Blocked string
}

// SetBookSource records bookID's source, replacing any it had.
func (s *SQLiteStore) SetBookSource(bookID string, src BookSource) error {
	_, err := s.db.Exec(
		`UPDATE books SET source_kind = ?, source_id = ?, source_url = ?, source_checked_at = ?, source_remote_chapters = ?, source_error = ?, source_blocked = ? WHERE id = ?`,
		src.Kind, src.SourceID, src.URL, src.CheckedAt, src.RemoteChapters, src.Error, src.Blocked, bookID)
	return err
}

// GetBookSource returns bookID's source, nil for a book without one (or no
// such book).
func (s *SQLiteStore) GetBookSource(bookID string) (*BookSource, error) {
	var src BookSource
	err := s.db.QueryRow(
		`SELECT source_kind, source_id, source_url, source_checked_at, source_remote_chapters, source_error, source_blocked FROM books WHERE id = ?`, bookID).
		Scan(&src.Kind, &src.SourceID, &src.URL, &src.CheckedAt, &src.RemoteChapters, &src.Error, &src.Blocked)
	if err == sql.ErrNoRows || (err == nil && src.Kind == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &src, nil
}

// FindBookBySource returns the id of the book imported from that source,
// "" if the library has none.
func (s *SQLiteStore) FindBookBySource(kind, sourceID string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM books WHERE source_kind = ? AND source_id = ? ORDER BY added_at LIMIT 1`, kind, sourceID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// ChapterSourceIDs returns the source's id for each of bookID's chapters
// that has one (ChapterInput.SourceID), in chapter order. A chapter that
// stands for several of the source's - an ebook chapter filling in for
// two web ones - holds all their ids, space-separated; each is listed.
func (s *SQLiteStore) ChapterSourceIDs(bookID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT source_id FROM chapters WHERE book_id = ? AND source_id <> '' ORDER BY idx`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var ids string
		if err := rows.Scan(&ids); err != nil {
			return nil, err
		}
		out = append(out, strings.Fields(ids)...)
	}
	return out, rows.Err()
}

// SourcedChapter is one chapter of a book imported from a website.
type SourcedChapter struct {
	Idx      int
	Title    string
	SourceID string // as stored: several ids space-separated, see ChapterSourceIDs
}

// SourcedChapters lists bookID's chapters that came from its source, in
// chapter order.
func (s *SQLiteStore) SourcedChapters(bookID string) ([]SourcedChapter, error) {
	rows, err := s.db.Query(`SELECT idx, title, source_id FROM chapters WHERE book_id = ? AND source_id <> '' ORDER BY idx`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourcedChapter
	for rows.Next() {
		var c SourcedChapter
		if err := rows.Scan(&c.Idx, &c.Title, &c.SourceID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AddLastChapterSourceID records that bookID's last chapter also stands
// for the source's chapter sourceID (see ChapterSourceIDs).
func (s *SQLiteStore) AddLastChapterSourceID(bookID, sourceID string) error {
	_, err := s.db.Exec(
		`UPDATE chapters SET source_id = trim(source_id || ' ' || ?)
		 WHERE book_id = ? AND idx = (SELECT MAX(idx) FROM chapters WHERE book_id = ?)`,
		sourceID, bookID, bookID)
	return err
}

// AppendChapters adds chapters to the end of bookID, in one transaction -
// how a book imported from a website grows as its source publishes more.
// Nothing the book already has is touched. Returned images are in the
// order the image blocks appeared, as CreateBook's are.
func (s *SQLiteStore) AppendChapters(bookID string, chapters []ChapterInput) (images []CreatedImage, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var next int
	if err := tx.QueryRow(`SELECT (SELECT COALESCE(MAX(idx), -1) + 1 FROM chapters WHERE book_id = b.id) FROM books b WHERE b.id = ?`, bookID).Scan(&next); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("book %s no longer exists", bookID)
		}
		return nil, fmt.Errorf("look up book: %w", err)
	}
	for i, ch := range chapters {
		chapterID := NewID()
		if _, err := tx.Exec(
			`INSERT INTO chapters (id, book_id, idx, title, source_id) VALUES (?, ?, ?, ?, ?)`,
			chapterID, bookID, next+i, ch.Title, ch.SourceID,
		); err != nil {
			return nil, fmt.Errorf("insert chapter: %w", err)
		}
		chImages, err := insertChapterBlocks(tx, chapterID, ch.Blocks)
		if err != nil {
			return nil, err
		}
		images = append(images, chImages...)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return images, nil
}
