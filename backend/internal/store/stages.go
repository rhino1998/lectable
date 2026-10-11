package store

import (
	"database/sql"
	"fmt"
	"time"
)

// CharacterStage is a character's voice from one chapter on: the casting
// instruction (Instruct) their lines are voiced from once the story has
// reached ChapterID, written for them at about Age years old. Stages are
// how a character who grows up or grows old over a book or series changes
// voice - a character with none sounds the same throughout, from
// Character.Summary. A stage holds until the character's next one, across
// the rest of its book and every later book of the series; see
// narration.StageVoice for how it becomes a voice.
type CharacterStage struct {
	ID          string
	CharacterID string
	BookID      string
	ChapterID   string
	// ChapterIdx is ChapterID's current place in its book, read with the
	// stage rather than stored.
	ChapterIdx int
	// Age is about how old the character is from here on, in years.
	Age      int
	Instruct string
	// JumpID is the TimeJump that made this stage, "" for one the reader
	// set for this character alone.
	JumpID string
	// Take counts the times the reader has asked for this stage's voice to
	// be rendered afresh: each is another seed, so another reading of the
	// same casting.
	Take      int
	CreatedAt int64
}

// TimeJump says years pass in the story before ChapterID of BookID - for
// everyone, where a CharacterStage is one character's. Applying one
// (httpapi) gives every character it ages a stage there, marked with its
// id; removing it removes them.
type TimeJump struct {
	ID        string
	BookID    string
	ChapterID string
	// ChapterIdx is ChapterID's current place in its book.
	ChapterIdx int
	Years      int
	CreatedAt  int64
}

