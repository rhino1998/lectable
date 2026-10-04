package jobs

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/sched"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
)

// depTask finishes a lazily-created dependency task: the scheduler only
// calls a Need's New when nothing under its key exists yet, so this is the
// one place such a task enters the queue's bookkeeping.
func (m *Manager) depTask(nt *task) func() sched.Task {
	return func() sched.Task {
		nt.m = m
		nt.pushedAt = time.Now()
		m.notePushed(nt)
		return nt
	}
}

// joinOnly is a dependency on key only while a task under it is queued or
// running (an explicit re-run, say): nothing is created.
func joinOnly(key string) []sched.Need { return []sched.Need{{Key: key}} }

func (m *Manager) lookupBook(bookID string) *store.Book {
	if m.store == nil {
		return nil
	}
	book, err := m.store.GetBook(bookID)
	if err != nil {
		return nil
	}
	return book
}

func (m *Manager) lookupChapter(chapterID string) *store.Chapter {
	if m.store == nil {
		return nil
	}
	ch, err := m.store.GetChapterByID(chapterID)
	if err != nil {
		return nil
	}
	return ch
}

// characterizationDependency returns t's own speaker's currently queued
// or in-flight KindSpeakerCharacterization task, if any, or lazily creates
// one via m.characterize (a no-op, returning no dependency at all, if that
// was never configured - see CharacterCharacterizer's own doc comment) the
// first time it discovers the speaker isn't characterized yet
// (char.Summary == "" - a stored fact, not a task-completion signal) -
// this package's one implementation of the "check before creating" rule
// Manager.needs' own doc comment describes. name is
// t.paragraph.Speaker for a KindVoiceClone/KindVoiceDesign task or
// t.label for a KindVoiceProvision task (see EnqueueVoiceProvision); any
// other kind never has a name to look up.
//
// A clone/design/provision task blocked on this never races ahead of a
// characterization that's about to change what that voice even sounds
// like: recharacterizing rewrites the assigned preset's own Instruct (see
// httpapi.recharacterizeAndInvalidate), which also deletes any paragraph
// audio already generated under the now-stale characterization - a clone
// dispatched moments before that finishes would just get deleted again
// immediately, wasting the render.
func (m *Manager) characterizationDependency(t *task) []sched.Need {
	if m.store == nil {
		return nil
	}
	var name string
	switch t.kind {
	case KindVoiceClone, KindVoiceDesign:
		name = t.paragraph.Speaker
	case KindVoiceProvision:
		name = t.label
	}
	if name == "" || name == "Narrator" {
		return nil
	}
	book := m.lookupBook(t.bookID)
	if book == nil {
		return nil
	}
	char, err := m.store.GetCharacterByName(store.SeriesScope(book), name)
	if err != nil || char == nil {
		return nil
	}
	key := "characterize:" + char.ID
	if char.Summary != "" || m.characterize == nil {
		return joinOnly(key) // already characterized
	}
	charVal := *char
	return []sched.Need{{Key: key, New: m.depTask(&task{
		kind:        KindSpeakerCharacterization,
		tier:        t.tier,
		bookID:      t.bookID,
		llmKey:      charVal.ID,
		label:       charVal.Name,
		runLLM:      func(ctx context.Context) (int, func(), error) { return 0, nil, m.characterize(ctx, t.bookID, charVal) },
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	})}}
}

// speechDirectionDependency is characterizationDependency's own sibling,
// one level up: t's own chapter's currently queued or in-flight
// KindSpeechDirection (emotion labeling) task, if any, or a lazily-created
// one via m.direct (a no-op, returning no dependency at all, if that was
// never configured - see ChapterDirector's own doc comment) the first time
// it discovers the chapter isn't labeled yet (!Passes.Direction). Any
// clone model: an emotion picks which reference clip a line clones from
// (see variantClipDependency), so a line generated before its chapter is
// labeled would just be invalidated and re-rendered once it is - the
// exact same wasted-render shape characterizationDependency prevents for
// a recharacterization changing what a voice sounds like mid-flight.
func (m *Manager) speechDirectionDependency(t *task) []sched.Need {
	chapterID := t.chapterID
	key := "direction:" + chapterID
	if m.direct == nil {
		return joinOnly(key)
	}
	book := m.lookupBook(t.bookID)
	ch := m.lookupChapter(chapterID)
	if book == nil || ch == nil || ch.Passes.Direction || !book.SpeechDirection {
		return joinOnly(key)
	}
	if needs, ok := m.windowDependency(KindSpeechDirection, m.directWindow, t, book, ch); ok {
		return needs
	}
	return []sched.Need{{Key: key, New: m.depTask(m.newPassTask(KindSpeechDirection, book, ch, t.tier))}}
}

