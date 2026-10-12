package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ProposalStatus says where something a pass found stands with the
// reader: nothing found automatically is in force until they accept it.
type ProposalStatus string

const (
	ProposalPending  ProposalStatus = "pending"
	ProposalAccepted ProposalStatus = "accepted"
	// ProposalSkipped is turned down: kept, out of sight, so the same thing
	// isn't proposed again.
	ProposalSkipped ProposalStatus = "skipped"
)

// TimeJumpProposal is a time jump a pass found and the reader hasn't
// ruled on: Years seem to pass before ChapterID of BookID, by the
// sentences in Evidence (one a line, each "ch N: ..."). Accepting it makes
// a TimeJump - at the chapter and length the reader settles on, which may
// not be these.
type TimeJumpProposal struct {
	ID         string
	BookID     string
	ChapterID  string
	ChapterIdx int
	Years      int
	Evidence   string
	Status     ProposalStatus
}

// ProposeTimeJump records a found time jump as pending, reporting whether
// it now waits for the reader. A chapter with a proposal already keeps it:
// one still pending is refreshed, one accepted or skipped is left alone.
func (s *SQLiteStore) ProposeTimeJump(bookID, chapterID string, years int, evidence string) (bool, error) {
	res, err := s.db.Exec(`
		INSERT INTO time_jump_proposals (id, book_id, chapter_id, years, evidence, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (book_id, chapter_id) DO UPDATE SET years = excluded.years, evidence = excluded.evidence
		WHERE time_jump_proposals.status = ?`,
		NewID(), bookID, chapterID, years, evidence, string(ProposalPending), time.Now().Unix(), string(ProposalPending))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

const timeJumpProposalColumns = `p.id, p.book_id, p.chapter_id, c.idx, p.years, p.evidence, p.status`

// ListTimeJumpProposals returns every proposal for bookID, whatever its
// status, in chapter order. One whose chapter is gone is left out.
func (s *SQLiteStore) ListTimeJumpProposals(bookID string) ([]TimeJumpProposal, error) {
	rows, err := s.db.Query(`SELECT `+timeJumpProposalColumns+`
		FROM time_jump_proposals p JOIN chapters c ON c.id = p.chapter_id
		WHERE p.book_id = ? ORDER BY c.idx`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimeJumpProposal
	for rows.Next() {
		var p TimeJumpProposal
		if err := rows.Scan(&p.ID, &p.BookID, &p.ChapterID, &p.ChapterIdx, &p.Years, &p.Evidence, &p.Status); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetTimeJumpProposal returns the proposal with id, nil when there is none.
func (s *SQLiteStore) GetTimeJumpProposal(id string) (*TimeJumpProposal, error) {
	var p TimeJumpProposal
	err := s.db.QueryRow(`SELECT `+timeJumpProposalColumns+`
		FROM time_jump_proposals p JOIN chapters c ON c.id = p.chapter_id WHERE p.id = ?`, id).
		Scan(&p.ID, &p.BookID, &p.ChapterID, &p.ChapterIdx, &p.Years, &p.Evidence, &p.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SetTimeJumpProposalStatus rules on a proposal.
func (s *SQLiteStore) SetTimeJumpProposalStatus(id string, status ProposalStatus) error {
	_, err := s.db.Exec(`UPDATE time_jump_proposals SET status = ? WHERE id = ?`, string(status), id)
	return err
}

// CharacterNoteKind is what a CharacterNote says about its character.
type CharacterNoteKind string

const (
	// NoteKind: Value is the kind of being they are ("goblin").
	NoteKind CharacterNoteKind = "kind"
	// NoteNationality: Value is where they are from or whose they are - a
	// nation, city, tribe or faction ("Pallassian").
	NoteNationality CharacterNoteKind = "nationality"
	// NoteAge: Value is their age in years at ChapterID ("12").
	NoteAge CharacterNoteKind = "age"
)

// CharacterNote is one thing the text states about a character, with the
// sentence it rests on - an entry of their dossier. It is found by a pass
// (httpapi.FindCharacterNotes) and waits as pending; only an accepted one
// is shown to casting, and it is shown beside the character's material,
// never instead of it.
type CharacterNote struct {
	ID          string
	CharacterID string
	Kind        CharacterNoteKind
	Value       string
	// ChapterID is where Evidence is - and, for an age, when they were
	// that old.
	ChapterID string
	Evidence  string
	Status    ProposalStatus
}

// ProposeCharacterNote records a found note as pending, reporting whether
// it now waits for the reader. The same value for the same character is
// one note: still pending it is refreshed, accepted or skipped it is left
// alone.
func (s *SQLiteStore) ProposeCharacterNote(n CharacterNote) (bool, error) {
	res, err := s.db.Exec(`
		INSERT INTO character_notes (id, character_id, kind, value, chapter_id, evidence, status, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (character_id, kind, value) DO UPDATE SET chapter_id = excluded.chapter_id, evidence = excluded.evidence
		WHERE character_notes.status = ?`,
		NewID(), n.CharacterID, string(n.Kind), n.Value, n.ChapterID, n.Evidence, string(ProposalPending), time.Now().Unix(), string(ProposalPending))
	if err != nil {
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

const characterNoteColumns = `n.id, n.character_id, n.kind, n.value, n.chapter_id, n.evidence, n.status`

func scanCharacterNotes(rows *sql.Rows) ([]CharacterNote, error) {
	defer rows.Close()
	var out []CharacterNote
	for rows.Next() {
		var n CharacterNote
		if err := rows.Scan(&n.ID, &n.CharacterID, &n.Kind, &n.Value, &n.ChapterID, &n.Evidence, &n.Status); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListCharacterNotes returns every note about scope's characters,
// whatever its status, oldest first.
func (s *SQLiteStore) ListCharacterNotes(scope string) ([]CharacterNote, error) {
	rows, err := s.db.Query(`SELECT `+characterNoteColumns+`
		FROM character_notes n JOIN characters c ON c.id = n.character_id
		WHERE c.scope = ? ORDER BY n.created_at, n.rowid`, scope)
	if err != nil {
		return nil, err
	}
	return scanCharacterNotes(rows)
}

// CharacterNotes returns the accepted notes about one character, oldest
// first.
func (s *SQLiteStore) CharacterNotes(characterID string) ([]CharacterNote, error) {
	rows, err := s.db.Query(`SELECT `+characterNoteColumns+`
		FROM character_notes n WHERE n.character_id = ? AND n.status = ? ORDER BY n.created_at, n.rowid`,
		characterID, string(ProposalAccepted))
	if err != nil {
		return nil, err
	}
	return scanCharacterNotes(rows)
}

// GetCharacterNote returns the note with id, nil when there is none.
func (s *SQLiteStore) GetCharacterNote(id string) (*CharacterNote, error) {
	rows, err := s.db.Query(`SELECT `+characterNoteColumns+` FROM character_notes n WHERE n.id = ?`, id)
	if err != nil {
		return nil, err
	}
	notes, err := scanCharacterNotes(rows)
	if err != nil || len(notes) == 0 {
		return nil, err
	}
	return &notes[0], nil
}

// SetCharacterNoteStatus rules on a note.
func (s *SQLiteStore) SetCharacterNoteStatus(id string, status ProposalStatus) error {
	_, err := s.db.Exec(`UPDATE character_notes SET status = ? WHERE id = ?`, string(status), id)
	return err
}

// SetCharacterNoteValue makes value the one accepted note of kind about a
// character - what the reader says their race or nationality is: any
// other note of that kind, accepted or still proposed, is turned down
// (someone is of one race), and value's own note
// is accepted (made, with no evidence, when the text never said it). ""
// leaves the character with none.
func (s *SQLiteStore) SetCharacterNoteValue(characterID string, kind CharacterNoteKind, value string) error {
	value = strings.TrimSpace(value)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE character_notes SET status = ? WHERE character_id = ? AND kind = ? AND status != ? AND lower(value) != lower(?)`,
		string(ProposalSkipped), characterID, string(kind), string(ProposalSkipped), value); err != nil {
		return err
	}
	if value != "" {
		res, err := tx.Exec(`UPDATE character_notes SET status = ? WHERE character_id = ? AND kind = ? AND lower(value) = lower(?)`,
			string(ProposalAccepted), characterID, string(kind), value)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			if _, err := tx.Exec(`INSERT INTO character_notes (id, character_id, kind, value, status, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
				NewID(), characterID, string(kind), value, string(ProposalAccepted), time.Now().Unix()); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// SkipOtherCharacterNotes turns down every other note of keep's kind about
// its character, accepted or still proposed: accepting that someone is a
// goblin settles the proposals that they are a hobgoblin or a drake. They
// stay listed as skipped, to be reviewed again.
func (s *SQLiteStore) SkipOtherCharacterNotes(keep CharacterNote) error {
	_, err := s.db.Exec(`UPDATE character_notes SET status = ? WHERE character_id = ? AND kind = ? AND id != ? AND status != ?`,
		string(ProposalSkipped), keep.CharacterID, string(keep.Kind), keep.ID, string(ProposalSkipped))
	return err
}
