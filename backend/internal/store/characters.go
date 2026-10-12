package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/rhino1998/lectable/backend/internal/namekey"
)

// RecentChapterSpeakers returns every distinct dialogue speaker in the
// n chapters of bookID right before chapterIdx - who's likely still on
// stage (see httpapi.prominentSpeakers). "", "Narrator" and "Unknown" are
// left out.
func (s *SQLiteStore) RecentChapterSpeakers(bookID string, chapterIdx, n int) ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT p.speaker FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id
		WHERE c.book_id = ? AND c.idx >= ? AND c.idx < ? AND p.is_quote AND p.speaker NOT IN ('', 'Narrator', 'Unknown') ORDER BY p.speaker`, bookID, chapterIdx-n, chapterIdx)
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

// SpeakerLineCounts returns how many of bookID's dialogue lines each
// speaker has - one GROUP BY, for picking a book's main cast (see
// httpapi.prominentSpeakers). "", "Narrator" and "Unknown" are left out.
func (s *SQLiteStore) SpeakerLineCounts(bookID string) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT p.speaker, count(*) FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id
		WHERE c.book_id = ? AND p.is_quote AND p.speaker NOT IN ('', 'Narrator', 'Unknown') GROUP BY p.speaker`, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// ListCharacters returns scope's (see SeriesScope) known characters in
