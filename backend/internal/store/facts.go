package store

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/voicefx"
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
	// Class says Key (and Aliases) is a word the roster rules must never
	// read as anyone's name (ClassedWords) - a title, a role, a race, a
	// nationality, a place; "" for a fact about anything else. Fact may be empty
	// for a classed word: the class is then all it says.
	Class WordClass
	// ClassProposed is a class a pass proposes for a fact that has none,
	// waiting for the reader: "" for none, and once they turned one down
	// that class behind WordClassDeclinedPrefix (see DeclinedClass).
	ClassProposed WordClass
	// Effect is a voice effect the lines of every character of this kind
	// are given once generated (narration.ResolvedVoice.Effect): the fact's
	// words are matched against a character's race. "" for none.
	Effect voicefx.VoiceEffect
	Source WorldFactSource
	Status WorldFactStatus
	// Evidence is the sentence of the book a proposed fact rests on, word
	// for word, and EvidenceChapterID the chapter it is in; both "" for a
	// fact the reader wrote.
	Evidence          string
	EvidenceChapterID string
	// CreatedAt and UpdatedAt are Unix milliseconds.
	CreatedAt int64
	UpdatedAt int64
}

// WordClass is what sort of word a WorldFact is filed under, for the
// roster rules.
type WordClass string

const (
	// WordTitle is a rank or office several people hold: "Prognugator
	// Tersk", "Prognugator Dekass".
	WordTitle WordClass = "title"
	// WordRole is a job, occupation or class, said of anyone who does it:
	// "Baker", "Guardsman".
	WordRole WordClass = "role"
	// WordRace is a kind of being: "Drake", "Gnoll".
	WordRace WordClass = "race"
	// WordNationality is where someone is from or whose they are - a
	// nation, city, tribe or faction: "Pallassian".
	WordNationality WordClass = "nationality"
	// WordPlace is the name of a place: "Pallass", "Khelt".
	WordPlace WordClass = "place"
)

// WordClassDeclinedPrefix marks WorldFact.ClassProposed as a class the
// reader turned down ("declined:race"): none is proposed for that fact
// again, until it is put back for review (ReviewWorldFact). wordClassNone
// is how a declined class was stored before it was kept: declined, class
// unknown.
const (
	WordClassDeclinedPrefix = "declined:"
	wordClassNone           = "none"
)

// DeclinedClass is the class the reader turned down for f and whether
// they turned one down at all - the class is "" for one declined before
// it was kept.
func (f WorldFact) DeclinedClass() (WordClass, bool) {
	if f.ClassProposed == wordClassNone {
		return "", true
	}
	if class, ok := strings.CutPrefix(string(f.ClassProposed), WordClassDeclinedPrefix); ok {
		return WordClass(class), true
	}
	return "", false
}

// WaitingClass is the class proposed for f and still waiting, "" for none
// (or one turned down).
func (f WorldFact) WaitingClass() WordClass {
	if _, declined := f.DeclinedClass(); declined {
		return ""
	}
	return f.ClassProposed
}

// Valid reports whether c is a class a fact can have, "" included.
func (c WordClass) Valid() bool {
	return c == "" || c == WordTitle || c == WordRole || c == WordRace || c == WordNationality || c == WordPlace
}

// WorldFactSource says who wrote a WorldFact.
type WorldFactSource string

const (
	WorldFactManual WorldFactSource = "manual"
	WorldFactAgent  WorldFactSource = "agent"
	WorldFactLLM    WorldFactSource = "llm"
	// WorldFactRule is a fact proposed by rules alone, no model (a title
	// several names share).
	WorldFactRule WorldFactSource = "rule"
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
	return ScopeWorldFacts(s, SeriesScope(book))
}

