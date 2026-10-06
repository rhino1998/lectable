package store

import (
	"database/sql"
	"strconv"
	"strings"
)

// AudioState is one (paragraph, voice) pair's generation status, as looked
// up by ParagraphAudioStatuses.
type AudioState struct {
	Status          string
	Error           string
	DurationSeconds float64
	WordTimings     string
	// PointerOffset/PointerSeconds: see paragraph_audio.pointer_offset's
	// own doc comment. PointerOffset 0 (the common case) means this
	// paragraph has its own real audio file; a positive value means its
	// real audio actually lives on the paragraph PointerOffset positions
	// earlier by Idx (same chapter+voice), starting PointerSeconds into
	// that paragraph's own clip - httpapi resolves this into an audioUrl
	// pointing straight at that other paragraph rather than this one.
	PointerOffset  int
	PointerSeconds float64
	// ClipKey is the shared narration clip this audio is (see
	// paragraph_audio.clip_key), "" for a clip of the paragraph's own.
	ClipKey string
}

// ParagraphAudioStatuses batch-looks-up audio state for paragraphIDs under
// one shared voiceID - the replacement for ListParagraphs' old chapter-wide
// JOIN now that a chapter's paragraphs don't all necessarily share one
// voice: callers group paragraph ids by their resolved voice_id first (see
// internal/narration.Resolver) and call this once per distinct voice_id
// actually in use, rather than once per paragraph. A paragraph absent from
// the returned map has never been touched by voiceID (equivalent to the
// old COALESCE(..., 'pending') default) - callers should treat that as
// AudioPending.
func (s *SQLiteStore) ParagraphAudioStatuses(paragraphIDs []string, voiceID string) (map[string]AudioState, error) {
	out := make(map[string]AudioState, len(paragraphIDs))
	if len(paragraphIDs) == 0 {
		return out, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(paragraphIDs)), ",")
	args := make([]any, 0, len(paragraphIDs)+1)
	for _, id := range paragraphIDs {
		args = append(args, id)
	}
	args = append(args, voiceID)

	rows, err := s.db.Query(
		`SELECT paragraph_id, status, error, duration_seconds, word_timings, pointer_offset, pointer_seconds, clip_key
		 FROM paragraph_audio
		 WHERE paragraph_id IN (`+placeholders+`) AND voice_id = ?`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var a AudioState
		if err := rows.Scan(&id, &a.Status, &a.Error, &a.DurationSeconds, &a.WordTimings, &a.PointerOffset, &a.PointerSeconds, &a.ClipKey); err != nil {
			return nil, err
		}
		out[id] = a
	}
	return out, rows.Err()
}

// CountReadyAudioForSpeaker counts bookID's paragraphs attributed to
// speaker (the "Narrator"/"" equivalence handled the same way
// ParagraphsForSpeaker's own does) whose voiceID audio is ready -
// httpapi.handleListSpeakers' own "X/Y generated" figure per row. Scoped
// via a JOIN on (book, speaker, voice) rather than
// ParagraphAudioStatuses' approach of one query placeholder per paragraph
// id, which becomes genuinely expensive for a speaker with many thousands
// of paragraphs (the
// Narrator, on a long book).
func (s *SQLiteStore) CountReadyAudioForSpeaker(bookID, speaker, voiceID string) (int, error) {
	counts, err := s.ReadyAudioCountsBySpeaker(bookID)
	if err != nil {
		return 0, err
	}
	return counts[speaker][voiceID], nil
}

// ReadyAudioCountsBySpeaker is CountReadyAudioForSpeaker for every speaker
// at once: speaker ("" folded into "Narrator") -> voice_id -> how many of
// that speaker's paragraphs have ready audio in that voice. One grouped
// query - the Speakers table used to issue one count per speaker, which on
// a book with hundreds of characters held the database connection for
// seconds per build.
func (s *SQLiteStore) ReadyAudioCountsBySpeaker(bookID string) (map[string]map[string]int, error) {
	rows, err := s.db.Query(`
		SELECT CASE WHEN p.speaker = '' THEN 'Narrator' ELSE p.speaker END AS speaker, pa.voice_id, COUNT(*)
		FROM paragraph_audio pa
		JOIN paragraphs p ON p.id = pa.paragraph_id
		JOIN chapters c ON c.id = p.chapter_id
		WHERE c.book_id = ? AND pa.status = ?
		GROUP BY 1, 2`, bookID, AudioReady)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var speaker, voiceID string
		var n int
		if err := rows.Scan(&speaker, &voiceID, &n); err != nil {
			return nil, err
		}
		if out[speaker] == nil {
			out[speaker] = map[string]int{}
		}
		out[speaker][voiceID] = n
	}
	return out, rows.Err()
}

