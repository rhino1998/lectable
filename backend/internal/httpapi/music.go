package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
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
	Prompt string `json:"prompt"`
	// Ambience is the region's ambient-soundscape prompt
	// (store.MusicRegion.Ambience), mixed under the music - "" for none.
	Ambience        string      `json:"ambience,omitempty"`
	Transition      string      `json:"transition" tstype:"'cut' | 'continuation'"` // "cut" or "continuation" - see store.MusicTransition
	Status          audioStatus `json:"status"`
	Error           string      `json:"error,omitempty"`
	DurationSeconds float64     `json:"durationSeconds"`
	AudioURL        string      `json:"audioUrl,omitempty"`
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
		Ambience:        r.Ambience,
		Transition:      string(r.Transition),
		Status:          audioStatus(r.Status),
		Error:           r.Error,
		DurationSeconds: r.DurationSeconds,
	}
	if r.Status == store.AudioReady {
		d.AudioURL = "/api/music-regions/" + r.ID + "/audio"
	}
	return d
}

// handleGetChapterMusic serves GET .../chapters/{idx}/music - the reader's
// own background-music mixer watches the same data live as the
// "chapterMusic" topic (see registerLiveTopics), and the Speakers page's chapter
// table reads book.chapters[idx].passes.music/store.Book.MusicEnabled
// directly (already part of the book fetch it already has) rather than
// this endpoint, which exists for the regions themselves.
func (s *Server) handleGetChapterMusic(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	v, err := s.buildChapterMusic(r.PathValue("id"), idx)
	writeBuilt(w, v, err)
}

func (s *Server) buildChapterMusic(bookID string, chapterIdx int) (chapterMusicDTO, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return chapterMusicDTO{}, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return chapterMusicDTO{}, httpError(http.StatusNotFound, "book not found")
	}
	ch, err := s.Store.GetChapterByIdx(bookID, chapterIdx)
	if err != nil {
		return chapterMusicDTO{}, httpError(http.StatusInternalServerError, err.Error())
	}
	if ch == nil {
		return chapterMusicDTO{}, httpError(http.StatusNotFound, "chapter not found")
	}
	regions, err := s.Store.ListMusicRegions(ch.ID)
	if err != nil {
		return chapterMusicDTO{}, httpError(http.StatusInternalServerError, err.Error())
	}
	dto := chapterMusicDTO{Enabled: book.MusicEnabled, Scored: ch.Passes.Music, Regions: make([]musicRegionDTO, len(regions))}
	for i, region := range regions {
		// "generating" is tracked in memory, never stored (see
		// jobs.Manager.MusicRegionGenerating) - overlay it here so the
		// reader still sees which region is being rendered.
		if s.Jobs.MusicRegionGenerating(region.ID) {
			region.Status = store.AudioGenerating
		}
		dto.Regions[i] = musicRegionDTOFrom(region)
	}
	return dto, nil
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
// allow a fresh run" shape the direction-tagging button already has - but
// re-running (or auto-retriggering, e.g. maybeScoreChapterMusic) a chapter
// that's only partially scored resumes from where the last attempt left
// off instead, rather than wiping and redoing that real work; see
// scoreChapterMusic's own doc comment for why.
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
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

// handleGenerateChapterMusic queues the whole-chapter music run for one
// chapter right now (jobs.Manager.GenerateChapterMusic) - the Speakers
// page's per-chapter button. 202 {"queued": n} with how many regions it
// queued (0 if every region already has music); 409 if the chapter isn't
// scored or its narration isn't fully generated yet, since each region's
// clip is sized to its own narration.
func (s *Server) handleGenerateChapterMusic(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	chapterIdx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
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
	queued, err := s.Jobs.GenerateChapterMusic(bookID, ch.ID)
	switch {
	case errors.Is(err, jobs.ErrChapterNotScored), errors.Is(err, jobs.ErrChapterNotVoiced):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, jobs.ErrTaskNotFound):
		writeError(w, http.StatusNotFound, "book not found")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: queued})
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

// resumeMusicScoring is scoreChapterMusic's own resume-vs-restart decision,
// pulled out as a pure function purely so it's cheaply unit-testable
// without a real Store/Speaker - see scoreChapterMusic's own doc comment
// for the full reasoning. existing is that chapter's own currently-
// persisted regions (empty for a true first-ever attempt); alreadyFullyScored
// is ch.Passes.Music. Returns resumeFrom (the paragraph Idx to start
// scoring from - 0 means "the very beginning") and wipeExisting (whether
// the caller should clear existing regions before scoring at all).
func resumeMusicScoring(existing []store.MusicRegion, alreadyFullyScored bool) (resumeFrom int, wipeExisting bool) {
	if len(existing) == 0 {
		return 0, false
	}
	if alreadyFullyScored {
		return 0, true
	}
	return existing[len(existing)-1].EndIdx + 1, false
}