// discovery order. Identity/summary only - see CharacterVoicesForModel for
// their (per-clone-model) voice assignments.
func (s *SQLiteStore) ListCharacters(scope string) ([]Character, error) {
	rows, err := s.db.Query(`SELECT id, scope, name, summary, ref_line, is_role, invalid, aliases, pronouns, created_at FROM characters WHERE scope = ? ORDER BY created_at ASC`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Character
	for rows.Next() {
		var c Character
		if err := rows.Scan(&c.ID, &c.Scope, &c.Name, &c.Summary, &c.RefLine, &c.IsRole, &c.Invalid, &c.Aliases, &c.Pronouns, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CastingsByScope returns every valid character's casting (Summary), by
// roster scope - for telling the words one series' castings use from the
// ones every series' do (httpapi.kindAccents).
func (s *SQLiteStore) CastingsByScope() (map[string][]string, error) {
	rows, err := s.db.Query(`SELECT scope, summary FROM characters WHERE invalid = 0 AND summary != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var scope, summary string
		if err := rows.Scan(&scope, &summary); err != nil {
			return nil, err
		}
		out[scope] = append(out[scope], summary)
	}
	return out, rows.Err()
}

// GetCharacter is a by-id lookup (e.g. httpapi's .../characters/{id}/...
// routes, which address a character directly rather than by scope+name).
func (s *SQLiteStore) GetCharacter(id string) (*Character, error) {
	var c Character
	err := s.db.QueryRow(`SELECT id, scope, name, summary, ref_line, is_role, invalid, aliases, pronouns, created_at FROM characters WHERE id = ?`, id).
		Scan(&c.ID, &c.Scope, &c.Name, &c.Summary, &c.RefLine, &c.IsRole, &c.Invalid, &c.Aliases, &c.Pronouns, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *SQLiteStore) GetCharacterByName(scope, name string) (*Character, error) {
	var c Character
	// By name key (namekey.Fold), an exact match first: "woman" is "Woman"
	// and "Ve'delina" is "Ve’delina", so attribution's UpsertCharacter
	// never makes a second row that differs only in case or in which
	// apostrophe it was typed with (Arise had woman/Woman, doctor/Doctor,
	// barmaid/Barmaid).
	err := s.db.QueryRow(`SELECT id, scope, name, summary, ref_line, is_role, invalid, aliases, pronouns, created_at FROM characters
		WHERE scope = ? AND namekey(name) = namekey(?) ORDER BY name = ? DESC, created_at LIMIT 1`, scope, name, name).
		Scan(&c.ID, &c.Scope, &c.Name, &c.Summary, &c.RefLine, &c.IsRole, &c.Invalid, &c.Aliases, &c.Pronouns, &c.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// UpsertCharacter registers name as a character of scope if it isn't
// already known - called as speaker attribution (internal/speakerattr)
// discovers character names, so each one gets exactly one row (and so one
// consistent identity/summary) no matter how many chapters, or books in
// the same series (narrated by whichever clone models), it ends up
// attributed in. created reports whether this call actually inserted a
// new row, for callers that care; characterization itself no longer runs
// at discovery time at all - it's deferred until a character's voice is
// actually needed (see httpapi.provisionCharacterVoice,
// jobs.Manager.provisionMissingCharacterVoices).
//
// isRole is only ever applied on the initial insert - an existing row's
// own IsRole is never overwritten by a later call, even one that
// disagrees. Deliberately sticky-from-first-sight rather than
// last-write-wins: a role label ("Guard", "Thug") and a real individual's
// name are both just strings once discovered, and a later batch
// misjudging one as the other (in either direction) shouldn't silently
// reclassify an identity every other chapter/book has already been
// building dialogue/voice history against.
func (s *SQLiteStore) UpsertCharacter(scope, name string, isRole bool) (character Character, created bool, err error) {
	existing, err := s.GetCharacterByName(scope, name)
	if err != nil {
		return Character{}, false, err
	}
	if existing != nil {
		return *existing, false, nil
	}
	c := Character{ID: NewID(), Scope: scope, Name: name, IsRole: isRole, CreatedAt: time.Now().Unix()}
	_, err = s.db.Exec(
		`INSERT INTO characters (id, scope, name, summary, is_role, created_at) VALUES (?, ?, ?, '', ?, ?)`,
		c.ID, c.Scope, c.Name, c.IsRole, c.CreatedAt,
	)
	if err != nil {
		return Character{}, false, err
	}
	return c, true, nil
}

// CharacterVoiceForModel returns id's assigned voice preset for cloneModel
// specifically, or "" if none has been assigned for that model yet (either
// never auto-assigned, or explicitly cleared - see SetCharacterVoice). A
// character can be assigned a different preset per clone model (a preset
// clones through one specific model and isn't portable to another - see
// store.Character's doc comment) - callers resolving a single paragraph's
// voice pass the *book's own* resolved clone model
// (internal/narration.Resolver.ForParagraph).
func (s *SQLiteStore) CharacterVoiceForModel(characterID, cloneModel string) (string, error) {
	var presetID string
	err := s.db.QueryRow(`SELECT voice_preset_id FROM character_voices WHERE character_id = ? AND clone_model = ?`, characterID, cloneModel).Scan(&presetID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return presetID, err
}

// CharacterVoicesForModel batch-looks-up cloneModel's assigned voice
// preset for each of characterIDs - the multi-character counterpart of
// CharacterVoiceForModel, for callers resolving a whole chapter/book's
// worth of characters at once (httpapi's handleGetChapter/
// handleListSpeakers) rather than one at a time. A character id absent
// from the returned map has no voice assigned for cloneModel.
func (s *SQLiteStore) CharacterVoicesForModel(characterIDs []string, cloneModel string) (map[string]string, error) {
	out := make(map[string]string, len(characterIDs))
	if len(characterIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(characterIDs)), ",")
	args := make([]any, 0, len(characterIDs)+1)
	for _, id := range characterIDs {
		args = append(args, id)
	}
	args = append(args, cloneModel)

	rows, err := s.db.Query(
		`SELECT character_id, voice_preset_id FROM character_voices WHERE character_id IN (`+placeholders+`) AND clone_model = ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id, presetID string
		if err := rows.Scan(&id, &presetID); err != nil {
			return nil, err
		}
		out[id] = presetID
	}
	return out, rows.Err()
}

// SetCharacterVoice assigns (or, with "", clears) a character's narration
// voice for cloneModel specifically - see internal/narration.Resolver for
// how this then overrides the book's own voice for paragraphs attributed
// to this character, when the book currently in use narrates through that
// same clone model.
func (s *SQLiteStore) SetCharacterVoice(characterID, cloneModel, voicePresetID string) error {
	if voicePresetID == "" {
		_, err := s.db.Exec(`DELETE FROM character_voices WHERE character_id = ? AND clone_model = ?`, characterID, cloneModel)
		return err
	}
	_, err := s.db.Exec(
		`INSERT INTO character_voices (character_id, clone_model, voice_preset_id) VALUES (?, ?, ?)
		 ON CONFLICT (character_id, clone_model) DO UPDATE SET voice_preset_id = excluded.voice_preset_id`,
		characterID, cloneModel, voicePresetID,
	)
	return err
}

// SetCharacterSummary records the LLM's characterization of how a
// character should sound, and its paired reference passage (see
// store.Character.Summary/RefLine, speakerattr.Client.CharacterizeVoice) -
// written together, in one call, since CharacterizeVoice always produces
// both from the same generation. Independent of SetCharacterVoice since a
// manual voice reassignment (handleSetCharacterVoice) shouldn't touch
// either.
func (s *SQLiteStore) SetCharacterSummary(id, summary, refLine string) error {
	_, err := s.db.Exec(`UPDATE characters SET summary = ?, ref_line = ? WHERE id = ?`, summary, refLine, id)
	return err
}

// SyncCharacterSummariesFromPreset copies an edited voice preset's
// instruct/refText back onto every character it's assigned to (across any
// clone model - see character_voices) as their Summary/RefLine - the
// reverse of recharacterizeAndInvalidate's Summary -> preset.Instruct
// sync, so editing a speaker's voice prompt (Voices page or the Speakers
// page's own voice editor) keeps the Speakers page's shown
// characterization in step with the voice actually narrating them. Only
// the fields the caller says changed are written, so a name/speed-only
// edit never touches a character's LLM-written characterization.
func (s *SQLiteStore) SyncCharacterSummariesFromPreset(presetID, instruct, refText string, instructChanged, refTextChanged bool) error {
	const assigned = `id IN (SELECT character_id FROM character_voices WHERE voice_preset_id = ?)`
	if instructChanged {
		if _, err := s.db.Exec(`UPDATE characters SET summary = ? WHERE `+assigned, instruct, presetID); err != nil {
			return err
		}
	}
	if refTextChanged {
		if _, err := s.db.Exec(`UPDATE characters SET ref_line = ? WHERE `+assigned, refText, presetID); err != nil {
			return err
		}
	}
	return nil
}

// VoicePresetCharacter is one character a voice preset is assigned to.
type VoicePresetCharacter struct {
	ID    string
	Scope string
}

// VoicePresetCharacters lists the characters presetID is the voice of,
// under any clone model.
func (s *SQLiteStore) VoicePresetCharacters(presetID string) ([]VoicePresetCharacter, error) {
	rows, err := s.db.Query(`SELECT DISTINCT c.id, c.scope FROM characters c JOIN character_voices cv ON cv.character_id = c.id WHERE cv.voice_preset_id = ? ORDER BY c.id`, presetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VoicePresetCharacter
	for rows.Next() {
		var c VoicePresetCharacter
		if err := rows.Scan(&c.ID, &c.Scope); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetCharacterPronouns sets id's pronouns (Character.Pronouns) - "" clears
// them. Callers normalize first (speakerattr.NormalizePronouns).
func (s *SQLiteStore) SetCharacterPronouns(id, pronouns string) error {
	_, err := s.db.Exec(`UPDATE characters SET pronouns = ? WHERE id = ?`, pronouns, id)
	return err
}

// RenameCharacter changes id's name - its row only; callers move the
// paragraphs attributed to the old name (ReassignCharacterSpeaker).
func (s *SQLiteStore) RenameCharacter(id, name string) error {
	_, err := s.db.Exec(`UPDATE characters SET name = ? WHERE id = ?`, name, id)
	return err
}

// SetCharacterRole marks (or, with false, unmarks) id as a generic role
// label - see store.Character.IsRole.
func (s *SQLiteStore) SetCharacterRole(id string, isRole bool) error {
	_, err := s.db.Exec(`UPDATE characters SET is_role = ? WHERE id = ?`, isRole, id)
	return err
}

// SetCharacterInvalid marks (or, with false, unmarks) id as invalid - see
// store.Character.Invalid. Identity/summary/voice assignments are left
// alone either way, so unmarking restores the character exactly as it was.
func (s *SQLiteStore) SetCharacterInvalid(id string, invalid bool) error {
	_, err := s.db.Exec(`UPDATE characters SET invalid = ? WHERE id = ?`, invalid, id)
	return err
}

// DeleteCharacter removes id's character_voices and character_stages rows and the characters
// row itself - manual cascade (no FK ON DELETE CASCADE - see this
// package's own doc comment). Deliberately leaves the voice_presets those
// character_voices rows may have pointed at alone: a preset is
// independently deletable and may still be referenced elsewhere (a
// book's own voice, the default voice, another character sharing it) -
// callers that also want those gone delete them explicitly themselves
// (see httpapi.handleDeleteBookSpeakerData).
func (s *SQLiteStore) DeleteCharacter(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM character_voices WHERE character_id = ?`, id); err != nil {
		return fmt.Errorf("delete character voices: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM character_stages WHERE character_id = ?`, id); err != nil {
		return fmt.Errorf("delete character stages: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM characters WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete character: %w", err)
	}
	return tx.Commit()
}

// SetCharacterAliases replaces characterID's aliases (Character.Aliases)
// with aliases - trimmed, with blanks, duplicates (by namekey.Fold), and the
// character's own name dropped. Validation against the rest of the roster
// (an alias that's another character's name) is the caller's job - see
// httpapi.handleSetCharacterAliases.
func (s *SQLiteStore) SetCharacterAliases(characterID string, aliases []string) error {
	c, err := s.GetCharacter(characterID)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("character %s not found", characterID)
	}
	clean := AliasList{}
	seen := map[string]bool{namekey.Fold(c.Name): true}
	for _, a := range aliases {
		a = strings.TrimSpace(a)
		if a == "" || seen[namekey.Fold(a)] {
			continue
		}
		seen[namekey.Fold(a)] = true
		clean = append(clean, a)
	}
	sort.Strings(clean)
	encoded, err := clean.Value()
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE characters SET aliases = ? WHERE id = ?`, encoded, characterID)
	return err
}

// AddCharacterAliases adds aliases to characterID's existing ones - see
// SetCharacterAliases.
func (s *SQLiteStore) AddCharacterAliases(characterID string, aliases ...string) error {
	c, err := s.GetCharacter(characterID)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("character %s not found", characterID)
	}
	return s.SetCharacterAliases(characterID, append(append([]string(nil), c.Aliases...), aliases...))
}

// ReassignCharacterSpeaker re-points every paragraph in bookID currently
// attributed to fromName (an exact match against paragraphs.speaker) at
// toName instead - used by httpapi.handleMergeCharacter to fold one
// character's lines into another's (or into "Narrator"/"Unknown", the two
// sentinel values - see handleMergeCharacter's own doc comment) and by
// handleDeleteCharacter (toName "Unknown" - the paragraphs were real
// quoted dialogue with a genuine speaker, just no longer this one, so
// "Unknown" is the same sentinel attribution itself falls back to for
// dialogue it can't confidently attribute; "" (Narrator) would be wrong -
// reserved for actual narration, never dialogue, see backend/CLAUDE.md's
// speakerattr section). Deliberately scoped to one book, not fromName's
// whole series scope, the same book-only split ClearBookSpeakers/
// handleDeleteBookSpeakerData use: deleting/merging a character's
// identity/voice reaches the whole series (the caller does that part
// separately, via DeleteCharacter), but no other book's paragraphs are
// touched here - a reader cleaning up a bad attribution on one book
// shouldn't silently re-attribute a sibling book they haven't looked at.
func (s *SQLiteStore) ReassignCharacterSpeaker(bookID, fromName, toName string) error {
	_, err := s.db.Exec(
		`UPDATE paragraphs SET speaker = ? WHERE speaker = ? AND chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`,
		toName, fromName, bookID,
	)
	return err
}

// NumberSpeakerLines gives every line in bookID attributed to name, and
// carrying no speaker number yet, the number index
// (Paragraph.SpeakerIndex) - for a role that was stored with its number
// in its name ("Drake 8"), just before its lines are folded into the
// plain role (httpapi.handleMergeCharacter), so they keep their slot.
func (s *SQLiteStore) NumberSpeakerLines(bookID, name string, index int) error {
	_, err := s.db.Exec(
		`UPDATE paragraphs SET speaker_index = ? WHERE speaker = ? AND is_quote AND speaker_index = 0 AND chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`,
		index, name, bookID,
	)
	return err
}

// RenameSpeakerRecords re-points bookID's other records of a speaker name
// at toName, the counterpart of ReassignCharacterSpeaker for everything
// that holds a name rather than a paragraph: speaker corrections
// (paragraph_edits - attribution reads earlier chapters' as lessons), the
// summary-pass notes' cast and scene lists (attribution lists the cast to
// every batch, and a stale name there would be offered, and recreated, as
// a separate character), and the book setting's first-person narrator.
// Notes match ignoring case, as the summary pass resolves names; notes
// stay marked as edited or not as they were.
func (s *SQLiteStore) RenameSpeakerRecords(bookID, fromName, toName string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	for _, col := range []string{"value", "original"} {
		if _, err := tx.Exec(`UPDATE paragraph_edits SET `+col+` = ? WHERE field = ? AND `+col+` = ?
			AND paragraph_id IN (SELECT p.id FROM paragraphs p JOIN chapters c ON c.id = p.chapter_id WHERE c.book_id = ?)`,
			toName, EditSpeaker, fromName, bookID); err != nil {
			return fmt.Errorf("rename in paragraph edits: %w", err)
		}
	}

	rows, err := tx.Query(`SELECT cs.chapter_id, CAST(cs.summary AS VARCHAR) FROM chapter_summaries cs JOIN chapters c ON c.id = cs.chapter_id WHERE c.book_id = ?`, bookID)
	if err != nil {
		return err
	}
	updated := map[string]string{}
	for rows.Next() {
		var chapterID, raw string
		if err := rows.Scan(&chapterID, &raw); err != nil {
			rows.Close()
			return err
		}
		var notes ChapterNotes
		if err := json.Unmarshal([]byte(raw), &notes); err != nil {
			continue // unreadable notes are the summary pass's to redo
		}
		changed := false
		for i := range notes.Cast {
			if namekey.Equal(notes.Cast[i].Name, fromName) {
				notes.Cast[i].Name, changed = toName, true
			}
		}
		for i := range notes.Scenes {
			for j, name := range notes.Scenes[i].Present {
				if namekey.Equal(name, fromName) {
					notes.Scenes[i].Present[j], changed = toName, true
				}
			}
		}
		if changed {
			encoded, err := json.Marshal(notes)
			if err != nil {
				rows.Close()
				return err
			}
			updated[chapterID] = string(encoded)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for chapterID, raw := range updated {
		if _, err := tx.Exec(`UPDATE chapter_summaries SET summary = ? WHERE chapter_id = ?`, raw, chapterID); err != nil {
			return fmt.Errorf("rename in chapter notes: %w", err)
		}
	}

	var rawSetting string
	switch err := tx.QueryRow(`SELECT CAST(setting AS VARCHAR) FROM book_settings WHERE book_id = ?`, bookID).Scan(&rawSetting); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	default:
		var setting BookSetting
		if json.Unmarshal([]byte(rawSetting), &setting) == nil && namekey.Equal(setting.Narrator, fromName) {
			setting.Narrator = toName
			encoded, err := json.Marshal(setting)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE book_settings SET setting = ? WHERE book_id = ?`, string(encoded), bookID); err != nil {
				return fmt.Errorf("rename book setting narrator: %w", err)
			}
		}
	}
	return tx.Commit()
}
