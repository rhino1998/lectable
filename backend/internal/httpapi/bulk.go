package httpapi

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
)

// Bulk scopes (the ?scope= query param of POST .../bulk/{action}): "rest"
// only touches items not done yet, "all" re-runs every one.
const (
	bulkScopeRest = "rest"
	bulkScopeAll  = "all"
)

// chapterBulkPass is one per-chapter pass the Speakers page can run across
// a whole book: kind is its per-chapter task kind (for promotion cascade -
// see jobs.ChildFilter), done reports whether a chapter already has it
// (skipped under bulkScopeRest), and run dispatches one chapter's real task,
// blocking until it finishes.
type chapterBulkPass struct {
	kind jobs.Kind
	done func(store.Passes) bool
	run  func(s *Server, ctx context.Context, tier int, book *store.Book, ch *store.Chapter) error
}

var chapterBulkPasses = map[string]chapterBulkPass{
	"attribution": {
		kind: jobs.KindSpeakerAttribution,
		done: func(p store.Passes) bool { return p.Attribution },
		run: func(s *Server, ctx context.Context, tier int, book *store.Book, ch *store.Chapter) error {
			_, err := s.Jobs.RunAttribution(ctx, book.ID, ch.ID, ch.Idx, tier, func(ctx context.Context) (int, func(), error) {
				return s.attributeChapter(ctx, book, ch, false)
			})
			return err
		},
	},
	"description": {
		kind: jobs.KindDescription,
		done: func(p store.Passes) bool { return p.Description },
		run: func(s *Server, ctx context.Context, tier int, book *store.Book, ch *store.Chapter) error {
			_, err := s.Jobs.RunDescription(ctx, book.ID, ch.ID, ch.Idx, tier, func(ctx context.Context) (int, func(), error) {
				return s.describeChapterForJob(ctx, book, ch)
			})
			return err
		},
	},
	"scare_quote": {
		kind: jobs.KindScareQuote,
		done: func(p store.Passes) bool { return p.ScareQuote },
		run: func(s *Server, ctx context.Context, tier int, book *store.Book, ch *store.Chapter) error {
			_, err := s.Jobs.RunScareQuote(ctx, book.ID, ch.ID, ch.Idx, tier, func(ctx context.Context) (int, func(), error) {
				return s.scareQuoteChapterForJob(ctx, book, ch)
			})
			return err
		},
	},
	"direction": {
		kind: jobs.KindSpeechDirection,
		done: func(p store.Passes) bool { return p.Direction },
		run: func(s *Server, ctx context.Context, tier int, book *store.Book, ch *store.Chapter) error {
			_, err := s.Jobs.RunDirection(ctx, book.ID, ch.ID, ch.Idx, tier, func(ctx context.Context) (int, func(), error) {
				return s.directChapter(ctx, book, ch, nil)
			})
			return err
		},
	},
	"pronunciation": {
		kind: jobs.KindPronunciation,
		done: func(p store.Passes) bool { return p.Pronunciation },
		run: func(s *Server, ctx context.Context, tier int, book *store.Book, ch *store.Chapter) error {
			_, err := s.Jobs.RunPronunciation(ctx, book.ID, ch.ID, ch.Idx, tier, func(ctx context.Context) (int, func(), error) {
				return s.pronounceChapter(ctx, book, ch, nil)
			})
			return err
		},
	},
	"music_scoring": {
		kind: jobs.KindMusicScoring,
		done: func(p store.Passes) bool { return p.Music },
		run: func(s *Server, ctx context.Context, tier int, book *store.Book, ch *store.Chapter) error {
			_, err := s.Jobs.RunMusicScoring(ctx, book.ID, ch.ID, ch.Idx, tier, func(ctx context.Context) (int, func(), error) {
				return s.scoreChapterMusic(ctx, book, ch)
			})
			return err
		},
	},
}

