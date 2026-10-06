package httpapi

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/jobs"
	"github.com/rhino1998/lectable/backend/internal/narration"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
	"github.com/rhino1998/lectable/backend/internal/voices"
)

// bulkScope is the ?scope= query param of POST .../bulk/{action}: "rest"
// only touches items not done yet, "all" re-runs every one.
type bulkScope string

const (
	bulkScopeRest bulkScope = "rest"
	bulkScopeAll  bulkScope = "all"
)

// bulkAction is a Speakers-page whole-book action (POST
// /api/books/{id}/bulk/{action}), in the page's display order. "generate"
// queues the same "pipeline_generate_book" row as the library page's
// Generate: All.
type bulkAction string

const (
	bulkAttribution      bulkAction = "attribution"
	bulkScareQuote       bulkAction = "scare_quote"
	bulkDirection        bulkAction = "direction"
	bulkPronunciation    bulkAction = "pronunciation"
	bulkSummary          bulkAction = "summary"
	bulkAmbience         bulkAction = "ambience"
	bulkMusicGeneration  bulkAction = "music_generation"
	bulkGenerate         bulkAction = "generate"
	bulkCharacterization bulkAction = "characterization"
	bulkVoices           bulkAction = "voices"
	bulkUsedVoices       bulkAction = "used_voices"
)

// resetPass is a pass the Speakers page can reset across a whole book
// (POST /api/books/{id}/reset/{pass}) - every bulkAction except the
// character-scoped ones.
type resetPass string

