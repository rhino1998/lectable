package store

import (
	"database/sql"
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
)

// LexiconEntry is one stored pronunciation rule (pronounce.LexiconEntry),
// scoped like the character roster (SeriesScope) - a name recurs across a
// series and should be said the same way in every book of it.
type LexiconEntry struct {
	ID    string
	Scope string
	pronounce.LexiconEntry
	CreatedAt int64
}

// PronounceLexicon drops the storage fields, leaving the rules
// ResolveGenerationText applies.
func PronounceLexicon(entries []LexiconEntry) []pronounce.LexiconEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]pronounce.LexiconEntry, len(entries))
	for i, e := range entries {
		out[i] = e.LexiconEntry
	}
	return out
}

// ListLexicon returns scope's rules, alphabetical by term.
func (s *DuckStore) ListLexicon(scope string) ([]LexiconEntry, error) {
	rows, err := s.db.Query(
		`SELECT id, scope, term, replacement, case_sensitive, created_at
		 FROM pronunciation_lexicon WHERE scope = ? ORDER BY term_key, term`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LexiconEntry
	for rows.Next() {
		var e LexiconEntry
		if err := rows.Scan(&e.ID, &e.Scope, &e.Term, &e.Replacement, &e.CaseSensitive, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpsertLexiconEntry adds a rule to scope, or replaces the one already
// there for the same term (compared case-insensitively, so "Nyx" and "nyx"
// can't both exist). previous is the replaced rule, nil for a new one -
// callers use it to find paragraphs the old rule applied to.
func (s *DuckStore) UpsertLexiconEntry(scope string, e pronounce.LexiconEntry) (entry LexiconEntry, previous *LexiconEntry, err error) {
	key := strings.ToLower(e.Term)
	var prev LexiconEntry
	err = s.db.QueryRow(
		`SELECT id, scope, term, replacement, case_sensitive, created_at
		 FROM pronunciation_lexicon WHERE scope = ? AND term_key = ?`, scope, key,
	).Scan(&prev.ID, &prev.Scope, &prev.Term, &prev.Replacement, &prev.CaseSensitive, &prev.CreatedAt)
	switch {
	case err == nil:
		entry = LexiconEntry{ID: prev.ID, Scope: scope, LexiconEntry: e, CreatedAt: prev.CreatedAt}
		_, err = s.db.Exec(
			`UPDATE pronunciation_lexicon SET term = ?, replacement = ?, case_sensitive = ? WHERE id = ?`,
			e.Term, e.Replacement, e.CaseSensitive, prev.ID)
		return entry, &prev, err
	case err != sql.ErrNoRows:
		return LexiconEntry{}, nil, err
	}
	entry = LexiconEntry{ID: NewID(), Scope: scope, LexiconEntry: e, CreatedAt: time.Now().UnixMilli()}
	_, err = s.db.Exec(
		`INSERT INTO pronunciation_lexicon (id, scope, term_key, term, replacement, case_sensitive, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		entry.ID, scope, key, e.Term, e.Replacement, e.CaseSensitive, entry.CreatedAt)
	return entry, nil, err
}

// DeleteLexiconEntry removes a rule, returning it (nil if there was none).
func (s *DuckStore) DeleteLexiconEntry(id string) (*LexiconEntry, error) {
	var e LexiconEntry
	err := s.db.QueryRow(
		`SELECT id, scope, term, replacement, case_sensitive, created_at
		 FROM pronunciation_lexicon WHERE id = ?`, id,
	).Scan(&e.ID, &e.Scope, &e.Term, &e.Replacement, &e.CaseSensitive, &e.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`DELETE FROM pronunciation_lexicon WHERE id = ?`, id); err != nil {
		return nil, err
	}
	return &e, nil
}