// handleBulkAction is POST /api/books/{id}/bulk/{action}?scope=rest|all -
// the Speakers page's whole-book buttons, each queued as one jobs.BulkGroup
// (Kind "pipeline_bulk_<action>") wrapping every per-chapter/per-character
// task it fans out into, rather than the client firing one request (and
// getting one Jobs row) per chapter. Canceling or promoting that one row
// covers the whole action - see jobs.BulkGroup.
//
// action is a chapterBulkPasses key, or one of:
//   - "generate": narration audio for every chapter (jobs.Manager.
//     EnqueueBookGenerate - only not-yet-ready paragraphs, so scope is moot)
//   - "music_generation": missing music for every eligible chapter
//     (jobs.Manager.EnqueueBookMusicGeneration; scope is moot)
//   - "characterization": re-characterize every valid character
//   - "voices": rest provisions characters with no voice for the book's clone
//     model yet; all force-regenerates every character's voice
//
// 202 {"queued": n} with how many chapters/characters the group covers (0,
// with nothing enqueued, if there's nothing to do). A repeat click while the
// same action+scope is still queued/in flight is a no-op.
func (s *Server) handleBulkAction(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	scope := r.URL.Query().Get("scope")
	if scope == "" {
		scope = bulkScopeRest
	}
	if scope != bulkScopeRest && scope != bulkScopeAll {
		writeError(w, http.StatusBadRequest, "scope must be \"rest\" or \"all\"")
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

	switch action {
	case "generate":
		s.Jobs.EnqueueBookGenerate(book.ID)
		writeJSON(w, http.StatusAccepted, map[string]int{"queued": 1})
		return
	case "music_generation":
		s.Jobs.EnqueueBookMusicGeneration(book.ID)
		writeJSON(w, http.StatusAccepted, map[string]int{"queued": 1})
		return
	}

	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker attribution is not configured (set SPEAKER_LLM_MODEL_PATH)")
		return
	}
	group := jobs.BulkGroup{
		Kind: jobs.Kind("pipeline_bulk_" + action),
		Key:  "bulk:" + action + ":" + scope,
		Tier: jobs.TierBackground,
	}

	var (
		queued int
		run    jobs.PipelinePhaseFunc
	)
	if pass, ok := chapterBulkPasses[action]; ok {
		group.Label = "All chapters"
		group.Children = &jobs.ChildFilter{Kinds: []jobs.Kind{pass.kind}}
		if scope == bulkScopeRest {
			group.Label = "Unfinished chapters"
		}
		queued, run, err = s.chapterBulkRun(book, scope, action, pass)
	} else {
		switch action {
		case "characterization":
			group.Label = "All characters"
			group.Children = &jobs.ChildFilter{Kinds: []jobs.Kind{jobs.KindSpeakerCharacterization}}
			queued, run, err = s.characterBulkRun(book, action, func(char store.Character) (bool, error) { return true, nil },
				func(ctx context.Context, tier int, char store.Character) error {
					return s.Jobs.RunCharacterizationAt(ctx, tier, book.ID, char.ID, char.Name, func(ctx context.Context) error {
						_, _, err := s.recharacterizeAndInvalidate(ctx, book, char.ID, func(ctx context.Context) error {
							_, err := s.characterizeVoice(ctx, book, char)
							return err
						})
						return err
					})
				})
		case "voices":
			group.Children = &jobs.ChildFilter{Kinds: []jobs.Kind{jobs.KindVoiceProvision}}
			queued, run, err = s.voicesBulkRun(book, scope, &group)
		default:
			writeError(w, http.StatusNotFound, "unknown bulk action "+action)
			return
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if queued > 0 {
		s.Jobs.EnqueueBulk(book.ID, group, run)
	}
	writeJSON(w, http.StatusAccepted, map[string]int{"queued": queued})
}

// chapterBulkRun picks the chapters a chapter pass covers under scope (read
// once, at click time - nothing but the pass itself sets its own flag, so a
// "not done" reading can't go stale in the wrong direction) and returns the
// group's run: every chapter dispatched at once, joined, best-effort. Same
// shape as a preprocessing phase (chapterPhase); per-chapter ordering, where
// it matters, is enforced by jobs (e.g. attributionOrderDependency).
func (s *Server) chapterBulkRun(book *store.Book, scope, action string, pass chapterBulkPass) (int, jobs.PipelinePhaseFunc, error) {
	summaries, err := s.Store.ListChapterSummaries(book.ID, "", nil)
	if err != nil {
		return 0, nil, err
	}
	var chapters []store.Chapter
	for _, cs := range summaries {
		if scope == bulkScopeRest && pass.done(cs.Passes) {
			continue
		}
		chapters = append(chapters, cs.Chapter)
	}
	return len(chapters), func(ctx context.Context, tier func() int) error {
		var wg sync.WaitGroup
		for _, ch := range chapters {
			wg.Go(func() {
				if err := pass.run(s, ctx, tier(), book, &ch); err != nil {
					log.Printf("httpapi: bulk %s for book %s chapter %d: %v", action, book.ID, ch.Idx, err)
				}
			})
		}
		wg.Wait()
		return nil
	}, nil
}

// characterBulkRun is chapterBulkRun's counterpart over the book's series
// roster (store.SeriesScope), skipping characters marked invalid - see
// store.Character.Invalid - and any include rejects.
func (s *Server) characterBulkRun(book *store.Book, action string, include func(store.Character) (bool, error), run func(ctx context.Context, tier int, char store.Character) error) (int, jobs.PipelinePhaseFunc, error) {
	all, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		return 0, nil, err
	}
	var characters []store.Character
	for _, char := range all {
		if char.Invalid {
			continue
		}
		ok, err := include(char)
		if err != nil {
			return 0, nil, err
		}
		if ok {
			characters = append(characters, char)
		}
	}
	return len(characters), func(ctx context.Context, tier func() int) error {
		var wg sync.WaitGroup
		for _, char := range characters {
			wg.Go(func() {
				if err := run(ctx, tier(), char); err != nil {
					log.Printf("httpapi: bulk %s for book %s character %q: %v", action, book.ID, char.Name, err)
				}
			})
		}
		wg.Wait()
		return nil
	}, nil
}

// voicesBulkRun is the "voices" action: under bulkScopeRest, provision a
// voice for every character that has none yet for the book's clone model
// (the same check preprocessVoiceProvisionPhase makes); under bulkScopeAll,
// force-regenerate every character's voice.
func (s *Server) voicesBulkRun(book *store.Book, scope string, group *jobs.BulkGroup) (int, jobs.PipelinePhaseFunc, error) {
	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		return 0, nil, err
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)
	if scope == bulkScopeAll {
		group.Label = "All characters"
		return s.characterBulkRun(book, "voices", func(store.Character) (bool, error) { return true, nil },
			func(ctx context.Context, tier int, char store.Character) error {
				_, err := s.Jobs.RunVoiceProvisionAt(ctx, tier, book.ID, char.ID, "regenerate", char.Name, func(ctx context.Context, attempt int) (string, error) {
					return s.regenerateCharacterVoice(ctx, book.ID, char, cloneModel, attempt)
				})
				return err
			})
	}
	group.Label = "Unvoiced characters"
	return s.characterBulkRun(book, "voices",
		func(char store.Character) (bool, error) {
			presetID, err := s.Store.CharacterVoiceForModel(char.ID, cloneModel)
			return presetID == "", err
		},
		func(ctx context.Context, tier int, char store.Character) error {
			_, err := s.Jobs.RunVoiceProvisionAt(ctx, tier, book.ID, char.ID, "generate", char.Name, func(ctx context.Context, attempt int) (string, error) {
				return s.provisionCharacterVoiceAttempt(ctx, book.ID, char, cloneModel, attempt)
			})
			return err
		})
}