// pronunciationDependency is speechDirectionDependency's sibling for
// pronunciation resolution: t's own chapter's queued or in-flight
// KindPronunciation task, if any, or a lazily-created one via m.pronounce
// the first time it finds the chapter's Passes.Pronunciation unset.
// Gated on the same book.SpeechDirection opt-in ("wait for tagging before
// generating"), but not on the clone model - a pronunciation fix applies
// to every one. Resolving a pronunciation invalidates the chapter's
// already-generated audio (see httpapi.pronounceChapter), so a clone
// dispatched before it finishes would just be deleted again.
func (m *Manager) pronunciationDependency(t *task) []sched.Need {
	chapterID := t.chapterID
	key := "pronunciation:" + chapterID
	if m.pronounce == nil {
		return joinOnly(key)
	}
	book := m.lookupBook(t.bookID)
	ch := m.lookupChapter(chapterID)
	if book == nil || ch == nil || ch.Passes.Pronunciation || !book.SpeechDirection {
		return joinOnly(key)
	}
	if needs, ok := m.windowDependency(KindPronunciation, m.pronounceWindow, t, book, ch); ok {
		return needs
	}
	return []sched.Need{{Key: key, New: m.depTask(m.newPassTask(KindPronunciation, book, ch, t.tier))}}
}

// scareQuoteDependency makes t's own chapter's scare-quote tagging
// (KindScareQuote) a hard dependency of a KindSpeakerAttribution/
// KindDescription task: returns that chapter's currently queued or
// in-flight KindScareQuote task, if any, or lazily creates one via
// m.scareQuote (a no-op if never configured) the first time it finds the
// chapter's Passes.ScareQuote unset - speechDirectionDependency's exact
// shape. Attribution needs scare quotes flagged first so it never assigns
// a character to a quoted span that isn't dialogue (httpapi.
// attributeChapter forces those to Narrator), and description tagging
// reads narration, which a scare quote is part of. A scare-quote run that
// fails leaves Passes.ScareQuote unset, so the next resolution recreates
// it and the dependent task keeps waiting - deliberate: the pass is
// sampled, so a retry is expected to succeed eventually, and running
// attribution against unflagged scare quotes would bake in wrong speakers.
func (m *Manager) scareQuoteDependency(t *task) []sched.Need {
	chapterID := t.chapterID
	key := "scare_quote:" + chapterID
	if m.scareQuote == nil {
		return joinOnly(key)
	}
	book := m.lookupBook(t.bookID)
	ch := m.lookupChapter(chapterID)
	if book == nil || ch == nil || ch.Passes.ScareQuote {
		return joinOnly(key)
	}
	chVal := *ch
	return []sched.Need{{Key: key, New: m.depTask(&task{
		kind:        KindScareQuote,
		tier:        t.tier,
		bookID:      t.bookID,
		chapterID:   chapterID,
		chapterIdx:  t.chapterIdx,
		llmKey:      chapterID,
		runLLM:      func(ctx context.Context) (int, func(), error) { return m.scareQuote(ctx, book, &chVal) },
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	})}}
}

