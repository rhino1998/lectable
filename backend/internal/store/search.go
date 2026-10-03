package store

import (
	"database/sql"
	"strings"
	"time"
	"unicode/utf8"
)

// SearchResult is one paragraph matching a book search, with just enough
// chapter context to display and jump to it.
type SearchResult struct {
	ChapterIdx   int
	ChapterTitle string
	ParagraphIdx int
	Text         string
}

// escapeLike escapes LIKE's own wildcard characters in user input so a
// search for e.g. "50%" or "under_stood" matches literally rather than
// being interpreted as a pattern.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ftsMinQueryRunes is the shortest query the trigram index can answer.
const ftsMinQueryRunes = 3

// SearchParagraphs finds paragraphs in bookID whose text contains query
// (case-insensitive substring match), in book order, capped at limit
// matches. Three or more characters go through the paragraphs_fts trigram
// index, as one phrase; anything shorter scans the book's paragraphs.
func (s *SQLiteStore) SearchParagraphs(bookID, query string, limit int) ([]SearchResult, error) {
	var rows *sql.Rows
	var err error
	if utf8.RuneCountInString(query) >= ftsMinQueryRunes {
		rows, err = s.db.Query(`
			SELECT c.idx, c.title, p.idx, p.content
			FROM paragraphs_fts f
			JOIN paragraphs p ON p.rowid = f.rowid
			JOIN chapters c ON c.id = p.chapter_id
			WHERE f.paragraphs_fts MATCH ? AND c.book_id = ?
			ORDER BY c.idx ASC, p.idx ASC
			LIMIT ?`, `"`+strings.ReplaceAll(query, `"`, `""`)+`"`, bookID, limit)
	} else {
		rows, err = s.db.Query(`
			SELECT c.idx, c.title, p.idx, p.content
			FROM paragraphs p
			JOIN chapters c ON c.id = p.chapter_id
			WHERE c.book_id = ? AND fold(p.content) LIKE fold(?) ESCAPE '\'
			ORDER BY c.idx ASC, p.idx ASC
			LIMIT ?`, bookID, "%"+escapeLike(query)+"%", limit)
	}
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
func (s *SQLiteStore) UpsertBookmark(paragraphID, note string) (string, error) {
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

func (s *SQLiteStore) DeleteBookmark(id string) error {
	_, err := s.db.Exec(`DELETE FROM bookmarks WHERE id = ?`, id)
	return err
}

// ListBookmarks returns bookID's bookmarks in book order (not creation
// order), matching how search results are ordered - a bookmarks list reads
// most naturally as "these are the flagged spots as you'd encounter them",
// not as a most-recent-first log.
func (s *SQLiteStore) ListBookmarks(bookID string) ([]Bookmark, error) {
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

// UpdateBookmarkNote edits an existing bookmark's note by its own id -
// UpsertBookmark's job is initial creation/toggling (addressed by
// paragraph_id); once a bookmark exists, further edits go by id instead.
func (s *SQLiteStore) UpdateBookmarkNote(id, note string) error {
	_, err := s.db.Exec(`UPDATE bookmarks SET note = ? WHERE id = ?`, note, id)
	return err
}