// ScopeWorldFacts is BookWorldFacts for a roster scope (SeriesScope).
func ScopeWorldFacts(s Store, scope string) ([]WorldFact, error) {
	series, err := s.ListWorldFacts(scope)
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
		// A series fact that only classes its word ("dwarf" is a race
		// here) says nothing in place of the global fact's text.
		if f.Fact != "" {
			for _, w := range f.Words() {
				if _, ok := owner[w]; !ok {
					owner[w] = len(out)
				}
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

// ClassedWords is the words book's facts in force give a class
// (WorldFact.Class), lowercased, each with its class: what
// speakerattr.NewWorldWords takes.
func ClassedWords(s Store, book *Book) (map[string]string, error) {
	facts, err := BookWorldFacts(s, book)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, f := range facts {
		if f.Class == "" {
			continue
		}
		for _, w := range f.Words() {
			if _, ok := out[w]; !ok {
				out[w] = string(f.Class)
			}
		}
	}
	return out, nil
}

const worldFactColumns = `id, scope, key, aliases, fact, word_class, word_class_proposed, voice_effect, source, status, evidence, evidence_chapter_id, created_at, updated_at`

func scanWorldFact(row interface{ Scan(...any) error }) (WorldFact, error) {
	var f WorldFact
	err := row.Scan(&f.ID, &f.Scope, &f.Key, &f.Aliases, &f.Fact, &f.Class, &f.ClassProposed, &f.Effect, &f.Source, &f.Status, &f.Evidence, &f.EvidenceChapterID, &f.CreatedAt, &f.UpdatedAt)
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
// Source and Status default to a reader's own active fact, except that a
// rewrite naming no source keeps the one the fact has.
func (s *SQLiteStore) UpsertWorldFact(scope string, f WorldFact) (*WorldFact, error) {
	// A rewrite that names no source keeps the fact's own: accepting a
	// proposal doesn't make it the reader's writing.
	keepSource := f.Source == ""
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
			`INSERT INTO world_facts (id, scope, key_norm, key, aliases, fact, word_class, voice_effect, source, status, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.ID, scope, key, f.Key, f.Aliases, f.Fact, string(f.Class), string(f.Effect), string(f.Source), string(f.Status), now, now)
		if err != nil {
			return nil, err
		}
		return &f, nil
	}
	res, err := s.db.Exec(
		`UPDATE world_facts SET key_norm = ?, key = ?, aliases = ?, fact = ?, word_class = ?, voice_effect = ?, word_class_proposed = CASE WHEN ? != '' THEN '' ELSE word_class_proposed END, source = CASE WHEN ? THEN source ELSE ? END, status = ?, updated_at = ?
		 WHERE id = ? AND scope = ?`,
		key, f.Key, f.Aliases, f.Fact, string(f.Class), string(f.Effect), string(f.Class), keepSource, string(f.Source), string(f.Status), now, f.ID, scope)
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

// ProposeWorldFact files f in scope as a pending fact for the reader to
// accept or skip, reporting whether it is now waiting. A key the scope
// already has a fact for is left alone - the reader's own, one they
// accepted, or one they skipped (so a fact turned down isn't proposed
// again) - except a proposal still pending, which is brought up to date,
// and a word skipped as one class when f says it is another (turned down
// as a title, "Pallassian" may still be proposed as a nationality).
func (s *SQLiteStore) ProposeWorldFact(scope string, f WorldFact) (bool, error) {
	if f.Source == "" {
		f.Source = WorldFactLLM
	}
	if f.Aliases == nil {
		f.Aliases = AliasList{}
	}
	now := time.Now().UnixMilli()
	res, err := s.db.Exec(
		`INSERT INTO world_facts (id, scope, key_norm, key, aliases, fact, word_class, source, status, evidence, evidence_chapter_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (scope, key_norm) DO UPDATE SET
		   key = excluded.key, aliases = excluded.aliases, fact = excluded.fact, word_class = excluded.word_class, source = excluded.source, status = excluded.status,
		   evidence = excluded.evidence, evidence_chapter_id = excluded.evidence_chapter_id, updated_at = excluded.updated_at
		 WHERE world_facts.status = ?
		    OR (world_facts.status = ? AND world_facts.fact = '' AND world_facts.word_class != '' AND world_facts.word_class != excluded.word_class)`,
		NewID(), scope, strings.ToLower(f.Key), f.Key, f.Aliases, f.Fact, string(f.Class), string(f.Source), string(WorldFactPending),
		f.Evidence, f.EvidenceChapterID, now, now, string(WorldFactPending), string(WorldFactSkipped))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetWorldFactStatus puts scope's fact id in status - skipping a proposal,
// say - reporting whether there was such a fact.
func (s *SQLiteStore) SetWorldFactStatus(scope, id string, status WorldFactStatus) (bool, error) {
	res, err := s.db.Exec(`UPDATE world_facts SET status = ?, updated_at = ? WHERE id = ? AND scope = ?`,
		string(status), time.Now().UnixMilli(), id, scope)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ProposeWorldFactClass proposes class for scope's fact id, for the
// reader to accept (UpsertWorldFact with the class) or decline
// (DeclineWorldFactClass), reporting whether it is now waiting. Only a
// fact in force with no class, none waiting and none declined takes one.
func (s *SQLiteStore) ProposeWorldFactClass(scope, id string, class WordClass) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE world_facts SET word_class_proposed = ?, updated_at = ?
		 WHERE id = ? AND scope = ? AND status = ? AND word_class = '' AND word_class_proposed = ''`,
		string(class), time.Now().UnixMilli(), id, scope, string(WorldFactActive))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeclineWorldFactClass turns down the class proposed for scope's fact
// id, for good, reporting whether one was waiting.
func (s *SQLiteStore) DeclineWorldFactClass(scope, id string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE world_facts SET word_class_proposed = ? || word_class_proposed, updated_at = ?
		 WHERE id = ? AND scope = ? AND word_class_proposed != '' AND word_class_proposed != ? AND word_class_proposed NOT LIKE ? || '%'`,
		WordClassDeclinedPrefix, time.Now().UnixMilli(), id, scope, wordClassNone, WordClassDeclinedPrefix)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ResetFoundWorldFacts undoes fact finding for scope, so its next run
// starts afresh: every fact a pass proposed (WorldFactLLM, WorldFactRule)
// is removed - waiting, accepted or skipped - and a class proposed for, or
// declined on, a fact that stays is forgotten. Facts the reader or an
// agent wrote stay, with the class they have. It reports how many were
// removed.
func (s *SQLiteStore) ResetFoundWorldFacts(scope string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM world_facts WHERE scope = ? AND source IN (?, ?)`, scope, string(WorldFactLLM), string(WorldFactRule))
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE world_facts SET word_class_proposed = '' WHERE scope = ? AND word_class_proposed != ''`, scope); err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), tx.Commit()
}

// ReviewWorldFact puts scope's fact id back in front of the reader,
// reporting whether anything was: a skipped fact waits as a proposal
// again, and a fact whose proposed class was turned down has that class
// proposed again (or, for one declined before the class was kept, may be
// proposed one by the next pass).
func (s *SQLiteStore) ReviewWorldFact(scope, id string) (bool, error) {
	now := time.Now().UnixMilli()
	res, err := s.db.Exec(`UPDATE world_facts SET status = ?, updated_at = ? WHERE id = ? AND scope = ? AND status = ?`,
		string(WorldFactPending), now, id, scope, string(WorldFactSkipped))
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	res, err = s.db.Exec(
		`UPDATE world_facts SET word_class_proposed = CASE WHEN word_class_proposed = ? THEN '' ELSE substr(word_class_proposed, ?) END, updated_at = ?
		 WHERE id = ? AND scope = ? AND (word_class_proposed = ? OR word_class_proposed LIKE ? || '%')`,
		wordClassNone, len(WordClassDeclinedPrefix)+1, now, id, scope, wordClassNone, WordClassDeclinedPrefix)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