// GetParagraphAudioStatus reports one paragraph's audio status for one
// voice specifically ("pending" if that voice has never touched it).
func (s *SQLiteStore) GetParagraphAudioStatus(paragraphID, voiceID string) (string, error) {
	var status string
	err := s.db.QueryRow(`SELECT status FROM paragraph_audio WHERE paragraph_id = ? AND voice_id = ?`, paragraphID, voiceID).
		Scan(&status)
	if err == sql.ErrNoRows {
		return AudioPending, nil
	}
	return status, err
}

// upsertParagraphAudio sets one (paragraph, voice) pair's audio state.
// Update first, and only insert if nothing existed to update (written
// for a database whose ON CONFLICT support wasn't trusted; SQLite's is
// fine, this just hasn't needed changing).
// Always resets pointer_offset/pointer_seconds to 0 - every caller of this
// helper (SetParagraphGenerating/SetParagraphReady/SetParagraphError)
// represents a paragraph getting its own ordinary, independent audio state,
// never a scare-quote merge group's own pointer (that's exclusively
// SetParagraphReadyPointer's job) - a paragraph regenerated independently
// after previously being a pointer (jobs.Manager.generateIndependently's
// own fallback) must never leave a stale pointer behind once it has real
// audio of its own again.
func (s *SQLiteStore) upsertParagraphAudio(paragraphID, voiceID, status, errMsg string, durationSeconds float64) error {
	res, err := s.db.Exec(
		`UPDATE paragraph_audio SET status = ?, error = ?, duration_seconds = ?, pointer_offset = 0, pointer_seconds = 0, clip_key = '', clip_parts = '' WHERE paragraph_id = ? AND voice_id = ?`,
		status, errMsg, durationSeconds, paragraphID, voiceID,
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
		`INSERT INTO paragraph_audio (paragraph_id, voice_id, status, error, duration_seconds, chapter_id, chars) VALUES (?, ?, ?, ?, ?, coalesce((SELECT chapter_id FROM paragraphs WHERE id = ?), ''), coalesce((SELECT length(content) FROM paragraphs WHERE id = ?), 0))`,
		paragraphID, voiceID, status, errMsg, durationSeconds, paragraphID, paragraphID,
	)
	return err
}

func (s *SQLiteStore) SetParagraphGenerating(id, voiceID string) error {
	return s.upsertParagraphAudio(id, voiceID, AudioGenerating, "", 0)
}

func (s *SQLiteStore) SetParagraphReady(id, voiceID string, durationSeconds float64) error {
	return s.upsertParagraphAudio(id, voiceID, AudioReady, "", durationSeconds)
}

// SetParagraphReadyPointer marks id ready under voiceID with no audio file
// of its own - see AudioState.PointerOffset's own doc comment. Used only
// for a scare-quote merge group's own non-anchor members
// (jobs.Manager.handleMergedResult): rather than decoding/slicing/
// re-encoding a shared clip into a separate file per paragraph (a real,
// observed source of audible artifacts at the cut point - forced-alignment
// word boundaries are precise enough for karaoke-highlighting but not for
// actually splicing audio), every member but the anchor just records where
// its own content starts within the anchor's own untouched, never-edited
// recording. durationSeconds is still this paragraph's own logical share
// (next member's pointerSeconds minus this one's, or the anchor's own
// total duration minus this one's for the last member) - used for chapter/
// book listening-time estimates exactly as it is for a paragraph with its
// own file, even though no playback boundary is ever actually enforced
// against it (see usePlayback.ts, which plays straight through the whole
// shared clip and only uses these values to track which paragraph is
// "current" for highlighting/position-reporting).
func (s *SQLiteStore) SetParagraphReadyPointer(id, voiceID string, pointerOffset int, pointerSeconds, durationSeconds float64) error {
	res, err := s.db.Exec(
		`UPDATE paragraph_audio SET status = ?, error = '', duration_seconds = ?, pointer_offset = ?, pointer_seconds = ?, clip_key = '', clip_parts = '' WHERE paragraph_id = ? AND voice_id = ?`,
		AudioReady, durationSeconds, pointerOffset, pointerSeconds, id, voiceID,
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
		`INSERT INTO paragraph_audio (paragraph_id, voice_id, status, duration_seconds, pointer_offset, pointer_seconds, chapter_id, chars) VALUES (?, ?, ?, ?, ?, ?, coalesce((SELECT chapter_id FROM paragraphs WHERE id = ?), ''), coalesce((SELECT length(content) FROM paragraphs WHERE id = ?), 0))`,
		id, voiceID, AudioReady, durationSeconds, pointerOffset, pointerSeconds, id, id,
	)
	return err
}

