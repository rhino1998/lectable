package httpapi

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"sort"
	"strconv"

	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// paragraphEditField is a paragraph field a reader's manual edit is
// recorded for (store.EditSpeaker/EditEmotion/EditScareQuote; a scare-quote
// edit's values are "true"/"false").
type paragraphEditField string

const (
	paragraphEditSpeaker    paragraphEditField = store.EditSpeaker
	paragraphEditEmotion    paragraphEditField = store.EditEmotion
	paragraphEditScareQuote paragraphEditField = store.EditScareQuote
)

// paragraphEditDTO is one recorded manual correction (store.ParagraphEdit):
// the machine's value before the first edit, and the reader's latest.
// Speakers read "Narrator" where the store holds "".
type paragraphEditDTO struct {
	ChapterIdx   int                `json:"chapterIdx"`
	ParagraphIdx int                `json:"paragraphIdx"`
	Field        paragraphEditField `json:"field"`
	Original     string             `json:"original"`
	Value        string             `json:"value"`
	EditedAt     int64              `json:"editedAt"`
	Text         string             `json:"text"`
}

type paragraphEditsResponse struct {
	Edits []paragraphEditDTO `json:"edits"`
}

// handleListParagraphEdits serves GET /api/books/{id}/paragraph-edits
// (?field=speaker|emotion|scare_quote, default all): every manual
// correction the reader made to the book's machine-labeled speakers,
// emotions and scare-quote flags, in
// reading order - the reference a change to attribution or the emotion
// pass is scored against.
func (s *Server) handleListParagraphEdits(w http.ResponseWriter, r *http.Request) {
	field := paragraphEditField(r.URL.Query().Get("field"))
	if field != "" && field != paragraphEditSpeaker && field != paragraphEditEmotion && field != paragraphEditScareQuote {
		writeError(w, http.StatusBadRequest, "unknown field "+strconv.Quote(string(field)))
		return
	}
	edits, err := s.Store.ParagraphEdits(r.PathValue("id"), string(field))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := paragraphEditsResponse{Edits: make([]paragraphEditDTO, 0, len(edits))}
	for _, e := range edits {
		d := paragraphEditDTO{ChapterIdx: e.ChapterIdx, ParagraphIdx: e.ParagraphIdx, Field: paragraphEditField(e.Field), Original: e.Original, Value: e.Value, EditedAt: e.EditedAt, Text: e.Text}
		if e.Field == store.EditSpeaker {
			d.Original, d.Value = speakerLabel(e.Original), speakerLabel(e.Value)
		}
		out.Edits = append(out.Edits, d)
	}
	writeJSON(w, http.StatusOK, out)
}

// speakerLabel is a stored speaker for display: "" is Narrator.
func speakerLabel(stored string) string {
	if stored == "" {
		return "Narrator"
	}
	return stored
}

// attributionPreviewLineDTO is one dialogue line of an attribution
// preview: its speaker now, what a fresh attribution run proposes, and -
// when the reader corrected it by hand - the machine's original answer
// before that edit (Current is then the reader's value).
type attributionPreviewLineDTO struct {
	ParagraphIdx int    `json:"paragraphIdx"`
	Text         string `json:"text"`
	Current      string `json:"current"`
	Proposed     string `json:"proposed"`
	// CurrentIndex and ProposedIndex are the line's stored and proposed
	// speaker numbers (store.Paragraph.SpeakerIndex), omitted when 0.
	CurrentIndex  int    `json:"currentIndex,omitempty"`
	ProposedIndex int    `json:"proposedIndex,omitempty"`
	Edited        bool   `json:"edited"`
	Original      string `json:"original,omitempty"`
	// Given is a corrected line the run was shown with its speaker
	// (?given=), so Proposed is just the reader's value back - leave it
	// out of any score.
	Given bool `json:"given,omitempty"`
}

type attributionPreviewResponse struct {
	Lines []attributionPreviewLineDTO `json:"lines"`
	// Error is a batch failure partway through; Lines still holds what
	// finished.
	Error string `json:"error,omitempty"`
}