// summaryDependency makes t's own chapter's summary pass (KindSummary) a
// dependency of a KindSpeakerAttribution/KindAmbience task -
// scareQuoteDependency's shape: the chapter's queued or in-flight
// KindSummary task, or a lazily created one via m.summarize the first time
// it finds Passes.Summary unset. Attribution reads the summary's cast
// (who's in the chapter, and what the text calls them), ambience and
// lighting its scenes (where, and what time of day). A failed summary
// leaves Passes.Summary unset and is recreated on the next resolution.
func (m *Manager) summaryDependency(t *task) []sched.Need {
	chapterID := t.chapterID
	key := "summary:" + chapterID
	if m.summarize == nil {
		return joinOnly(key)
	}
	book := m.lookupBook(t.bookID)
	ch := m.lookupChapter(chapterID)
	if book == nil || ch == nil || ch.Passes.Summary {
		return joinOnly(key)
	}
	chVal := *ch
	return []sched.Need{{Key: key, New: m.depTask(&task{
		kind:        KindSummary,
		tier:        t.tier,
		bookID:      t.bookID,
		chapterID:   chapterID,
		chapterIdx:  t.chapterIdx,
		llmKey:      chapterID,
		runLLM:      func(ctx context.Context) (int, func(), error) { return m.summarize(ctx, book, &chVal) },
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	})}}
}

// bookSettingDependency makes t's book's setting pass (KindBookSetting) a
// dependency of the tasks whose prompts read the setting (ambience,
// summary, characterization) while the book has no setting: the book's
// queued or in-flight KindBookSetting task, or a lazily created one. Unlike
// scareQuoteDependency, a failed run doesn't keep blocking them - those
// passes work without a setting, as they did before it existed - so a book
// whose setting pass has run in this process (m.settingTried) is never
// waited on again; a later explicit re-derive (EnqueueBookSetting) still
// can be.
func (m *Manager) bookSettingDependency(t *task) []sched.Need {
	if m.store == nil {
		return nil
	}
	if t.bookID == "" {
		return nil
	}
	key := "book_setting:" + t.bookID
	if m.deriveSetting == nil {
		return joinOnly(key)
	}
	if st, err := m.store.GetBookSetting(t.bookID); err != nil || st != nil {
		return joinOnly(key) // has one - or an error, which isn't worth blocking on
	}
	book := m.lookupBook(t.bookID)
	if book == nil {
		return joinOnly(key)
	}
	// Unless the book requires its setting (store.Book.SettingRequired), the
	// pass is only waited for, as a member: its failure doesn't fail t, and
	// once it has run in this process nothing waits on it again.
	if _, tried := m.settingTried.Load(t.bookID); tried && !book.SettingRequired {
		return joinOnly(key)
	}
	return []sched.Need{{Key: key, Member: !book.SettingRequired, New: m.depTask(&task{
		kind:        KindBookSetting,
		tier:        t.tier,
		bookID:      t.bookID,
		llmKey:      t.bookID,
		label:       "Book setting",
		runLLM:      m.settingRun(book, m.deriveSetting),
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	})}}
}

// settingRun wraps a setting-pass run as a KindBookSetting task's work,
// marking the book tried whatever the outcome (see bookSettingDependency).
func (m *Manager) settingRun(book *store.Book, fn BookSettingDeriver) func(ctx context.Context) (int, func(), error) {
	return func(ctx context.Context) (int, func(), error) {
		err := fn(ctx, book)
		m.settingTried.Store(book.ID, true)
		return 0, nil, err
	}
}

// EnqueueBookSetting queues bookID's setting pass now (the Speakers page's
// re-read button), at TierUrgent like a characterization - one short call.
// fn is the work; deduped per book with any lazily-created one.
func (m *Manager) EnqueueBookSetting(book *store.Book, fn BookSettingDeriver) {
	m.pushTask(&task{
		kind:        KindBookSetting,
		tier:        TierUrgent,
		bookID:      book.ID,
		llmKey:      book.ID,
		label:       "Book setting",
		runLLM:      m.settingRun(book, fn),
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	})
}

