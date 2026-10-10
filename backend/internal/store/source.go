package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// BookSource is one place a book's chapters come from: a website it was
// imported from or that was added to it since, and how the last check for
// new chapters there went. A book can track several (BookSources); one
// from an uploaded epub has none until a source is added.
type BookSource struct {
	// ID names this row; assigned by SaveBookSource.
	ID     string
	BookID string
	// Kind names the site ("royalroad"), SourceID the work's id there
	// (a fiction's number, "vol-3").
	Kind     string
	SourceID string
	URL      string
	// Prefix goes before this source's chapter ids wherever the book
	// stores them (chapters.source_id, the plan): "" for the source a
	// book was imported from, "<site>:" for one added later.
	Prefix string
	// CheckedAt is when the source was last checked (unix seconds), 0 if
	// never.
	CheckedAt int64
	// RemoteChapters is how many chapters the book would have with what
	// the source offered at the last check: the book's own plus any still
	// to be fetched.
	RemoteChapters int
	// Error is why the last check stopped early, "" if it finished.
	Error string
	// Blocked is the title of the chapter the last check stopped at
	// because the source lists it but doesn't serve it yet, "" if none.
	Blocked string
}

const bookSourceColumns = `id, book_id, kind, source_id, url, prefix, checked_at, remote_chapters, error, blocked`

func scanBookSource(row interface{ Scan(...any) error }) (BookSource, error) {
	var src BookSource
	err := row.Scan(&src.ID, &src.BookID, &src.Kind, &src.SourceID, &src.URL, &src.Prefix, &src.CheckedAt, &src.RemoteChapters, &src.Error, &src.Blocked)
	return src, err
}

// SaveBookSource stores src: a new row for its BookID when it has no ID
// (one is assigned), otherwise that row's new state. Its plan is kept
// apart (SetBookSourcePlan).
func (s *SQLiteStore) SaveBookSource(src *BookSource) error {
	if src.ID == "" {
		id := NewID()
		if _, err := s.db.Exec(
			`INSERT INTO book_sources (id, book_id, kind, source_id, url, prefix, checked_at, remote_chapters, error, blocked, added_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, (SELECT COALESCE(MAX(added_at), 0) + 1 FROM book_sources WHERE book_id = ?))`,
			id, src.BookID, src.Kind, src.SourceID, src.URL, src.Prefix, src.CheckedAt, src.RemoteChapters, src.Error, src.Blocked, src.BookID); err != nil {
			return err
		}
		src.ID = id
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE book_sources SET kind = ?, source_id = ?, url = ?, checked_at = ?, remote_chapters = ?, error = ?, blocked = ? WHERE id = ?`,
		src.Kind, src.SourceID, src.URL, src.CheckedAt, src.RemoteChapters, src.Error, src.Blocked, src.ID)
	return err
}

// BookSources returns the sources bookID tracks, in the order they were
// added; none for a book from an epub (or no such book).
func (s *SQLiteStore) BookSources(bookID string) ([]BookSource, error) {
	rows, err := s.db.Query(`SELECT `+bookSourceColumns+` FROM book_sources WHERE book_id = ? ORDER BY added_at, rowid`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BookSource
	for rows.Next() {
		src, err := scanBookSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// GetBookSource returns one source by its id, nil if there is none.
func (s *SQLiteStore) GetBookSource(id string) (*BookSource, error) {
	src, err := scanBookSource(s.db.QueryRow(`SELECT `+bookSourceColumns+` FROM book_sources WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &src, nil
}

// DeleteBookSource stops tracking a source. The chapters the book took
// from it stay, with their ids.
func (s *SQLiteStore) DeleteBookSource(id string) error {
	_, err := s.db.Exec(`DELETE FROM book_sources WHERE id = ?`, id)
	return err
}

// SourcePlan is which chapters of one source a book takes, when that isn't
// simply "all the source lists" - see the book_sources migration.
type SourcePlan struct {
	// Skip are the source ids of chapters the reader left out.
	Skip []string `json:"skip,omitempty"`
	// Extra are chosen chapters the source no longer lists, in reading
	// order: what a check needs to fetch them from an archive.
	Extra []PlannedChapter `json:"extra,omitempty"`
	// Approved are the source ids the reader has agreed to take: the ones
	// chosen at import, and each entry found since that they said to
	// import. Anything else a check finds waits in Pending. Reviewed is
	// false for a plan from before entries were reviewed at all - its
	// next check approves everything listed then.
	Approved []string `json:"approved,omitempty"`
	Reviewed bool     `json:"reviewed,omitempty"`
	// Pending are entries the last check found that the reader hasn't
	// decided on, in reading order. The book stops before the first.
	Pending []PlannedChapter `json:"pending,omitempty"`
	// Title, if set, says which of the source's entries are chapters and
	// what each is called (TitleRule).
	Title *TitleRule `json:"title,omitempty"`
	// Matched are the source ids of entries the book already has from
	// another of its sources (the same chapter posted in two places), so
	// no check fetches them again.
	Matched []string `json:"matched,omitempty"`
}

// TitleRule is a regular expression over the titles a source lists its
// entries under, for a source that posts more than chapters or names them
// its own way.
type TitleRule struct {
	// Pattern is the expression (Go syntax), matched anywhere in a title.
	Pattern string `json:"pattern"`
	// Output, if not empty, is what an entry whose title matches is
	// called instead: a template over the match's capture groups ("Chapter
	// $1", "${name}").
	Output string `json:"output,omitempty"`
	// Only leaves out every entry whose title doesn't match.
	Only bool `json:"only,omitempty"`
}

// PlannedChapter is one chapter of a SourcePlan's Extra or Pending.
type PlannedChapter struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
	// Archived is when the archive saved its page, if known.
	Archived string `json:"archived,omitempty"`
	// Suspect marks a pending entry whose title reads like an author's
	// notice rather than a chapter.
	Suspect bool `json:"suspect,omitempty"`
	// Unreadable marks a pending entry that was agreed to but couldn't be
	// read: a delisted chapter the archive has no usable copy of. The
	// reader says skip it (the book goes on without) or try again.
	Unreadable bool `json:"unreadable,omitempty"`
	// Locked marks a pending entry the source lists but won't serve to
	// this reader (patrons only, early access): it can't be imported
	// until it opens.
	Locked bool `json:"locked,omitempty"`
}

