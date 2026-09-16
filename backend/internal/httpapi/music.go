package httpapi

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// musicRegionDTO is one store.MusicRegion, reshaped for the frontend's own
// background-music playback engine (see store.MusicRegion's own doc
// comment for the generation pipeline this reflects). AudioURL is only
// set once Status is AudioReady - the same "no URL until it means
// something" convention paragraphDTO already uses.
type musicRegionDTO struct {
	ID       string `json:"id"`
	StartIdx int    `json:"startIdx"`
	EndIdx   int    `json:"endIdx"`
	Mood     string `json:"mood"`
	// Prompt is the actual Stable Audio generation prompt ScoreMusic wrote
	// for this region (store.MusicRegion.Prompt) - Mood is just a short
	// label for compact display (table/badge text); Prompt is the fuller
	// "score" a reader can see without needing to actually hear the
	// result, surfaced by the reader's annotations view as this region's
	// own boundary marker tooltip (see ChapterSection.MusicRegionBoundary).
	Prompt          string  `json:"prompt"`
	Transition      string  `json:"transition"` // "cut" or "continuation" - see store.MusicTransition
	Status          string  `json:"status"`
	Error           string  `json:"error,omitempty"`
	DurationSeconds float64 `json:"durationSeconds"`
	AudioURL        string  `json:"audioUrl,omitempty"`
}

type chapterMusicDTO struct {
	Enabled bool             `json:"enabled"`
	Scored  bool             `json:"scored"`
	Regions []musicRegionDTO `json:"regions"`
}

func musicRegionDTOFrom(r store.MusicRegion) musicRegionDTO {
	d := musicRegionDTO{
		ID:              r.ID,
		StartIdx:        r.StartIdx,
		EndIdx:          r.EndIdx,
		Mood:            r.Mood,
		Prompt:          r.Prompt,
		Transition:      string(r.Transition),
		Status:          r.Status,
		Error:           r.Error,
		DurationSeconds: r.DurationSeconds,
	}
	if r.Status == store.AudioReady {
		d.AudioURL = "/api/music-regions/" + r.ID + "/audio"
	}
	return d
}