// referenceClipDependency returns t's own "ensure reference clip" task -
// this package's third dependency kind, creating one the first time some
// paragraph discovers its own resolved preset has no cached reference
// clip rendered yet (checked directly against disk - os.Stat - the
// "already done" precheck for this dependency kind, no store lookup
// needed), so it's never dispatched into a poolGeneration slot only to
// sit there blocking on a nested, cross-pool call to finish: this used to
// be a real, confirmed-live deadlock for a TierUrgent/TierLookahead outer
// task (nested inside generate() itself, occupying its own poolGeneration
// slot for as long as that blocking call took, with no way for its own
// nested poolDesign child to ever dispatch while poolGeneration stayed
// active - the scheduler runs one context at a time).
// Routing the render through a real, independently-dispatched poolDesign
// task instead - one t merely *depends on*, never blocks inside - means t
// itself is simply left queued (not occupying any slot at all) until that
// dependency clears, the same "never actually put in flight until it can
// really run" treatment characterization/direction dependencies already
// get above. Only ever called for t.kind == KindVoiceClone (see
// Manager.needs) - KindVoiceDesign renders straight from an
// instruct, no reference clip involved at all.
func (m *Manager) referenceClipDependency(t *task) []sched.Need {
	if t.presetID == "" {
		return nil
	}
	if _, err := os.Stat(audiopath.VoicePresetRefFile(m.dataDir, t.presetID)); err == nil {
		return nil // already rendered - the overwhelmingly common case
	}
	label := t.paragraph.Speaker
	if label == "" {
		label = "Narrator"
	}
	nt := m.newReferenceClipTask(t.tier, t.bookID, label, taskBaseClip(t))
	return []sched.Need{{Key: nt.Key(), New: m.depTask(nt)}}
}

// variantClipDependency is referenceClipDependency's own counterpart for
// an emotional line: when t's paragraph has an effective emotion
// (store.Paragraph.EffectiveEmotion - real dialogue only) and its preset's
// variant clip for that emotion hasn't been rendered yet, it returns the
// KindVoiceProvision task rendering it (voicerefs.EnsureVariantFile - a
// BreezeTTS instructed clone of the base clip), creating one keyed by
// (preset, emotion) if nobody's rendering it yet. Variants are provisioned
// lazily this way, one per emotion a voice's lines actually use, rather
// than every emotion for every voice up front.
//
// A render that fails on its final attempt marks the variant failed
// (voicerefs.MarkVariantFailed), which clears this dependency: lines in
// that emotion then clone from the base clip (see generate) instead of
// blocking forever. Skipped for a scare-quote merge group (one call
// renders narration and quote together, so no single emotion applies).
func (m *Manager) variantClipDependency(t *task) []sched.Need {
	if t.presetID == "" || len(t.mergeParagraphs) > 1 {
		return nil
	}
	emotion := t.paragraph.EffectiveEmotion()
	if emotion == emotions.Neutral {
		return nil
	}
	if _, err := os.Stat(audiopath.VoicePresetRefFile(m.dataDir, t.presetID)); err != nil {
		// The base clip comes first (referenceClipDependency); once it
		// ends, t is prepared again and reaches the variant.
		return nil
	}
	if voicerefs.VariantExists(m.dataDir, t.presetID, emotion) || voicerefs.VariantFailed(m.dataDir, t.presetID, emotion) {
		return nil
	}
	label := t.paragraph.Speaker
	if label == "" {
		label = "Narrator"
	}
	nt := m.newVariantTask(t.tier, t.bookID, label, taskBaseClip(t), emotion)
	return []sched.Need{{Key: nt.Key(), New: m.depTask(nt)}}
}

// variantKey is an emotion variant render's llmKey - one per (preset,
// emotion), so the lazy dependency path and an explicit regenerate
// (EnqueueEmotionVariant) join rather than rendering twice.
// taskBaseClip is the base-clip recipe t captured when it was queued -
// only a fallback: see Manager.freshBaseClip.
func taskBaseClip(t *task) voicerefs.BaseClip {
	return voicerefs.BaseClip{
		PresetID:        t.presetID,
		Instruct:        t.instruct,
		Seed:            t.seed,
		RefText:         t.refText,
		SpeedMultiplier: t.speedMultiplier,
		DesignModel:     t.designModel,
	}
}

