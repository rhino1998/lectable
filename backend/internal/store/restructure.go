package store

import (
	"database/sql"
	"fmt"
)

// Changing which chapters a book has, and in what order, after import:
// InsertChapters, MoveChapter and DeleteChapter. Nearly everything about a
// chapter hangs off its id, so moving one only renumbers chapters.idx;
// what names a chapter by idx instead - the reading position, the recap's
// position - follows the chapter it named, and the sync tree (a cache
// keyed by idx) is dropped for the book.

// InsertChapters adds chapters to bookID starting at idx at (0 puts them
// first, the chapter count appends); the chapters from there on move down.
// Returns each image's assigned id, like AppendChapters.
func (s *SQLiteStore) InsertChapters(bookID string, at int, chapters []ChapterInput) (images []CreatedImage, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	order, err := chapterOrder(tx, bookID)
	if err != nil {
		return nil, err
	}
	if at < 0 || at > len(order) {
		return nil, fmt.Errorf("no place %d in a book of %d chapters", at, len(order))
	}
	// Out of the way first: idx is unique within a book.
	if err := parkChapters(tx, bookID); err != nil {
		return nil, err
	}
	fresh := make([]string, len(chapters))
	for i, ch := range chapters {
		fresh[i] = NewID()
		if _, err := tx.Exec(`INSERT INTO chapters (id, book_id, idx, title, source_id) VALUES (?, ?, ?, ?, ?)`,
			fresh[i], bookID, at+i, ch.Title, ch.SourceID); err != nil {
			return nil, fmt.Errorf("insert chapter: %w", err)
		}
		chImages, err := insertChapterBlocks(tx, fresh[i], ch.Blocks)
		if err != nil {
			return nil, err
		}
		images = append(images, chImages...)
	}
	after := append(append(append([]string{}, order[:at]...), fresh...), order[at:]...)
	if err := renumberChapters(tx, bookID, order, after); err != nil {
		return nil, err
	}
	return images, tx.Commit()
}

