package store

import (
	"database/sql"
	"encoding/json"
)

// ClipPart is one later member's share of a scare-quote merge group's
// clip: where its words start in it, how long they run, and their timings
// (raw JSON) - what its pointer row holds (see AudioState.PointerOffset),
// kept with the clip so it outlives those rows.
type ClipPart struct {
	PointerSeconds  float64 `json:"pointerSeconds"`
	DurationSeconds float64 `json:"durationSeconds"`
	WordTimings     string  `json:"wordTimings"`
}

func encodeClipParts(parts []ClipPart) string {
	if len(parts) == 0 {
		return ""
	}
	b, err := json.Marshal(parts)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeClipParts(s string) []ClipPart {
	if s == "" {
		return nil
	}
	var parts []ClipPart
	if err := json.Unmarshal([]byte(s), &parts); err != nil {
		return nil
	}
	return parts
}

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
// paragraph's own. parts are, for a merge group's anchor, the parts of the
// clip that belong to the members after it (nil otherwise). One statement,
// so the row is never seen ready without its clip.
func (s *SQLiteStore) SetParagraphReadyClip(id, voiceID, key string, durationSeconds float64, wordTimingsJSON string, parts []ClipPart) error {
	book := s.paragraphBook(id)
	if wordTimingsJSON == "" {
		wordTimingsJSON = "[]"
	}
	clipParts := encodeClipParts(parts)
	res, err := s.db.execScoped(book,
		`UPDATE paragraph_audio SET status = ?, error = '', duration_seconds = ?, word_timings = ?, pointer_offset = 0, pointer_seconds = 0, clip_key = ?, clip_parts = ? WHERE paragraph_id = ? AND voice_id = ?`,
		AudioReady, durationSeconds, wordTimingsJSON, key, clipParts, id, voiceID,
	)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		return nil
	}
	_, err = s.db.execScoped(book,
		`INSERT INTO paragraph_audio (paragraph_id, voice_id, status, duration_seconds, word_timings, clip_key, clip_parts, chapter_id, chars) VALUES (?, ?, ?, ?, ?, ?, ?, coalesce((SELECT chapter_id FROM paragraphs WHERE id = ?), ''), coalesce((SELECT length(content) FROM paragraphs WHERE id = ?), 0))`,
		id, voiceID, AudioReady, durationSeconds, wordTimingsJSON, key, clipParts, id, id,
	)
	return err
}

