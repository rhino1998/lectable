package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"time"
)

type DefaultVoice struct {
	PresetID string
	Instruct string
	Language string
	Seed     int
	// CloneModel is the clone model a newly created book narrates through
	// (Book.CloneModel) - see the books.clone_model schema comment.
	CloneModel string
}

func (s *DuckStore) GetDefaultVoice() (DefaultVoice, error) {
	var v DefaultVoice
	err := s.db.QueryRow(`SELECT preset_id, instruct, language, seed, clone_model FROM default_voice WHERE id = 0`).
		Scan(&v.PresetID, &v.Instruct, &v.Language, &v.Seed, &v.CloneModel)
	return v, err
}

func (s *DuckStore) SetDefaultVoice(presetID, instruct, language string, seed int, cloneModel string) error {
	_, err := s.db.Exec(
		`UPDATE default_voice SET preset_id = ?, instruct = ?, language = ?, seed = ?, clone_model = ? WHERE id = 0`,
		presetID, instruct, language, seed, cloneModel,
	)
	return err
}

// VoiceID derives a stable identifier for a narration voice from its full
// config, so the same (preset, instruct, language) combination always
// lands on the same cached audio regardless of when or how it was set -
// and two different configs never collide.
func VoiceID(presetID, instruct, language string) string {
	h := sha256.Sum256([]byte(presetID + "\x00" + instruct + "\x00" + language))
	return hex.EncodeToString(h[:])[:16]
}

func (s *DuckStore) ListVoicePresets() ([]VoicePreset, error) {
	rows, err := s.db.Query(`SELECT id, name, instruct, ref_text, seed, speed_multiplier, created_at, design_model FROM voice_presets ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []VoicePreset
	for rows.Next() {
		var p VoicePreset
		if err := rows.Scan(&p.ID, &p.Name, &p.Instruct, &p.RefText, &p.Seed, &p.SpeedMultiplier, &p.CreatedAt, &p.DesignModel); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// VoicePresetGroups maps every custom voice preset currently assigned to a
// character (across any clone model - see character_voices) to a display
// label for the Voices page's grouping: the series name characters sharing
// that preset's owner are scoped under (see SeriesScope), or that lone
// book's title when there's no series. A preset never assigned to a
// character (a reader's own general-purpose narrator voice, not one
// auto-created for a speaker - see httpapi.provisionCharacterVoice) is
// simply absent from the returned map, letting the caller tell "this
// belongs to a series/book's speaker roster" apart from "just one of my
// voices". Scope strings are "series:<name>" or "book:<id>" (see
// SeriesScope); substr offsets below skip past those literal prefixes
// (DuckDB substr is 1-indexed).
func (s *DuckStore) VoicePresetGroups() (map[string]string, error) {
	rows, err := s.db.Query(`
		SELECT cv.voice_preset_id,
			CASE WHEN c.scope LIKE 'series:%' THEN substr(c.scope, 8) ELSE b.title END AS label
		FROM character_voices cv
		JOIN characters c ON c.id = cv.character_id
		LEFT JOIN books b ON c.scope LIKE 'book:%' AND b.id = substr(c.scope, 6)
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var presetID string
		var label sql.NullString
		if err := rows.Scan(&presetID, &label); err != nil {
			return nil, err
		}
		if label.Valid && label.String != "" {
			out[presetID] = label.String
		}
	}
	return out, rows.Err()
}

func (s *DuckStore) GetVoicePreset(id string) (*VoicePreset, error) {
	var p VoicePreset
	err := s.db.QueryRow(`SELECT id, name, instruct, ref_text, seed, speed_multiplier, created_at, design_model FROM voice_presets WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Instruct, &p.RefText, &p.Seed, &p.SpeedMultiplier, &p.CreatedAt, &p.DesignModel)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *DuckStore) CreateVoicePreset(name, instruct, refText string, seed int, speedMultiplier float64, designModel string) (VoicePreset, error) {
	p := VoicePreset{
		ID: NewID(), Name: name, Instruct: instruct, RefText: refText, Seed: seed,
		SpeedMultiplier: speedMultiplier, CreatedAt: time.Now().Unix(), DesignModel: designModel,
	}
	_, err := s.db.Exec(
		`INSERT INTO voice_presets (id, name, instruct, ref_text, seed, speed_multiplier, created_at, design_model) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Instruct, p.RefText, p.Seed, p.SpeedMultiplier, p.CreatedAt, p.DesignModel,
	)
	if err != nil {
		return VoicePreset{}, err
	}
	return p, nil
}

func (s *DuckStore) UpdateVoicePreset(id, name, instruct, refText string, seed int, speedMultiplier float64, designModel string) error {
	_, err := s.db.Exec(
		`UPDATE voice_presets SET name = ?, instruct = ?, ref_text = ?, seed = ?, speed_multiplier = ?, design_model = ? WHERE id = ?`,
		name, instruct, refText, seed, speedMultiplier, designModel, id,
	)
	return err
}

// UpdateVoicePresetSeed persists a bumped-on-retry seed (see
// jobs.task.runProvision's own doc comment) back onto an existing preset,
// touching only that one column - unlike UpdateVoicePreset's full-row
// replace, a caller here typically only has the bumped seed value in hand,
// not a freshly-read copy of every other field, and shouldn't have to fetch
// the row just to round-trip its own unchanged fields back through
// UpdateVoicePreset.
func (s *DuckStore) UpdateVoicePresetSeed(id string, seed int) error {
	_, err := s.db.Exec(`UPDATE voice_presets SET seed = ? WHERE id = ?`, seed, id)
	return err
}

func (s *DuckStore) DeleteVoicePreset(id string) error {
	_, err := s.db.Exec(`DELETE FROM voice_presets WHERE id = ?`, id)
	return err
}

// VoicePresetIDsForCharacter returns every voice_preset_id currently
// assigned to characterID, across every clone model it has one for - the
// "all models, one character" counterpart of CharacterVoiceForModel's
// "one model, one character" and CharacterVoicesForModel's "one model,
// many characters". Used to invalidate every one of a character's
// assigned voices after a re-characterization changes their Summary (see
// httpapi.handleCharacterizeSpeaker) - unlike an ordinary read path
// resolving narration for one specific book (which only ever cares about
// that book's own resolved clone model), invalidation has to reach every
// clone model this character has ever been assigned a voice under, not
// just whichever one happens to be in use right now.
func (s *DuckStore) VoicePresetIDsForCharacter(characterID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT voice_preset_id FROM character_voices WHERE character_id = ?`, characterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var presetID string
		if err := rows.Scan(&presetID); err != nil {
			return nil, err
		}
		out = append(out, presetID)
	}
	return out, rows.Err()
}