// SetParagraphWordTimings records alignment results for an already-ready
// paragraph - a follow-up call after SetParagraphReady, not part of the
// same upsert, since alignment runs as a separate best-effort step after
// audio is already generated and playable (see jobs.Manager.alignParagraph).
// A no-op if the (paragraph, voice) row doesn't exist yet, which shouldn't
// happen in practice (SetParagraphReady always runs first) but isn't worth
// erroring over if it somehow did. Timings describe the clip, so every
// other row sharing this one's clip (paragraph_audio.clip_key) gets them
// too.
func (s *SQLiteStore) SetParagraphWordTimings(id, voiceID, wordTimingsJSON string) error {
	if _, err := s.db.Exec(
		`UPDATE paragraph_audio SET word_timings = ? WHERE paragraph_id = ? AND voice_id = ?`,
		wordTimingsJSON, id, voiceID,
	); err != nil {
		return err
	}
	var key string
	err := s.db.QueryRow(`SELECT clip_key FROM paragraph_audio WHERE paragraph_id = ? AND voice_id = ?`, id, voiceID).Scan(&key)
	if err != nil || key == "" {
		if err == sql.ErrNoRows {
			err = nil
		}
		return err
	}
	// "clip_key <> ''" is what lets the partial index serve this.
	_, err = s.db.Exec(
		`UPDATE paragraph_audio SET word_timings = ?
		 WHERE clip_key = ? AND clip_key <> '' AND status = ? AND NOT (paragraph_id = ? AND voice_id = ?)`,
		wordTimingsJSON, key, AudioReady, id, voiceID,
	)
	return err
}

func (s *SQLiteStore) SetParagraphError(id, voiceID, message string) error {
	return s.upsertParagraphAudio(id, voiceID, AudioError, message, 0)
}

// ResetParagraphAudio puts an already-generated (ready or error) paragraph
// back to pending for voiceID, so the job queue picks it up again - for an
// explicit "regenerate this paragraph" request, not the normal pending ->
// generating -> ready path. Also clears word_timings (the old alignment
// belongs to audio that's about to be overwritten, and would otherwise
// linger and mismatch the new clip until re-alignment finishes) and any
// pointer_offset/pointer_seconds (whether this paragraph ends up its own
// real file or a pointer again is re-decided fresh by whatever regenerates
// it - see AudioState.PointerOffset's own doc comment - so a stale pointer
// value must never survive past this reset).
func (s *SQLiteStore) ResetParagraphAudio(id, voiceID string) error {
	res, err := s.db.Exec(
		`UPDATE paragraph_audio SET status = ?, error = '', duration_seconds = 0, word_timings = '[]', pointer_offset = 0, pointer_seconds = 0, clip_key = '', clip_parts = '' WHERE paragraph_id = ? AND voice_id = ?`,
		AudioPending, id, voiceID,
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
		`INSERT INTO paragraph_audio (paragraph_id, voice_id, status, chapter_id, chars) VALUES (?, ?, ?, coalesce((SELECT chapter_id FROM paragraphs WHERE id = ?), ''), coalesce((SELECT length(content) FROM paragraphs WHERE id = ?), 0))`,
		id, voiceID, AudioPending, id, id,
	)
	return err
}

// DeleteBookAudio drops every paragraph_audio row for bookID, across every
// voice_id it's ever been generated under (see that table's own doc
// comment on why more than one can exist per paragraph) - the "delete
// generated audio" button's DB half, for reclaiming space/starting a
// book's narration over without touching its text, chapters, or speaker
// attribution. httpapi.handleDeleteBookAudio pairs this with removing the
// book's own audio directory on disk (the actual .wav files) - this alone
// only clears the DB's own bookkeeping about them, the same split
// DeleteBook's own paragraph/file cleanup already follows.
func (s *SQLiteStore) DeleteBookAudio(bookID string) error {
	_, err := s.db.Exec(
		`DELETE FROM paragraph_audio WHERE paragraph_id IN (
			SELECT id FROM paragraphs WHERE chapter_id IN (
				SELECT id FROM chapters WHERE book_id = ?
			)
		)`,
		bookID,
	)
	return err
}

