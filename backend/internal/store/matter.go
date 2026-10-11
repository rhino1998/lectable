package store

import "strings"

// matterOpeningParagraphs is how many of a chapter's first paragraphs
// MatterSections reads for its opening.
const matterOpeningParagraphs = 6

// MatterSection is one chapter as a book's matter pass reads it
// (MatterSections).
type MatterSection struct {
	ID       string
	Idx      int
	Title    string
	MatterBy string
	// Chars is the length of the chapter's text.
	Chars int
	// Opening is the chapter's first paragraphs.
	Opening []string
}

// MatterSections returns bookID's chapters in order as its matter pass
// reads them: title, length, and how each begins.
func (s *SQLiteStore) MatterSections(bookID string) ([]MatterSection, error) {
	rows, err := s.db.Query(`
		SELECT c.id, c.idx, c.title, c.matter_by, COALESCE(t.chars, 0), p.content
		FROM chapters c
		LEFT JOIN chapter_text t ON t.chapter_id = c.id
		LEFT JOIN paragraphs p ON p.chapter_id = c.id AND p.idx < ?
		WHERE c.book_id = ?
		ORDER BY c.idx, p.idx`, matterOpeningParagraphs, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MatterSection
	for rows.Next() {
		var m MatterSection
		var content *string
		if err := rows.Scan(&m.ID, &m.Idx, &m.Title, &m.MatterBy, &m.Chars, &content); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != m.ID {
			out = append(out, m)
		}
		if content != nil {
			out[len(out)-1].Opening = append(out[len(out)-1].Opening, *content)
		}
	}
	return out, rows.Err()
}

// needsMatterPass is the books whose matter pass has something to do: a
// chapter nobody has judged, in a book that didn't come from a website (a
// serial's chapter list is its chapters; what isn't story there is an
// author's notice, which titles don't tell apart).
const needsMatterPass = `EXISTS (SELECT 1 FROM chapters c WHERE c.book_id = b.id AND c.matter_by = '')
	AND NOT EXISTS (SELECT 1 FROM book_sources bs WHERE bs.book_id = b.id)`

// BookNeedsMatterPass reports whether bookID's matter pass should run:
// see needsMatterPass.
func (s *SQLiteStore) BookNeedsMatterPass(bookID string) (bool, error) {
	var need bool
	err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM books b WHERE b.id = ? AND `+needsMatterPass+`)`, bookID).Scan(&need)
	return need, err
}

// BooksNeedingMatterPass lists the books whose matter pass should run.
func (s *SQLiteStore) BooksNeedingMatterPass() ([]string, error) {
	rows, err := s.db.Query(`SELECT b.id FROM books b WHERE ` + needsMatterPass + ` ORDER BY b.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetBookMatter stores a matter pass's answer for bookID: the chapters in
// matterChapterIDs are front or back matter and every other one is story.
// A chapter the reader has set (MatterByReader) keeps what they said.
func (s *SQLiteStore) SetBookMatter(bookID string, matterChapterIDs []string) error {
	args := []any{MatterByPass, bookID, MatterByReader}
	in := "0"
	if len(matterChapterIDs) > 0 {
		in = "id IN (?" + strings.Repeat(", ?", len(matterChapterIDs)-1) + ")"
		args = []any{}
		for _, id := range matterChapterIDs {
			args = append(args, id)
		}
		args = append(args, MatterByPass, bookID, MatterByReader)
	}
	_, err := s.db.execScoped(bookID,
		`UPDATE chapters SET matter = (`+in+`), matter_by = ? WHERE book_id = ? AND matter_by <> ?`, args...)
	return err
}

// SetChapterMatter is the reader saying chapterID is, or isn't, front or
// back matter - which the book's matter pass then leaves alone.
func (s *SQLiteStore) SetChapterMatter(chapterID string, matter bool) error {
	book := s.chapterBook(chapterID)
	_, err := s.db.execScoped(book,
		`UPDATE chapters SET matter = ?, matter_by = ? WHERE id = ?`, matter, MatterByReader, chapterID)
	return err
}