// handleResetPass is POST /api/books/{id}/reset/{pass} - the Speakers
// page's per-column reset buttons: clears one pass across the whole book as
// though it had never run, and deletes whatever generated audio it made
// stale. pass is a bulk action name:
//   - "attribution"/"description"/"scare_quote"/"direction"/"pronunciation":
//     the paragraph data that pass wrote plus its chapters.passes flag (see
//     store.PassResets), invalidating audio for just the paragraphs whose
//     narration it changed (and their scare-quote merge groups). The
//     character roster is left alone - "Delete speaker data" covers that.
//   - "music_scoring": every music region (and its clip), unsetting
//     passes.music
//   - "music_generation": every music region's clip, back to pending
//   - "generate": every narration clip - handleDeleteBookAudio's action
//
// The paragraph passes accept ?fromChapter=<idx> to reset only chapters at
// or after that index instead of the whole book; the other passes reject it.
//
// Doesn't cancel an in-flight run of the same pass, which would set its
// flag again as it finishes - the Speakers page disables a reset while its
// pass is running. 200 {"invalidated": n}, n = paragraphs whose audio was
// deleted (0 for the music/generate resets).
func (s *Server) handleResetPass(w http.ResponseWriter, r *http.Request) {
	pass := r.PathValue("pass")
	book, err := s.Store.GetBook(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if book == nil {
		writeError(w, http.StatusNotFound, "book not found")
		return
	}

	removeMusicDirs := func() error {
		chapters, err := s.Store.ListChapterSummaries(book.ID, "", nil)
		if err != nil {
			return err
		}
		for _, cs := range chapters {
			_ = os.RemoveAll(audiopath.MusicDir(s.DataDir, book.ID, cs.Chapter.ID))
		}
		_ = os.RemoveAll(audiopath.AmbienceDir(s.DataDir, book.ID))
		return nil
	}

	fromIdx := 0
	if v := r.URL.Query().Get("fromChapter"); v != "" {
		if _, ok := store.PassResets[pass]; !ok {
			writeError(w, http.StatusBadRequest, "fromChapter is only supported for paragraph passes")
			return
		}
		if fromIdx, err = strconv.Atoi(v); err != nil || fromIdx < 0 {
			writeError(w, http.StatusBadRequest, "invalid fromChapter")
			return
		}
	}

	invalidated := 0
	switch pass {
	case "generate":
		err = s.Store.DeleteBookAudio(book.ID)
		if err == nil {
			err = s.Store.ResetAllChapterMusicAudioForBook(book.ID)
		}
		if err == nil {
			_ = os.RemoveAll(fmt.Sprintf("%s/audio/%s", s.DataDir, book.ID))
		}
	case "music_generation":
		err = s.Store.ResetAllChapterMusicAudioForBook(book.ID)
		if err == nil {
			err = removeMusicDirs()
		}
	case "music_scoring":
		err = s.Store.ClearBookMusicRegions(book.ID)
		if err == nil {
			err = removeMusicDirs()
		}
	default:
		if _, ok := store.PassResets[pass]; !ok {
			writeError(w, http.StatusNotFound, "unknown pass "+pass)
			return
		}
		var affected map[string][]int
		affected, err = s.Store.ResetBookPass(book.ID, pass, fromIdx)
		for chapterID, idxs := range affected {
			refs, derr := s.Store.DeleteParagraphAudioForIdxs(book.ID, chapterID, idxs)
			if derr != nil {
				log.Printf("httpapi: reset %s for book %s: invalidate audio for chapter %s: %v", pass, book.ID, chapterID, derr)
			}
			for _, ref := range refs {
				if rerr := os.Remove(s.paragraphAudioPath(ref.BookID, ref.ChapterID, ref.VoiceID, ref.Idx)); rerr != nil && !os.IsNotExist(rerr) {
					log.Printf("httpapi: reset %s for book %s: remove stale audio file: %v", pass, book.ID, rerr)
				}
			}
			invalidated += len(refs)
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"invalidated": invalidated})
}