// freshBaseClip re-reads base's preset when a render actually runs, since
// the casting may have changed since the task that asked for it was queued
// (a recast rewrites the preset's instruct and reference text, and deletes
// its clip, under hundreds of already-queued lines). A built-in preset,
// which isn't in the store, keeps base as is.
func (m *Manager) freshBaseClip(base voicerefs.BaseClip) voicerefs.BaseClip {
	p, err := m.store.GetVoicePreset(base.PresetID)
	if err != nil || p == nil {
		return base
	}
	return voicerefs.BaseClip{PresetID: p.ID, Instruct: p.Instruct, Seed: p.Seed, RefText: p.RefText, SpeedMultiplier: p.SpeedMultiplier, DesignModel: p.DesignModel}
}

// newReferenceClipTask renders a preset's base reference clip if it isn't
// on disk, keyed "generate:<presetID>" - the task referenceClipDependency
// finds and every line cloned from the preset waits on.
func (m *Manager) newReferenceClipTask(tier int, bookID, label string, base voicerefs.BaseClip) *task {
	return &task{
		kind:   KindVoiceProvision,
		tier:   tier,
		bookID: bookID,
		llmKey: "generate:" + base.PresetID,
		label:  label,
		runProvision: func(ctx context.Context, attempt int) (string, error) {
			b := m.freshBaseClip(base)
			// VoiceDesign is fully deterministic on (instruct, seed,
			// refText, language) - see EnqueueVoiceProvision's own doc
			// comment on why a retry (attempt > 0) needs a perturbed seed
			// rather than reproducing a numerically-unstable crash
			// identically every time. Persisted back onto the preset row
			// on success so a later regenerate/edit-save-unchanged picks
			// up the seed that's actually on disk.
			seed := b.Seed + attempt
			path, err := voicerefs.EnsureFile(ctx, m.tts, m.dataDir, b.PresetID, b.Instruct, seed, b.RefText, b.SpeedMultiplier, b.DesignModel)
			if err == nil && attempt > 0 {
				if uerr := m.store.UpdateVoicePresetSeed(b.PresetID, seed); uerr != nil {
					log.Printf("jobs: persist bumped seed for preset %s: %v", b.PresetID, uerr)
				}
			}
			return path, err
		},
	}
}

// EnqueueReferenceClip queues newReferenceClipTask for presetID in the
// background - a recast pre-rendering the voice it just invalidated, under
// the same key the lines waiting on that clip look for, so they wait on
// this render instead of starting their own.
func (m *Manager) EnqueueReferenceClip(bookID, presetID, label string) {
	m.pushTask(m.newReferenceClipTask(TierBackground, bookID, label, voicerefs.BaseClip{PresetID: presetID}))
}

func variantKey(presetID, emotion string) string {
	return "variant:" + presetID + ":" + emotion
}

// newVariantTask builds the KindVoiceProvision task rendering base's
// emotion variant (voicerefs.EnsureVariantFile). A render that fails on
// its final attempt marks the variant failed so its lines fall back to the
// base clip - see variantClipDependency.
func (m *Manager) newVariantTask(tier int, bookID, speaker string, base voicerefs.BaseClip, emotion string) *task {
	return &task{
		kind:   KindVoiceProvision,
		tier:   tier,
		bookID: bookID,
		llmKey: variantKey(base.PresetID, emotion),
		label:  speaker + " (" + emotion + ")",
		runProvision: func(ctx context.Context, attempt int) (string, error) {
			path, err := voicerefs.EnsureVariantFile(ctx, m.tts, m.dataDir, m.freshBaseClip(base), emotion)
			if err != nil && !errors.Is(err, context.Canceled) && attempt+1 >= maxTaskAttempts {
				log.Printf("jobs: %s variant for preset %s failed for good, falling back to the base clip: %v", emotion, base.PresetID, err)
				if merr := voicerefs.MarkVariantFailed(m.dataDir, base.PresetID, emotion); merr != nil {
					log.Printf("jobs: mark variant failed: %v", merr)
				}
			}
			return path, err
		},
	}
}

// EnqueueEmotionVariant renders base's emotion variant now rather than
// waiting for the next line in that emotion to need it - the Speakers
// page's explicit "regenerate variant" action (after
// voicerefs.DeleteVariant), so the reader can hear the new take right
// away. Fire-and-forget at TierUrgent; joins a render already queued for
// the same (preset, emotion).
func (m *Manager) EnqueueEmotionVariant(bookID, speaker string, base voicerefs.BaseClip, emotion string) {
	m.pushTask(m.newVariantTask(TierUrgent, bookID, speaker, base, emotion))
}