// handleGetChapterMusic serves GET .../chapters/{idx}/music - the reader's
// own background-music mixer polls this while scoring/generation is in
// progress (no websocket push for this yet, unlike paragraph audio - music
// regions are few enough per chapter, and generate slowly enough, that
// polling is fine for a first version), and the Speakers page's chapter
// table reads book.chapters[idx].passes.music/store.Book.MusicEnabled
// directly (already part of the book fetch it already has) rather than
// this endpoint, which exists for the regions themselves.
func (s *Server) handleGetChapterMusic(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	chapterIdx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	ch, err := s.Store.GetChapterByIdx(bookID, chapterIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	regions, err := s.Store.ListMusicRegions(ch.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	dto := chapterMusicDTO{Enabled: book.MusicEnabled, Scored: ch.Passes.Music, Regions: make([]musicRegionDTO, len(regions))}
	for i, region := range regions {
		dto.Regions[i] = musicRegionDTOFrom(region)
	}
	writeJSON(w, http.StatusOK, dto)
}

// handleScoreChapterMusic is POST .../chapters/{idx}/score-music - triggers
// internal/speakerattr.Client.ScoreMusic for one chapter, handleAttributeSpeakers/
// handleTagDirections' own exact fire-and-forget shape. Explicit and
// always allowed regardless of store.Book.MusicEnabled (the Speakers
// page's per-row/bulk buttons can score a chapter's music the same way
// they can attribute or direction-tag one, whether or not the reader has
// ever turned the book-wide toggle on) - MusicEnabled only gates whether
// jobs.Manager.MaybeAdvanceChapterMusic goes on to actually *generate* a
// scored region's audio, not whether scoring itself can run. Re-running
// this for an already-scored chapter re-scores it from scratch
// (scoreChapterMusic's own Store.ClearMusicRegions), the same "always
// allow a fresh run" shape the direction-tagging button already has.
func (s *Server) handleScoreChapterMusic(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "background music scoring is not configured (set SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")
	chapterIdx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	ch, err := s.Store.GetChapterByIdx(bookID, chapterIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}

	s.Jobs.EnqueueMusicScoring(book.ID, ch.ID, ch.Idx, func(ctx context.Context) (int, func(), error) {
		return s.scoreChapterMusic(ctx, book, ch)
	})
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

// musicTransitionFromString maps a speakerattr.MusicRegionResult's own
// Transition string (already normalized to "cut"/"continuation" by
// speakerattr's own parseMusicBoundaries) onto store.MusicTransition - anything else
// (shouldn't happen, given that normalization) defaults to the safer
// MusicTransitionCut.
func musicTransitionFromString(s string) store.MusicTransition {
	if s == string(store.MusicTransitionContinuation) {
		return store.MusicTransitionContinuation
	}
	return store.MusicTransitionCut
}

// scoreChapterMusic is the KindMusicScoring task's actual work (see
// jobs.MusicScoreFunc), supplied as a closure from handleScoreChapterMusic.
// Always starts from a clean slate (Store.ClearMusicRegions) - a chapter's
// regions must fully partition its paragraphs with no gaps/overlaps (see
// store.MusicRegion's own doc comment), so a partial old set can never
// usefully coexist with a fresh run's own output.
//
// Persists each batch's regions as speakerattr.Client.ScoreMusic produces
// them (Store.AppendMusicRegions), not only once the whole chapter
// finishes - so a pause partway through (s.Jobs.HasHigherPriorityWork,
// checked between batches) still keeps whatever already scored, and
// MaybeAdvanceChapterMusic can start generating those regions' own audio
// immediately rather than waiting for the rest of the chapter to score
// too. Unlike attributeChapter/directChapter, a pause here is never
// auto-resumed via a requeue closure - ScoreMusic's own batches carry no
// cross-batch context (see musicBatchParagraphs' own doc comment), so a
// later resumed batch picking up mid-chapter would have no more basis for
// judging its own first region's transition than a fresh run's later
// batches already do; simplest and most honest to just leave
// passes.music unset and let a reader-retriggered rescore (which clears
// and restarts cleanly) finish the job, rather than adding resume
// machinery that can't actually do better.
func (s *Server) scoreChapterMusic(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
	oldRegions, err := s.Store.ClearMusicRegions(ch.ID)
	if err != nil {
		return 0, nil, err
	}
	// ClearMusicRegions never touches the filesystem itself (see its own
	// doc comment) - a re-score deletes every region row for this chapter,
	// including any already-AudioReady one, so their own on-disk clips
	// are about to become unreachable (a fresh AppendMusicRegions below
	// hands out new ids, never reusing an old one) - best-effort removal,
	// same as DeleteChapterAudio's own disk-delete pairing: a clip that
	// was never actually generated (still pending/generating) simply has
	// no file to remove, not an error.
	for _, r := range oldRegions {
		_ = os.Remove(audiopath.MusicRegionFile(s.DataDir, book.ID, ch.ID, r.ID))
	}
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		return 0, nil, err
	}
	if len(all) == 0 {
		if err := s.Store.SetChapterMusicScored(ch.ID); err != nil {
			return 0, nil, err
		}
		return 0, nil, nil
	}

	inputs := make([]speakerattr.ParagraphInput, len(all))
	for i, p := range all {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote}
	}

	regions, remaining, scoreErr := s.Speaker.ScoreMusic(ctx, book.Title, ch.Title, inputs, s.Jobs.HasHigherPriorityWork)
	if len(regions) > 0 {
		inputRegions := make([]store.MusicRegionInput, len(regions))
		for i, region := range regions {
			inputRegions[i] = store.MusicRegionInput{
				StartIdx:   region.StartIdx,
				Mood:       region.Mood,
				Prompt:     region.Prompt,
				Transition: musicTransitionFromString(region.Transition),
			}
		}
		// all is ordered by Idx (ListParagraphsRaw), so its own last entry
		// is this chapter's real last paragraph - AppendMusicRegions' own
		// provisional EndIdx for whichever region turns out to be this
		// batch's last one.
		if _, serr := s.Store.AppendMusicRegions(ch.ID, inputRegions, all[len(all)-1].Idx); serr != nil {
			return 0, nil, serr
		}
		// This chapter's narration may already be fully (or partly)
		// generated - e.g. music was just turned on for a chapter that was
		// read/generated long ago - in which case some of these freshly-
		// scored regions are already dispatchable right now, with no future
		// paragraph-completion event left to notice that on its own.
		s.Jobs.MaybeAdvanceChapterMusic(book.ID, ch.ID)
	}
	if scoreErr != nil {
		return len(regions), nil, scoreErr
	}
	if len(remaining) > 0 {
		log.Printf("httpapi: music scoring for chapter %s paused with %d paragraph(s) remaining - retrigger scoring to finish", ch.ID, len(remaining))
		return len(regions), nil, nil
	}
	if err := s.Store.SetChapterMusicScored(ch.ID); err != nil {
		return len(regions), nil, err
	}
	return len(regions), nil, nil
}

