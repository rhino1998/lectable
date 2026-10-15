package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/rhino1998/lectable/backend/internal/direction"
	"github.com/rhino1998/lectable/backend/internal/namekey"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// The direction pass's second stage (store.Passes.Delivery): free-form
// speech direction for the lines the labels pick out, under a clone model
// that takes direction as text. See speakerattr.Client.DirectLines and
// backend/docs/drama3-direction-plan.md.

// deliveryBefore/deliveryAfter are how many paragraphs around a line its
// prompt shows: the beat leading in, and the speech tag that often
// follows ("...," she said, her voice breaking).
const (
	deliveryBefore = 3
	deliveryAfter  = 1
)

// bookDirected reports whether book's clone model takes direction as
// text, so the direction pass writes free-form directions for it.
func bookDirected(book *store.Book) bool {
	return voices.Directed(narration.BookCloneModel(book))
}

// wantsDirection reports whether all[i] - a real dialogue line - gets a
// free-form direction: it has an emotion label, or narration beside it
// says how it is delivered (speakerattr.DeliveryCue).
func wantsDirection(all []store.Paragraph, i int) bool {
	p := all[i]
	if !p.IsQuote || p.ScareQuote || p.Unvoiced() {
		return false
	}
	if p.EffectiveEmotion() != "" {
		return true
	}
	for _, j := range []int{i - 1, i + 1} {
		if j >= 0 && j < len(all) && !all[j].IsQuote && speakerattr.DeliveryCue(all[j].Text) {
			return true
		}
	}
	return false
}

// deliverChapter runs the second stage over ch: a direction for every
// line wantsDirection picks (all is the chapter's paragraphs, with the
// first stage's labels already applied), and none for every other
// dialogue line, so a line no longer labeled loses its old one. Lines
// the reader directed by hand (store.EditDirection) are left alone.
// Invalidates the audio of each line whose effective direction changed,
// and marks the chapter's Delivery pass done when every line was answered.
func (s *Server) deliverChapter(ctx context.Context, book *store.Book, ch *store.Chapter, all []store.Paragraph) (int, error) {
	edited, err := s.Store.EditedParagraphIdx(ch.ID, store.EditDirection)
	if err != nil {
		return 0, err
	}
	profiles := s.speakerProfiles(book)
	scenes := s.chapterScenes(ch.ID)
	dc := speakerattr.DirectionContext{BookTitle: book.Title, ChapterTitle: ch.Title}
	if st := s.bookSetting(ctx, book); st != nil {
		dc.Setting = strings.Join(nonEmpty(st.Era, st.Place, st.Genre), "; ")
	}

	var lines []speakerattr.DirectionLine
	updates := map[int]direction.Direction{}
	for i, p := range all {
		if edited[p.Idx] || !p.IsQuote {
			continue
		}
		if !wantsDirection(all, i) {
			if !p.Direction.Empty() {
				updates[p.Idx] = direction.Direction{}
			}
			continue
		}
		line := speakerattr.DirectionLine{
			Idx: p.Idx, Text: p.Text, Speaker: p.Speaker,
			Profile: profiles[namekey.Fold(p.Speaker)],
			Label:   p.EffectiveEmotion(),
			Scene:   sceneAt(scenes, p.Idx),
		}
		for j := max(0, i-deliveryBefore); j < i; j++ {
			line.Before = append(line.Before, passageLine(all[j]))
		}
		for j := i + 1; j < len(all) && j <= i+deliveryAfter; j++ {
			line.After = append(line.After, passageLine(all[j]))
		}
		lines = append(lines, line)
	}

	directed, runErr := s.Speaker.DirectLines(ctx, dc, lines)
	for idx, d := range directed {
		updates[idx] = d
	}
	var changed []store.Paragraph
	for _, p := range all {
		d, ok := updates[p.Idx]
		if !ok || d.Equal(p.Direction) && d.For == p.Direction.For {
			delete(updates, p.Idx)
			continue
		}
		before := p.EffectiveDirection()
		p.Direction = d
		if !p.EffectiveDirection().Equal(before) {
			changed = append(changed, p)
		}
	}
	if err := s.Store.SetParagraphDirections(ch.ID, updates); err != nil {
		return 0, err
	}
	if len(changed) > 0 {
		s.invalidateParagraphAudio(book, s.expandToInlineSets(ch.ID, changed))
	}
	if runErr != nil {
		return len(directed), runErr
	}
	if err := s.Store.SetChapterDelivered(ch.ID); err != nil {
		log.Printf("deliverChapter: could not advance chapter %s passes: %v", ch.ID, err)
	}
	return len(directed), nil
}

