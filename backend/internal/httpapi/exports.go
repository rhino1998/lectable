package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"

	"github.com/rhino1998/lectable/backend/internal/bookexport"
	"github.com/rhino1998/lectable/backend/internal/jobs"
)

// DepExports is the live invalidation name for bookexport.Manager's
// in-memory build state (queued/building/progress/failed) and its files -
// cmd/server wires the Manager's notify to it.
const DepExports = "exports"

// exportDeps covers everything an export's staleness is computed from
// (store.ExportFingerprints), the build state itself, and the job queue
// (whether its task is still queued).
var exportDeps = withDeps(narrationDeps, DBDep("images"), DBDep("breaks"), DBDep("sfx"), DBDep("music_regions"), DBDep("bookmarks"), DepExports, DepJobs)

// exportJobKey is an export's job-queue task (a jobs.BulkGroup) key.
func exportJobKey(exportID string) string { return "export:" + exportID }

// exportFormat is an export file format on the wire - bookexport.Format.
type exportFormat string

// The wire values, read by cmd/apigen.
const exportFormatEPUB exportFormat = exportFormat(bookexport.FormatEPUB)

type createExportRequest struct {
	// Format defaults to epub when empty.
	Format exportFormat `json:"format"`
	// WordLevel highlights word by word (where the alignment matches the
	// text) instead of paragraph by paragraph.
	WordLevel bool `json:"wordLevel"`
	// PhraseSeconds (word-level only) groups words into phrases at least
	// this long, so readers that track playback coarsely stay in sync at
	// high speeds - see bookexport.Options.PhraseSeconds. 0 is per word.
	PhraseSeconds float64 `json:"phraseSeconds"`
	// ExcludeMusic (whole-book only) leaves background music out - most of
	// a scored book's size; see bookexport.Options.ExcludeMusic.
	ExcludeMusic bool `json:"excludeMusic"`
	// Chapters is the chapter indexes to include; empty (or every
	// chapter) is the whole book - the only kind another library can
	// import.
	Chapters []int `json:"chapters"`
	// AsIs skips rendering: by default the export first generates every
	// paragraph of its chapters that has no audio yet and waits for it
	// (failing if some can't be), while AsIs builds right away with
	// whatever audio exists, the rest text-only.
	AsIs bool `json:"asIs"`
	// Latents builds a latent-only lectable transfer - see
	// bookexport.Options.Latents. Always as-is; with Chapters it imports as
	// a standalone book of just those chapters.
	Latents bool `json:"latents"`
}

// bookExportDTO is one export of a book: a built file, a build in
// progress, or a failed one - an existing file stays downloadable while
// it's rebuilt.
type bookExportDTO struct {
	ID            string       `json:"id"`
	Format        exportFormat `json:"format"`
	WordLevel     bool         `json:"wordLevel"`
	PhraseSeconds float64      `json:"phraseSeconds"`
	ExcludeMusic  bool         `json:"excludeMusic"`
	// Latents marks a latent-only lectable transfer (createExportRequest.
	// Latents).
	Latents bool `json:"latents"`
	// Chapters is the included chapter indexes; empty for the whole book.
	Chapters []int `json:"chapters"`
	// ChapterLabel describes Chapters for people ("Chapters 1–40"), ""
	// for the whole book.
	ChapterLabel string `json:"chapterLabel"`
	// Full is true for a whole-book export, which carries lectable's own
	// data and can be imported into another library.
	Full bool `json:"full"`
	// Ready means there's a built file to download (DownloadURL).
	Ready bool `json:"ready"`
	// Stale means the book changed since the file was built.
	Stale bool `json:"stale"`
	// Queued means its job is waiting in the job queue.
	Queued bool `json:"queued"`
	// Rendering means its job is generating the chapters' missing audio
	// first.
	Rendering bool `json:"rendering"`
	Building  bool `json:"building"`
	// Progress is a running build's fraction done, 0-1.
	Progress float64 `json:"progress"`
	// Error is the last build's failure.
	Error           string  `json:"error,omitempty"`
	FileName        string  `json:"fileName,omitempty"`
	SizeBytes       int64   `json:"sizeBytes,omitempty"`
	CreatedAt       int64   `json:"createdAt,omitempty"` // unix ms
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
	// MissingAudio is how many paragraphs had no narration when built -
	// text-only in the file.
	MissingAudio int    `json:"missingAudio,omitempty"`
	DownloadURL  string `json:"downloadUrl,omitempty"`
}

func (s *Server) handleListExports(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildExports(r.PathValue("id"))
	writeBuilt(w, v, err)
}

