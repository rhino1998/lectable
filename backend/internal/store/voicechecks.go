package store

import (
	"encoding/json"
	"time"

	"github.com/rhino1998/lectable/backend/internal/voicecheck"
)

// VoiceCheck is what was heard in one voice's reference clip beside what
// its casting asks for - see migration 00031 and httpapi.CheckVoices.
type VoiceCheck struct {
	Scope string
	// PresetID is the voice: a character's own preset, or the derived one
	// an age stage speaks in (narration.StageVoice), which StageID names.
	PresetID    string
	CharacterID string
	StageID     string
	// RefIdentity is the render of the clip that was heard
	// (voicerefs.RefIdentity); the check applies while the clip is still
	// that one.
	RefIdentity string
	Cast        voicecheck.Traits
	Heard       voicecheck.Traits
	// Mismatches is the traits on which the two clearly disagree, empty
	// for a voice that passed.
	Mismatches []voicecheck.Trait
	// Dismissed is the reader's "this voice is fine".
	Dismissed bool
	CheckedAt int64
}

// UpsertVoiceCheck records a check, replacing the voice's last one. A
// dismissal is kept only while the check is of the same render.
func (s *SQLiteStore) UpsertVoiceCheck(c VoiceCheck) error {
	if c.CheckedAt == 0 {
		c.CheckedAt = time.Now().Unix()
	}
	if c.Mismatches == nil {
		c.Mismatches = []voicecheck.Trait{}
	}
	cast, err := json.Marshal(c.Cast)
	if err != nil {
		return err
	}
	heard, err := json.Marshal(c.Heard)
	if err != nil {
		return err
	}
	mismatches, err := json.Marshal(c.Mismatches)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO voice_checks (scope, preset_id, character_id, stage_id, ref_identity, cast_traits, heard_traits, mismatches, dismissed, checked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, false, ?) ON CONFLICT (scope, preset_id) DO UPDATE SET
			character_id = excluded.character_id, stage_id = excluded.stage_id,
			dismissed = voice_checks.dismissed AND voice_checks.ref_identity = excluded.ref_identity,
			ref_identity = excluded.ref_identity, cast_traits = excluded.cast_traits, heard_traits = excluded.heard_traits,
			mismatches = excluded.mismatches, checked_at = excluded.checked_at`,
		c.Scope, c.PresetID, c.CharacterID, c.StageID, c.RefIdentity, string(cast), string(heard), string(mismatches), c.CheckedAt)
	return err
}

// ListVoiceChecks returns every check stored for a roster.
func (s *SQLiteStore) ListVoiceChecks(scope string) ([]VoiceCheck, error) {
	rows, err := s.db.Query(`SELECT scope, preset_id, character_id, stage_id, ref_identity, cast_traits, heard_traits, mismatches, dismissed, checked_at
		FROM voice_checks WHERE scope = ? ORDER BY checked_at ASC, rowid ASC`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VoiceCheck
	for rows.Next() {
		var c VoiceCheck
		var cast, heard, mismatches string
		if err := rows.Scan(&c.Scope, &c.PresetID, &c.CharacterID, &c.StageID, &c.RefIdentity, &cast, &heard, &mismatches, &c.Dismissed, &c.CheckedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(cast), &c.Cast); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(heard), &c.Heard); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(mismatches), &c.Mismatches); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DismissVoiceCheck marks a roster's check of one voice as looked at and
// left alone, reporting whether there was one.
func (s *SQLiteStore) DismissVoiceCheck(scope, presetID string) (bool, error) {
	res, err := s.db.Exec(`UPDATE voice_checks SET dismissed = true WHERE scope = ? AND preset_id = ?`, scope, presetID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
