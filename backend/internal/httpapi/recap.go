package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/speakerattr"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/wav"
)

// The "previously on" recap: a short account of what a reader has already
// heard, written by the LLM from the chapter summaries (and the read part
// of the current chapter) and spoken in the book's narrator voice. One per
// book (store.BookRecap). At a book's start it is instead a recap of the
// book before it in its series. See the `.../recap` entry in docs/api.md.

const (
	// recapStartChars is how much of a book a reader has behind them
	// before it has anything of its own to recap: under it they are at the
	// book's start (front matter, a first page), and get the series recap
	// or none.
	recapStartChars = 3000
	// A position recap reaches back over at least recapMinChapters
	// chapters, and on until it has covered recapReachChars of text
	// (~35k words, three or four hours of listening) or recapMaxChapters.
	// Chapters run from 150 to 600 paragraphs in one book here and far
	// longer in a web serial, so a fixed chapter count would cover
	// anything from twenty minutes to a day.
	recapMinChapters = 2
	recapMaxChapters = 6
	recapReachChars  = 200_000
	// recapSkipChars is the chapter length under which a chapter is front
	// or back matter, not story - left out of a position recap.
	recapSkipChars = 200
	// recapTextChars caps the text of the current chapter a recap is shown
	// (one summary window's worth).
	recapTextChars = 24000
	// recapReuseParagraphs is how far past the paragraph a recap was
	// written at a reader can be and still be offered it.
	recapReuseParagraphs = 40
	// recapEagerMissing is the most chapter summaries a recap prepared
	// unasked (prepareIdleRecaps) will queue - past it, it waits to be
	// asked for.
	recapEagerMissing = 10
	// recapIdleFloor is the least time a book's position must have been
	// still before its recap is prepared unasked - what "immediately"
	// waits, so a reader mid-session isn't written a recap every pause -
	// and recapIdleWithin how recently the book must have been read.
	recapIdleFloor  = 5 * time.Minute
	recapIdleWithin = 14 * 24 * time.Hour
	// RecapCheckEvery is how often PrepareIdleRecaps is run (cmd/server's
	// cron task).
	RecapCheckEvery = 2 * time.Minute
	// recapRetryAfter is how long a recap that failed at a position is left
	// alone before one is prepared unasked for it again.
	recapRetryAfter = 6 * time.Hour
	recapChunkChars = 500
	recapGapSeconds = 0.45

	recapJobKey = "recap"
)