func (s *Server) buildExports(bookID string) ([]bookExportDTO, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return nil, httpError(http.StatusNotFound, "book not found")
	}
	list, err := s.Exports.List(bookID)
	if err != nil {
		return nil, httpError(http.StatusInternalServerError, err.Error())
	}
	out := make([]bookExportDTO, 0, len(list))
	for _, st := range list {
		// A request whose task is gone without running (canceled on the
		// Jobs page while queued) was never finished by its run.
		queued, running := s.Jobs.BulkState(bookID, exportJobKey(st.ID))
		if st.Pending && !queued && !running {
			st.Pending, st.Rendering, st.Building = false, false, false
			if st.Artifact == nil && st.Error == "" {
				continue
			}
		}
		d := bookExportDTO{
			ID: st.ID, Format: exportFormat(st.Options.Format), WordLevel: st.Options.WordLevel, PhraseSeconds: st.Options.PhraseSeconds,
			ExcludeMusic: st.Options.ExcludeMusic, Latents: st.Options.Latents,
			Chapters: st.Options.Chapters, ChapterLabel: bookexport.ChapterLabel(st.Options.Chapters),
			Full: st.Options.Full(), Stale: st.Stale, Queued: st.Pending && queued,
			Rendering: st.Rendering && running, Building: st.Building && running,
			Progress: st.Progress, Error: st.Error,
		}
		if d.Chapters == nil {
			d.Chapters = []int{}
		}
		if a := st.Artifact; a != nil {
			d.Ready = true
			d.FileName, d.SizeBytes, d.CreatedAt = a.FileName, a.SizeBytes, a.CreatedAt.UnixMilli()
			d.DurationSeconds, d.MissingAudio = a.DurationSeconds, a.MissingAudio
			d.DownloadURL = "/api/books/" + bookID + "/exports/" + st.ID + "/file?v=" + a.Fingerprint
		}
		out = append(out, d)
	}
	return out, nil
}

// handleCreateExport queues an export as a job-queue task (a Jobs-page
// row, Kind "pipeline_export") - rebuilding it if one with the same
// settings already exists. Unless AsIs, the task first renders the
// chapters' missing audio (jobs.Manager.RenderChapters) and waits for it.
// Progress and the result show up on the bookExports live topic.
func (s *Server) handleCreateExport(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	var req createExportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
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
	if req.Format == "" {
		req.Format = exportFormatEPUB
	}
	opts, err := s.Exports.Normalize(bookID, bookexport.Options{
		Format: bookexport.Format(req.Format), WordLevel: req.WordLevel, PhraseSeconds: req.PhraseSeconds,
		ExcludeMusic: req.ExcludeMusic, Chapters: req.Chapters, Latents: req.Latents,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.enqueueExport(bookID, opts, req.AsIs || opts.Latents)
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
}

func (s *Server) enqueueExport(bookID string, opts bookexport.Options, asIs bool) {
	id := s.Exports.Request(bookID, opts)
	label := bookexport.ChapterLabel(opts.Chapters)
	if label == "" {
		label = "Whole book"
	}
	if asIs {
		label += " (as-is)"
	}
	s.Jobs.EnqueueBulk(bookID, jobs.BulkGroup{
		Kind:  "pipeline_export",
		Key:   exportJobKey(id),
		Label: label,
		Tier:  jobs.TierBackground,
		// Promoting the export promotes the audio it's waiting on.
		Children: &jobs.ChildFilter{Kinds: []jobs.Kind{jobs.KindVoiceClone, jobs.KindVoiceDesign}},
	}, func(ctx context.Context, _ func() int) error {
		if !asIs {
			idxs := opts.Chapters
			if opts.Full() {
				n, err := s.Store.CountChapters(bookID)
				if err != nil {
					s.Exports.Abort(bookID, opts, err)
					return err
				}
				for i := range n {
					idxs = append(idxs, i)
				}
			}
			s.Exports.Rendering(bookID, opts)
			if err := s.Jobs.RenderChapters(ctx, bookID, idxs); err != nil {
				s.Exports.Abort(bookID, opts, err)
				return err
			}
			if err := ctx.Err(); err != nil {
				s.Exports.Abort(bookID, opts, err)
				return nil
			}
		}
		return s.Exports.Build(ctx, bookID, opts, !asIs)
	})
}

func (s *Server) handleGetExportFile(w http.ResponseWriter, r *http.Request) {
	path, a, err := s.Exports.File(r.PathValue("id"), r.PathValue("exportId"))
	if errors.Is(err, bookexport.ErrNotFound) {
		writeError(w, http.StatusNotFound, "export not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", a.Options.Format.ContentType())
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": a.FileName}))
	http.ServeFile(w, r, path)
}

// handleDeleteExport removes an export's file, or cancels its task.
func (s *Server) handleDeleteExport(w http.ResponseWriter, r *http.Request) {
	bookID, exportID := r.PathValue("id"), r.PathValue("exportId")
	canceled := s.Jobs.Cancel(jobs.BulkID(bookID, exportJobKey(exportID)))
	err := s.Exports.Delete(bookID, exportID)
	if errors.Is(err, bookexport.ErrNotFound) && canceled {
		err = nil
	}
	if errors.Is(err, bookexport.ErrNotFound) {
		writeError(w, http.StatusNotFound, "export not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}
