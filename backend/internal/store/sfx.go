package store

import (
	"database/sql"
	"strings"
)

// SFXState is one paragraph's own sound-effect (Stable Audio SFX) state, as
// looked up by ParagraphSFXStates/GetParagraphSFXState - AudioState's own
// counterpart for the sfx table, which (unlike paragraph_audio) has at
// most one row per paragraph, never keyed by voice - see that table's own
// doc comment.
type SFXState struct {
	Prompt          string
	Status          string
	Error           string
	DurationSeconds float64
	TriggerWord     int
}

// ParagraphSFXStates batch-looks-up sfx state for paragraphIDs -
// ParagraphAudioStatuses' own counterpart for the sfx table. A paragraph
// absent from the returned map has no sfx row at all (the overwhelmingly
// common case - most paragraphs never get one), equivalent to the zero
// SFXState{} (Status "").
func (s *DuckStore) ParagraphSFXStates(paragraphIDs []string) (map[string]SFXState, error) {
	out := make(map[string]SFXState, len(paragraphIDs))
	if len(paragraphIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(paragraphIDs)), ",")
	args := make([]any, len(paragraphIDs))
	for i, id := range paragraphIDs {
		args[i] = id
	}

	rows, err := s.db.Query(
		`SELECT paragraph_id, prompt, status, error, duration_seconds, trigger_word FROM sfx WHERE paragraph_id IN (`+placeholders+`)`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var st SFXState
		if err := rows.Scan(&id, &st.Prompt, &st.Status, &st.Error, &st.DurationSeconds, &st.TriggerWord); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// GetParagraphSFXState is ParagraphSFXStates' own single-paragraph
// counterpart, for the per-paragraph SFX handlers (chapters.go) that only
// ever need one row at a time. Returns the zero SFXState{}, not an error,
// for a paragraph with no sfx row yet - same "absent means never touched"
// convention ParagraphSFXStates' own map follows.
func (s *DuckStore) GetParagraphSFXState(paragraphID string) (SFXState, error) {
	var st SFXState
	err := s.db.QueryRow(
		`SELECT prompt, status, error, duration_seconds, trigger_word FROM sfx WHERE paragraph_id = ?`, paragraphID,
	).Scan(&st.Prompt, &st.Status, &st.Error, &st.DurationSeconds, &st.TriggerWord)
	if err == sql.ErrNoRows {
		return SFXState{}, nil
	}
	return st, err
}

// SetParagraphSFXPrompt records id's own reader-entered sound-effect
// prompt, independent of status - saving a prompt doesn't by itself
// invalidate any already-generated clip (a reader may be drafting a prompt
// before generating at all); handleGenerateParagraphSFX is what actually
// triggers a new render and updates status. Insert-if-missing, same
// "UPDATE, check RowsAffected, INSERT on zero" pattern
// upsertParagraphAudio uses (see its own doc comment) - sfx rows are
// sparse, so a paragraph's very first prompt always needs to insert, not
// update.
func (s *DuckStore) SetParagraphSFXPrompt(id, prompt string) error {
	res, err := s.db.Exec(`UPDATE sfx SET prompt = ? WHERE paragraph_id = ?`, prompt, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	_, err = s.db.Exec(`INSERT INTO sfx (paragraph_id, prompt) VALUES (?, ?)`, id, prompt)
	return err
}

// SetParagraphSFXTriggerWord records which word (a 0-based index into this
// paragraph's own forced-alignment word_timings) playback should start the
// sound effect at - see SFXState.TriggerWord. Same insert-if-missing
// shape as SetParagraphSFXPrompt.
func (s *DuckStore) SetParagraphSFXTriggerWord(id string, wordIdx int) error {
	res, err := s.db.Exec(`UPDATE sfx SET trigger_word = ? WHERE paragraph_id = ?`, wordIdx, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	_, err = s.db.Exec(`INSERT INTO sfx (paragraph_id, trigger_word) VALUES (?, ?)`, id, wordIdx)
	return err
}

// upsertSFXStatus is the shared insert-if-missing helper behind
// SetParagraphSFXGenerating/SetParagraphSFXReady/SetParagraphSFXError -
// upsertParagraphAudio's own exact pattern, reused here for the same
// "sfx rows are sparse" reason SetParagraphSFXPrompt's own doc comment
// gives.
func (s *DuckStore) upsertSFXStatus(paragraphID, status, errMsg string, durationSeconds float64) error {
	res, err := s.db.Exec(
		`UPDATE sfx SET status = ?, error = ?, duration_seconds = ? WHERE paragraph_id = ?`,
		status, errMsg, durationSeconds, paragraphID,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	_, err = s.db.Exec(
		`INSERT INTO sfx (paragraph_id, status, error, duration_seconds) VALUES (?, ?, ?, ?)`,
		paragraphID, status, errMsg, durationSeconds,
	)
	return err
}

func (s *DuckStore) SetParagraphSFXGenerating(id string) error {
	return s.upsertSFXStatus(id, AudioGenerating, "", 0)
}

func (s *DuckStore) SetParagraphSFXReady(id string, durationSeconds float64) error {
	return s.upsertSFXStatus(id, AudioReady, "", durationSeconds)
}

func (s *DuckStore) SetParagraphSFXError(id, message string) error {
	return s.upsertSFXStatus(id, AudioError, message, 0)
}