// handleGetMusicRegionAudio serves GET /api/music-regions/{id}/audio - the
// frontend's own background-music mixer fetches each region's clip by this
// URL (musicRegionDTO.AudioURL) as narration crosses into its paragraph
// range. Range-request-capable via http.ServeFile, same as
// handleGetAudio/handleGetParagraphSFXAudio.
func (s *Server) handleGetMusicRegionAudio(w http.ResponseWriter, r *http.Request) {
	regionID := r.PathValue("id")
	region, err := s.Store.GetMusicRegion(regionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if region == nil {
		writeError(w, http.StatusNotFound, "music region not found")
		return
	}
	if region.Status != store.AudioReady {
		writeError(w, http.StatusNotFound, "music region audio not ready")
		return
	}
	ch, err := s.chapterByID(region.ChapterID)
	if err != nil || ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	http.ServeFile(w, r, audiopath.MusicRegionFile(s.DataDir, ch.BookID, ch.ID, region.ID))
}

// handleRegenerateMusicRegion is POST /api/music-regions/{id}/regenerate -
// the reader's own per-region "Generate"/"Regenerate" button in
// annotations view (ChapterSection.MusicRegionBoundary), for a region
// that's either never been generated yet (still AudioPending, or
// AudioError from an earlier failed attempt) or one whose already-ready
// clip the reader just wants re-rolled. Always allowed regardless of
// current status - store.Store.ResetMusicRegionAudio's own doc comment
// covers why resetting an already-pending region is harmless.
//
// Deletes the old on-disk clip first (best-effort - a region that was
// never generated has none to remove), then re-dispatches through the
// same jobs.Manager.MaybeAdvanceChapterMusic path narration completion
// already uses, and immediately promotes the freshly (re-)queued
// generation task to TierUrgent - an explicit reader click deserves the
// same "don't wait behind unrelated background work" treatment
// EnqueueParagraphRegenerate's own TierUrgent already gives narration.
// Generation dispatches as one batch task per chapter, not per region (see
// jobs.Manager.EnqueueMusicGeneration's own doc comment), so promoting
// "music_gen:"+chapterID by jobs.Manager.PromoteTier directly already
// covers this region along with whatever else in the chapter is eligible
// to generate alongside it - there's no narrower per-region task id left
// to promote instead.
//
// Known imperfection: doesn't cascade-invalidate a *following*
// MusicTransitionContinuation region that was already generated seeded
// from this region's own (about-to-change) clip - that region's own
// already-ready audio just keeps playing, now seeded from audio that no
// longer matches. Regenerating that one too, if the mismatch is audible,
// is a second manual click away; not worth chasing automatically for a
// first version of this button.
func (s *Server) handleRegenerateMusicRegion(w http.ResponseWriter, r *http.Request) {
	regionID := r.PathValue("id")
	region, err := s.Store.GetMusicRegion(regionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if region == nil {
		writeError(w, http.StatusNotFound, "music region not found")
		return
	}
	ch, err := s.chapterByID(region.ChapterID)
	if err != nil || ch == nil {
		writeError(w, http.StatusNotFound, "chapter not found")
		return
	}
	if err := s.Store.ResetMusicRegionAudio(regionID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.Remove(audiopath.MusicRegionFile(s.DataDir, ch.BookID, ch.ID, regionID))

	s.Jobs.MaybeAdvanceChapterMusic(ch.BookID, ch.ID)
	_ = s.Jobs.PromoteTier("music_gen:"+ch.ID, jobs.TierUrgent) // best-effort - a no-op if not queued yet
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}