// handlePreviewAttribution serves POST
// /api/books/{id}/chapters/{idx}/attribute-speakers/preview: runs speaker
// attribution over the chapter exactly as attributeChapter would and
// returns every dialogue line's proposed speaker beside its current one,
// storing nothing - for checking an attribution change against a chapter
// (and against the reader's recorded corrections) without overwriting
// it. Synchronous, run as one KindLLMPreview task at TierNormal: a
// diagnostic, not something a client should call routinely.
//
// Unlike attributeChapter, the chapter's own corrected lines are asked
// about, not shown, unless ?given=half (lines at even paragraph idx shown
// with the reader's speaker, the odd ones held out to score) or
// ?given=all. ?corrections=0 leaves out earlier chapters' corrections, for
// an A/B against them in one build.
func (s *Server) handlePreviewAttribution(w http.ResponseWriter, r *http.Request) {
	var opts attributionOptions
	switch given := r.URL.Query().Get("given"); given {
	case "", "none":
	case "half":
		opts.given = func(idx int) bool { return idx%2 == 0 }
	case "all":
		opts.given = allCorrections.given
	default:
		writeError(w, http.StatusBadRequest, "given must be none, half or all, not "+strconv.Quote(given))
		return
	}
	opts.noCorrections = r.URL.Query().Get("corrections") == "0"
	book, ch, ok := s.chapterLLMTarget(w, r)
	if !ok {
		return
	}
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Through the job queue's LLM slot, never straight to the worker: the
	// queue keeps LLM and TTS work from overlapping in the worker (see
	// jobs.Manager.RunLLMPreviewAt). TierNormal: behind a reader's
	// lookahead, ahead of background preprocessing.
	var run attributionRun
	_, err = s.Jobs.RunLLMPreviewAt(r.Context(), jobs.TierNormal, fmt.Sprintf("attribution preview: %s ch %d", book.Title, ch.Idx), "", func(ctx context.Context) (string, error) {
		var rerr error
		run, rerr = s.runAttribution(ctx, book, ch, all, all, opts)
		return "", rerr
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	edits, err := s.Store.ParagraphEdits(book.ID, store.EditSpeaker)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	original := map[int]string{}
	for _, e := range edits {
		if e.ChapterIdx == ch.Idx {
			original[e.ParagraphIdx] = e.Original
		}
	}
	// Who attributeChapter would store as a role: the roster's own flags,
	// then, for a name this run introduces, the same check it applies.
	roster, err := s.Store.ListCharacters(run.scope)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	isRole := map[string]bool{}
	maps.Copy(isRole, s.realRoles(book, run.roleNames))
	for _, c := range roster {
		isRole[c.Name] = c.IsRole
	}
	out := attributionPreviewResponse{Lines: []attributionPreviewLineDTO{}}
	if run.err != nil {
		out.Error = run.err.Error()
	}
	for _, p := range all {
		if !p.IsQuote || p.ScareQuote {
			continue
		}
		proposed, answered := run.speakers[p.Idx]
		if !answered {
			continue
		}
		if run.invalid.has(proposed) || speakerattr.IsGroupSpeaker(proposed) {
			proposed = "Unknown"
		}
		line := attributionPreviewLineDTO{ParagraphIdx: p.Idx, Text: p.Text, Current: speakerLabel(p.Speaker), Proposed: speakerLabel(proposed), CurrentIndex: p.SpeakerIndex}
		// Like attributeChapter: a number only counts on a role's line.
		if isRole[proposed] {
			line.ProposedIndex = run.indexes[p.Idx]
		}
		if o, ok := original[p.Idx]; ok {
			line.Edited, line.Original = true, speakerLabel(o)
			line.Given = opts.given != nil && opts.given(p.Idx)
		}
		out.Lines = append(out.Lines, line)
	}
	sort.Slice(out.Lines, func(i, j int) bool { return out.Lines[i].ParagraphIdx < out.Lines[j].ParagraphIdx })
	writeJSON(w, http.StatusOK, out)
}

// scareQuotePreviewLineDTO is one quoted line a scare-quote preview asked
// about or flagged: whether it is flagged now, whether a fresh pass flags
// it, and whether the reader set it by hand.
type scareQuotePreviewLineDTO struct {
	ParagraphIdx int    `json:"paragraphIdx"`
	Text         string `json:"text"`
	Current      bool   `json:"current"`
	Proposed     bool   `json:"proposed"`
	Candidate    bool   `json:"candidate"`
	Edited       bool   `json:"edited"`
}

type scareQuotePreviewResponse struct {
	Lines []scareQuotePreviewLineDTO `json:"lines"`
	Error string                     `json:"error,omitempty"`
}

// handlePreviewScareQuotes serves POST
// /api/books/{id}/chapters/{idx}/retag-scare-quotes/preview: runs the
// scare-quote pass over the chapter and returns every candidate or
// currently flagged quoted line with the flag a fresh pass would give it,
// storing nothing - handlePreviewAttribution's sibling for scoring the
// pass against the reader's corrections. Same queue slot and tier.
func (s *Server) handlePreviewScareQuotes(w http.ResponseWriter, r *http.Request) {
	book, ch, ok := s.chapterLLMTarget(w, r)
	if !ok {
		return
	}
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	inputs := make([]speakerattr.ParagraphInput, len(all))
	for i, p := range all {
		inputs[i] = speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote}
	}
	var flagged map[int]bool
	var passErr error
	_, err = s.Jobs.RunLLMPreviewAt(r.Context(), jobs.TierNormal, fmt.Sprintf("scare-quote preview: %s ch %d", book.Title, ch.Idx), "", func(ctx context.Context) (string, error) {
		flagged, _, passErr = s.Speaker.ScareQuoteChapter(ctx, book.Title, ch.Title, inputs, nil)
		return "", nil
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	edited, err := s.Store.EditedParagraphIdx(ch.ID, store.EditScareQuote)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	candidate := speakerattr.ScareQuoteCandidates(inputs)
	out := scareQuotePreviewResponse{Lines: []scareQuotePreviewLineDTO{}}
	if passErr != nil {
		out.Error = passErr.Error()
	}
	for _, p := range all {
		if !p.IsQuote || !candidate[p.Idx] && !p.ScareQuote && !flagged[p.Idx] {
			continue
		}
		out.Lines = append(out.Lines, scareQuotePreviewLineDTO{ParagraphIdx: p.Idx, Text: p.Text, Current: p.ScareQuote, Proposed: flagged[p.Idx], Candidate: candidate[p.Idx], Edited: edited[p.Idx]})
	}
	writeJSON(w, http.StatusOK, out)
}