// bookRecapDTO is a book's "previously on" recap.
type bookRecapDTO struct {
	Kind store.RecapKind `json:"kind"`
	// ChapterIdx/ParagraphIdx is the reading position it was written for -
	// it tells nothing from that paragraph on - and ThroughIdx the last
	// paragraph of that chapter a reader can be at and still be offered
	// it: it is current for a position in ChapterIdx with ParagraphIdx <=
	// paragraph <= ThroughIdx.
	ChapterIdx   int `json:"chapterIdx"`
	ParagraphIdx int `json:"paragraphIdx"`
	ThroughIdx   int `json:"throughIdx"`
	// Title is the book it recaps, as spoken ("Previously, in <Title>") -
	// this book, or for kind "series" the one before it.
	Title string `json:"title"`
	// Text is the recap, paragraphs separated by a blank line; "" when
	// writing it failed (Error says why).
	Text  string `json:"text"`
	Error string `json:"error,omitempty"`
	// AudioURL is the recap spoken in the book's narrator voice, opening
	// with "Previously, in <Title>."; "" until it has been rendered, or
	// once the book's voice has changed.
	AudioURL     string  `json:"audioUrl,omitempty"`
	AudioSeconds float64 `json:"audioSeconds,omitempty"`
	// Segments lay the clip out for following along: the stretches it was
	// spoken in, in order (the first opens with "Previously, in <Title>.",
	// then Text's paragraphs), each with where it sits in the clip and its
	// words' timings. Empty without a clip.
	Segments []recapSegmentDTO `json:"segments"`
	// ReadAt is when the reader stopped at the position it was written for
	// (the book's lastReadAt then, unix seconds) - how long they have been
	// away is measured from it.
	ReadAt    int64 `json:"readAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

// recapSegmentDTO is one stretch of a recap's clip - what one generation
// call spoke. Start/End and the words' times are seconds into the clip.
// Words is empty when the stretch couldn't be aligned; its text then
// spreads evenly over Start..End.
type recapSegmentDTO struct {
	Text  string          `json:"text"`
	Start float64         `json:"start"`
	End   float64         `json:"end"`
	Words []wordTimingDTO `json:"words"`
}

// bookRecapResponse is GET .../recap's reply and the bookRecap live
// topic's value. Recap is nil until one has been written; Preparing is
// true while one is queued or being written or spoken.
type bookRecapResponse struct {
	Recap     *bookRecapDTO `json:"recap"`
	Preparing bool          `json:"preparing"`
}

// recapSettingsDTO is the library-wide recap setting (GET|PUT
// /api/settings/recap, live topic recapSettings).
type recapSettingsDTO struct {
	// AfterMinutes is how long a reader must have been away from a book
	// before the clients offer its recap unasked: 0 offers it every time
	// the book is opened, -1 never.
	AfterMinutes int `json:"afterMinutes"`
}

// buildRecapSettings is the recapSettings live topic's value and GET
// /api/settings/recap's reply.
func (s *Server) buildRecapSettings() (recapSettingsDTO, error) {
	minutes, err := s.Store.RecapAfterMinutes()
	if err != nil {
		return recapSettingsDTO{}, httpError(http.StatusInternalServerError, err.Error())
	}
	return recapSettingsDTO{AfterMinutes: minutes}, nil
}

// handleGetRecapSettings serves GET /api/settings/recap.
func (s *Server) handleGetRecapSettings(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildRecapSettings()
	writeBuilt(w, v, err)
}

// handleSetRecapSettings serves PUT /api/settings/recap - the one recap
// setting, shared by every client of the library.
func (s *Server) handleSetRecapSettings(w http.ResponseWriter, r *http.Request) {
	var req recapSettingsDTO
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.AfterMinutes < store.RecapNever {
		writeError(w, http.StatusBadRequest, "afterMinutes must be -1 (never), 0 (immediately) or a number of minutes")
		return
	}
	if err := s.Store.SetRecapAfterMinutes(req.AfterMinutes); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeNoContent(w)
}

// recapPlan is what a recap for a book's current position will be written
// from.
type recapPlan struct {
	kind                                 store.RecapKind
	chapterIdx, paragraphIdx, throughIdx int
	// source is the recapped book for a series recap, nil otherwise.
	source *store.Book
	// chapters are the chapters whose summaries it reads, oldest first:
	// the ones before the current chapter, or all of source's.
	chapters []store.ChapterExtent
	// current is the chapter the reader is partway through (position
	// recaps only), nil at a chapter's start.
	current *store.ChapterExtent
	// summarizeCurrent asks for current's summary too: what the reader has
	// heard of it is more than a recap is shown as text.
	summarizeCurrent bool
}

// missing counts the chapter summaries p needs that don't exist yet.
func (p *recapPlan) missing() int {
	n := 0
	for _, e := range p.chapters {
		if !e.Summarized {
			n++
		}
	}
	if p.summarizeCurrent && !p.current.Summarized {
		n++
	}
	return n
}

// planRecap works out what a recap for book's saved position covers, nil
// when there is nothing to recap (the start of a book with no earlier one
// in its series in the library).
func (s *Server) planRecap(book *store.Book) (*recapPlan, error) {
	extents, err := s.Store.ChapterExtents(book.ID)
	if err != nil || len(extents) == 0 {
		return nil, err
	}
	pos := min(max(book.PosChapterIdx, 0), len(extents)-1)
	cur := extents[pos]
	paragraphs, err := s.Store.ListParagraphsRaw(cur.ID)
	if err != nil {
		return nil, err
	}
	before := 0 // text behind the reader, in this book
	for _, e := range extents[:pos] {
		before += e.Chars
	}
	plan := &recapPlan{chapterIdx: cur.Idx}
	last := 0
	if n := len(paragraphs); n > 0 {
		last = paragraphs[n-1].Idx
	}
	posParagraph := min(max(book.PosParagraphIdx, 0), last)
	heard := 0 // of the current chapter
	for _, p := range paragraphs {
		if p.Idx < posParagraph {
			heard += len(p.Text)
		}
	}
	if before+heard < recapStartChars {
		prev, err := s.previousInSeries(book)
		if err != nil || prev == nil {
			return nil, err
		}
		prevExtents, err := s.Store.ChapterExtents(prev.ID)
		if err != nil || len(prevExtents) == 0 {
			return nil, err
		}
		plan.kind, plan.source, plan.chapters = store.RecapSeries, prev, storyChapters(prevExtents)
		// Current for as long as the reader is still at the book's start.
		room := recapStartChars - before
		for _, p := range paragraphs {
			if room -= len(p.Text); room <= 0 {
				break
			}
			plan.throughIdx = p.Idx
		}
		return plan, nil
	}
	plan.kind, plan.paragraphIdx = store.RecapPosition, posParagraph
	plan.throughIdx = min(posParagraph+recapReuseParagraphs, last)
	reach := 0
	for i := pos - 1; i >= 0; i-- {
		n := len(plan.chapters)
		if n >= recapMaxChapters || n >= recapMinChapters && reach >= recapReachChars {
			break
		}
		if extents[i].Chars < recapSkipChars {
			continue
		}
		plan.chapters = append([]store.ChapterExtent{extents[i]}, plan.chapters...)
		reach += extents[i].Chars
	}
	if heard > 0 {
		plan.current = &cur
		plan.summarizeCurrent = heard > recapTextChars
	}
	return plan, nil
}

// storyChapters drops front and back matter (chapters under
// recapSkipChars) from extents.
func storyChapters(extents []store.ChapterExtent) []store.ChapterExtent {
	var out []store.ChapterExtent
	for _, e := range extents {
		if e.Chars >= recapSkipChars {
			out = append(out, e)
		}
	}
	return out
}

// previousInSeries returns the book right before book in its series, nil
// when it has none in the library: the one with the highest series index
// below book's, and no more than one below it - with volumes 1 and 10 of a
// serial in the library, volume 10 doesn't open with a recap of volume 1.
func (s *Server) previousInSeries(book *store.Book) (*store.Book, error) {
	if book.SeriesName == "" || book.SeriesIndex <= 0 {
		return nil, nil
	}
	books, err := s.Store.ListSeriesBooks(book.SeriesName)
	if err != nil {
		return nil, err
	}
	var prev *store.Book
	for i := range books {
		b := &books[i]
		if b.ID == book.ID || b.SeriesIndex <= 0 || b.SeriesIndex >= book.SeriesIndex || book.SeriesIndex-b.SeriesIndex > 1 {
			continue
		}
		if prev == nil || b.SeriesIndex > prev.SeriesIndex {
			prev = b
		}
	}
	return prev, nil
}

// nextInSeries is previousInSeries' inverse: the book whose previous book
// is book.
func (s *Server) nextInSeries(book *store.Book) (*store.Book, error) {
	if book.SeriesName == "" || book.SeriesIndex <= 0 {
		return nil, nil
	}
	books, err := s.Store.ListSeriesBooks(book.SeriesName)
	if err != nil {
		return nil, err
	}
	for i := range books {
		if books[i].ID == book.ID || books[i].SeriesIndex <= book.SeriesIndex {
			continue
		}
		prev, err := s.previousInSeries(&books[i])
		if err != nil {
			return nil, err
		}
		if prev != nil && prev.ID == book.ID {
			// ListSeriesBooks leaves out the reading position.
			return s.Store.GetBook(books[i].ID)
		}
	}
	return nil, nil
}

// recapVoice is book's narrator voice and the key a recap's clip is
// recorded under (store.BookRecap.AudioVoice) - the voice and the clone
// model speaking it, so changing either leaves the clip stale.
func (s *Server) recapVoice(book *store.Book) (narration.ResolvedVoice, string, error) {
	voice, err := s.Narration.BookVoice(book)
	if err != nil {
		return voice, "", err
	}
	return voice, voice.VoiceID() + "/" + voice.CloneModel, nil
}

// recapAudioFresh reports whether r's clip exists, was spoken in book's
// current narrator voice, and has its layout - a clip from before layouts
// were kept (no segments) is spoken again the next time its recap is asked
// for, so it can be followed word by word.
func (s *Server) recapAudioFresh(book *store.Book, r *store.BookRecap) bool {
	if r == nil || r.AudioVoice == "" || s.Narration == nil || r.Segments == "" || r.Segments == "[]" {
		return false
	}
	if _, key, err := s.recapVoice(book); err != nil || key != r.AudioVoice {
		return false
	}
	_, err := os.Stat(audiopath.Resolve(audiopath.RecapFile(s.DataDir, book.ID)))
	return err == nil
}

// buildBookRecap is the bookRecap live topic's value and GET .../recap's
// reply.
func (s *Server) buildBookRecap(bookID string) (bookRecapResponse, error) {
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		return bookRecapResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	if book == nil {
		return bookRecapResponse{}, httpError(http.StatusNotFound, "book not found")
	}
	r, err := s.Store.GetBookRecap(bookID)
	if err != nil {
		return bookRecapResponse{}, httpError(http.StatusInternalServerError, err.Error())
	}
	var out bookRecapResponse
	if s.Jobs != nil {
		queued, running := s.Jobs.BulkState(bookID, recapJobKey)
		out.Preparing = queued || running
	}
	if r == nil {
		return out, nil
	}
	out.Recap = &bookRecapDTO{
		Kind: r.Kind, ChapterIdx: r.ChapterIdx, ParagraphIdx: r.ParagraphIdx, ThroughIdx: r.ThroughIdx,
		Title: r.Title, Text: r.Content, Error: r.Error, ReadAt: r.ReadAt, UpdatedAt: r.UpdatedAt,
	}
	out.Recap.Segments = []recapSegmentDTO{}
	if s.recapAudioFresh(book, r) {
		out.Recap.AudioSeconds = r.AudioSeconds
		if err := json.Unmarshal([]byte(r.Segments), &out.Recap.Segments); err != nil {
			out.Recap.Segments = []recapSegmentDTO{}
		}
		// The version changes with the recap and with the voice that spoke it.
		out.Recap.AudioURL = fmt.Sprintf("/api/books/%s/recap/audio?v=%d-%s", bookID, r.UpdatedAt, store.VoiceID(r.AudioVoice, "", "")[:8])
	}
	return out, nil
}

// handleGetBookRecap serves GET /api/books/{id}/recap.
func (s *Server) handleGetBookRecap(w http.ResponseWriter, r *http.Request) {
	v, err := s.buildBookRecap(r.PathValue("id"))
	writeBuilt(w, v, err)
}

// handleGetBookRecapAudio serves GET /api/books/{id}/recap/audio - the
// recap as spoken (bookRecapDTO.AudioURL).
func (s *Server) handleGetBookRecapAudio(w http.ResponseWriter, r *http.Request) {
	path := audiopath.RecapFile(s.DataDir, r.PathValue("id"))
	if _, err := os.Stat(audiopath.Resolve(path)); err != nil {
		writeError(w, http.StatusNotFound, "this book's recap has not been spoken yet")
		return
	}
	serveClip(w, r, path)
}

// handlePrepareBookRecap serves POST /api/books/{id}/recap: queues the
// recap for the book's saved reading position (the pipeline_recap job) and
// answers 202 with how many it queued - 0 when the book already has one
// for that position, or there is nothing to recap there. ?force=true
// writes it again regardless. The bookRecap live topic carries the result.
func (s *Server) handlePrepareBookRecap(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "true"
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "recaps are not configured (set LECTABLE_SPEAKER_LLM_MODEL_PATH)")
		return
	}
	book, err := s.Store.GetBook(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	queued, err := s.prepareRecap(book, jobs.TierUrgent, force, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	n := 0
	if queued {
		n = 1
	}
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: n})
}

// prepareRecap queues the recap for book's saved position at tier - or,
// when its job is already queued or running (prepared ahead, at background
// priority), raises that job to tier - unless the book has one for it
// already (written, and spoken in its current voice) - force
// queues it regardless - and reports whether it queued anything. A recap
// that failed at this position is tried again only when asked (force, or
// !eager). eager is a recap nobody asked for (prepareIdleRecaps): it is
// left alone when it would need more than recapEagerMissing chapters
// summarized first.
func (s *Server) prepareRecap(book *store.Book, tier int, force, eager bool) (bool, error) {
	plan, err := s.planRecap(book)
	if err != nil || plan == nil {
		return false, err
	}
	existing, err := s.Store.GetBookRecap(book.ID)
	if err != nil {
		return false, err
	}
	// rewrite is false when the stored recap stands and at most its clip
	// is missing or stale.
	rewrite := true
	if !force && existing != nil && existing.Kind == plan.kind {
		switch {
		case plan.kind == store.RecapSeries:
			// The same earlier book: its recap stands wherever in this
			// book's opening pages the reader now is.
			rewrite = existing.Content == "" || existing.SourceBookID != plan.source.ID
		case existing.Covers(plan.chapterIdx, plan.paragraphIdx):
			if existing.Content == "" && eager && time.Since(time.Unix(existing.UpdatedAt, 0)) < recapRetryAfter {
				return false, nil // failed here not long ago; wait to be asked
			}
			// Asked for, one written a little way back will do. Prepared
			// ahead for the next time the book is opened, it is written
			// for exactly where the reader stopped.
			rewrite = existing.Content == "" || eager && existing.ParagraphIdx != plan.paragraphIdx
		}
	}
	if !rewrite {
		kept := *existing
		if plan.kind == store.RecapSeries {
			kept.ChapterIdx, kept.ParagraphIdx, kept.ThroughIdx = plan.chapterIdx, plan.paragraphIdx, plan.throughIdx
		}
		if eager {
			kept.ReadAt = book.LastReadAt
		}
		if kept != *existing {
			if kept.UpdatedAt, err = s.Store.SetBookRecap(kept); err != nil {
				return false, err
			}
			existing = &kept
		}
		if s.recapAudioFresh(book, existing) {
			return false, nil
		}
	}
	if eager && rewrite && plan.missing() > recapEagerMissing {
		return false, nil
	}
	s.Jobs.EnqueueBulk(book.ID, jobs.BulkGroup{
		Kind:  "pipeline_recap",
		Key:   recapJobKey,
		Label: "Recap",
		Tier:  tier,
		// Not KindSummary: promoting the recap's row would promote every
		// summary the book has queued. The ones it waits on follow it anyway.
		Children: &jobs.ChildFilter{Kinds: []jobs.Kind{jobs.KindRecap, jobs.KindRecapNarration}},
	}, func(ctx context.Context, tier func() int) error {
		err := s.runRecap(ctx, book, plan, rewrite, tier)
		if errors.Is(err, context.Canceled) {
			return nil
		}
		if err != nil {
			log.Printf("httpapi: recap for book %s: %v", book.ID, err)
		}
		return err
	})
	return true, nil
}

// runRecap is the pipeline_recap job: summarize the chapters plan reads
// that aren't yet, write the recap (unless !rewrite: the stored one only
// needs speaking), then speak it. A recap that can't be written is stored
// as its error; one that can't be spoken stays, as text.
func (s *Server) runRecap(ctx context.Context, book *store.Book, plan *recapPlan, rewrite bool, tier func() int) error {
	if rewrite {
		if err := s.summarizeForRecap(ctx, plan, tier); err != nil {
			return err
		}
		err := s.Jobs.RunRecap(ctx, book.ID, tier(), func(ctx context.Context) (int, func(), error) {
			return 0, nil, s.writeRecap(ctx, book, plan)
		})
		if err != nil {
			if !errors.Is(err, context.Canceled) && ctx.Err() == nil {
				s.storeRecap(book, plan, "", err)
			}
			return err
		}
	}
	if s.Narration == nil {
		return nil
	}
	return s.Jobs.RunRecapNarration(ctx, book.ID, tier(), func(ctx context.Context, _ int) error {
		return s.speakRecap(ctx, book.ID)
	})
}

// summarizeForRecap runs the summary pass for each chapter plan reads that
// has no notes, and waits for them. A chapter whose summary fails is left
// out of the recap rather than failing it.
func (s *Server) summarizeForRecap(ctx context.Context, plan *recapPlan, tier func() int) error {
	chapters := plan.chapters
	if plan.summarizeCurrent {
		chapters = append(chapters[:len(chapters):len(chapters)], *plan.current)
	}
	var wg sync.WaitGroup
	for _, e := range chapters {
		if e.Summarized {
			continue
		}
		ch, err := s.Store.GetChapterByID(e.ID)
		if err != nil || ch == nil {
			continue
		}
		book, err := s.Store.GetBook(ch.BookID)
		if err != nil || book == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Jobs.RunSummary(ctx, book.ID, ch.ID, ch.Idx, tier(), func(ctx context.Context) (int, func(), error) {
				return s.summarizeChapter(ctx, book, ch)
			}); err != nil && ctx.Err() == nil {
				log.Printf("httpapi: recap: summary of chapter %d of book %s: %v", ch.Idx, book.ID, err)
			}
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// recapNotes reads the summaries of chapters, leaving out the ones with
// none.
func (s *Server) recapNotes(chapters []store.ChapterExtent) ([]speakerattr.RecapChapter, error) {
	var out []speakerattr.RecapChapter
	for _, e := range chapters {
		notes, err := s.Store.GetChapterNotes(e.ID)
		if err != nil {
			return nil, err
		}
		if notes == nil || strings.TrimSpace(notes.Summary) == "" {
			continue
		}
		out = append(out, speakerattr.RecapChapter{Title: e.Title, Summary: notes.Summary})
	}
	return out, nil
}

// writeRecap is the KindRecap task: one LLM call (a few for a long earlier
// book) writing plan's recap, stored as the book's.
func (s *Server) writeRecap(ctx context.Context, book *store.Book, plan *recapPlan) error {
	chapters, err := s.recapNotes(plan.chapters)
	if err != nil {
		return err
	}
	var text string
	if plan.kind == store.RecapSeries {
		if len(chapters) == 0 {
			return fmt.Errorf("%q has no chapter summaries to recap it from", plan.source.Title)
		}
		text, err = s.Speaker.SeriesRecap(ctx, plan.source.Title, book.Title, chapters)
	} else {
		in := speakerattr.RecapInput{BookTitle: book.Title, Chapters: chapters}
		if plan.current != nil {
			if err := s.recapCurrentChapter(&in, plan); err != nil {
				return err
			}
		}
		if len(in.Chapters) == 0 && len(in.CurrentParts) == 0 && len(in.CurrentText) == 0 {
			return errors.New("nothing to write a recap from")
		}
		text, err = s.Speaker.Recap(ctx, in, s.bookSetting(ctx, book))
	}
	if err != nil {
		return err
	}
	s.storeRecap(book, plan, text, nil)
	return nil
}

// recapCurrentChapter fills in what the reader has heard of plan's current
// chapter: the summaries of the windows of it they have finished (when its
// notes keep them), then the text from there to where they stopped.
func (s *Server) recapCurrentChapter(in *speakerattr.RecapInput, plan *recapPlan) error {
	in.CurrentTitle = plan.current.Title
	paragraphs, err := s.Store.ListParagraphsRaw(plan.current.ID)
	if err != nil {
		return err
	}
	notes, err := s.Store.GetChapterNotes(plan.current.ID)
	if err != nil {
		return err
	}
	textFrom := 0
	if notes != nil {
		// A part is finished once the reader has reached the next one.
		for i, part := range notes.Parts {
			if i+1 >= len(notes.Parts) || notes.Parts[i+1].StartIdx > plan.paragraphIdx {
				break
			}
			in.CurrentParts = append(in.CurrentParts, part.Summary)
			textFrom = notes.Parts[i+1].StartIdx
		}
	}
	for _, p := range paragraphs {
		if p.Idx < textFrom || p.Idx >= plan.paragraphIdx || p.Unvoiced() {
			continue
		}
		in.CurrentText = append(in.CurrentText, speakerattr.ParagraphInput{Idx: p.Idx, Text: p.Text, Inline: p.Inline, IsQuote: p.IsQuote && !p.ScareQuote})
	}
	// Keep the end: what they heard last matters most.
	size := 0
	for i := len(in.CurrentText) - 1; i >= 0; i-- {
		if size += len(in.CurrentText[i].Text); size > recapTextChars && i < len(in.CurrentText)-1 {
			in.CurrentText = in.CurrentText[i+1:]
			in.Truncated = len(in.CurrentParts) == 0
			break
		}
	}
	return nil
}

// storeRecap stores plan's recap as book's: text, or failure when writing
// it failed.
func (s *Server) storeRecap(book *store.Book, plan *recapPlan, text string, failure error) {
	r := store.BookRecap{
		BookID: book.ID, Kind: plan.kind, ChapterIdx: plan.chapterIdx, ParagraphIdx: plan.paragraphIdx, ThroughIdx: plan.throughIdx,
		Title: spokenTitle(book.Title), Content: text,
	}
	if plan.source != nil {
		r.SourceBookID, r.Title = plan.source.ID, spokenTitle(plan.source.Title)
	}
	if failure != nil {
		r.Error = failure.Error()
	}
	// When the reader stopped here: the position's own timestamp, re-read
	// since a recap can take a while to reach the front of the queue.
	if b, err := s.Store.GetBook(book.ID); err == nil && b != nil {
		r.ReadAt = b.LastReadAt
	}
	if _, err := s.Store.SetBookRecap(r); err != nil {
		log.Printf("httpapi: store recap for book %s: %v", book.ID, err)
	}
}

// speakRecap is the KindRecapNarration task: the book's stored recap,
// opened with "Previously, in <title>.", spoken a stretch at a time in the
// book's narrator voice and saved as one clip.
func (s *Server) speakRecap(ctx context.Context, bookID string) error {
	book, err := s.Store.GetBook(bookID)
	if err != nil || book == nil {
		return err
	}
	r, err := s.Store.GetBookRecap(bookID)
	if err != nil || r == nil || r.Content == "" {
		return err
	}
	voice, voiceKey, err := s.recapVoice(book)
	if err != nil {
		return err
	}
	var clip *wav.Clip
	segments := []recapSegmentDTO{}
	for _, chunk := range recapChunks(spokenRecap(r.Title, r.Content)) {
		audio, words, err := s.Jobs.NarrateText(ctx, bookID, voice, chunk)
		if err != nil {
			return err
		}
		part, err := wav.Decode(audio)
		if err != nil {
			return fmt.Errorf("decode recap audio: %w", err)
		}
		if clip == nil {
			clip = &wav.Clip{SampleRate: part.SampleRate, Channels: part.Channels}
		} else {
			gap := &wav.Clip{Samples: make([]float32, int(recapGapSeconds*float64(part.SampleRate))*part.Channels), SampleRate: part.SampleRate, Channels: part.Channels}
			if clip, err = wav.Concat(clip, gap); err != nil {
				return err
			}
		}
		start := clipSeconds(clip)
		if clip, err = wav.Concat(clip, part); err != nil {
			return err
		}
		seg := recapSegmentDTO{Text: chunk, Start: start, End: clipSeconds(clip), Words: []wordTimingDTO{}}
		for _, w := range words {
			seg.Words = append(seg.Words, wordTimingDTO{Text: w.Text, Start: start + w.Start, End: start + w.End})
		}
		segments = append(segments, seg)
	}
	if clip == nil {
		return nil
	}
	encoded, err := wav.Encode(clip.Samples, clip.SampleRate, clip.Channels)
	if err != nil {
		return err
	}
	seconds, err := wav.Duration(encoded)
	if err != nil {
		return err
	}
	path := audiopath.RecapFile(s.DataDir, bookID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := audiopath.WriteClip(path, encoded); err != nil {
		return err
	}
	// Not recorded if the recap was replaced meanwhile: the new one's own
	// narration overwrites the clip.
	layout, err := json.Marshal(segments)
	if err != nil {
		return err
	}
	_, err = s.Store.SetBookRecapAudio(bookID, r.UpdatedAt, voiceKey, seconds.Seconds(), string(layout))
	return err
}

// clipSeconds is how long c plays.
func clipSeconds(c *wav.Clip) float64 {
	if c.SampleRate == 0 || c.Channels == 0 {
		return 0
	}
	return float64(len(c.Samples)/c.Channels) / float64(c.SampleRate)
}

// spokenRecap is a recap as it is read aloud: the opener, then its text.
func spokenRecap(title, text string) string {
	if title == "" {
		return "Previously. " + text
	}
	return "Previously, in " + strings.TrimRight(title, ".!?") + ". " + text
}

// subtitleWords mark a title segment as a subtitle or a series tag rather
// than part of the name ("A Dark Fantasy LitRPG Adventure").
var subtitleWords = regexp.MustCompile(`(?i)\b(litrpg|gamelit|novel|novella|adventure|fantasy|saga|series|trilogy|romance|thriller|mystery|story|tale|epic|book \d+)\b`)

// spokenTitle shortens a book's title to what a narrator would say: no
// parenthesized series tag, no subtitle - "City of Fallen Souls: A Dark
// Fantasy LitRPG Adventure (UnderVerse Book 3)" is "City of Fallen Souls",
// "Arise: Reclaimer: A LitRPG Progression Fantasy." is "Arise: Reclaimer".
func spokenTitle(title string) string {
	if i := strings.IndexAny(title, "([{"); i > 0 {
		title = title[:i]
	}
	parts := strings.Split(title, ": ")
	for len(parts) > 1 && subtitleWords.MatchString(parts[len(parts)-1]) {
		parts = parts[:len(parts)-1]
	}
	return strings.TrimSpace(strings.Join(parts, ": "))
}

// recapSentenceEnd is where recapChunks may split a long paragraph.
var recapSentenceEnd = regexp.MustCompile(`[.!?]["”’)]*\s+`)

// recapChunks splits a recap into the stretches spoken one generation call
// each: its paragraphs, a paragraph over recapChunkChars broken between
// sentences.
func recapChunks(text string) []string {
	var out []string
	for _, para := range strings.Split(text, "\n\n") {
		para = strings.TrimSpace(para)
		for len(para) > recapChunkChars {
			cut := -1
			for _, m := range recapSentenceEnd.FindAllStringIndex(para, -1) {
				if m[1] > recapChunkChars {
					break
				}
				cut = m[1]
			}
			if cut < 0 {
				break
			}
			out = append(out, strings.TrimSpace(para[:cut]))
			para = para[cut:]
		}
		if para != "" {
			out = append(out, para)
		}
	}
	return out
}

// PrepareIdleRecaps is one pass of preparing recaps before they are asked
// for - a cron task (cmd/server, every RecapCheckEvery). A book whose
// reader hasn't touched it for as long as the library's offer threshold
// (store.RecapAfterMinutes - the same time away after which the clients
// offer the recap; at least recapIdleFloor, and never when the threshold
// is "never") and that was read within recapIdleWithin gets the recap for
// where they stopped queued at background priority, so it is written and
// spoken by the time it is offered. For a book left at its end it is the
// next book in the series' recap of it instead. Each position is looked
// at once per run of the server. Does nothing without the LLM.
func (s *Server) PrepareIdleRecaps(ctx context.Context) error {
	if s.Speaker == nil || s.Jobs == nil {
		return nil
	}
	after, err := s.Store.RecapAfterMinutes()
	if err != nil || after < 0 {
		return err
	}
	idleAfter := max(time.Duration(after)*time.Minute, recapIdleFloor)
	books, err := s.Store.ListBooks()
	if err != nil {
		return err
	}
	now := time.Now()
	for i := range books {
		if ctx.Err() != nil {
			return nil
		}
		b := &books[i]
		if b.LastReadAt == 0 {
			continue
		}
		idle := now.Sub(time.Unix(b.LastReadAt, 0))
		if idle < idleAfter || idle > recapIdleWithin {
			continue
		}
		at := fmt.Sprintf("%d:%d", b.PosChapterIdx, b.PosParagraphIdx)
		if prev, _ := s.idleRecapTried.Swap(b.ID, at); prev == at {
			continue
		}
		if err := s.prepareIdleRecap(b); err != nil {
			log.Printf("httpapi: idle recap for book %s: %v", b.ID, err)
		}
	}
	return nil
}

// prepareIdleRecap queues book's recap for where its reader stopped - or,
// when they stopped at its end, the recap of it that the next book in the
// series opens with.
func (s *Server) prepareIdleRecap(book *store.Book) error {
	extents, err := s.Store.ChapterExtents(book.ID)
	if err != nil || len(extents) == 0 {
		return err
	}
	if last := extents[len(extents)-1]; book.PosChapterIdx >= last.Idx && book.PosParagraphIdx >= last.Paragraphs-1 {
		next, err := s.nextInSeries(book)
		if err != nil || next == nil {
			return err
		}
		// Only for a reader yet to start it - a next book already read or
		// under way has its own recaps, when it is read again.
		if plan, err := s.planRecap(next); err != nil || plan == nil || plan.kind != store.RecapSeries {
			return err
		}
		book = next
	}
	_, err = s.prepareRecap(book, jobs.TierBackground, false, true)
	return err
}