// DeleteChapterAudio is DeleteBookAudio's single-chapter counterpart - the
// chapter-header "Clear generation" button's DB half, for resetting just
// one chapter's narration (across every voice_id it's ever been generated
// under) without touching the rest of the book. httpapi.handleDeleteChapterAudio
// pairs this with removing that chapter's own audio directory on disk,
// same split as the book-level version.
func (s *SQLiteStore) DeleteChapterAudio(chapterID string) error {
	_, err := s.db.Exec(
		`DELETE FROM paragraph_audio WHERE paragraph_id IN (
			SELECT id FROM paragraphs WHERE chapter_id = ?
		)`,
		chapterID,
	)
	return err
}

// AudioRefs is everything under LECTABLE_DATA_DIR/audio the database still
// refers to - what audiomaint.SweepOrphans keeps.
type AudioRefs struct {
	// ChapterBooks maps every chapter id to its book id; every book id
	// is in Books.
	ChapterBooks map[string]string
	Books        map[string]bool
	// ChapterVoices holds, per chapter, every voice_id with any
	// paragraph_audio row there - rows are created (as generating)
	// before their file is written, and an old voice's rows are kept
	// until explicitly deleted, so its files are too.
	ChapterVoices map[string]map[string]bool
	// ChapterRegions holds, per chapter, every music region id.
	ChapterRegions map[string]map[string]bool
	// BookAmbience holds, per book, every non-empty ambience prompt a
	// region uses (see audiopath.AmbienceLoopFile).
	BookAmbience map[string]map[string]bool
	// OwnClips holds every (chapter, voice, paragraph idx) with a ready
	// paragraph_audio row that isn't a merge group's pointer, as
	// OwnClipRef gives it - the only slots a file at a paragraph's own
	// path (audiopath.ParagraphFile) can still be the audio of. Nearly all
	// of those rows name a shared clip instead, but one may still be
	// served from its own path (audiopath.NarrationClipFile), so none of
	// their files are orphans.
	OwnClips map[string]bool
}

// OwnClipRef is AudioRefs.OwnClips' key.
func OwnClipRef(chapterID, voiceID string, idx int) string {
	return chapterID + "/" + voiceID + "/" + strconv.Itoa(idx)
}

// AudioRefs reads the whole library's AudioRefs in four scans.
func (s *SQLiteStore) AudioRefs() (AudioRefs, error) {
	refs := AudioRefs{
		ChapterBooks:   map[string]string{},
		Books:          map[string]bool{},
		ChapterVoices:  map[string]map[string]bool{},
		ChapterRegions: map[string]map[string]bool{},
		BookAmbience:   map[string]map[string]bool{},
		OwnClips:       map[string]bool{},
	}
	addTo := func(m map[string]map[string]bool, k, v string) {
		if m[k] == nil {
			m[k] = map[string]bool{}
		}
		m[k][v] = true
	}
	scan := func(query string, row func(a, b, c string)) error {
		rows, err := s.db.Query(query)
		if err != nil {
			return err
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			return err
		}
		vals := make([]string, 3)
		ptrs := []any{&vals[0], &vals[1], &vals[2]}[:len(cols)]
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			row(vals[0], vals[1], vals[2])
		}
		return rows.Err()
	}
	if err := scan(`SELECT id FROM books`, func(id, _, _ string) {
		refs.Books[id] = true
	}); err != nil {
		return refs, err
	}
	if err := scan(`SELECT id, book_id FROM chapters`, func(id, bookID, _ string) {
		refs.ChapterBooks[id] = bookID
	}); err != nil {
		return refs, err
	}
	if err := scan(`SELECT DISTINCT chapter_id, voice_id FROM paragraph_audio WHERE chapter_id <> ''`, func(chapterID, voiceID, _ string) {
		addTo(refs.ChapterVoices, chapterID, voiceID)
	}); err != nil {
		return refs, err
	}
	if err := scan(`SELECT p.chapter_id, pa.voice_id, p.idx FROM paragraph_audio pa JOIN paragraphs p ON p.id = pa.paragraph_id
		WHERE pa.status = 'ready' AND pa.pointer_offset = 0`, func(chapterID, voiceID, idx string) {
		if n, err := strconv.Atoi(idx); err == nil {
			refs.OwnClips[OwnClipRef(chapterID, voiceID, n)] = true
		}
	}); err != nil {
		return refs, err
	}
	if err := scan(`SELECT r.chapter_id, r.id, r.ambience FROM music_regions r`, func(chapterID, regionID, ambience string) {
		addTo(refs.ChapterRegions, chapterID, regionID)
		if bookID, ok := refs.ChapterBooks[chapterID]; ok && ambience != "" {
			addTo(refs.BookAmbience, bookID, ambience)
		}
	}); err != nil {
		return refs, err
	}
	return refs, nil
}