// RunReferenceClipAt renders base's reference clip if it isn't on disk
// (newReferenceClipTask) and blocks until that's done - a bulk group
// pre-rendering the voices a book uses. Joins a render already queued for
// the preset, the lazy dependency path's included.
func (m *Manager) RunReferenceClipAt(ctx context.Context, tier int, bookID, label string, base voicerefs.BaseClip) error {
	return m.runProvisionTask(ctx, m.newReferenceClipTask(tier, bookID, label, base))
}

// RunEmotionVariantAt is RunReferenceClipAt for one of base's emotion
// variants (newVariantTask).
func (m *Manager) RunEmotionVariantAt(ctx context.Context, tier int, bookID, speaker string, base voicerefs.BaseClip, emotion string) error {
	return m.runProvisionTask(ctx, m.newVariantTask(tier, bookID, speaker, base, emotion))
}

func (m *Manager) runProvisionTask(ctx context.Context, t *task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	resultCh := make(chan provisionOutcome, 1)
	t.provisionWaiters = []chan provisionOutcome{resultCh}
	t.parent = parentOf(ctx)
	m.pushTask(t)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case out := <-resultCh:
		return out.err
	}
}

// attributionOrderDependency makes chapter t.chapterIdx-1 of the same
// book's own speaker attribution (KindSpeakerAttribution) a hard dependency
// of t's own. httpapi.attributeChapter reads its "already known characters"
// list live from the store at call-start (Store.ListCharacters), so if two
// chapters of the same book ran attribution concurrently, or out of the
// order a reader would actually read them in (exactly what an unpatched
// preprocessAttributionPhase/"Attribute all" does today - each not-yet-
// attributed chapter fires its own call at once, relying only on poolLLM's
// slot admission to gate how many actually run together, never on chapter
// order), a character first introduced in the earlier one might not exist
// in that list yet by the time the later one's own call reads it -
// silently fragmenting one recurring character into two different names
// within the same book, worse than the cross-book series-scope duplicate-
// name problem canonicalizeSpeakerNames already exists to patch up, since
// there's no equivalent patch for a within-book split.
//
// Skipped for a book's own first chapter (chapterIdx 0 - nothing to wait
// on) and once the previous chapter's own store.Passes.Attribution is
// already true (already done, same finished-state check
// characterizationDependency/referenceClipDependency/speechDirectionDependency
// use). Otherwise returns that chapter's own currently queued/in-flight
// KindSpeakerAttribution task if there is one - the common case, since
// bulk preprocessing/"Attribute all" pushes every not-yet-attributed
// chapter's own task before any of them actually dispatches - or lazily
// creates one via m.attribute (a no-op, returning no dependency at all, if
// that was never configured - see ChapterAttributor's own doc comment),
// covering a lone "Attribute speakers" click on a later chapter whose own
// earlier chapter was never separately queued.
func (m *Manager) attributionOrderDependency(t *task) []sched.Need {
	if m.store == nil {
		return nil
	}
	prevIdx := t.chapterIdx - 1
	if prevIdx < 0 {
		return nil
	}
	prevCh, err := m.store.GetChapterByIdx(t.bookID, prevIdx)
	if err != nil || prevCh == nil {
		return nil
	}
	key := "attr:" + prevCh.ID
	if prevCh.Passes.Attribution || m.attribute == nil {
		return joinOnly(key) // already done
	}
	book := m.lookupBook(t.bookID)
	if book == nil {
		return joinOnly(key)
	}
	chVal := *prevCh
	return []sched.Need{{Key: key, New: m.depTask(&task{
		kind:        KindSpeakerAttribution,
		tier:        t.tier,
		bookID:      t.bookID,
		chapterID:   prevCh.ID,
		chapterIdx:  prevIdx,
		llmKey:      prevCh.ID,
		runLLM:      func(ctx context.Context) (int, func(), error) { return m.attribute(ctx, book, &chVal) },
		seriesName:  t.seriesName,
		seriesIndex: t.seriesIndex,
	})}}
}
