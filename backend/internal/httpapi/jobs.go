package httpapi

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"

	"github.com/rhino1998/lectable/backend/internal/jobs"
)

type queueTaskDTO struct {
	// ID is jobs.QueueTask.ID verbatim - the handle Cancel (DELETE
	// /api/jobs/{id}) takes, and what the frontend keys a dashboard row on.
	ID string `json:"id"`
	// Kind is one of: "voice_clone" | "voice_design" | "voice_design_preview"
	// | "voice_provision" | "speaker_attribution" | "speaker_characterization"
	// | "speech_direction" | "ambience" | "scare_quote_tagging" |
	// "chapter_summary" | "book_setting" (per-item work, see
	// jobs.Kind) or
	// "pipeline_scare_quote" | "pipeline_attribution" |
	// "pipeline_characterization" |
	// "pipeline_voice_provision" | "pipeline_direction" | "pipeline_ambience"
	// | "pipeline_summary"
	// (one of a book's own preprocessing phases - see
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
	// paragraph-scoped; for "music_live_generation" it's the first
	// paragraph of the task's one music region - the frontend should key its display on Kind
	// rather than assuming every task has a paragraph.
	ParagraphIdx int     `json:"paragraphIdx"`
	Tier         jobTier `json:"tier"`
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
	// Emotion is jobs.QueueTask.Emotion verbatim - the emotion variant a
	// clone task's line generates in, "" for neutral.
	Emotion string `json:"emotion,omitempty"`
	// Chars is jobs.QueueTask.Chars verbatim - how much paragraph text a
	// narration task ("voice_clone"/"voice_design") renders, omitted for
	// every other kind. Summed over a book's narration tasks and divided by
	// jobsSnapshotDTO.NarrationCharsPerSecond, it's the Jobs views' "time
	// left" estimate.
	Chars int `json:"chars,omitempty"`
}

type jobsSnapshotDTO struct {
	InFlight []queueTaskDTO `json:"inFlight"`
	Queued   []queueTaskDTO `json:"queued"`
	// Paused mirrors jobs.Manager.Paused() - see handlePauseJobs/
	// handleResumeJobs, the Jobs dashboard's own pause/resume button.
	Paused bool `json:"paused"`
	// Drained is jobs.Manager.Drained(): paused, everything that was in
	// flight finished and resident models unloaded - from then until
	// resume, only explicit previews run (see POST /api/jobs/pause?drain=1).
	Drained bool `json:"drained"`
	// NarrationCharsPerSecond is jobs.Manager.Throughput: how fast
	// narration has been rendering lately, in characters of paragraph text
	// per second across every generation slot together, rounded to 0.1.
	// Omitted (0) until there's a recent continuous run to measure - no
	// estimate is better than a wild one.
	NarrationCharsPerSecond float64 `json:"narrationCharsPerSecond,omitempty"`
	// LastNarrationCharsPerSecond is the same measurement for the most
	// recent run even after it ended (jobs.Manager.LastThroughput) - the
	// library's "time to finish narrating" estimate for books with nothing
	// queued. Omitted (0) until anything has been measured since startup.
	LastNarrationCharsPerSecond float64 `json:"lastNarrationCharsPerSecond,omitempty"`
	// NarrationRealtimeFactor is jobs.Manager.RealtimeFactor: the same run
	// NarrationCharsPerSecond measures, as seconds of audio rendered per
	// wall-clock second (12 = an hour of narration every five minutes),
	// rounded to 0.1. Omitted (0) whenever that is.
	NarrationRealtimeFactor float64 `json:"narrationRealtimeFactor,omitempty"`
}

// jobsSummaryDTO is the job queue in a few numbers: what a screen that
// only wants the narration rate or a queue count reads instead of
// jobsSnapshotDTO, which lists every task - 29,000 of them (20 MB of JSON)
// with three books queued for generation, enough to run the Android app
// out of memory on its library screen.
type jobsSummaryDTO struct {
	// InFlight and Queued count the tasks jobsSnapshotDTO lists.
	InFlight int `json:"inFlight"`
	Queued   int `json:"queued"`
	// Paused and Drained are jobsSnapshotDTO's.
	Paused  bool `json:"paused"`
	Drained bool `json:"drained"`
	// The narration rates are jobsSnapshotDTO's, omitted (0) the same way.
	NarrationCharsPerSecond     float64 `json:"narrationCharsPerSecond,omitempty"`
	LastNarrationCharsPerSecond float64 `json:"lastNarrationCharsPerSecond,omitempty"`
	NarrationRealtimeFactor     float64 `json:"narrationRealtimeFactor,omitempty"`
}