// deliverAfterLabels runs the second stage after the first finished ch,
// under a directed clone model - reading the chapter again, since the
// labels just changed. Best-effort: the labels stand either way, and a
// chapter left without Delivery is picked up again (directChapterRange).
func (s *Server) deliverAfterLabels(ctx context.Context, book *store.Book, ch *store.Chapter) {
	if !bookDirected(book) {
		return
	}
	all, err := s.Store.ListParagraphsRaw(ch.ID)
	if err == nil {
		_, err = s.deliverChapter(ctx, book, ch, all)
	}
	if err != nil {
		log.Printf("directChapter: directions for chapter %s: %v", ch.ID, err)
	}
}

// speakerProfiles maps each of book's characters (namekey.Fold of the
// name) to their casting summary.
func (s *Server) speakerProfiles(book *store.Book) map[string]string {
	out := map[string]string{}
	chars, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		log.Printf("deliverChapter: characters for %s: %v", book.ID, err)
		return out
	}
	for _, c := range chars {
		if c.Summary != "" {
			out[namekey.Fold(c.Name)] = c.Summary
		}
	}
	return out
}

// chapterScenes is chapterID's scenes from the summary pass, nil without.
func (s *Server) chapterScenes(chapterID string) []store.SummaryScene {
	notes, err := s.Store.GetChapterNotes(chapterID)
	if err != nil || notes == nil {
		return nil
	}
	return notes.Scenes
}

// sceneAt describes the scene paragraph idx falls in, "" for none.
func sceneAt(scenes []store.SummaryScene, idx int) string {
	var at *store.SummaryScene
	for i := range scenes {
		if scenes[i].StartIdx <= idx {
			at = &scenes[i]
		}
	}
	if at == nil {
		return ""
	}
	return strings.Join(nonEmpty(at.Location, at.Time), ", ")
}

// passageLine is how a paragraph appears in a direction prompt's passage.
func passageLine(p store.Paragraph) string {
	if p.IsQuote && p.Speaker != "" && p.Speaker != "Unknown" {
		return p.Speaker + ": " + p.Text
	}
	return p.Text
}

// nonEmpty is vals without the empty ones.
func nonEmpty(vals ...string) []string {
	out := vals[:0:0]
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

type setParagraphDirectionRequest struct {
	// Direction is the line with its direction written into it, as
	// paragraphDTO.Direction shows it ("[tired] Fine. [sighs] Go."); ""
	// clears it.
	Direction string `json:"direction"`
}

// handleSetParagraphDirection is a reader's override of one dialogue
// line's free-form direction - handleSetParagraphEmotion's counterpart for
// a clone model that takes direction as text. The body is the line with
// direction written in, read by the same parser the pass uses
// (direction.Parse): 400 when it changes the line's words or breaks a
// tag. Recorded as a store.EditDirection, so the pass leaves the line
// alone after. Invalidates and re-enqueues the line's audio when its
// effective direction changed under the book's clone model.
func (s *Server) handleSetParagraphDirection(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	chapterIdx, err := strconv.Atoi(r.PathValue("idx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid chapter index")
		return
	}
	paragraphIdx, err := strconv.Atoi(r.PathValue("pidx"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid paragraph index")
		return
	}
	var req setParagraphDirectionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
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
	paragraphID, err := s.Store.GetParagraphIDByIdx(ch.ID, paragraphIdx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var p *store.Paragraph
	if paragraphID != "" {
		if p, err = s.Store.GetParagraph(paragraphID); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "paragraph not found")
		return
	}
	if !p.IsQuote {
		writeError(w, http.StatusBadRequest, "only quoted dialogue can have a direction")
		return
	}
	var d direction.Direction
	if strings.TrimSpace(req.Direction) != "" {
		if d, err = direction.Parse(p.Text, req.Direction); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	old := ownDirection(*p)
	if d.Equal(p.Direction) && (d.Empty() || p.Direction.Current(p.Text)) {
		writeNoContent(w)
		return
	}
	if err := s.Store.SetParagraphDirections(ch.ID, map[int]direction.Direction{paragraphIdx: d}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	newAnnotated := ""
	if !d.Empty() {
		newAnnotated = d.Annotate(p.Text)
	}
	if err := s.Store.RecordParagraphEdit(paragraphID, store.EditDirection, old, newAnnotated); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	before := p.EffectiveDirection()
	p.Direction = d
	if bookDirected(book) && !p.EffectiveDirection().Equal(before) {
		s.invalidateParagraphAudio(book, []store.Paragraph{*p})
		s.Jobs.EnqueueParagraphRegenerate(bookID, ch.ID, ch.Idx, *p)
	}
	writeNoContent(w)
}