// MoveChapter moves bookID's chapter at idx from to idx to; the chapters
// between shift by one to make room.
func (s *SQLiteStore) MoveChapter(bookID string, from, to int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	order, err := chapterOrder(tx, bookID)
	if err != nil {
		return err
	}
	if from < 0 || from >= len(order) || to < 0 || to >= len(order) {
		return fmt.Errorf("no such place in a book of %d chapters", len(order))
	}
	if from == to {
		return nil
	}
	after := append([]string{}, order[:from]...)
	after = append(after, order[from+1:]...)
	after = append(after[:to], append([]string{order[from]}, after[to:]...)...)
	if err := parkChapters(tx, bookID); err != nil {
		return err
	}
	if err := renumberChapters(tx, bookID, order, after); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteChapter removes bookID's chapter at idx with everything of its
// own - paragraphs, audio rows, bookmarks, music regions, summary - and
// closes the gap. Returns the chapter's id and images, for the caller to
// remove its files. A reader inside it is left at the start of the
// chapter that follows.
func (s *SQLiteStore) DeleteChapter(bookID string, idx int) (chapterID string, images []CreatedImage, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", nil, err
	}
	defer tx.Rollback()
	order, err := chapterOrder(tx, bookID)
	if err != nil {
		return "", nil, err
	}
	if idx < 0 || idx >= len(order) {
		return "", nil, fmt.Errorf("no chapter %d in a book of %d", idx, len(order))
	}
	if len(order) == 1 {
		return "", nil, fmt.Errorf("a book keeps at least one chapter")
	}
	chapterID = order[idx]
	rows, err := tx.Query(`SELECT id, ext FROM images WHERE chapter_id = ?`, chapterID)
	if err != nil {
		return "", nil, fmt.Errorf("list images: %w", err)
	}
	for rows.Next() {
		img := CreatedImage{ChapterID: chapterID}
		if err := rows.Scan(&img.ImageID, &img.Ext); err != nil {
			rows.Close()
			return "", nil, err
		}
		images = append(images, img)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	for _, stmt := range []struct{ what, sql string }{
		{"bookmarks", `DELETE FROM bookmarks WHERE paragraph_id IN (SELECT id FROM paragraphs WHERE chapter_id = ?)`},
		{"paragraph edits", `DELETE FROM paragraph_edits WHERE paragraph_id IN (SELECT id FROM paragraphs WHERE chapter_id = ?)`},
		{"paragraph audio", `DELETE FROM paragraph_audio WHERE paragraph_id IN (SELECT id FROM paragraphs WHERE chapter_id = ?)`},
		{"sfx", `DELETE FROM sfx WHERE paragraph_id IN (SELECT id FROM paragraphs WHERE chapter_id = ?)`},
		{"paragraphs", `DELETE FROM paragraphs WHERE chapter_id = ?`},
		{"images", `DELETE FROM images WHERE chapter_id = ?`},
		{"breaks", `DELETE FROM breaks WHERE chapter_id = ?`},
		{"music regions", `DELETE FROM music_regions WHERE chapter_id = ?`},
		{"summary", `DELETE FROM chapter_summaries WHERE chapter_id = ?`},
	} {
		if _, err := tx.Exec(stmt.sql, chapterID); err != nil {
			return "", nil, fmt.Errorf("delete %s: %w", stmt.what, err)
		}
	}
	if err := parkChapters(tx, bookID); err != nil {
		return "", nil, err
	}
	if _, err := tx.Exec(`DELETE FROM chapters WHERE id = ?`, chapterID); err != nil {
		return "", nil, fmt.Errorf("delete chapter: %w", err)
	}
	after := append(append([]string{}, order[:idx]...), order[idx+1:]...)
	if err := renumberChapters(tx, bookID, order, after); err != nil {
		return "", nil, err
	}
	return chapterID, images, tx.Commit()
}

// chapterOrder lists bookID's chapter ids in order.
func chapterOrder(tx *notifyTx, bookID string) ([]string, error) {
	rows, err := tx.Query(`SELECT id FROM chapters WHERE book_id = ? ORDER BY idx`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("book %s has no chapters", bookID)
	}
	return ids, nil
}

// parkChapters moves every chapter of bookID to a negative idx, so that
// renumbering can't trip over the (book_id, idx) uniqueness part-way.
func parkChapters(tx *notifyTx, bookID string) error {
	if _, err := tx.Exec(`UPDATE chapters SET idx = -1 - idx WHERE book_id = ? AND idx >= 0`, bookID); err != nil {
		return fmt.Errorf("renumber chapters: %w", err)
	}
	return nil
}

// renumberChapters gives bookID's chapters the idxs of after (every
// chapter the book now has, in order) and carries along what named a
// chapter of before by its idx: the reading position and the recap's.
// A position in a chapter that is gone lands at the start of whichever
// chapter now sits where it was.
func renumberChapters(tx *notifyTx, bookID string, before, after []string) error {
	now := make(map[string]int, len(after))
	for i, id := range after {
		now[id] = i
		if _, err := tx.Exec(`UPDATE chapters SET idx = ? WHERE id = ?`, i, id); err != nil {
			return fmt.Errorf("renumber chapters: %w", err)
		}
	}
	var pos int
	if err := tx.QueryRow(`SELECT pos_chapter_idx FROM books WHERE id = ?`, bookID).Scan(&pos); err != nil {
		return fmt.Errorf("read position: %w", err)
	}
	if pos >= 0 && pos < len(before) {
		if to, kept := now[before[pos]]; kept {
			if _, err := tx.Exec(`UPDATE books SET pos_chapter_idx = ? WHERE id = ?`, to, bookID); err != nil {
				return fmt.Errorf("move position: %w", err)
			}
		} else if _, err := tx.Exec(`UPDATE books SET pos_chapter_idx = ?, pos_paragraph_idx = 0, pos_seconds = 0 WHERE id = ?`, min(pos, len(after)-1), bookID); err != nil {
			return fmt.Errorf("move position: %w", err)
		}
	}
	var recap int
	switch err := tx.QueryRow(`SELECT chapter_idx FROM book_recaps WHERE book_id = ?`, bookID).Scan(&recap); {
	case err == sql.ErrNoRows:
	case err != nil:
		return fmt.Errorf("read recap: %w", err)
	default:
		to, kept := -1, false
		if recap >= 0 && recap < len(before) {
			to, kept = now[before[recap]]
		}
		if kept {
			_, err = tx.Exec(`UPDATE book_recaps SET chapter_idx = ? WHERE book_id = ?`, to, bookID)
		} else {
			_, err = tx.Exec(`DELETE FROM book_recaps WHERE book_id = ?`, bookID)
		}
		if err != nil {
			return fmt.Errorf("move recap: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM sync_tree_nodes WHERE book_id = ?`, bookID); err != nil {
		return fmt.Errorf("drop sync tree: %w", err)
	}
	return nil
}

// RenameChapter gives a chapter a new title. The title is also the
// chapter's opening paragraph, read aloud with the rest: that paragraph
// is rewritten to match and its audio dropped, to be generated again.
// Nothing else about the chapter changes.
func (s *SQLiteStore) RenameChapter(chapterID, title string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var bookID, old string
	var idx int
	if err := tx.QueryRow(`SELECT book_id, idx, title FROM chapters WHERE id = ?`, chapterID).Scan(&bookID, &idx, &old); err != nil {
		return fmt.Errorf("look up chapter: %w", err)
	}
	if _, err := tx.Exec(`UPDATE chapters SET title = ? WHERE id = ?`, title, chapterID); err != nil {
		return fmt.Errorf("rename chapter: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM paragraph_audio WHERE paragraph_id IN (SELECT id FROM paragraphs WHERE chapter_id = ? AND idx = 0 AND content = ?)`, chapterID, old); err != nil {
		return fmt.Errorf("drop title audio: %w", err)
	}
	if _, err := tx.Exec(`UPDATE paragraphs SET content = ? WHERE chapter_id = ? AND idx = 0 AND content = ?`, title, chapterID, old); err != nil {
		return fmt.Errorf("rename title paragraph: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM sync_tree_nodes WHERE book_id = ? AND chapter_idx = ?`, bookID, idx); err != nil {
		return fmt.Errorf("drop sync tree node: %w", err)
	}
	return tx.Commit()
}