// scoreChapterMusic is the KindMusicScoring task's actual work (see
// jobs.MusicScoreFunc), supplied as a closure from handleScoreChapterMusic.
//
// Wipes and restarts from scratch (Store.ClearMusicRegions) only for an
// explicit, deliberate re-score of a chapter that's already fully scored
// (Passes.Music already true - handleScoreChapterMusic's own doc comment
// documents this as the "always allow a fresh run" button behavior) -
// otherwise, a chapter with some already-persisted regions but
// Passes.Music still false is a genuinely partial run from an earlier
// interruption, and resumes from just past the last persisted region's
// own EndIdx instead of wiping and redoing that real, already-completed
// work.
//
// ScoreMusic's own shouldPause parameter is passed nil below (see its own
// call site) - every LLM pass in this package now runs each dispatched
// batch through to completion instead of yielding mid-run to
// higher-priority work (see the direction/sfx/pronunciation call site in
// characters.go's own doc comment for the full reasoning: a whole-book
// preprocess phase dispatches each chapter's own pass exactly once, with
// no automatic retry loop, so routine pausing under real contention left
// most chapters permanently stuck half-scored with nothing left in the
// queue to ever finish them). The resume machinery below still matters
// regardless: a chapter can still be interrupted by a genuine batch error,
// a task requeue, or the server restarting/crashing mid-run, and without
// it every one of those would wipe and redo whatever that run had already
// finished instead of building on it.
//
// The resumed run's own first region is forced to "cut" (see
// musicSystemPrompt's own "always use cut for the very first region"
// rule) - speakerattr.Client.ScoreMusic has no visibility into the
// previous run's own last region at all in a resumed call, so it can't
// genuinely judge whether this one continues from it.
//
// Persists each batch's regions as speakerattr.Client.ScoreMusic produces
// them (Store.AppendMusicRegions), not only once the whole chapter
// finishes - so an interrupted run still keeps whatever already scored,
// and MaybeAdvanceChapterMusic can start generating those regions' own
// audio immediately rather than waiting for the rest of the chapter to
// score too. lastCoveredIdx (not simply this chapter's own true last
// paragraph) is what AppendMusicRegions is told the run's own last region
// actually ends at - critical for resume to work at all: whenever
// ScoreMusic's own remaining return value is non-empty (now only a
// genuine batch error, since shouldPause is nil - see this function's own
// doc comment above), the true stopping point is whatever paragraph
// immediately precedes remaining's own first entry,
// not the chapter's real end. Getting this wrong would silently skip the
// entire unscored middle of the chapter on the next resume (starting
// straight from a bogus, too-late resume point) rather than merely being
// a cosmetic inaccuracy.
func (s *Server) scoreChapterMusic(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error) {
	existing, err := s.Store.ListMusicRegions(ch.ID)
	if err != nil {
		return 0, nil, err
	}

	resumeFrom, wipeExisting := resumeMusicScoring(existing, ch.Passes.Music)
	if wipeExisting {
		// Already fully scored - an explicit reader-triggered rescore
		// wants a genuinely fresh result, not a resume of a run that
		// already finished once. ClearMusicRegions never touches the
		// filesystem itself (see its own doc comment) - the region rows
		// are about to disappear, including any already-AudioReady one,
		// so their own on-disk clips are about to become unreachable
		// (AppendMusicRegions below hands out new ids, never reusing an
		// old one) - best-effort removal, same as DeleteChapterAudio's
		// own disk-delete pairing: a clip that was never actually
		// generated simply has no file to remove, not an error.
		for _, r := range existing {
			audiopath.RemoveMusicRegionFiles(s.DataDir, book.ID, ch.ID, r.ID)
		}
		if _, err := s.Store.ClearMusicRegions(ch.ID); err != nil {
			return 0, nil, err
		}
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
	toScore := all
	if resumeFrom > 0 {
		toScore = nil
		for _, p := range all {
			if p.Idx >= resumeFrom {
				toScore = append(toScore, p)
			}
		}
		if len(toScore) == 0 {
			// Every paragraph is already covered by a persisted region,
			// yet Passes.Music was somehow never set - shouldn't happen,
			// but finish cleanly rather than looping forever on an empty
			// ScoreMusic call.
			if err := s.Store.SetChapterMusicScored(ch.ID); err != nil {
				return 0, nil, err
			}
			return 0, nil, nil
		}
	}

	inputs := make([]speakerattr.ParagraphInput, len(toScore))
	for i, p := range toScore {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote}
	}

	regions, remaining, scoreErr := s.Speaker.ScoreMusic(ctx, book.Title, ch.Title, inputs, nil)
	if len(regions) > 0 {
		if resumeFrom > 0 {
			regions[0].Transition = "cut"
		}
		inputRegions := make([]store.MusicRegionInput, len(regions))
		for i, region := range regions {
			inputRegions[i] = store.MusicRegionInput{
				StartIdx:   region.StartIdx,
				Mood:       region.Mood,
				Prompt:     region.Prompt,
				Ambience:   region.Ambience,
				Transition: musicTransitionFromString(region.Transition),
			}
		}
		// lastCoveredIdx is this run's own real stopping point - the
		// chapter's true last paragraph if ScoreMusic finished everything
		// it was given (remaining empty), or the paragraph immediately
		// before wherever it paused otherwise. See this function's own
		// doc comment for why using the chapter's true end unconditionally
		// here would break resume.
		lastCoveredIdx := toScore[len(toScore)-1].Idx
		if len(remaining) > 0 {
			lastCoveredIdx = remaining[0].Idx - 1
		}
		if _, serr := s.Store.AppendMusicRegions(ch.ID, inputRegions, lastCoveredIdx); serr != nil {
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
		log.Printf("httpapi: music scoring for chapter %s paused with %d paragraph(s) remaining - will resume from where it left off next time it's triggered", ch.ID, len(remaining))
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
	serveClip(w, r, audiopath.MusicRegionFile(s.DataDir, ch.BookID, ch.ID, region.ID))
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
	audiopath.RemoveMusicRegionFiles(s.DataDir, ch.BookID, ch.ID, regionID)

	s.Jobs.MaybeAdvanceChapterMusic(ch.BookID, ch.ID)
	_ = s.Jobs.PromoteTier("music_gen:"+ch.ID, jobs.TierUrgent) // best-effort - a no-op if not queued yet
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}