// SpeakerAudioRef identifies one on-disk audio file
// (audiopath.ParagraphFile(dataDir, BookID, ChapterID, VoiceID, Idx)) that
// DeleteParagraphAudioForSpeaker's matching DB delete just orphaned -
// returned so the caller can remove it too, the same DB-delete/disk-delete
// pairing handleDeleteBookAudio/handleDeleteChapterAudio already use, just
// per-file here since a speaker's paragraphs are scattered through a
// chapter rather than filling it entirely (RemoveAll-ing a whole chapter's
// audio directory the way those two do would also wipe every other
// speaker's and the narrator's own lines in it).
type SpeakerAudioRef struct {
	BookID    string
	ChapterID string
	VoiceID   string
	Idx       int
}

// DeleteParagraphAudioForSpeaker deletes every paragraph_audio row - across
// every voice_id it's ever been generated under, not just whichever one
// currently resolves, same "every voice_id" scope DeleteBookAudio/
// DeleteChapterAudio already use - for paragraphs attributed to name,
// across bookIDs. This is the speaker-scoped counterpart of those two,
// used when a character's voice characterization changes (see
// httpapi.recharacterizeAndInvalidate) so their previously generated
// lines - narrated in a voice/characterization that no longer matches who
// they are - don't linger reachable (and wasting disk space) under their
// old voice_id. Reads every matching row before deleting anything so the
// caller has enough to remove the matching files, mirroring how
// ListParagraphsRaw's callers always fully drain a result set before
// running another query.
//
// name is compared with a plain equality, not ParagraphsForSpeaker's own
// "Narrator"/"" folding - this is only ever called with a real character's
// name (Narrator isn't a Character row and is never recharacterized), so
// that special case doesn't apply here.
func (s *SQLiteStore) DeleteParagraphAudioForSpeaker(bookIDs []string, name string) ([]SpeakerAudioRef, error) {
	var out []SpeakerAudioRef
	for _, bookID := range bookIDs {
		rows, err := s.db.Query(`
			SELECT c.book_id, p.chapter_id, pa.voice_id, p.idx
			FROM paragraph_audio pa
			JOIN paragraphs p ON p.id = pa.paragraph_id
			JOIN chapters c ON c.id = p.chapter_id
			WHERE c.book_id = ? AND p.speaker = ?`, bookID, name)
		if err != nil {
			return out, err
		}
		var refs []SpeakerAudioRef
		for rows.Next() {
			var ref SpeakerAudioRef
			if err := rows.Scan(&ref.BookID, &ref.ChapterID, &ref.VoiceID, &ref.Idx); err != nil {
				rows.Close()
				return out, err
			}
			refs = append(refs, ref)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return out, err
		}
		rows.Close()

		if _, err := s.db.Exec(`
			DELETE FROM paragraph_audio WHERE paragraph_id IN (
				SELECT id FROM paragraphs WHERE chapter_id IN (
					SELECT id FROM chapters WHERE book_id = ?
				) AND speaker = ?
			)`, bookID, name); err != nil {
			return out, err
		}
		out = append(out, refs...)
	}
	return out, nil
}

