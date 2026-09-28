package store

import (
	"database/sql"
)

// SetMusicRegionLighting stores each region id's lighting cue in one
// transaction - one lighting sub-pass call's worth (a region plus the
// length-only splits sharing its description).
func (s *DuckStore) SetMusicRegionLighting(lighting map[string]Lighting) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, l := range lighting {
		if _, err := tx.Exec(`UPDATE music_regions SET lighting = ? WHERE id = ?`, l, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ClearBookLighting drops every music region's lighting cue across bookID
// and unsets each chapter's passes.lighting, leaving the music itself
// alone - the Speakers page's "lighting" reset, so the next ambience run
// re-lights every chapter without re-scoring its music.
func (s *DuckStore) ClearBookLighting(bookID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE music_regions SET lighting = NULL WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, bookID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE chapters SET passes = json_merge_patch(passes, '{"lighting": null}') WHERE book_id = ?`, bookID); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearMusicRegions deletes every music_regions row for chapterID and
// clears the chapter's own passes.ambience and passes.lighting flags back
// to unset (lighting lives on the regions, so it goes with them) (same
// json_merge_patch-null-deletes-the-key pattern ResetBookAttribution's own
// bulk equivalent uses) - called once, up front, by
// httpapi.ambienceChapter's own wipeExisting branch before a (re)scoring
// run starts on a chapter that's already fully scored: a chapter's regions
// must always fully partition its paragraphs with no gaps/overlaps, so a
// partial old set can never usefully coexist with a fresh run's own output
// the way, say, tts_tags accumulates incrementally across reruns.
//
// The passes.ambience reset is not just tidiness: ambienceChapter only
// takes its "wipe and restart from paragraph 0" branch when
// ch.Passes.Ambience is already true (resumeMusicScoring's own
// alreadyFullyScored case) - its own "resume from the last persisted
// region" branch, the one that actually accumulates progress across
// retries, only ever triggers when passes.ambience is still false. Before
// this reset existed, a fresh run dispatched from here that itself paused
// partway (e.g. real scheduling contention - s.Jobs.HasHigherPriorityWork,
// checked between ScoreMusic's own batches) left the chapter with a
// leftover true flag and only its own newest partial region: every later
// rescore attempt would see that same still-true flag, take this same
// wipe-and-restart-from-0 branch again, and could stall hitting the same
// pause point every single time - zero net progress across retries no
// matter how many times it's retriggered, the exact failure mode
// ambienceChapter's own resume path exists to prevent, just reached via
// this wipe branch instead of the original always-wipe design it replaced.
//
// Returns whatever existed before deleting (same "read before delete,
// return refs" shape DeleteParagraphAudioForSpeaker below already uses) so
// the caller can remove each region's own on-disk audio file - this
// package never touches the filesystem itself, see DeleteChapterAudio's
// own doc comment for that DB-delete/disk-delete split.
func (s *DuckStore) ClearMusicRegions(chapterID string) ([]MusicRegion, error) {
	regions, err := s.ListMusicRegions(chapterID)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM music_regions WHERE chapter_id = ?`, chapterID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(
		`UPDATE chapters SET passes = json_merge_patch(passes, '{"ambience": null, "lighting": null}') WHERE id = ?`,
		chapterID,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return regions, nil
}

// AppendMusicRegions inserts regions as the next contiguous run of a
// chapter's own music_regions.idx sequence, starting from whatever the
// chapter's current max idx + 1 is (0 if the chapter has none yet) - see
// ScoreMusic's own doc comment for why regions are persisted incrementally,
// batch by batch, rather than all at once at the very end. Assigns each
// inserted region a fresh ID and returns the persisted rows (with ID/Idx
// filled in) so the caller (httpapi.ambienceChapter) can react to newly
// available regions - e.g. kick maybeAdvanceChapterMusic - without a
// separate re-list.
//
// Computes and stores every inserted region's own EndIdx here - the only
// place that ever does - rather than deriving it fresh on every read
// (every real caller lists a whole chapter's regions at once, so storing
// it once at write time is cheaper than recomputing it on every list).
// Within this one batch, a region's EndIdx is simply its own next
// sibling's StartIdx minus one; regions[len(regions)-1] instead gets
// lastParagraphIdx as a provisional value - the chapter's own real last
// paragraph if this call's caller (httpapi.ambienceChapter) covered the
// chapter all the way to its own end, or wherever that call's own attempt
// actually stopped otherwise (a pause partway through, resumed by a later
// call rather than restarted - see ambienceChapter's own doc comment),
// since whether anything comes after it depends on whether a *later*
// batch gets appended too. If one does, that later call's own first step
// fixes this batch's own last region back up to the real boundary, before
// inserting anything new - see the UPDATE below.
func (s *DuckStore) AppendMusicRegions(chapterID string, regions []MusicRegionInput, lastParagraphIdx int) ([]MusicRegion, error) {
	if len(regions) == 0 {
		return nil, nil
	}
	var nextIdx int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(idx) + 1, 0) FROM music_regions WHERE chapter_id = ?`, chapterID).Scan(&nextIdx)
	if err != nil {
		return nil, err
	}
	if nextIdx > 0 {
		// This chapter already has at least one region from an earlier
		// batch - its own last one (idx = nextIdx-1) had its EndIdx set
		// provisionally to lastParagraphIdx, since at the time it was
		// inserted there was no way yet to know a following batch (this
		// one) would exist. Now that one does, it actually ends right
		// where this batch's own first region begins.
		if _, err := s.db.Exec(
			`UPDATE music_regions SET end_idx = ? WHERE chapter_id = ? AND idx = ?`,
			regions[0].StartIdx-1, chapterID, nextIdx-1,
		); err != nil {
			return nil, err
		}
	}
	out := make([]MusicRegion, 0, len(regions))
	for i, r := range regions {
		endIdx := lastParagraphIdx
		if i+1 < len(regions) {
			endIdx = regions[i+1].StartIdx - 1
		}
		region := MusicRegion{
			ID:         NewID(),
			ChapterID:  chapterID,
			Idx:        nextIdx + i,
			StartIdx:   r.StartIdx,
			EndIdx:     endIdx,
			Mood:       r.Mood,
			Prompt:     r.Prompt,
			Ambience:   r.Ambience,
			Transition: r.Transition,
			Status:     AudioPending,
		}
		_, err := s.db.Exec(
			`INSERT INTO music_regions (id, chapter_id, idx, start_idx, end_idx, mood, prompt, ambience, transition, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			region.ID, region.ChapterID, region.Idx, region.StartIdx, region.EndIdx, region.Mood, region.Prompt, region.Ambience, region.Transition, region.Status,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, region)
	}
	return out, nil
}

// MusicRegionInput is AppendMusicRegions' own insert shape - just the
// fields internal/speakerattr.Client.ScoreMusic actually decides (only a
// StartIdx, no end - see MusicRegion.EndIdx's own doc comment), leaving
// ID/Status (always freshly generated/AudioPending on insert) and
// DurationSeconds (only meaningful once generation finishes) out.
type MusicRegionInput struct {
	StartIdx   int
	Mood       string
	Prompt     string
	Ambience   string
	Transition MusicTransition
}

// ListMusicRegions returns chapterID's own music regions in idx order.
func (s *DuckStore) ListMusicRegions(chapterID string) ([]MusicRegion, error) {
	rows, err := s.db.Query(`SELECT id, chapter_id, idx, start_idx, end_idx, mood, prompt, ambience, transition, lighting, status, error, duration_seconds FROM music_regions WHERE chapter_id = ? ORDER BY idx ASC`, chapterID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MusicRegion
	for rows.Next() {
		var r MusicRegion
		if err := rows.Scan(&r.ID, &r.ChapterID, &r.Idx, &r.StartIdx, &r.EndIdx, &r.Mood, &r.Prompt, &r.Ambience, &r.Transition, &r.Lighting, &r.Status, &r.Error, &r.DurationSeconds); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetMusicRegion looks up one region by id - httpapi.handleGetMusicRegionAudio's
// own single-row lookup, not a whole chapter's list.
func (s *DuckStore) GetMusicRegion(id string) (*MusicRegion, error) {
	var r MusicRegion
	err := s.db.QueryRow(`SELECT id, chapter_id, idx, start_idx, end_idx, mood, prompt, ambience, transition, lighting, status, error, duration_seconds FROM music_regions WHERE id = ?`, id).
		Scan(&r.ID, &r.ChapterID, &r.Idx, &r.StartIdx, &r.EndIdx, &r.Mood, &r.Prompt, &r.Ambience, &r.Transition, &r.Lighting, &r.Status, &r.Error, &r.DurationSeconds)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// MusicRegionCount is one chapter's background-music progress - see
// MusicRegionCounts.
type MusicRegionCount struct {
	Total, Ready, Error int
}

// MusicRegionCounts returns every scored chapter of bookID's own music
// region counts, keyed by chapter id - a chapter with no regions (not
// scored) is simply absent. One grouped query for the whole book, for the
// Speakers page's chapter table.
func (s *DuckStore) MusicRegionCounts(bookID string) (map[string]MusicRegionCount, error) {
	rows, err := s.db.Query(`
		SELECT r.chapter_id, COUNT(*),
			COALESCE(SUM(CASE WHEN r.status = ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN r.status = ? THEN 1 ELSE 0 END), 0)
		FROM music_regions r
		JOIN chapters c ON c.id = r.chapter_id
		WHERE c.book_id = ?
		GROUP BY r.chapter_id`, AudioReady, AudioError, bookID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]MusicRegionCount{}
	for rows.Next() {
		var id string
		var c MusicRegionCount
		if err := rows.Scan(&id, &c.Total, &c.Ready, &c.Error); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}

// SetMusicRegionGenerating/SetMusicRegionReady/SetMusicRegionError are
// music_regions' own status-transition writes - SetParagraphGenerating/
// SetParagraphReady/SetParagraphError's own counterparts (upsertSFXStatus
// below), just against this table instead.
func (s *DuckStore) SetMusicRegionGenerating(id string) error {
	_, err := s.db.Exec(`UPDATE music_regions SET status = ?, error = '' WHERE id = ?`, AudioGenerating, id)
	return err
}

func (s *DuckStore) SetMusicRegionReady(id string, durationSeconds float64) error {
	_, err := s.db.Exec(`UPDATE music_regions SET status = ?, error = '', duration_seconds = ? WHERE id = ?`, AudioReady, durationSeconds, id)
	return err
}

func (s *DuckStore) SetMusicRegionError(id, message string) error {
	_, err := s.db.Exec(`UPDATE music_regions SET status = ?, error = ? WHERE id = ?`, AudioError, message, id)
	return err
}

// ResetMusicRegionAudio puts a single already-touched (ready, error, or
// even still-pending) region back to AudioPending - ResetParagraphAudio's
// own counterpart, for the reader's per-region "Generate"/"Regenerate"
// button (see httpapi.handleRegenerateMusicRegion) rather than
// ResetChapterMusicAudio's whole-chapter scope. Never touches this
// region's own scored mood/prompt/transition - only its generation state,
// so jobs.Manager.MaybeAdvanceChapterMusic picks it back up as the next
// eligible region to generate.
func (s *DuckStore) ResetMusicRegionAudio(id string) error {
	_, err := s.db.Exec(`UPDATE music_regions SET status = ?, error = '', duration_seconds = 0 WHERE id = ?`, AudioPending, id)
	return err
}

// ResetChapterMusicAudio resets every one of chapterID's own music regions
// back to AudioPending (duration 0) while leaving their own scored mood/
// prompt/transition untouched - called whenever this chapter's narration
// audio itself gets invalidated (DeleteChapterAudio) since a region's
// target duration is derived from its paragraphs' own real narration
// durations (see store.MusicRegion's own doc comment) and so goes stale
// the moment that narration is deleted/regenerated. Never touches
// passes.ambience or deletes any row - re-scoring is not needed
// just because narration changed, only re-generating the audio once
// narration is ready again. Returns the regions as they stood before the
// reset (same reasoning as ClearMusicRegions) so the caller can remove
// each one's now-stale on-disk clip.
func (s *DuckStore) ResetChapterMusicAudio(chapterID string) ([]MusicRegion, error) {
	regions, err := s.ListMusicRegions(chapterID)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`UPDATE music_regions SET status = ?, error = '', duration_seconds = 0 WHERE chapter_id = ?`, AudioPending, chapterID); err != nil {
		return nil, err
	}
	return regions, nil
}

// ClearBookMusicRegions is ClearMusicRegions for every chapter of bookID at
// once: deletes every music region and unsets each chapter's
// passes.ambience/passes.lighting.
// The caller removes the regions' audio files (audiopath.MusicDir).
func (s *DuckStore) ClearBookMusicRegions(bookID string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM music_regions WHERE chapter_id IN (SELECT id FROM chapters WHERE book_id = ?)`, bookID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE chapters SET passes = json_merge_patch(passes, '{"ambience": null, "lighting": null}') WHERE book_id = ?`, bookID); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetAllChapterMusicAudioForBook resets every music region across every
// chapter of bookID back to AudioPending (duration 0) - DeleteBookAudio's
// own music counterpart. Pure DB, like ResetChapterMusicAudio - the
// caller (handleDeleteBookAudio) already removes this whole book's
// `audio/<bookID>` directory wholesale, which already covers every
// chapter's own music subdirectory (audiopath.MusicDir nests under it,
// the same way SFXDir/VoiceDir do), so there's no per-region file to
// return/remove here the way ClearMusicRegions/ResetChapterMusicAudio
// have to for their own single-chapter scope.
func (s *DuckStore) ResetAllChapterMusicAudioForBook(bookID string) error {
	_, err := s.db.Exec(
		`UPDATE music_regions SET status = ?, error = '', duration_seconds = 0 WHERE chapter_id IN (
			SELECT id FROM chapters WHERE book_id = ?
		)`,
		AudioPending, bookID,
	)
	return err
}
