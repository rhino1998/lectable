package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/rhino1998/lectable/backend/internal/jobs"
)

type queueTaskDTO struct {
	// ID is jobs.QueueTask.ID verbatim - the handle Cancel (DELETE
	// /api/jobs/{id}) takes, and what the frontend keys a dashboard row on.
	ID string `json:"id"`
	// Kind is one of: "voice_clone" | "voice_design" | "voice_design_preview"
	// | "voice_provision" | "speaker_attribution" | "speaker_characterization"
	// | "speech_direction" | "music_scoring" (per-item work, see jobs.Kind) or
	// "pipeline_attribution" | "pipeline_characterization" |
	// "pipeline_voice_provision" | "pipeline_direction" | "pipeline_music"
	// (one of a book's own five preprocessing phases - see
	// jobs.EnqueuePipeline/toPipelineQueueTask; deliberately "pipeline_"-
	// prefixed so these can never collide with the per-item kind sharing a
	// phase's own bare name, e.g. "voice_provision").
	Kind string `json:"kind"`
	// Label is a human-readable identifier for a kind that isn't chapter/
	// paragraph-scoped - the character's name for "speaker_characterization",
	// or the fixed string "Whole book" for every "pipeline_*" kind (see
	// jobs.toPipelineQueueTask) - "" for every other kind, which already has
	// a chapter/paragraph to show (see jobs.QueueTask.Label).
	Label string `json:"label,omitempty"`

	BookID    string `json:"bookId"`
	BookTitle string `json:"bookTitle"`
	// ChapterIdx/ChapterTitle are meaningless (0/"") for
	// "speaker_characterization", which is character- rather than
	// chapter-scoped - see Label above.
	ChapterIdx   int    `json:"chapterIdx"`
	ChapterTitle string `json:"chapterTitle"`
	// ParagraphIdx is meaningless (0) for the two LLM kinds
	// ("speaker_attribution"/"speaker_characterization"), which aren't
	// paragraph-scoped - the frontend should key its display on Kind
	// rather than assuming every task has a paragraph.
	ParagraphIdx int    `json:"paragraphIdx"`
	Tier         string `json:"tier"` // "urgent" | "lookahead" | "background"
	// PresetID/Instruct are "" for a "speaker_attribution" task, and for a
	// "voice_design" task PresetID alone is "" (a pure custom instruct with
	// no preset backing it) - the frontend resolves PresetID to a friendly
	// name against the built-in and custom preset lists it already fetches
	// elsewhere, falling back to Instruct when there's no preset to resolve.
	PresetID string `json:"presetId"`
	Instruct string `json:"instruct"`
	// Attempt is jobs.QueueTask.Attempt verbatim - 0 for a task on its
	// first ever dispatch, only bumped by a genuine failure-retry, never
	// by a cooperative pause (see its own doc comment). The frontend uses
	// this to tell "this is stuck retrying after real failures" apart
	// from "this is just slowly working through a long chapter one batch
	// at a time" - both can look identical (the same row reappearing
	// over and over) without it.
	Attempt int `json:"attempt"`
}

type jobsSnapshotDTO struct {
	InFlight []queueTaskDTO `json:"inFlight"`
	Queued   []queueTaskDTO `json:"queued"`
	// Paused mirrors jobs.Manager.Paused() - see handlePauseJobs/
	// handleResumeJobs, the Jobs dashboard's own pause/resume button.
	Paused bool `json:"paused"`
}

// buildJobsSnapshot reports the job queue's live state - what's dispatched
// to a worker slot right now (voice cloning/design generation, or a
// speaker-attribution/characterization/provision run - see
// internal/jobs.Kind) and what's still waiting - as the same DTO shape
// both handleJobsSnapshot (GET /api/jobs) and the "jobs" live topic (see
// registerLiveTopics) use, so the two can never silently drift apart. Book/chapter titles are resolved once per unique
// id seen (cached within one call), not once per task, since the same
// book and chapter often account for most of a lookahead burst.
func (s *Server) buildJobsSnapshot() jobsSnapshotDTO {
	inFlight, queued := s.Jobs.Snapshot()

	bookTitles := map[string]string{}
	resolveBook := func(id string) string {
		if title, ok := bookTitles[id]; ok {
			return title
		}
		title := id
		if b, err := s.Store.GetBook(id); err == nil && b != nil {
			title = b.Title
		}
		bookTitles[id] = title
		return title
	}

	chapterTitles := map[string]string{}
	resolveChapter := func(id string) string {
		if title, ok := chapterTitles[id]; ok {
			return title
		}
		title := ""
		if ch, err := s.Store.GetChapterByID(id); err == nil && ch != nil {
			title = ch.Title
		}
		chapterTitles[id] = title
		return title
	}

	toDTO := func(t jobs.QueueTask) queueTaskDTO {
		tier := "background"
		switch t.Tier {
		case jobs.TierUrgent:
			tier = "urgent"
		case jobs.TierLookahead:
			tier = "lookahead"
		}
		return queueTaskDTO{
			ID:           t.ID,
			Kind:         string(t.Kind),
			Label:        t.Label,
			BookID:       t.BookID,
			BookTitle:    resolveBook(t.BookID),
			ChapterIdx:   t.ChapterIdx,
			ChapterTitle: resolveChapter(t.ChapterID),
			ParagraphIdx: t.ParagraphIdx,
			Tier:         tier,
			PresetID:     t.PresetID,
			Instruct:     t.Instruct,
			Attempt:      t.Attempt,
		}
	}

	out := jobsSnapshotDTO{InFlight: []queueTaskDTO{}, Queued: []queueTaskDTO{}, Paused: s.Jobs.Paused()}
	for _, t := range inFlight {
		out.InFlight = append(out.InFlight, toDTO(t))
	}
	for _, t := range queued {
		out.Queued = append(out.Queued, toDTO(t))
	}
	return out
}