const (
	resetAttribution     = resetPass(bulkAttribution)
	resetScareQuote      = resetPass(bulkScareQuote)
	resetDirection       = resetPass(bulkDirection)
	resetPronunciation   = resetPass(bulkPronunciation)
	resetSummary         = resetPass(bulkSummary)
	resetAmbience        = resetPass(bulkAmbience)
	resetMusicGeneration = resetPass(bulkMusicGeneration)
	resetGenerate        = resetPass(bulkGenerate)
	// resetLighting has no bulk action of its own: the next "ambience" run
	// re-lights every chapter it clears, without re-scoring its music.
	resetLighting resetPass = "lighting"
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

var chapterBulkPasses = map[bulkAction]chapterBulkPass{
	bulkAttribution: {
		kind: jobs.KindSpeakerAttribution,
		done: func(p store.Passes) bool { return p.Attribution },
		run: func(s *Server, ctx context.Context, tier int, book *store.Book, ch *store.Chapter) error {
			_, err := s.Jobs.RunAttribution(ctx, book.ID, ch.ID, ch.Idx, tier, func(ctx context.Context) (int, func(), error) {
				return s.attributeChapter(ctx, book, ch, attributeFull)
			})
			return err
		},
	},
	bulkScareQuote: {
		kind: jobs.KindScareQuote,
		done: func(p store.Passes) bool { return p.ScareQuote },
		run: func(s *Server, ctx context.Context, tier int, book *store.Book, ch *store.Chapter) error {
			_, err := s.Jobs.RunScareQuote(ctx, book.ID, ch.ID, ch.Idx, tier, func(ctx context.Context) (int, func(), error) {
				return s.scareQuoteChapterForJob(ctx, book, ch)
			})
			return err
		},
	},
	bulkDirection: {
		kind: jobs.KindSpeechDirection,
		done: func(p store.Passes) bool { return p.Direction },
		run: func(s *Server, ctx context.Context, tier int, book *store.Book, ch *store.Chapter) error {
			_, err := s.Jobs.RunDirection(ctx, book.ID, ch.ID, ch.Idx, tier, func(ctx context.Context) (int, func(), error) {
				return s.directChapter(ctx, book, ch, nil)
			})
			return err
		},
	},
	bulkPronunciation: {
		kind: jobs.KindPronunciation,
		done: func(p store.Passes) bool { return p.Pronunciation },
		run: func(s *Server, ctx context.Context, tier int, book *store.Book, ch *store.Chapter) error {
			_, err := s.Jobs.RunPronunciation(ctx, book.ID, ch.ID, ch.Idx, tier, func(ctx context.Context) (int, func(), error) {
				return s.pronounceChapter(ctx, book, ch, nil)
			})
			return err
		},
	},
	bulkSummary: {
		kind: jobs.KindSummary,
		done: func(p store.Passes) bool { return p.Summary },
		run: func(s *Server, ctx context.Context, tier int, book *store.Book, ch *store.Chapter) error {
			_, err := s.Jobs.RunSummary(ctx, book.ID, ch.ID, ch.Idx, tier, func(ctx context.Context) (int, func(), error) {
				return s.summarizeChapter(ctx, book, ch)
			})
			return err
		},
	},
	bulkAmbience: {
		kind: jobs.KindAmbience,
		done: func(p store.Passes) bool { return p.AmbienceDone() },
		run: func(s *Server, ctx context.Context, tier int, book *store.Book, ch *store.Chapter) error {
			_, err := s.Jobs.RunAmbience(ctx, book.ID, ch.ID, ch.Idx, tier, func(ctx context.Context) (int, func(), error) {
				return s.ambienceChapter(ctx, book, ch)
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
//   - "used_voices": everything the voices this book's own lines use still
//     lack - see usedVoicesBulkRun (scope is moot)
//
// 202 {"queued": n} with how many chapters/characters the group covers (0,
// with nothing enqueued, if there's nothing to do). A repeat click while the
// same action+scope is still queued/in flight is a no-op.
func (s *Server) handleBulkAction(w http.ResponseWriter, r *http.Request) {
	action := bulkAction(r.PathValue("action"))
	scope := bulkScope(r.URL.Query().Get("scope"))
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
	case bulkGenerate:
		s.Jobs.EnqueueBookGenerate(book.ID)
		writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
		return
	case bulkMusicGeneration:
		s.Jobs.EnqueueBookMusicGeneration(book.ID)
		writeJSON(w, http.StatusAccepted, queuedResponse{Queued: 1})
		return
	case bulkUsedVoices:
		// Ahead of the LLM check below: only a character with no voice at
		// all needs it (to be characterized); reference clips and emotion
		// variants don't.
		queued, run, err := s.usedVoicesBulkRun(book)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if queued > 0 {
			s.Jobs.EnqueueBulk(book.ID, jobs.BulkGroup{
				Kind:     jobs.Kind("pipeline_bulk_" + action),
				Key:      "bulk:" + string(action),
				Label:    "Voices this book uses",
				Tier:     jobs.TierBackground,
				Children: &jobs.ChildFilter{Kinds: []jobs.Kind{jobs.KindVoiceProvision, jobs.KindSpeakerCharacterization}},
			}, run)
		}
		writeJSON(w, http.StatusAccepted, queuedResponse{Queued: queued})
		return
	}

	if s.Speaker == nil {
		writeError(w, http.StatusServiceUnavailable, "speaker attribution is not configured (set LECTABLE_SPEAKER_LLM_MODEL_PATH)")
		return
	}
	group := jobs.BulkGroup{
		Kind: jobs.Kind("pipeline_bulk_" + action),
		Key:  "bulk:" + string(action) + ":" + string(scope),
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
		case bulkCharacterization:
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
		case bulkVoices:
			group.Children = &jobs.ChildFilter{Kinds: []jobs.Kind{jobs.KindVoiceProvision}}
			queued, run, err = s.voicesBulkRun(book, scope, &group)
		default:
			writeError(w, http.StatusNotFound, "unknown bulk action "+string(action))
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
	writeJSON(w, http.StatusAccepted, queuedResponse{Queued: queued})
}

// chapterBulkRun picks the chapters a chapter pass covers under scope (read
// once, at click time - nothing but the pass itself sets its own flag, so a
// "not done" reading can't go stale in the wrong direction) and returns the
// group's run: every chapter dispatched at once, joined, best-effort. Same
// shape as a preprocessing phase (chapterPhase); per-chapter ordering, where
// it matters, is enforced by jobs (e.g. attributionOrderDependency).
func (s *Server) chapterBulkRun(book *store.Book, scope bulkScope, action bulkAction, pass chapterBulkPass) (int, jobs.PipelinePhaseFunc, error) {
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
func (s *Server) characterBulkRun(book *store.Book, action bulkAction, include func(store.Character) (bool, error), run func(ctx context.Context, tier int, char store.Character) error) (int, jobs.PipelinePhaseFunc, error) {
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
func (s *Server) voicesBulkRun(book *store.Book, scope bulkScope, group *jobs.BulkGroup) (int, jobs.PipelinePhaseFunc, error) {
	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		return 0, nil, err
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)
	if scope == bulkScopeAll {
		group.Label = "All characters"
		return s.characterBulkRun(book, bulkVoices, func(store.Character) (bool, error) { return true, nil },
			func(ctx context.Context, tier int, char store.Character) error {
				_, err := s.Jobs.RunVoiceProvisionAt(ctx, tier, book.ID, char.ID, "regenerate", char.Name, func(ctx context.Context, attempt int) (string, error) {
					return s.regenerateCharacterVoice(ctx, book.ID, char, cloneModel, attempt)
				})
				return err
			})
	}
	group.Label = "Unvoiced characters"
	return s.characterBulkRun(book, bulkVoices,
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

// usedVoice is one voice this book's lines generate under, with what
// usedVoicesBulkRun found it still lacks.
type usedVoice struct {
	// label names it on its Jobs rows: the speaker, or "Narrator" for the
	// book's own voice.
	label string
	// unvoiced is the character to provision a voice for first, when they
	// have none for the book's clone model; voice is then resolved after.
	unvoiced *store.Character
	// index is the speaker number of the lines this voice is for - a
	// role's numbered lines each have their own (narration.MemberVoice).
	index int
	voice narration.ResolvedVoice
	// emotions are the ones this voice's lines use, in emotions.All order.
	emotions []string
}

// usedVoicesBulkRun is the "used_voices" action: get every voice this
// book's own lines generate under ready ahead of generation, instead of
// each being provisioned the first time a line needs it (jobs'
// provisionMissingCharacterVoices/referenceClipDependency/
// variantClipDependency, which hold that line - and the reader - until it's
// done). Unlike "voices", which covers the whole series roster, it only
// touches speakers with lines in this book, and it goes past the voice
// itself: for each one, a voice for the book's clone model if a character
// has none (multi-voice books only), the voice's base reference clip, and
// the emotion variant of every emotion its lines carry. Whatever already
// exists is left alone - a variant that failed for good included.
//
// queued counts the voices with anything missing.
func (s *Server) usedVoicesBulkRun(book *store.Book) (int, jobs.PipelinePhaseFunc, error) {
	paragraphs, err := s.Store.ListParagraphsRawForBook(book.ID)
	if err != nil {
		return 0, nil, err
	}
	characters, err := s.Store.ListCharacters(store.SeriesScope(book))
	if err != nil {
		return 0, nil, err
	}
	charByName := make(map[string]store.Character, len(characters))
	characterIDs := make([]string, len(characters))
	for i, c := range characters {
		charByName[c.Name] = c
		characterIDs[i] = c.ID
	}
	bookVoice, err := s.Narration.BookVoice(book)
	if err != nil {
		return 0, nil, err
	}
	cloneModel := narration.EffectiveCloneModel(bookVoice)
	presetIDByChar, err := s.Store.CharacterVoicesForModel(characterIDs, cloneModel)
	if err != nil {
		return 0, nil, err
	}

	// Who speaks in this book (and under which number), in first-appearance
	// order, and in which emotions.
	type speakerLine struct {
		name  string
		index int
	}
	var speakers []speakerLine
	emotionsBySpeaker := map[speakerLine]map[string]bool{}
	for _, p := range paragraphs {
		sp := speakerLine{p.Speaker, p.SpeakerIndex}
		if sp.name == "" {
			sp.name = "Narrator"
		}
		if emotionsBySpeaker[sp] == nil {
			emotionsBySpeaker[sp] = map[string]bool{}
			speakers = append(speakers, sp)
		}
		if e := p.EffectiveEmotion(); e != emotions.Neutral {
			emotionsBySpeaker[sp][e] = true
		}
	}

	// Several speakers can share a voice (everyone without their own falls
	// back to the book's), so clips are gathered per preset.
	byPreset := map[string]*usedVoice{}
	presetEmotions := map[string]map[string]bool{}
	var order []string
	var unvoiced []usedVoice
	// A character this book voices through its narrator's own voice, styled
	// by their characterization, never gets a preset - nothing to provision
	// once they're characterized (see provisionCharacterVoiceAttempt).
	instructed := (book.InstructCharacterVoices() || book.InstructAllCharacterVoices()) && cloneModel == voices.InstructedCloneModel
	for _, sp := range speakers {
		name := sp.name
		uv := usedVoice{label: "Narrator", voice: bookVoice}
		if c, ok := charByName[name]; ok && book.MultiVoice() && name != "Narrator" && !c.Invalid {
			uv.label, uv.index = name, sp.index
			if sp.index > 1 && c.IsRole {
				uv.label = fmt.Sprintf("%s %d", name, sp.index)
			}
			if presetIDByChar[c.ID] == "" && (!instructed || c.Summary == "") {
				uv.unvoiced = &c
				uv.emotions = sortedEmotions(emotionsBySpeaker[sp])
				unvoiced = append(unvoiced, uv)
				continue
			}
			resolved, err := s.Narration.ResolveCharacterVoice(book, bookVoice, c, cloneModel, presetIDByChar[c.ID])
			if err != nil {
				return 0, nil, err
			}
			uv.voice = narration.MemberVoice(resolved, c, sp.index)
		}
		if uv.voice.PresetID == "" {
			continue // a designed voice with no preset has no reference clip to render
		}
		if byPreset[uv.voice.PresetID] == nil {
			byPreset[uv.voice.PresetID] = &uv
			presetEmotions[uv.voice.PresetID] = map[string]bool{}
			order = append(order, uv.voice.PresetID)
		}
		for e := range emotionsBySpeaker[sp] {
			presetEmotions[uv.voice.PresetID][e] = true
		}
	}
	work := unvoiced
	for _, presetID := range order {
		uv := byPreset[presetID]
		uv.emotions = sortedEmotions(presetEmotions[presetID])
		if s.missingVoiceClips(uv.voice.PresetID, uv.emotions) {
			work = append(work, *uv)
		}
	}

	return len(work), func(ctx context.Context, tier func() int) error {
		var wg sync.WaitGroup
		for _, uv := range work {
			wg.Go(func() {
				if err := s.provisionUsedVoice(ctx, tier, book, bookVoice, cloneModel, uv); err != nil {
					log.Printf("httpapi: bulk %s for book %s voice %q: %v", bulkUsedVoices, book.ID, uv.label, err)
				}
			})
		}
		wg.Wait()
		return nil
	}, nil
}

func sortedEmotions(set map[string]bool) []string {
	var out []string
	for _, e := range emotions.All {
		if set[e.ID] {
			out = append(out, e.ID)
		}
	}
	return out
}

// missingVoiceClips reports whether presetID's base reference clip, or the
// variant of any of emotions, has yet to be rendered (a variant marked
// failed counts as settled - its lines clone from the base clip).
func (s *Server) missingVoiceClips(presetID string, emotionIDs []string) bool {
	if _, err := os.Stat(audiopath.VoicePresetRefFile(s.DataDir, presetID)); err != nil {
		return true
	}
	return slices.ContainsFunc(emotionIDs, func(e string) bool { return s.missingVariant(presetID, e) })
}

func (s *Server) missingVariant(presetID, emotion string) bool {
	return !voicerefs.VariantExists(s.DataDir, presetID, emotion) && !voicerefs.VariantFailed(s.DataDir, presetID, emotion)
}

// provisionUsedVoice runs one usedVoice's missing steps in order - each a
// real jobs task it blocks on, at the bulk row's live tier: the character's
// voice, then the base clip (a variant is cloned from it), then each
// variant. A failed variant doesn't stop the rest.
func (s *Server) provisionUsedVoice(ctx context.Context, tier func() int, book *store.Book, bookVoice narration.ResolvedVoice, cloneModel string, uv usedVoice) error {
	voice := uv.voice
	if char := uv.unvoiced; char != nil {
		presetID, err := s.Jobs.RunVoiceProvisionAt(ctx, tier(), book.ID, char.ID, "generate", char.Name, func(ctx context.Context, attempt int) (string, error) {
			return s.provisionCharacterVoiceAttempt(ctx, book.ID, *char, cloneModel, attempt)
		})
		if err != nil {
			return err
		}
		// Re-read: provisioning characterizes them, which an instructed
		// narrator voice (no preset of their own) is resolved from.
		fresh, err := s.Store.GetCharacterByName(store.SeriesScope(book), char.Name)
		if err != nil {
			return err
		}
		if fresh == nil {
			return nil // deleted or merged away meanwhile
		}
		if voice, err = s.Narration.ResolveCharacterVoice(book, bookVoice, *fresh, cloneModel, presetID); err != nil {
			return err
		}
		voice = narration.MemberVoice(voice, *fresh, uv.index)
	}
	if voice.PresetID == "" {
		return nil
	}
	base := voicerefs.BaseClip{
		PresetID:        voice.PresetID,
		Instruct:        voice.Instruct,
		Seed:            voice.Seed,
		RefText:         voice.RefText,
		SpeedMultiplier: voice.SpeedMultiplier,
		DesignModel:     voice.DesignModel,
	}
	if _, err := os.Stat(audiopath.VoicePresetRefFile(s.DataDir, voice.PresetID)); err != nil {
		if err := s.Jobs.RunReferenceClipAt(ctx, tier(), book.ID, uv.label, base); err != nil {
			return err
		}
	}
	for _, emotion := range uv.emotions {
		if !s.missingVariant(voice.PresetID, emotion) {
			continue
		}
		if err := s.Jobs.RunEmotionVariantAt(ctx, tier(), book.ID, uv.label, base, emotion); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("httpapi: bulk %s for book %s: %s variant of %q: %v", bulkUsedVoices, book.ID, emotion, uv.label, err)
		}
	}
	return nil
}

// handleResetPass is POST /api/books/{id}/reset/{pass} - the Speakers
// page's per-column reset buttons: clears one pass across the whole book as
// though it had never run, and deletes whatever generated audio it made
// stale. pass is a bulk action name:
//   - "attribution"/"scare_quote"/"direction"/"pronunciation":
//     the paragraph data that pass wrote plus its chapters.passes flag (see
//     store.PassResets), invalidating audio for just the paragraphs whose
//     narration it changed (and their scare-quote merge groups). The
//     character roster is left alone - "Delete speaker data" covers that.
//   - "ambience": every music region (and its clip, and its lighting),
//     unsetting passes.ambience and passes.lighting
//   - "lighting": every music region's lighting cue, unsetting
//     passes.lighting - the music is kept
//   - "music_generation": every music region's clip, back to pending
//   - "generate": every narration clip - handleDeleteBookAudio's action
//
// The paragraph passes accept ?fromChapter=<idx> to reset only chapters at
// or after that index instead of the whole book; the other passes reject it.
//
// Doesn't cancel an in-flight run of the same pass, which would set its
// flag again as it finishes - the Speakers page disables a reset while its
// pass is running. 200 {"invalidated": n}, n = paragraphs whose audio was
// deleted (0 for the music/lighting/generate resets).
func (s *Server) handleResetPass(w http.ResponseWriter, r *http.Request) {
	pass := resetPass(r.PathValue("pass"))
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
		if _, ok := store.PassResets[string(pass)]; !ok {
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
	case resetGenerate:
		cleared := time.Now()
		err = s.Store.DeleteBookAudio(book.ID)
		if err == nil {
			err = s.Store.ResetAllChapterMusicAudioForBook(book.ID)
		}
		if err == nil {
			_ = os.RemoveAll(fmt.Sprintf("%s/audio/%s", s.DataDir, book.ID))
			// Asked for new renders - see handleDeleteBookAudio.
			s.Jobs.DiscardOrphanedClips(cleared)
		}
	case resetMusicGeneration:
		err = s.Store.ResetAllChapterMusicAudioForBook(book.ID)
		if err == nil {
			err = removeMusicDirs()
		}
	case resetAmbience:
		err = s.Store.ClearBookMusicRegions(book.ID)
		if err == nil {
			err = removeMusicDirs()
		}
	case resetLighting:
		err = s.Store.ClearBookLighting(book.ID)
	case resetSummary:
		// Notes change no paragraph, so no audio goes stale.
		_, err = s.Store.ClearBookSummaries(book.ID, fromIdx)
	default:
		if _, ok := store.PassResets[string(pass)]; !ok {
			writeError(w, http.StatusNotFound, "unknown pass "+string(pass))
			return
		}
		var affected map[string][]int
		affected, err = s.Store.ResetBookPass(book.ID, string(pass), fromIdx)
		for chapterID, idxs := range affected {
			refs, derr := s.Store.DeleteParagraphAudioForIdxs(book.ID, chapterID, idxs)
			if derr != nil {
				log.Printf("httpapi: reset %s for book %s: invalidate audio for chapter %s: %v", pass, book.ID, chapterID, derr)
			}
			invalidated += len(refs)
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, invalidatedResponse{Invalidated: invalidated})
}