// BackfillClipParts fills clip_parts on merge group anchors that are on a
// shared clip but have none - groups from before the column - from their
// members' pointer rows. Returns how many anchors it filled; a scan of the
// pointer rows and nothing more once they all are.
func (s *SQLiteStore) BackfillClipParts() (int, error) {
	// The pointer rows alone, grouped here: joining them to their anchors
	// in SQL took over a minute on a 180k-row library.
	rows, err := s.db.Query(`
		SELECT p.chapter_id, p.idx - pa.pointer_offset, pa.voice_id, pa.pointer_offset, pa.pointer_seconds, pa.duration_seconds, pa.word_timings
		FROM paragraph_audio pa
		JOIN paragraphs p ON p.id = pa.paragraph_id
		WHERE pa.status = ? AND pa.pointer_offset > 0
		ORDER BY p.chapter_id, pa.voice_id, p.idx`, AudioReady)
	if err != nil {
		return 0, err
	}
	type anchor struct {
		chapterID, voiceID string
		idx                int
	}
	var order []anchor
	groups := map[anchor][]ClipPart{}
	broken := map[anchor]bool{}
	for rows.Next() {
		var a anchor
		var offset int
		var part ClipPart
		if err := rows.Scan(&a.chapterID, &a.idx, &a.voiceID, &offset, &part.PointerSeconds, &part.DurationSeconds, &part.WordTimings); err != nil {
			rows.Close()
			return 0, err
		}
		if _, seen := groups[a]; !seen {
			order = append(order, a)
		}
		// Members come in order; a gap means the group isn't whole.
		if offset != len(groups[a])+1 {
			broken[a] = true
		}
		groups[a] = append(groups[a], part)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	// Which of those anchors still lack their parts.
	var todo []anchor
	for _, a := range order {
		if broken[a] {
			continue
		}
		var needs bool
		err := s.db.QueryRow(`
			SELECT pa.clip_key <> '' AND pa.clip_parts = '' AND pa.status = ?
			FROM paragraph_audio pa
			WHERE pa.voice_id = ? AND pa.paragraph_id = (SELECT id FROM paragraphs WHERE chapter_id = ? AND idx = ?)`,
			AudioReady, a.voiceID, a.chapterID, a.idx).Scan(&needs)
		if err == nil && needs {
			todo = append(todo, a)
		}
	}
	if len(todo) == 0 {
		return 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, a := range todo {
		if _, err := tx.Exec(`
			UPDATE paragraph_audio SET clip_parts = ?
			WHERE voice_id = ? AND paragraph_id = (SELECT id FROM paragraphs WHERE chapter_id = ? AND idx = ?)`,
			encodeClipParts(groups[a]), a.voiceID, a.chapterID, a.idx); err != nil {
			return 0, err
		}
	}
	return len(todo), tx.Commit()
}

// SetParagraphClipKey records that id's existing ready audio under voiceID
// is the shared clip key, changing nothing else about it.
func (s *SQLiteStore) SetParagraphClipKey(id, voiceID, key string) error {
	book := s.paragraphBook(id)
	_, err := s.db.execScoped(book, `UPDATE paragraph_audio SET clip_key = ? WHERE paragraph_id = ? AND voice_id = ?`, key, id, voiceID)
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

// KeylessClips lists every ready paragraph_audio row whose clip is a file
// at the paragraph's own path rather than a shared clip: no clip_key, and
// not a merge group's pointer row.
func (s *SQLiteStore) KeylessClips() ([]ClipUser, error) {
	rows, err := s.db.Query(`
		SELECT pa.paragraph_id, pa.voice_id, c.book_id, p.chapter_id, p.idx
		FROM paragraph_audio pa
		JOIN paragraphs p ON p.id = pa.paragraph_id
		JOIN chapters c ON c.id = p.chapter_id
		WHERE pa.status = ? AND pa.clip_key = '' AND pa.pointer_offset = 0`, AudioReady)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClipUser
	for rows.Next() {
		u := ClipUser{State: AudioState{Status: AudioReady}}
		if err := rows.Scan(&u.ParagraphID, &u.VoiceID, &u.BookID, &u.ChapterID, &u.Idx); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// KeyedClips lists every ready paragraph_audio row that names a shared
// clip, with that key in State.ClipKey and whether it is a merge group's
// anchor (State.PointerOffset is -1 then; it is never a pointer row).
func (s *SQLiteStore) KeyedClips() ([]ClipUser, error) {
	rows, err := s.db.Query(`
		SELECT pa.paragraph_id, pa.voice_id, c.book_id, p.chapter_id, p.idx, pa.clip_key, pa.clip_parts <> ''
		FROM paragraph_audio pa
		JOIN paragraphs p ON p.id = pa.paragraph_id
		JOIN chapters c ON c.id = p.chapter_id
		WHERE pa.status = ? AND pa.clip_key <> ''`, AudioReady)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClipUser
	for rows.Next() {
		u := ClipUser{State: AudioState{Status: AudioReady}}
		var group bool
		if err := rows.Scan(&u.ParagraphID, &u.VoiceID, &u.BookID, &u.ChapterID, &u.Idx, &u.State.ClipKey, &group); err != nil {
			return nil, err
		}
		if group {
			u.State.PointerOffset = -1
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ClipOrphan is what the last row on a shared clip knew about it when it
// went away (see the clip_orphans table). Only meaningful while no
// paragraph_audio row names Key.
type ClipOrphan struct {
	Key             string
	DurationSeconds float64
	WordTimings     string
	// OrphanedAt is when the last row left, in Unix seconds.
	OrphanedAt int64
	// Parts are a merge group clip's later members' parts, nil for a clip
	// of one paragraph (or a group orphaned before they were recorded).
	Parts []ClipPart
}

// ClipOrphan returns key's ClipOrphan, nil if none was recorded.
func (s *SQLiteStore) ClipOrphan(key string) (*ClipOrphan, error) {
	o := ClipOrphan{Key: key}
	var parts string
	err := s.db.QueryRow(`SELECT duration_seconds, word_timings, orphaned_at, parts FROM clip_orphans WHERE key = ?`, key).
		Scan(&o.DurationSeconds, &o.WordTimings, &o.OrphanedAt, &parts)
	o.Parts = decodeClipParts(parts)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ClipOrphansSince lists the keys orphaned at or after since (Unix
// seconds).
func (s *SQLiteStore) ClipOrphansSince(since int64) ([]string, error) {
	rows, err := s.db.Query(`SELECT key FROM clip_orphans WHERE orphaned_at >= ?`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// DeleteClipOrphans forgets keys' ClipOrphans.
func (s *SQLiteStore) DeleteClipOrphans(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, key := range keys {
		if _, err := tx.Exec(`DELETE FROM clip_orphans WHERE key = ?`, key); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteClipRows deletes the paragraph_audio rows users name, and with
// them the pointer rows of any merge group one of them anchored (a pointer
// row whose anchor has no row under its voice is nothing's audio).
func (s *SQLiteStore) DeleteClipRows(users []ClipUser) error {
	if len(users) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, u := range users {
		if _, err := tx.Exec(`DELETE FROM paragraph_audio WHERE paragraph_id = ? AND voice_id = ?`, u.ParagraphID, u.VoiceID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`
		DELETE FROM paragraph_audio WHERE pointer_offset > 0 AND NOT EXISTS (
			SELECT 1 FROM paragraphs p
			JOIN paragraphs ap ON ap.chapter_id = p.chapter_id AND ap.idx = p.idx - paragraph_audio.pointer_offset
			JOIN paragraph_audio apa ON apa.paragraph_id = ap.id AND apa.voice_id = paragraph_audio.voice_id
			WHERE p.id = paragraph_audio.paragraph_id)`); err != nil {
		return err
	}
	return tx.Commit()
}
