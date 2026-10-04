package store

import (
	"fmt"
)

// SetChapterAmbienceScored marks chapterID's own passes.ambience true, leaving
// every other Passes field untouched - set by httpapi.ambienceChapter
// once internal/speakerattr.Client.ScoreMusic completes a full,
// uninterrupted pass over the whole chapter, never on a pause or a genuine
// failure partway through. SetChapterAttributed/SetChapterDirected's own
// json_patch shape (see SetChapterAttributed's own doc comment for
// why a single statement like this, not a Go-side read-modify-write).
func (s *SQLiteStore) SetChapterAmbienceScored(chapterID string) error {
	_, err := s.db.Exec(
		`UPDATE chapters SET passes = json_patch(passes, '{"ambience": true}') WHERE id = ?`,
		chapterID,
	)
	return err
}

// SetChapterLit marks chapterID's own passes.lighting true - set by
// httpapi.lightChapter once every music region has a Lighting cue.
func (s *SQLiteStore) SetChapterLit(chapterID string) error {
	_, err := s.db.Exec(
		`UPDATE chapters SET passes = json_patch(passes, '{"lighting": true}') WHERE id = ?`,
		chapterID,
	)
	return err
}

// SetChapterAttributed marks chapterID's own passes.attribution true,
// leaving every other pass untouched. Set by httpapi.attributeChapter once a run finishes covering the whole
// chapter in one uninterrupted pass, never on a pause or a genuine failure
// partway through.
//
// Uses SQLite's json_patch (RFC 7396) rather than a Go-side
// read-modify-write: merging the literal patch object directly into the
// stored value, in one statement, is both simpler and race-free (no
// separate SELECT to go stale between reading and writing) - a merge
// patch's own semantics are exactly "set these keys, leave every other key
// alone" already, precisely what every one of this package's chapters.
// passes writers wants. The patch itself is always one of the two literal
// strings below (no external/request-supplied text ever reaches this
// query), so it's inlined directly rather than bound as a parameter.
func (s *SQLiteStore) SetChapterAttributed(chapterID string) error {
	_, err := s.db.Exec(
		`UPDATE chapters SET passes = json_patch(passes, '{"attribution": true}') WHERE id = ?`,
		chapterID,
	)
	return err
}

// SetChapterScareQuoted marks chapterID's own passes.scareQuote true - set
// by httpapi.scareQuoteChapterForJob once a scare-quote tagging run
// completes over the whole chapter. SetChapterAttributed's shape.
func (s *SQLiteStore) SetChapterScareQuoted(chapterID string) error {
	_, err := s.db.Exec(
		`UPDATE chapters SET passes = json_patch(passes, '{"scareQuote": true}') WHERE id = ?`,
		chapterID,
	)
	return err
}

// SetChapterDirected marks chapterID's own PassDirection true, leaving
// PassAttribution untouched - SetChapterAttributed's own
// sibling, set by httpapi.directChapter once a run finishes covering the
// whole chapter in one uninterrupted pass. See SetChapterAttributed's own
// doc comment for why this is a single json_patch statement rather
// than a Go-side read-modify-write.
func (s *SQLiteStore) SetChapterDirected(chapterID string) error {
	_, err := s.db.Exec(
		`UPDATE chapters SET passes = json_patch(passes, '{"direction": true}') WHERE id = ?`,
		chapterID,
	)
	return err
}

// SetChapterPronounced marks chapterID's own passes.pronunciation true -
// set by httpapi.pronounceChapter once a pronunciation resolution run
// finishes covering the whole chapter. SetChapterDirected's shape.
func (s *SQLiteStore) SetChapterPronounced(chapterID string) error {
	_, err := s.db.Exec(
		`UPDATE chapters SET passes = json_patch(passes, '{"pronunciation": true}') WHERE id = ?`,
		chapterID,
	)
	return err
}

// PassReset describes how ResetBookPass clears one chapter pass: Flag is
// the chapters.passes key, Set is the paragraphs SET clause restoring the
// pass's own columns to their never-run defaults, Where picks which
// paragraphs Set applies to, and Affected picks the paragraphs whose
// already-generated audio was rendered with the old value (and so is stale
// once it's cleared) - "" when the pass never changes generated audio.
type PassReset struct {
	Flag     string
	Set      string
	Where    string
	Affected string
}

// PassResets are the per-chapter passes ResetBookPass knows how to clear,
// keyed by the same names httpapi's bulk actions use. The ambience pass isn't
// here - its output lives in music_regions, not paragraphs (see
// ClearBookMusicRegions).
var PassResets = map[string]PassReset{
	// Attribution leaves a scare-quote paragraph's "Narrator" alone - that
	// comes from scare-quote tagging, not attribution (see
	// SetParagraphScareQuotes). ""/"Narrator" both narrate in the book's
	// own voice, so only a real character's lines have stale audio. A
	// play's line goes back to the speaker the book names for it
	// (Paragraph.SourceSpeaker), not to none.
	"attribution": {
		Flag:     "attribution",
		Set:      "speaker = source_speaker, speaker_index = 0",
		Where:    "speaker <> source_speaker AND NOT scare_quote",
		Affected: "speaker NOT IN ('', 'Narrator') AND speaker <> source_speaker AND NOT scare_quote",
	},
	"scare_quote": {
		Flag:     "scareQuote",
		Set:      "scare_quote = false, speaker = CASE WHEN speaker = 'Narrator' THEN '' ELSE speaker END",
		Where:    "scare_quote",
		Affected: "scare_quote",
	},
	// Only a real dialogue line's emotion is ever applied (see
	// Paragraph.EffectiveEmotion).
	"direction": {
		Flag:     "direction",
		Set:      "emotion = ''",
		Where:    "emotion <> ''",
		Affected: "emotion <> '' AND is_quote AND NOT scare_quote",
	},
	"pronunciation": {
		Flag:     "pronunciation",
		Set:      "pronunciation = '[]'",
		Where:    "pronunciation NOT IN ('[]', 'null')",
		Affected: "pronunciation NOT IN ('[]', 'null')",
	},
}

// ResetBookPass clears pass (a PassResets key) across every chapter of
// bookID with idx >= fromIdx (0 = the whole book) in one transaction: its
// paragraph columns back to defaults and its
// chapters.passes flag unset, as though it had never run. Returns the
// paragraphs PassReset.Affected matched beforehand, as chapterID -> idxs,
// for the caller to invalidate their audio (DeleteParagraphAudioForIdxs) -
// this package never touches the filesystem itself.
func (s *SQLiteStore) ResetBookPass(bookID, pass string, fromIdx int) (map[string][]int, error) {
	pr, ok := PassResets[pass]
	if !ok {
		return nil, fmt.Errorf("unknown pass %q", pass)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	affected := map[string][]int{}
	if pr.Affected != "" {
		rows, err := tx.Query(`SELECT chapter_id, idx FROM paragraphs
			WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ? AND idx >= ?) AND (`+pr.Affected+`)`, bookID, fromIdx)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var chapterID string
			var idx int
			if err := rows.Scan(&chapterID, &idx); err != nil {
				rows.Close()
				return nil, err
			}
			affected[chapterID] = append(affected[chapterID], idx)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	if _, err := tx.Exec(`UPDATE paragraphs SET `+pr.Set+`
		WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ? AND idx >= ?) AND (`+pr.Where+`)`, bookID, fromIdx); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE chapters SET passes = json_patch(passes, json_object(?, NULL)) WHERE book_id = ? AND idx >= ?`, pr.Flag, bookID, fromIdx); err != nil {
		return nil, err
	}
	return affected, tx.Commit()
}
