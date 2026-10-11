package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
)

// WorldFact is one keyed fact about a story's world - "kobold": "a
// short-lived kind; thirteen is old for one" - scoped like the character
// roster (SeriesScope), or to every book (GlobalFactScope). The casting
// prompt is shown the facts whose Key or Aliases appear in a character's
// material (speakerattr.MatchWorldNotes).
type WorldFact struct {
	ID    string
	Scope string
	// Key is the word the fact is filed under and found by.
	Key string
	// Aliases are other words that bring the fact up ("kobolds" needs
	// none: a plain plural matches by itself).
	Aliases AliasList
	Fact    string
	Source  WorldFactSource
	Status  WorldFactStatus
	// CreatedAt and UpdatedAt are Unix milliseconds.
	CreatedAt int64
	UpdatedAt int64
}

// WorldFactSource says who wrote a WorldFact.
type WorldFactSource string

const (
	WorldFactManual WorldFactSource = "manual"
	WorldFactAgent  WorldFactSource = "agent"
	WorldFactLLM    WorldFactSource = "llm"
)

// WorldFactStatus says whether a WorldFact is in force. Only an active
// one is ever shown to a model: a proposed fact waits as pending for the
// reader to accept or skip.
type WorldFactStatus string

const (
	WorldFactActive  WorldFactStatus = "active"
	WorldFactPending WorldFactStatus = "pending"
	WorldFactSkipped WorldFactStatus = "skipped"
)

// GlobalFactScope is the scope of facts that hold in every book, unless
// the book's series has its own fact for the same key (see BookWorldFacts).
const GlobalFactScope = "global"

// ErrFactKeyTaken is UpsertWorldFact's error for moving a fact onto a key
// another fact in its scope already has.
var ErrFactKeyTaken = errors.New("another fact already has that key")

// Words is every word f is found by, lowercased: its key and aliases.
func (f WorldFact) Words() []string {
	out := []string{strings.ToLower(f.Key)}
	for _, a := range f.Aliases {
		out = append(out, strings.ToLower(a))
	}
	return out
}

// BookWorldFacts is every active fact that applies to book: its series'
// own, then the global ones no series fact supersedes - a global fact is
// superseded when it shares a key or alias with a series fact, so a series
// where dwarves aren't Scottish says so once under "dwarf". The series
// fact is then found by the superseded one's words too (returned among its
// Aliases): "dwarven" must not bring up nothing where it used to bring up
// the rule being replaced.
func BookWorldFacts(s Store, book *Book) ([]WorldFact, error) {
	series, err := s.ListWorldFacts(SeriesScope(book))
	if err != nil {
		return nil, err
	}
	global, err := s.ListWorldFacts(GlobalFactScope)
	if err != nil {
		return nil, err
	}
	var out []WorldFact
	// owner is the index in out of the series fact each word belongs to.
	owner := map[string]int{}
	for _, f := range series {
		if f.Status != WorldFactActive {
			continue
		}
		f.Aliases = slices.Clone(f.Aliases)
		for _, w := range f.Words() {
			if _, ok := owner[w]; !ok {
				owner[w] = len(out)
			}
		}
		out = append(out, f)
	}
	nSeries := len(out)
next:
	for _, f := range global {
		if f.Status != WorldFactActive {
			continue
		}
		for _, w := range f.Words() {
			i, ok := owner[w]
			if !ok || i >= nSeries {
				continue
			}
			have := out[i].Words()
			for _, extra := range append([]string{f.Key}, f.Aliases...) {
				if !slices.Contains(have, strings.ToLower(extra)) {
					out[i].Aliases = append(out[i].Aliases, extra)
				}
			}
			continue next
		}
		out = append(out, f)
	}
	return out, nil
}

const worldFactColumns = `id, scope, key, aliases, fact, source, status, created_at, updated_at`

func scanWorldFact(row interface{ Scan(...any) error }) (WorldFact, error) {
	var f WorldFact
	err := row.Scan(&f.ID, &f.Scope, &f.Key, &f.Aliases, &f.Fact, &f.Source, &f.Status, &f.CreatedAt, &f.UpdatedAt)
	return f, err
}

// ListWorldFacts returns scope's facts of every status, alphabetical by key.
func (s *SQLiteStore) ListWorldFacts(scope string) ([]WorldFact, error) {
	rows, err := s.db.Query(`SELECT `+worldFactColumns+` FROM world_facts WHERE scope = ? ORDER BY key_norm`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorldFact
	for rows.Next() {
		f, err := scanWorldFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpsertWorldFact saves f in scope. With f.ID set it rewrites that fact
// (its key included; ErrFactKeyTaken when another fact of the scope has
// the new key, nil when the id is gone); otherwise it adds f, or replaces
// the fact already filed under the same key, compared case-insensitively.
// Source and Status default to a reader's own active fact.
func (s *SQLiteStore) UpsertWorldFact(scope string, f WorldFact) (*WorldFact, error) {
	if f.Source == "" {
		f.Source = WorldFactManual
	}
	if f.Status == "" {
		f.Status = WorldFactActive
	}
	if f.Aliases == nil {
		f.Aliases = AliasList{}
	}
	key := strings.ToLower(f.Key)
	now := time.Now().UnixMilli()
	var holder string
	err := s.db.QueryRow(`SELECT id FROM world_facts WHERE scope = ? AND key_norm = ?`, scope, key).Scan(&holder)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	switch {
	case f.ID != "" && holder != "" && holder != f.ID:
		return nil, ErrFactKeyTaken
	case f.ID == "" && holder != "":
		f.ID = holder
	case f.ID == "":
		f.ID, f.Scope, f.CreatedAt, f.UpdatedAt = NewID(), scope, now, now
		_, err := s.db.Exec(
			`INSERT INTO world_facts (id, scope, key_norm, key, aliases, fact, source, status, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.ID, scope, key, f.Key, f.Aliases, f.Fact, string(f.Source), string(f.Status), now, now)
		if err != nil {
			return nil, err
		}
		return &f, nil
	}
	res, err := s.db.Exec(
		`UPDATE world_facts SET key_norm = ?, key = ?, aliases = ?, fact = ?, source = ?, status = ?, updated_at = ?
		 WHERE id = ? AND scope = ?`,
		key, f.Key, f.Aliases, f.Fact, string(f.Source), string(f.Status), now, f.ID, scope)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil
	}
	saved, err := scanWorldFact(s.db.QueryRow(`SELECT `+worldFactColumns+` FROM world_facts WHERE id = ?`, f.ID))
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

// DeleteWorldFact removes scope's fact id, reporting whether there was one.
func (s *SQLiteStore) DeleteWorldFact(scope, id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM world_facts WHERE id = ? AND scope = ?`, id, scope)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