// buildJobsSummary is buildJobsSnapshot without the task lists.
func (s *Server) buildJobsSummary() (jobsSummaryDTO, error) {
	inFlight, queued := s.Jobs.Snapshot()
	out := jobsSummaryDTO{InFlight: len(inFlight), Queued: len(queued), Paused: s.Jobs.Paused(), Drained: s.Jobs.Drained()}
	if rate, ok := s.Jobs.Throughput(); ok {
		out.NarrationCharsPerSecond = math.Round(rate*10) / 10
	}
	if factor, ok := s.Jobs.RealtimeFactor(); ok {
		out.NarrationRealtimeFactor = math.Round(factor*10) / 10
	}
	if rate, ok := s.Jobs.LastThroughput(); ok {
		out.LastNarrationCharsPerSecond = math.Round(rate*10) / 10
	}
	return out, nil
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
		tier := tierBackground
		switch t.Tier {
		case jobs.TierUrgent:
			tier = tierUrgent
		case jobs.TierLookahead:
			tier = tierLookahead
		case jobs.TierNormal:
			tier = tierNormal
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
			Emotion:      t.Emotion,
			Chars:        t.Chars,
		}
	}

	out := jobsSnapshotDTO{InFlight: []queueTaskDTO{}, Queued: []queueTaskDTO{}, Paused: s.Jobs.Paused(), Drained: s.Jobs.Drained()}
	if rate, ok := s.Jobs.Throughput(); ok {
		out.NarrationCharsPerSecond = math.Round(rate*10) / 10
	}
	if factor, ok := s.Jobs.RealtimeFactor(); ok {
		out.NarrationRealtimeFactor = math.Round(factor*10) / 10
	}
	if rate, ok := s.Jobs.LastThroughput(); ok {
		out.LastNarrationCharsPerSecond = math.Round(rate*10) / 10
	}
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
	writeNoContent(w)
}

// jobTierRequest is PUT /api/jobs/{id}/tier's own request body - the same
// four string values queueTaskDTO.Tier already reports (buildJobsSnapshot's
// own toDTO), so the frontend can round-trip a row's Tier field straight
// back as a request without a separate encoding to keep in sync.
type jobTierRequest struct {
	Tier jobTier `json:"tier"`
}

// jobTier is a job-queue priority tier on the wire - jobs.Tier*'s names,
// most urgent first.
type jobTier string

const (
	tierUrgent     jobTier = "urgent"
	tierLookahead  jobTier = "lookahead"
	tierNormal     jobTier = "normal"
	tierBackground jobTier = "background"
)

// parseTier maps a tier DTO string to its jobs.Tier* int constant - the
// decode-side counterpart of toDTO's own encode-side switch.
func parseTier(s jobTier) (int, bool) {
	switch s {
	case tierUrgent:
		return jobs.TierUrgent, true
	case tierLookahead:
		return jobs.TierLookahead, true
	case tierNormal:
		return jobs.TierNormal, true
	case tierBackground:
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
		writeError(w, http.StatusBadRequest, `tier must be one of: "urgent", "lookahead", "normal", "background"`)
		return
	}

	err := s.Jobs.PromoteTier(id, tier)
	switch {
	case err == nil:
		writeNoContent(w)
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
	writeJSON(w, http.StatusOK, canceledCountResponse{Canceled: n})
}

// handlePauseJobs stops the queue from dispatching any *new* task - the
// Jobs dashboard's "Pause" button. Whatever's already in flight keeps
// running to completion (see jobs.Manager.Pause's own doc comment) -
// this is not a cancel, and reverses cleanly via handleResumeJobs.
// ?drain=1 also waits for that in-flight work to finish and resident
// models to unload (jobs.Manager.WaitDrained) before answering; from then
// until resume, only explicit previews run, one pool at a time with
// nothing else - what an eval run (backend/attreval) wants, so its many
// previews don't swap models back and forth against generation.
func (s *Server) handlePauseJobs(w http.ResponseWriter, r *http.Request) {
	s.Jobs.Pause()
	if r.URL.Query().Get("drain") != "" {
		if err := s.Jobs.WaitDrained(r.Context()); err != nil {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
	}
	writeNoContent(w)
}

// handleResumeJobs undoes handlePauseJobs.
func (s *Server) handleResumeJobs(w http.ResponseWriter, r *http.Request) {
	s.Jobs.Resume()
	writeNoContent(w)
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
	writeNoContent(w)
}
