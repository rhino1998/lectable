package httpapi

import (
	"context"
	"log"
	"net/http"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// isPreprocessing reports whether bookID currently has a preprocessing
// pipeline run in progress - the read side of jobs.Manager.
// IsPipelineRunning, used by buildBookSummary/handleListBooks to set
// bookSummaryDTO.Preprocessing so the library page can show a spinner
// without polling a separate endpoint.
func (s *Server) isPreprocessing(bookID string) bool {
	return s.Jobs.IsPipelineRunning(bookID)
}

// handleGenerateBook enqueues background TTS generation for every chapter
// in a book at once - the library page's own "Generate audio" -> "All"
// button (LibraryPage.tsx), for a reader who wants a whole book ready to
// listen to without opening it chapter by chapter first.
//
// A single call into jobs.Manager.EnqueueBookGenerate, not a loop over
// every chapter here: that whole-book walk (and its own cancelability) now
// lives on the Manager side, as one cancelable poolPipeline task rather
// than this handler pushing N independent per-chapter tasks with no
// single handle to stop them all at once - see EnqueueBookGenerate's own
// doc comment. Fire-and-forget either way: this never itself blocks on an
// actual TTS render or LLM call the way a preprocessing phase does, and
// calling it twice in a row, or alongside a reader actively reading the
// book, just re-enqueues whatever isn't already ready/generating,
// harmlessly (EnqueueBookGenerate's own task is idempotent per book, same
// as EnqueueChapter always was per chapter).
func (s *Server) handleGenerateBook(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	s.Jobs.EnqueueBookGenerate(bookID)
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

// handleGenerateRemaining enqueues background TTS generation for every
// paragraph in a book from its current stored reading position
// (store.Book.PosChapterIdx/PosParagraphIdx) through the end of the book -
// the "Remaining" option on the library page's "Generate audio" button
// (LibraryPage.tsx), alongside handleGenerateBook's "All" option. Position
// is read server-side rather than taken from the request body: it's the
// same value GET /api/books already returns per book, so a caller (the
// library page, or the Android app's own long-press equivalent) needs
// nothing beyond the book ID to mean "the rest of this book, from wherever
// I've actually gotten to". See jobs.Manager.EnqueueRemaining.
func (s *Server) handleGenerateRemaining(w http.ResponseWriter, r *http.Request) {
	bookID := r.PathValue("id")
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	s.Jobs.EnqueueRemaining(bookID)
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

// handlePreprocessBook kicks off the five-phase preprocessing pipeline for
// the whole book (see jobs.Manager.EnqueuePipeline) and returns
// immediately (202) - the same fire-and-forget shape as
// handleAttributeSpeakers/handleTagDirections, for the same reason: a
// whole book can take anywhere from minutes to hours to fully attribute/
// characterize/provision/tag, so nothing should hold an HTTP request (or a
// browser connection) open for it. Progress is observable the same way
// running each phase's own buttons individually already is - via GET
// /api/jobs (each chapter/character's own attribution/characterization/
// direction/provision task appears there as it's dispatched, same Kinds
// as the single-item buttons use), by refetching the book's speaker table
// as chapters/characters complete, and via bookSummaryDTO.Preprocessing
// (see isPreprocessing) for a simple whole-book "still running" indicator
// - the library page polls this to show a spinner over the cover for as
// long as it's true.
//
// Rejects (409) a second call for a book that already has one of these
// running - see jobs.ErrPipelineAlreadyRunning.
//
// chapters is listed once here, before any phase is even enqueued, and
// shared unchanged by preprocessAttributionPhase, preprocessDirectionPhase,
// and preprocessMusicPhase below (see their own doc comments for why that
// staleness is safe) - mirroring the one piece of shared, hoisted state
// the old single-goroutine runBookPipeline used to keep across its own
// four inline phases; a failure listing it aborts the whole preprocessing
// request up front (500) rather than starting phase 1 with nothing to do.
func (s *Server) handlePreprocessBook(w http.ResponseWriter, r *http.Request) {
	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker attribution is not configured (set SPEAKER_LLM_MODEL_PATH)")
		return
	}
	bookID := r.PathValue("id")
	book, err := s.Store.GetBook(bookID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}
	chapters, err := s.Store.ListChapterSummaries(book.ID, "", nil)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	phases := [5]jobs.PipelinePhaseFunc{
		s.preprocessAttributionPhase(book, chapters),
		s.preprocessCharacterizationPhase(book),
		s.preprocessVoiceProvisionPhase(book),
		s.preprocessDirectionPhase(book, chapters),
		s.preprocessMusicPhase(book, chapters),
	}
	if err := s.Jobs.EnqueuePipeline(book.ID, phases); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

// preprocessAttributionPhase is book preprocessing's Phase 1 (see
// jobs.PipelinePhaseFunc/jobs.Manager.EnqueuePipeline): speaker attribution
// for every one of chapters that hasn't already finished it -
// store.Passes.Attribution (chapters.passes) already tells us this
// directly, without needing attributeChapter's own onlyUnattributed
// re-derivation (that flag means something narrower - "only paragraphs
// still missing a speaker within an otherwise-attributed chapter", not
// "skip this chapter entirely"). A chapter whose Passes.Attribution is
// already true has finished attribution at least once, so a second
// preprocess run (or one resumed after an earlier cancel) doesn't re-pay
// for the whole chapter's own LLM batches again - the "the pre-process
// task should be aware of which jobs have already happened and skip them"
// reasoning this column exists for in the first place.
//
// Within the phase, every not-yet-attributed chapter is enqueued at once
// (one goroutine per chapter, each blocking on its own jobs.Manager.
// RunAttribution call, joined by a sync.WaitGroup before the phase itself
// finishes) rather than one at a time - this loop no longer decides how
// much of that actually runs concurrently, jobs.Manager does: every
// chapter's own KindSpeakerAttribution task carries a hard dependency
// (jobs.Manager.attributionOrderDependency) on its own book's immediately
// preceding chapter's own attribution finishing first, so this book's own
// chapters still only ever attribute one at a time no matter how many of
// these goroutines are blocked at once - see that dependency's own doc
// comment for why chapter order matters here (attributeChapter's "already
// known characters" list is read live from the store at call-start, so two
// chapters of the same book attributing concurrently or out of order risks
// splitting one recurring character into two different names). Firing
// every chapter's own call at once (the same way SpeakersPage's own
// "Attribute all" button already does via EnqueueAttribution) still isn't
// wasted, though: poolLLM's up-to-4 concurrent slots (maxAttributionInFlight)
// remain free to run this book's own next-in-line chapter alongside a
// *different* book's attribution, or alongside characterization, at the
// same time - only same-book chapter ordering is now serialized, not
// poolLLM itself. This phase's own goroutine still never occupies a
// poolLLM slot itself even while blocked waiting on one of these, since
// RunAttribution is exactly the same non-slot-occupying blocking call
// SpeakersPage's individual actions already use (see RunAttribution's own
// doc comment).
//
// Best-effort: one chapter's failure is logged and skipped, never aborts
// the phase - so one bad LLM response partway through a long book doesn't
// stop attribution for everything after it. A chapter's own attribution
// run can itself pause partway through (see jobs.AttributionFunc's own
// doc comment) if a more urgent poolLLM task arrives while this bulk phase
// is going - that chapter's own child goroutine (and so this phase's own
// WaitGroup) moves on as soon as the *current* dispatch returns, which can
// be a partial result whose continuation keeps running detached from this
// phase entirely; rerunning preprocess (or the chapter's own button)
// later picks up anything left unfinished.
func (s *Server) preprocessAttributionPhase(book *store.Book, chapters []store.ChapterSummary) jobs.PipelinePhaseFunc {
	return func(ctx context.Context, tier func() int) error {
		var wg sync.WaitGroup
		for _, cs := range chapters {
			if cs.Passes.Attribution {
				continue
			}
			ch := cs.Chapter
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Jobs.RunAttribution(ctx, book.ID, ch.ID, ch.Idx, tier(), func(ctx context.Context) (int, func(), error) {
					return s.attributeChapter(ctx, book, &ch, false)
				}); err != nil {
					log.Printf("httpapi: preprocess book %s: attribute chapter %d: %v", book.ID, ch.Idx, err)
				}
			}()
		}
		wg.Wait()
		if ctx.Err() != nil {
			log.Printf("httpapi: preprocess book %s: canceled during attribution", book.ID)
		}
		return nil
	}
}

// preprocessCharacterizationPhase is Phase 2 - characterization for every
// character discovered so far (including ones from earlier books in the
// same series - see store.SeriesScope) who isn't characterized yet;
// ensureCharacterized's own fast path skips anyone who already is. Listed
// fresh here (unlike Phase 1's chapters, which the pipeline's caller
// hoists once and shares with Phase 4's own direction-tagging phase) since
// this phase only ever needs the roster as it stands once Phase 1 has
// actually finished discovering new characters - see jobs.pipelineResolver
// for the dependency that guarantees this phase never starts before that.
//
// Same concurrent-fan-out-then-join shape as Phase 1 (one goroutine per
// character, joined by a WaitGroup) and the same best-effort framing - one
// character's characterization failing is logged and skipped, never
// aborts the phase.
func (s *Server) preprocessCharacterizationPhase(book *store.Book) jobs.PipelinePhaseFunc {
	return func(ctx context.Context, tier func() int) error {
		characters, err := s.Store.ListCharacters(store.SeriesScope(book))
		if err != nil {
			log.Printf("httpapi: preprocess book %s: list characters: %v", book.ID, err)
		}
		var wg sync.WaitGroup
		for _, char := range characters {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.ensureCharacterized(ctx, book, char); err != nil {
					log.Printf("httpapi: preprocess book %s: characterize %q: %v", book.ID, char.Name, err)
				}
			}()
		}
		wg.Wait()
		if ctx.Err() != nil {
			log.Printf("httpapi: preprocess book %s: canceled during characterization", book.ID)
		}
		return nil
	}
}

// preprocessVoiceProvisionPhase is Phase 3 - voice provisioning for every
// character that doesn't already have one, for the book's own currently-
// resolved clone model. Re-lists characters rather than reusing Phase 2's
// own roster: characterization can discover brand-new characters too (see
// store.Store.UpsertCharacter's call sites), and this phase only actually
// needs the roster as it stands once Phase 2 has finished (see
// jobs.pipelineResolver again). Routed through jobs.Manager.
// RunVoiceProvision, not provisionCharacterVoice directly, so an implicit
// reference-clip render here never spawns its own worker-side model
// instance uncoordinated with the rest of poolGeneration - see
// ensureVoiceRef's own doc comment (voices.go) for the general rule this
// follows. Same fan-out/join/best-effort shape as the other three phases.
//
// Checks store.CharacterVoiceForModel up front, the same signal
// jobs.provisionMissingCharacterVoices' own lazy path (and this book's own
// Speakers page - a character's VoicePresetID/RefAudioURL) already gate
// on, and skips dispatching a RunVoiceProvision task at all for a
// character that already has one - not just relying on
// provisionCharacterVoiceAttempt's own downstream check (which does exist,
// and returns early for the same reason), because even a task that
// returns near-instantly still occupies poolLLM/poolDesign's mutual-
// exclusion "active pool" slot for its duration and shows up as a real
// voice_provision entry on the Jobs dashboard. On a book with a large,
// already-fully-provisioned roster (the common case for a rerun - this
// phase used to unconditionally re-dispatch one task per character, every
// single preprocess run, regardless of whether any of them actually
// needed it), that's a lot of pointless task-queue churn and pool-
// switching noise for work that was already done.
func (s *Server) preprocessVoiceProvisionPhase(book *store.Book) jobs.PipelinePhaseFunc {
	return func(ctx context.Context, tier func() int) error {
		bookVoice, err := s.Narration.BookVoice(book)
		if err != nil {
			log.Printf("httpapi: preprocess book %s: resolve book voice: %v", book.ID, err)
			return nil
		}
		cloneModel := narration.EffectiveCloneModel(bookVoice)
		characters, err := s.Store.ListCharacters(store.SeriesScope(book))
		if err != nil {
			log.Printf("httpapi: preprocess book %s: list characters: %v", book.ID, err)
		}
		var wg sync.WaitGroup
		for _, char := range characters {
			if presetID, err := s.Store.CharacterVoiceForModel(char.ID, cloneModel); err != nil {
				log.Printf("httpapi: preprocess book %s: check voice for %q: %v", book.ID, char.Name, err)
				continue
			} else if presetID != "" {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Jobs.RunVoiceProvision(ctx, book.ID, char.ID, "generate", char.Name, func(ctx context.Context, attempt int) (string, error) {
					return s.provisionCharacterVoiceAttempt(ctx, book.ID, char, cloneModel, attempt)
				}); err != nil {
					log.Printf("httpapi: preprocess book %s: provision voice for %q: %v", book.ID, char.Name, err)
				}
			}()
		}
		wg.Wait()
		if ctx.Err() != nil {
			log.Printf("httpapi: preprocess book %s: canceled during voice provisioning", book.ID)
		}
		return nil
	}
}

// preprocessDirectionPhase is Phase 4 - speech-direction tagging for every
// one of chapters (the same slice Phase 1 shares, hoisted once by
// handlePreprocessBook) whose Passes.Direction isn't already set - same
// skip reasoning as Phase 1. Unlike Phases 2/3, this one carries no
// dependency on any earlier phase at all (see jobs.pipelineResolver/the
// pipelinePhase* constants' own doc comments): directChapter needs only the
// book's own already-resolved voice and each paragraph's own IsQuote/Text,
// none of which attribution/characterization/voice-provisioning touch, so
// this phase dispatches immediately alongside Phase 1 rather than waiting
// for Phases 1-3 to clear first. chapters' own Passes values were read
// before any phase ever ran and are otherwise stale by the time this phase
// actually starts, but that's safe specifically for this check: no other
// phase ever sets Passes.Direction (Phase 1 only ever sets
// Passes.Attribution/Passes.Description), so a chapter this condition sees
// as not-yet-directed genuinely still isn't, regardless of what any other
// phase did to it in the meantime, running concurrently or not.
// directChapter's own two Higgs-only sub-passes still silently no-op for
// any other clone model (see higgsTags), but its third sub-pass
// (pronunciation) runs regardless, so there's still real work - and a real
// Passes.Direction advance - for this phase to do even then. Same
// fan-out/join/best-effort shape as the other three phases.
func (s *Server) preprocessDirectionPhase(book *store.Book, chapters []store.ChapterSummary) jobs.PipelinePhaseFunc {
	return func(ctx context.Context, tier func() int) error {
		var wg sync.WaitGroup
		for _, cs := range chapters {
			if cs.Passes.Direction {
				continue
			}
			ch := cs.Chapter
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Jobs.RunDirection(ctx, book.ID, ch.ID, ch.Idx, tier(), func(ctx context.Context) (int, func(), error) {
					return s.directChapter(ctx, book, &ch, nil)
				}); err != nil {
					log.Printf("httpapi: preprocess book %s: tag directions for chapter %d: %v", book.ID, ch.Idx, err)
				}
			}()
		}
		wg.Wait()
		if ctx.Err() != nil {
			log.Printf("httpapi: preprocess book %s: canceled during direction tagging", book.ID)
		}
		return nil
	}
}