// handleJobsSnapshot is buildJobsSnapshot over HTTP - GET /api/jobs.
func (s *Server) handleJobsSnapshot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildJobsSnapshot())
}

// handleCancelJob cancels one job-queue task by its id (queueTaskDTO.ID,
// i.e. jobs.QueueTask.ID) - the Jobs dashboard's per-row "Cancel" button.
// 404 if id doesn't match anything currently queued or in flight (already
// finished on its own, or never existed) - see jobs.Manager.Cancel's own
// doc comment for exactly what "cancel" means for a queued vs. in-flight
// task.
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.Jobs.Cancel(id) {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"canceled": true})
}

// jobTierRequest is PUT /api/jobs/{id}/tier's own request body - the same
// three string values queueTaskDTO.Tier already reports (buildJobsSnapshot's
// own toDTO), so the frontend can round-trip a row's Tier field straight
// back as a request without a separate encoding to keep in sync.
type jobTierRequest struct {
	Tier string `json:"tier"` // "urgent" | "lookahead" | "background"
}

// parseTier maps a tier DTO string to its jobs.Tier* int constant - the
// decode-side counterpart of toDTO's own encode-side switch.
func parseTier(s string) (int, bool) {
	switch s {
	case "urgent":
		return jobs.TierUrgent, true
	case "lookahead":
		return jobs.TierLookahead, true
	case "background":
		return jobs.TierBackground, true
	default:
		return 0, false
	}
}

// handleSetJobTier raises one job-queue task's own priority tier - the
// Jobs dashboard's per-row priority menu (click a row's Tier badge, pick
// something more urgent). See jobs.Manager.PromoteTier's own doc comment
// for why this can only ever raise urgency, never lower it: 409 if the
// requested tier isn't strictly more urgent than the task's current one
// (e.g. re-requesting the tier it's already at, or something already more
// urgent), 404 if id doesn't match anything currently queued or in
// flight, 400 for a malformed body or an unrecognized tier string.
func (s *Server) handleSetJobTier(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req jobTierRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	tier, ok := parseTier(req.Tier)
	if !ok {
		writeError(w, http.StatusBadRequest, `tier must be one of: "urgent", "lookahead", "background"`)
		return
	}

	err := s.Jobs.PromoteTier(id, tier)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]bool{"promoted": true})
	case errors.Is(err, jobs.ErrTaskNotFound):
		writeError(w, http.StatusNotFound, "job not found")
	case errors.Is(err, jobs.ErrTierNotMoreUrgent):
		writeError(w, http.StatusConflict, "requested tier is not more urgent than the task's current tier")
	default:
		writeError(w, http.StatusBadRequest, err.Error())
	}
}

// handleCancelAllJobs cancels every queued and in-flight task at once - the
// Jobs dashboard's "Cancel all" button. See jobs.Manager.CancelAll's own
// doc comment for what the returned count actually reflects.
func (s *Server) handleCancelAllJobs(w http.ResponseWriter, r *http.Request) {
	n := s.Jobs.CancelAll()
	writeJSON(w, http.StatusOK, map[string]int{"canceled": n})
}

// handlePauseJobs stops the queue from dispatching any *new* task - the
// Jobs dashboard's "Pause" button. Whatever's already in flight keeps
// running to completion (see jobs.Manager.Pause's own doc comment) -
// this is not a cancel, and reverses cleanly via handleResumeJobs.
func (s *Server) handlePauseJobs(w http.ResponseWriter, r *http.Request) {
	s.Jobs.Pause()
	writeJSON(w, http.StatusOK, map[string]bool{"paused": true})
}

// handleResumeJobs undoes handlePauseJobs.
func (s *Server) handleResumeJobs(w http.ResponseWriter, r *http.Request) {
	s.Jobs.Resume()
	writeJSON(w, http.StatusOK, map[string]bool{"paused": false})
}

// handleRestartWorker forces an immediate ttsworker restart - the Jobs
// dashboard's "Restart ttsworker" button, for recovering a stuck or
// visibly-leaking worker (see ttsworker.Manager's own RSS-threshold
// watchdog for the automatic version of this) without waiting for a
// crash or the RSS threshold to be crossed on its own. Blocks until the
// new worker is confirmed healthy (ttsworker.Manager.Restart's own
// restartGate already blocks every other in-flight TTS/LLM call for the
// same duration), then reports success/failure directly rather than
// firing-and-forgetting it - a restart is rare and worth the reader
// actually seeing whether it worked.
func (s *Server) handleRestartWorker(w http.ResponseWriter, r *http.Request) {
	if err := s.TTS.Restart(r.Context(), "manual restart via Jobs dashboard"); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"restarted": true})
}
