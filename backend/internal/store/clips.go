package store

// ClipUser is one paragraph_audio row whose audio is a shared narration
// clip (paragraph_audio.clip_key), with what it takes to find that
// paragraph's own clip path (audiopath.ParagraphFile).
type ClipUser struct {
	ParagraphID string
	VoiceID     string
	BookID      string
	ChapterID   string
	Idx         int
	State       AudioState
}

// ClipUsers lists every paragraph_audio row using the shared clip key.
func (s *SQLiteStore) ClipUsers(key string) ([]ClipUser, error) {
	if key == "" {
		return nil, nil
	}
	rows, err := s.db.Query(`
		SELECT pa.paragraph_id, pa.voice_id, c.book_id, p.chapter_id, p.idx,
			pa.status, pa.error, pa.duration_seconds, pa.word_timings
		FROM paragraph_audio pa
		JOIN paragraphs p ON p.id = pa.paragraph_id
		JOIN chapters c ON c.id = p.chapter_id
		WHERE pa.clip_key = ? AND pa.clip_key <> ''`, key) // the second term is what lets the partial index serve this
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClipUser
	for rows.Next() {
		u := ClipUser{State: AudioState{ClipKey: key}}
		if err := rows.Scan(&u.ParagraphID, &u.VoiceID, &u.BookID, &u.ChapterID, &u.Idx,
			&u.State.Status, &u.State.Error, &u.State.DurationSeconds, &u.State.WordTimings); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetParagraphReadyClip marks id ready under voiceID on the shared clip
// key, with that clip's duration and word timings (raw JSON) - the
// shared-clip counterpart of SetParagraphReady, which marks a clip of the
// paragraph's own.
func (s *SQLiteStore) SetParagraphReadyClip(id, voiceID, key string, durationSeconds float64, wordTimingsJSON string) error {
	if wordTimingsJSON == "" {
		wordTimingsJSON = "[]"
	}
	if err := s.upsertParagraphAudio(id, voiceID, AudioReady, "", durationSeconds); err != nil {
		return err
	}
	_, err := s.db.Exec(
		`UPDATE paragraph_audio SET clip_key = ?, word_timings = ? WHERE paragraph_id = ? AND voice_id = ?`,
		key, wordTimingsJSON, id, voiceID,
	)
	return err
}

// SetParagraphClipKey records that id's existing ready audio under voiceID
// is the shared clip key, changing nothing else about it.
func (s *SQLiteStore) SetParagraphClipKey(id, voiceID, key string) error {
	_, err := s.db.Exec(`UPDATE paragraph_audio SET clip_key = ? WHERE paragraph_id = ? AND voice_id = ?`, key, id, voiceID)
	return err
}

// ClipKeys is every shared clip key some paragraph_audio row names - what
// audiomaint.SweepSharedClips keeps.
func (s *SQLiteStore) ClipKeys() (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT DISTINCT clip_key FROM paragraph_audio WHERE clip_key <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		out[key] = true
	}
	return out, rows.Err()
}