// preprocessMusicPhase is Phase 5 - background-music tone-region scoring
// for every one of chapters (the same slice Phases 1/4 share, hoisted once
// by handlePreprocessBook) whose Passes.Music isn't already set - same
// skip reasoning as Phase 1/Phase 4. Carries no dependency on any earlier
// phase, the same reason Phase 4 doesn't: scoreChapterMusic needs only a
// chapter's own paragraph text (see jobs.pipelineResolver/the
// pipelinePhase* constants' own doc comments), untouched by attribution/
// characterization/voice-provisioning, so this phase dispatches
// immediately alongside Phases 1 and 4 rather than waiting for Phases 1-3
// to clear first.
//
// Deliberately not gated on store.Book.MusicEnabled - handleScoreChapterMusic
// (the per-chapter/bulk "Score music" buttons on the Speakers page) already
// allows scoring regardless of that book-wide toggle, since MusicEnabled
// only gates whether a scored region's audio actually gets *generated*
// (jobs.Manager.MaybeAdvanceChapterMusic), not whether scoring itself can
// run - "run everything" follows that same rule rather than silently
// skipping this phase for a book that hasn't turned music on yet. Same
// fan-out/join/best-effort shape as the other four phases.
func (s *Server) preprocessMusicPhase(book *store.Book, chapters []store.ChapterSummary) jobs.PipelinePhaseFunc {
	return func(ctx context.Context, tier func() int) error {
		var wg sync.WaitGroup
		for _, cs := range chapters {
			if cs.Passes.Music {
				continue
			}
			ch := cs.Chapter
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := s.Jobs.RunMusicScoring(ctx, book.ID, ch.ID, ch.Idx, tier(), func(ctx context.Context) (int, func(), error) {
					return s.scoreChapterMusic(ctx, book, &ch)
				}); err != nil {
					log.Printf("httpapi: preprocess book %s: score music for chapter %d: %v", book.ID, ch.Idx, err)
				}
			}()
		}
		wg.Wait()
		if ctx.Err() != nil {
			log.Printf("httpapi: preprocess book %s: canceled during music scoring", book.ID)
		}
		return nil
	}
}