// ListCharacterStages returns every stage of every character in scope
// (SeriesScope), in no particular order. A stage whose chapter no longer
// exists is left out.
func (s *SQLiteStore) ListCharacterStages(scope string) ([]CharacterStage, error) {
	rows, err := s.db.Query(`
		SELECT st.id, st.character_id, st.book_id, st.chapter_id, c.idx, st.age, st.instruct, st.jump_id, st.take, st.created_at
		FROM character_stages st
		JOIN characters ch ON ch.id = st.character_id
		JOIN chapters c ON c.id = st.chapter_id
		WHERE ch.scope = ?
		ORDER BY st.created_at ASC, st.id ASC`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CharacterStage
	for rows.Next() {
		var st CharacterStage
		if err := rows.Scan(&st.ID, &st.CharacterID, &st.BookID, &st.ChapterID, &st.ChapterIdx, &st.Age, &st.Instruct, &st.JumpID, &st.Take, &st.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// CharacterStageScopes returns the roster scopes (SeriesScope) in which
// some character has a stage - few, so a caller resolving voices for many
// books can skip the rest without reading anything about them.
func (s *SQLiteStore) CharacterStageScopes() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT DISTINCT ch.scope FROM character_stages st JOIN characters ch ON ch.id = st.character_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, err
		}
		out[scope] = true
	}
	return out, rows.Err()
}

// GetCharacterStage returns the stage with id, or nil.
func (s *SQLiteStore) GetCharacterStage(id string) (*CharacterStage, error) {
	var st CharacterStage
	err := s.db.QueryRow(`
		SELECT st.id, st.character_id, st.book_id, st.chapter_id, COALESCE(c.idx, 0), st.age, st.instruct, st.jump_id, st.take, st.created_at
		FROM character_stages st LEFT JOIN chapters c ON c.id = st.chapter_id
		WHERE st.id = ?`, id).Scan(&st.ID, &st.CharacterID, &st.BookID, &st.ChapterID, &st.ChapterIdx, &st.Age, &st.Instruct, &st.JumpID, &st.Take, &st.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// SetCharacterStage creates characterID's stage starting at chapterID, or
// replaces the age and instruction of the one already starting there, and
// returns it. jumpID is the TimeJump a new stage is made for ("" for one
// set by hand); a stage that already exists keeps whichever it had.
func (s *SQLiteStore) SetCharacterStage(characterID, bookID, chapterID string, age int, instruct, jumpID string) (*CharacterStage, error) {
	_, err := s.db.Exec(`
		INSERT INTO character_stages (id, character_id, book_id, chapter_id, age, instruct, jump_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (character_id, chapter_id) DO UPDATE SET age = excluded.age, instruct = excluded.instruct`,
		NewID(), characterID, bookID, chapterID, age, instruct, jumpID, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	var id string
	if err := s.db.QueryRow(`SELECT id FROM character_stages WHERE character_id = ? AND chapter_id = ?`, characterID, chapterID).Scan(&id); err != nil {
		return nil, err
	}
	st, err := s.GetCharacterStage(id)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, fmt.Errorf("character stage %s not found after write", id)
	}
	return st, nil
}

// NextCharacterStageTake moves the stage with id on to its next take (see
// CharacterStage.Take).
func (s *SQLiteStore) NextCharacterStageTake(id string) error {
	_, err := s.db.Exec(`UPDATE character_stages SET take = take + 1 WHERE id = ?`, id)
	return err
}

// ChapterIdxByID maps each of bookID's chapter ids to its index.
func (s *SQLiteStore) ChapterIdxByID(bookID string) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT id, idx FROM chapters WHERE book_id = ?`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var idx int
		if err := rows.Scan(&id, &idx); err != nil {
			return nil, err
		}
		out[id] = idx
	}
	return out, rows.Err()
}

// DeleteCharacterStage removes the stage with id.
func (s *SQLiteStore) DeleteCharacterStage(id string) error {
	_, err := s.db.Exec(`DELETE FROM character_stages WHERE id = ?`, id)
	return err
}

// ListTimeJumps returns bookID's time jumps in chapter order. One whose
// chapter no longer exists is left out.
func (s *SQLiteStore) ListTimeJumps(bookID string) ([]TimeJump, error) {
	rows, err := s.db.Query(`
		SELECT j.id, j.book_id, j.chapter_id, c.idx, j.years, j.created_at
		FROM book_time_jumps j JOIN chapters c ON c.id = j.chapter_id
		WHERE j.book_id = ?
		ORDER BY c.idx ASC`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimeJump
	for rows.Next() {
		var j TimeJump
		if err := rows.Scan(&j.ID, &j.BookID, &j.ChapterID, &j.ChapterIdx, &j.Years, &j.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// GetTimeJump returns the time jump with id, or nil.
func (s *SQLiteStore) GetTimeJump(id string) (*TimeJump, error) {
	var j TimeJump
	err := s.db.QueryRow(`
		SELECT j.id, j.book_id, j.chapter_id, COALESCE(c.idx, 0), j.years, j.created_at
		FROM book_time_jumps j LEFT JOIN chapters c ON c.id = j.chapter_id
		WHERE j.id = ?`, id).Scan(&j.ID, &j.BookID, &j.ChapterID, &j.ChapterIdx, &j.Years, &j.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// CreateTimeJump records that years pass before chapterID of bookID and
// returns the jump. created is false when that chapter already has one,
// which is returned as it stands.
func (s *SQLiteStore) CreateTimeJump(bookID, chapterID string, years int) (jump *TimeJump, created bool, err error) {
	res, err := s.db.Exec(`
		INSERT INTO book_time_jumps (id, book_id, chapter_id, years, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (book_id, chapter_id) DO NOTHING`,
		NewID(), bookID, chapterID, years, time.Now().Unix())
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	var id string
	if err := s.db.QueryRow(`SELECT id FROM book_time_jumps WHERE book_id = ? AND chapter_id = ?`, bookID, chapterID).Scan(&id); err != nil {
		return nil, false, err
	}
	if jump, err = s.GetTimeJump(id); err != nil {
		return nil, false, err
	}
	if jump == nil {
		return nil, false, fmt.Errorf("time jump %s not found after write", id)
	}
	return jump, n > 0, nil
}

// DeleteTimeJump removes the time jump with id and every stage made for it.
func (s *SQLiteStore) DeleteTimeJump(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM character_stages WHERE jump_id = ?`, id); err != nil {
		return fmt.Errorf("delete the jump's stages: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM book_time_jumps WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete time jump: %w", err)
	}
	return tx.Commit()
}

// TimeJumpStageCounts returns, for each of bookID's time jumps that made
// any, how many stages it made.
func (s *SQLiteStore) TimeJumpStageCounts(bookID string) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT jump_id, COUNT(*) FROM character_stages WHERE book_id = ? AND jump_id != '' GROUP BY jump_id`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// SpeakersInChapters returns the names bookID's dialogue is attributed to
// in its chapters from index fromIdx up to but not including toIdx.
func (s *SQLiteStore) SpeakersInChapters(bookID string, fromIdx, toIdx int) ([]string, error) {
	rows, err := s.db.Query(`
		SELECT DISTINCT p.speaker
		FROM chapters c JOIN paragraphs p ON p.chapter_id = c.id
		WHERE c.book_id = ? AND c.idx >= ? AND c.idx < ? AND p.speaker != ''`, bookID, fromIdx, toIdx)
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

// ChapterTextRevs maps each of bookID's chapter ids to its chapter_text
// content_rev - a value that changes whenever one of the chapter's
// paragraphs is added, removed or reworded (and not when one is given
// another speaker). What the semantic index compares to find the chapters
// it has to read again.
func (s *SQLiteStore) ChapterTextRevs(bookID string) (map[string]int64, error) {
	all, err := s.chapterTextRevs(`WHERE c.book_id = ?`, bookID)
	return all[bookID], err
}

// AllChapterTextRevs is ChapterTextRevs for every book at once, by book id.
func (s *SQLiteStore) AllChapterTextRevs() (map[string]map[string]int64, error) {
	return s.chapterTextRevs(``)
}

func (s *SQLiteStore) chapterTextRevs(where string, args ...any) (map[string]map[string]int64, error) {
	rows, err := s.db.Query(`
		SELECT c.book_id, c.id, COALESCE(t.content_rev, 0)
		FROM chapters c
		LEFT JOIN chapter_text t ON t.chapter_id = c.id `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int64{}
	for rows.Next() {
		var bookID, id string
		var rev int64
		if err := rows.Scan(&bookID, &id, &rev); err != nil {
			return nil, err
		}
		if out[bookID] == nil {
			out[bookID] = map[string]int64{}
		}
		out[bookID][id] = rev
	}
	return out, rows.Err()
}