// DeleteParagraphAudioForIdxs deletes every paragraph_audio row (across
// every voice_id) for the paragraphs of chapterID at idxs - plus, per
// voice, every other row in the same scare-quote merge group (see
// paragraph_audio.pointer_offset), since a pointer paragraph's speech lives
// in its group head's file and the head's file holds every member's: one
// member's text changing means the whole group's clip is stale. The
// paragraph-scoped counterpart of DeleteChapterAudio, used by
// httpapi.pronounceChapter so re-resolving pronunciation only throws away
// the audio whose generation text actually changed. Returns every deleted
// row so the caller can remove the matching files (a pointer row has no
// file of its own - removing its path is a harmless no-op).
func (s *SQLiteStore) DeleteParagraphAudioForIdxs(bookID, chapterID string, idxs []int) ([]SpeakerAudioRef, error) {
	if len(idxs) == 0 {
		return nil, nil
	}
	want := make(map[int]bool, len(idxs))
	for _, idx := range idxs {
		want[idx] = true
	}
	type audioRow struct {
		paragraphID, voiceID string
		idx, pointerOffset   int
	}
	rows, err := s.db.Query(`
		SELECT pa.paragraph_id, pa.voice_id, p.idx, pa.pointer_offset
		FROM paragraph_audio pa
		JOIN paragraphs p ON p.id = pa.paragraph_id
		WHERE p.chapter_id = ?`, chapterID)
	if err != nil {
		return nil, err
	}
	var all []audioRow
	for rows.Next() {
		var r audioRow
		if err := rows.Scan(&r.paragraphID, &r.voiceID, &r.idx, &r.pointerOffset); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	type groupKey struct {
		voiceID string
		head    int
	}
	stale := make(map[groupKey]bool)
	for _, r := range all {
		if want[r.idx] {
			stale[groupKey{r.voiceID, r.idx - r.pointerOffset}] = true
		}
	}
	if len(stale) == 0 {
		return nil, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var out []SpeakerAudioRef
	for _, r := range all {
		if !stale[groupKey{r.voiceID, r.idx - r.pointerOffset}] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM paragraph_audio WHERE paragraph_id = ? AND voice_id = ?`, r.paragraphID, r.voiceID); err != nil {
			return nil, err
		}
		out = append(out, SpeakerAudioRef{BookID: bookID, ChapterID: chapterID, VoiceID: r.voiceID, Idx: r.idx})
	}
	return out, tx.Commit()
}

// DeleteParagraphAudioByVoiceID deletes every paragraph_audio row within
// bookID matching voiceID exactly - the voice-preset-scoped counterpart of
// DeleteParagraphAudioForSpeaker (which filters by speaker name across
// every voice_id) for when the caller already knows the precise voice_id a
// change (e.g. re-rendering a custom preset's reference clip - see
// httpapi's "Regenerate" button on the Voices page) actually invalidates,
// rather than every voice_id a speaker identity has ever used. No speaker
// filter needed: voice_id already uniquely encodes (presetID, instruct,
// language) per store.VoiceID, so any paragraph whose audio was generated
// under this exact voice - book-level or character-level, whichever
// speaker it's attributed to - necessarily has this same voice_id, and
// nothing generated under a different voice can collide with it.
func (s *SQLiteStore) DeleteParagraphAudioByVoiceID(bookID, voiceID string) ([]SpeakerAudioRef, error) {
	rows, err := s.db.Query(`
		SELECT c.book_id, p.chapter_id, pa.voice_id, p.idx
		FROM paragraph_audio pa
		JOIN paragraphs p ON p.id = pa.paragraph_id
		JOIN chapters c ON c.id = p.chapter_id
		WHERE c.book_id = ? AND pa.voice_id = ?`, bookID, voiceID)
	if err != nil {
		return nil, err
	}
	var refs []SpeakerAudioRef
	for rows.Next() {
		var ref SpeakerAudioRef
		if err := rows.Scan(&ref.BookID, &ref.ChapterID, &ref.VoiceID, &ref.Idx); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if _, err := s.db.Exec(`
		DELETE FROM paragraph_audio WHERE voice_id = ? AND paragraph_id IN (
			SELECT id FROM paragraphs WHERE chapter_id IN (
				SELECT id FROM chapters WHERE book_id = ?
			)
		)`, voiceID, bookID); err != nil {
		return refs, err
	}
	return refs, nil
}

// NarrationStats summarizes a book's text/audio so far, used to derive a
// self-calibrating narration-length estimate: once at least one paragraph
// has real generated audio, seconds-per-character for that book's voice is
// known and can be extrapolated across the rest of the (mostly ungenerated)
// text.
type NarrationStats struct {
	TotalChars int64
	// TotalWords counts the text's words as its space-separated runs.
	TotalWords   int64
	ReadyChars   int64
	ReadySeconds float64
}