// SetBookSourcePlan stores the plan of the source with that id, replacing
// any it had.
func (s *SQLiteStore) SetBookSourcePlan(sourceID string, plan SourcePlan) error {
	raw, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE book_sources SET plan = ? WHERE id = ?`, string(raw), sourceID)
	return err
}

// GetBookSourcePlan returns the plan of the source with that id, empty
// when it has none.
func (s *SQLiteStore) GetBookSourcePlan(sourceID string) (SourcePlan, error) {
	var raw string
	err := s.db.QueryRow(`SELECT plan FROM book_sources WHERE id = ?`, sourceID).Scan(&raw)
	if err == sql.ErrNoRows {
		return SourcePlan{}, nil
	}
	if err != nil {
		return SourcePlan{}, err
	}
	var plan SourcePlan
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &plan); err != nil {
			return SourcePlan{}, fmt.Errorf("read source plan: %w", err)
		}
	}
	return plan, nil
}

// FindBookBySource returns the id of a book tracking that source, "" if
// the library has none.
func (s *SQLiteStore) FindBookBySource(kind, sourceID string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT book_id FROM book_sources WHERE kind = ? AND source_id = ? ORDER BY added_at, rowid LIMIT 1`, kind, sourceID).Scan(&id)
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

// ChapterSources lists every chapter of bookID with the source ids it
// holds ("" for one that came from an epub), in chapter order.
func (s *SQLiteStore) ChapterSources(bookID string) ([]SourcedChapter, error) {
	rows, err := s.db.Query(`SELECT idx, title, source_id FROM chapters WHERE book_id = ? ORDER BY idx`, bookID)
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

// SourceCheckHours is how often, in hours, books imported from websites
// are checked for new entries without being asked: 0 never. Library-wide.
func (s *SQLiteStore) SourceCheckHours() (int, error) {
	var hours int
	err := s.db.QueryRow(`SELECT source_check_hours FROM default_voice WHERE id = 0`).Scan(&hours)
	return hours, err
}

// SetSourceCheckHours sets SourceCheckHours.
func (s *SQLiteStore) SetSourceCheckHours(hours int) error {
	_, err := s.db.Exec(`UPDATE default_voice SET source_check_hours = ? WHERE id = 0`, hours)
	return err
}

// PatreonSession is the Patreon session books are read with - a signed-in
// browser's session_id cookie - and the name of the account it belongs
// to; both "" when not signed in. Library-wide.
func (s *SQLiteStore) PatreonSession() (session, account string, err error) {
	err = s.db.QueryRow(`SELECT patreon_session, patreon_account FROM default_voice WHERE id = 0`).Scan(&session, &account)
	return session, account, err
}

// SetPatreonSession sets PatreonSession.
func (s *SQLiteStore) SetPatreonSession(session, account string) error {
	_, err := s.db.Exec(`UPDATE default_voice SET patreon_session = ?, patreon_account = ? WHERE id = 0`, session, account)
	return err
}

// SourcedBook is a book that tracks at least one source, and when the
// least recently checked of them was checked.
type SourcedBook struct {
	BookID    string
	CheckedAt int64
}

// SourcedBooks lists every book that has a source, least recently checked
// first.
func (s *SQLiteStore) SourcedBooks() ([]SourcedBook, error) {
	rows, err := s.db.Query(`SELECT book_id, MIN(checked_at) FROM book_sources GROUP BY book_id ORDER BY 2, MIN(rowid)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SourcedBook
	for rows.Next() {
		var b SourcedBook
		if err := rows.Scan(&b.BookID, &b.CheckedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ChapterOpening is how one chapter starts: see ChapterOpenings.
type ChapterOpening struct {
	Idx      int
	Title    string
	SourceID string
	Text     string
}

// ChapterOpenings returns how each of bookID's chapters opens, in order:
// its first paragraphs joined, up to about limit characters. For telling
// which chapter of another edition a chapter is.
func (s *SQLiteStore) ChapterOpenings(bookID string, limit int) ([]ChapterOpening, error) {
	rows, err := s.db.Query(`
		SELECT c.idx, c.title, c.source_id, p.content
		FROM chapters c JOIN paragraphs p ON p.chapter_id = c.id
		WHERE c.book_id = ? AND p.idx < 12
		ORDER BY c.idx, p.idx`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChapterOpening
	for rows.Next() {
		var o ChapterOpening
		var content string
		if err := rows.Scan(&o.Idx, &o.Title, &o.SourceID, &content); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Idx != o.Idx {
			out = append(out, o)
		}
		last := &out[len(out)-1]
		// The title is the chapter's first paragraph; it isn't its text.
		if content != o.Title && len(last.Text) < limit {
			last.Text += content + " "
		}
	}
	return out, rows.Err()
}
