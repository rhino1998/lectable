package jobs

import (
	"context"
	"errors"
	"log"
	"os"

	"github.com/rhino1998/lectable/backend/internal/audiopath"
	"github.com/rhino1998/lectable/backend/internal/emotions"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/taskqueue"
	"github.com/rhino1998/lectable/backend/internal/voicerefs"
)

// The memo* helpers below wrap every store/disk lookup the dependency
// resolvers make in taskqueue.LockedQueue.Memo, so each distinct book/
// chapter/character/preset is looked up once per resolve pass instead of
// once per queued task - with hundreds of a book's paragraphs queued,
// that's the difference between a handful of queries and thousands on
// every single Pop (and every Snapshot/HasHigherPriorityWork), all on the
// store's one shared DuckDB connection. Errors are memoized alongside the
// value; every caller already treats a lookup error as "no dependency".

type bookLookup struct {
	book *store.Book
	err  error
}

func (m *Manager) memoBook(lq *taskqueue.LockedQueue, bookID string) (*store.Book, error) {
	r := lq.Memo("book:"+bookID, func() any {
		b, err := m.store.GetBook(bookID)
		return bookLookup{b, err}
	}).(bookLookup)
	return r.book, r.err
}

type chapterLookup struct {
	ch  *store.Chapter
	err error
}

func (m *Manager) memoChapterByID(lq *taskqueue.LockedQueue, chapterID string) (*store.Chapter, error) {
	r := lq.Memo("chapter:"+chapterID, func() any {
		ch, err := m.store.GetChapterByID(chapterID)
		return chapterLookup{ch, err}
	}).(chapterLookup)
	return r.ch, r.err
}

type chapterIdxKey struct {
	bookID string
	idx    int
}

func (m *Manager) memoChapterByIdx(lq *taskqueue.LockedQueue, bookID string, idx int) (*store.Chapter, error) {
	r := lq.Memo(chapterIdxKey{bookID, idx}, func() any {
		ch, err := m.store.GetChapterByIdx(bookID, idx)
		return chapterLookup{ch, err}
	}).(chapterLookup)
	return r.ch, r.err
}

type characterKey struct{ scope, name string }

type characterLookup struct {
	char *store.Character
	err  error
}

func (m *Manager) memoCharacterByName(lq *taskqueue.LockedQueue, scope, name string) (*store.Character, error) {
	r := lq.Memo(characterKey{scope, name}, func() any {
		c, err := m.store.GetCharacterByName(scope, name)
		return characterLookup{c, err}
	}).(characterLookup)
	return r.char, r.err
}

// characterizationTaskByLabel indexes every queued/in-flight
// KindSpeakerCharacterization task by label, built once per resolve pass -
// characterizationDependency matches on speaker name rather than an exact
// Key, so without this each clone task paid a full linear scan of the
// queue. A characterization task pushed later in the same pass isn't in
// the index, but a second lookup for that speaker still finds it: it
// falls through to pushDependency, whose Push dedups on Key and returns
// the existing task.
type characterizationIndex struct{}

func characterizationTaskByLabel(lq *taskqueue.LockedQueue, name string) (taskqueue.Task, bool) {
	idx := lq.Memo(characterizationIndex{}, func() any {
		byLabel := map[string]taskqueue.Task{}
		lq.Find(func(c taskqueue.Task) bool {
			if ct := c.(*task); ct.kind == KindSpeakerCharacterization {
				if _, ok := byLabel[ct.label]; !ok {
					byLabel[ct.label] = c
				}
			}
			return false
		})
		return byLabel
	}).(map[string]taskqueue.Task)
	t, ok := idx[name]
	return t, ok
}

// characterizationDependency returns t's own speaker's currently queued
// or in-flight KindSpeakerCharacterization task, if any, or lazily creates
// one via m.characterize (a no-op, returning no dependency at all, if that
// was never configured - see CharacterCharacterizer's own doc comment) the
// first time it discovers the speaker isn't characterized yet
// (char.Summary == "" - a stored fact, not a task-completion signal) -
// this package's one implementation of the "check before creating" rule
// resolveDependencies' own doc comment describes. name is
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
func (m *Manager) characterizationDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
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
	if found, ok := characterizationTaskByLabel(lq, name); ok {
		return found
	}
	if m.characterize == nil {
		return nil
	}
	book, err := m.memoBook(lq, t.bookID)
	if err != nil || book == nil {
		return nil
	}
	char, err := m.memoCharacterByName(lq, store.SeriesScope(book), name)
	if err != nil || char == nil {
		return nil
	}
	if char.Summary != "" {
		return nil // already characterized
	}
	charVal := *char
	nt := &task{
		kind:        KindSpeakerCharacterization,
		tier:        t.tier,
		bookID:      t.bookID,
		llmKey:      charVal.ID,
		label:       charVal.Name,
		runLLM:      func(ctx context.Context) (int, func(), error) { return 0, nil, m.characterize(ctx, t.bookID, charVal) },
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	}
	return m.pushDependency(lq, nt)
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
func (m *Manager) speechDirectionDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	chapterID := t.chapterID
	// Every KindSpeechDirection task is keyed by its own chapterID (see
	// dedupKey), so an exact-key lookup finds the same task a scan by
	// kind+chapterID would.
	if found, ok := lq.Get("direction:" + chapterID); ok {
		return found
	}
	if m.direct == nil {
		return nil
	}
	book, err := m.memoBook(lq, t.bookID)
	if err != nil || book == nil {
		return nil
	}
	// book.SpeechDirection is this dependency's own explicit opt-in - off
	// by default (see its own doc comment), so a paragraph clones
	// immediately rather than waiting on an LLM tagging pass most books
	// never need. A reader who does want tagged audio turns this on
	// (httpapi.handleUpdateVoice, which also invalidates any audio already
	// generated without that guarantee).
	if !book.SpeechDirection {
		return nil
	}
	ch, err := m.memoChapterByID(lq, chapterID)
	if err != nil || ch == nil {
		return nil
	}
	if ch.Passes.Direction {
		return nil // already tagged
	}
	chVal := *ch
	nt := &task{
		kind:        KindSpeechDirection,
		tier:        t.tier,
		bookID:      t.bookID,
		chapterID:   chapterID,
		chapterIdx:  t.chapterIdx,
		llmKey:      chapterID,
		runLLM:      func(ctx context.Context) (int, func(), error) { return m.direct(ctx, book, &chVal) },
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	}
	return m.pushDependency(lq, nt)
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
func (m *Manager) pronunciationDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	chapterID := t.chapterID
	if found, ok := lq.Get("pronunciation:" + chapterID); ok {
		return found
	}
	if m.pronounce == nil {
		return nil
	}
	book, err := m.memoBook(lq, t.bookID)
	if err != nil || book == nil || !book.SpeechDirection {
		return nil
	}
	ch, err := m.memoChapterByID(lq, chapterID)
	if err != nil || ch == nil {
		return nil
	}
	if ch.Passes.Pronunciation {
		return nil // already resolved
	}
	chVal := *ch
	nt := &task{
		kind:        KindPronunciation,
		tier:        t.tier,
		bookID:      t.bookID,
		chapterID:   chapterID,
		chapterIdx:  t.chapterIdx,
		llmKey:      chapterID,
		runLLM:      func(ctx context.Context) (int, func(), error) { return m.pronounce(ctx, book, &chVal) },
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	}
	return m.pushDependency(lq, nt)
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
func (m *Manager) scareQuoteDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	chapterID := t.chapterID
	if found, ok := lq.Get("scare_quote:" + chapterID); ok {
		return found
	}
	if m.scareQuote == nil {
		return nil
	}
	ch, err := m.memoChapterByID(lq, chapterID)
	if err != nil || ch == nil {
		return nil
	}
	if ch.Passes.ScareQuote {
		return nil // already tagged
	}
	book, err := m.memoBook(lq, t.bookID)
	if err != nil || book == nil {
		return nil
	}
	chVal := *ch
	nt := &task{
		kind:        KindScareQuote,
		tier:        t.tier,
		bookID:      t.bookID,
		chapterID:   chapterID,
		chapterIdx:  t.chapterIdx,
		llmKey:      chapterID,
		runLLM:      func(ctx context.Context) (int, func(), error) { return m.scareQuote(ctx, book, &chVal) },
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	}
	return m.pushDependency(lq, nt)
}

// summaryDependency makes t's own chapter's summary pass (KindSummary) a
// dependency of a KindSpeakerAttribution/KindAmbience task -
// scareQuoteDependency's shape: the chapter's queued or in-flight
// KindSummary task, or a lazily created one via m.summarize the first time
// it finds Passes.Summary unset. Attribution reads the summary's cast
// (who's in the chapter, and what the text calls them), ambience and
// lighting its scenes (where, and what time of day). A failed summary
// leaves Passes.Summary unset and is recreated on the next resolution.
func (m *Manager) summaryDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	chapterID := t.chapterID
	if found, ok := lq.Get("summary:" + chapterID); ok {
		return found
	}
	if m.summarize == nil {
		return nil
	}
	ch, err := m.memoChapterByID(lq, chapterID)
	if err != nil || ch == nil || ch.Passes.Summary {
		return nil
	}
	book, err := m.memoBook(lq, t.bookID)
	if err != nil || book == nil {
		return nil
	}
	chVal := *ch
	nt := &task{
		kind:        KindSummary,
		tier:        t.tier,
		bookID:      t.bookID,
		chapterID:   chapterID,
		chapterIdx:  t.chapterIdx,
		llmKey:      chapterID,
		runLLM:      func(ctx context.Context) (int, func(), error) { return m.summarize(ctx, book, &chVal) },
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	}
	return m.pushDependency(lq, nt)
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
func (m *Manager) bookSettingDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	if t.bookID == "" {
		return nil
	}
	if found, ok := lq.Get("book_setting:" + t.bookID); ok {
		return found
	}
	if m.deriveSetting == nil {
		return nil
	}
	if _, tried := m.settingTried.Load(t.bookID); tried {
		return nil
	}
	has := lq.Memo("setting:"+t.bookID, func() any {
		st, err := m.store.GetBookSetting(t.bookID)
		return err != nil || st != nil // an error: don't block on it
	}).(bool)
	if has {
		return nil
	}
	book, err := m.memoBook(lq, t.bookID)
	if err != nil || book == nil {
		return nil
	}
	nt := &task{
		kind:        KindBookSetting,
		tier:        t.tier,
		bookID:      t.bookID,
		llmKey:      t.bookID,
		label:       "Book setting",
		runLLM:      m.settingRun(book, m.deriveSetting),
		seriesName:  book.SeriesName,
		seriesIndex: book.SeriesIndex,
	}
	return m.pushDependency(lq, nt)
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
// active - see taskqueue.Queue's own "one pool at a time" doc comment).
// Routing the render through a real, independently-dispatched poolDesign
// task instead - one t merely *depends on*, never blocks inside - means t
// itself is simply left queued (not occupying any slot at all) until that
// dependency clears, the same "never actually put in flight until it can
// really run" treatment characterization/direction dependencies already
// get above. Only ever called for t.kind == KindVoiceClone (see
// resolveDependencies) - KindVoiceDesign renders straight from an
// instruct, no reference clip involved at all.
func (m *Manager) referenceClipDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	if t.presetID == "" {
		// A real KindVoiceClone task always has one - pushResolvedTask only
		// ever picks this Kind when rp.voice.PresetID is non-empty (falling
		// back to KindVoiceDesign otherwise) - so this only ever fires for
		// a task built by hand without one (test setups that construct a
		// bare task{kind: KindVoiceClone} literal directly, bypassing the
		// real resolution path entirely), where there's no real preset to
		// check or render anything for.
		return nil
	}
	rendered := lq.Memo("refclip:"+t.presetID, func() any {
		_, err := os.Stat(audiopath.VoicePresetRefFile(m.dataDir, t.presetID))
		return err == nil
	}).(bool)
	if rendered {
		return nil // already rendered - the overwhelmingly common case
	}
	if found, ok := lq.Get("voice_provision:generate:" + t.presetID); ok {
		return found
	}
	// Nobody's rendering it yet - create the task now, carrying everything
	// it needs straight from t's own already-resolved fields (see
	// pushResolvedTask), no httpapi/character context required.
	label := t.paragraph.Speaker
	if label == "" {
		label = "Narrator"
	}
	nt := &task{
		kind:   KindVoiceProvision,
		tier:   t.tier,
		bookID: t.bookID,
		llmKey: "generate:" + t.presetID,
		label:  label,
		runProvision: func(ctx context.Context, attempt int) (string, error) {
			// VoiceDesign is fully deterministic on (instruct, seed,
			// refText, language) - see EnqueueVoiceProvision's own doc
			// comment on why a retry (attempt > 0) needs a perturbed seed
			// rather than reproducing a numerically-unstable crash
			// identically every time. Persisted back onto the preset row
			// on success so a later regenerate/edit-save-unchanged picks
			// up the seed that's actually on disk instead of reverting to
			// the original, crash-triggering one.
			seed := t.seed + attempt
			path, err := voicerefs.EnsureFile(ctx, m.tts, m.dataDir, t.presetID, t.instruct, seed, t.refText, t.speedMultiplier, t.designModel)
			if err == nil && attempt > 0 {
				if uerr := m.store.UpdateVoicePresetSeed(t.presetID, seed); uerr != nil {
					log.Printf("jobs: persist bumped seed for preset %s: %v", t.presetID, uerr)
				}
			}
			return path, err
		},
	}
	return m.pushDependency(lq, nt)
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
func (m *Manager) variantClipDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	if t.presetID == "" || len(t.mergeParagraphs) > 1 {
		return nil
	}
	emotion := t.paragraph.EffectiveEmotion()
	if emotion == emotions.Neutral {
		return nil
	}
	settled := lq.Memo("variantclip:"+t.presetID+":"+emotion, func() any {
		return voicerefs.VariantExists(m.dataDir, t.presetID, emotion) || voicerefs.VariantFailed(m.dataDir, t.presetID, emotion)
	}).(bool)
	if settled {
		return nil
	}
	if found, ok := lq.Get("voice_provision:" + variantKey(t.presetID, emotion)); ok {
		return found
	}
	label := t.paragraph.Speaker
	if label == "" {
		label = "Narrator"
	}
	base := voicerefs.BaseClip{
		PresetID:        t.presetID,
		Instruct:        t.instruct,
		Seed:            t.seed,
		RefText:         t.refText,
		SpeedMultiplier: t.speedMultiplier,
		DesignModel:     t.designModel,
	}
	return m.pushDependency(lq, m.newVariantTask(t.tier, t.bookID, label, base, emotion))
}

// variantKey is an emotion variant render's llmKey - one per (preset,
// emotion), so the lazy dependency path and an explicit regenerate
// (EnqueueEmotionVariant) join rather than rendering twice.
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
			path, err := voicerefs.EnsureVariantFile(ctx, m.tts, m.dataDir, base, emotion)
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
func (m *Manager) attributionOrderDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	prevIdx := t.chapterIdx - 1
	if prevIdx < 0 {
		return nil
	}
	prevCh, err := m.memoChapterByIdx(lq, t.bookID, prevIdx)
	if err != nil || prevCh == nil {
		return nil
	}
	if found, ok := lq.Get("attr:" + prevCh.ID); ok {
		return found
	}
	if prevCh.Passes.Attribution {
		return nil // already done
	}
	if m.attribute == nil {
		return nil
	}
	book, err := m.memoBook(lq, t.bookID)
	if err != nil || book == nil {
		return nil
	}
	chVal := *prevCh
	nt := &task{
		kind:        KindSpeakerAttribution,
		tier:        t.tier,
		bookID:      t.bookID,
		chapterID:   prevCh.ID,
		chapterIdx:  prevIdx,
		llmKey:      prevCh.ID,
		runLLM:      func(ctx context.Context) (int, func(), error) { return m.attribute(ctx, book, &chVal) },
		seriesName:  t.seriesName,
		seriesIndex: t.seriesIndex,
	}
	return m.pushDependency(lq, nt)
}

// globalAttributionSlotDependency makes t (a KindSpeakerAttribution task)
// depend on any OTHER currently in-flight KindSpeakerAttribution task,
// regardless of book - attributionOrderDependency's own sibling, but
// narrower on purpose: that one only orders a book's own chapters against
// each other, so two DIFFERENT books' attribution tasks (no shared
// attributionOrderDependency chain between them at all) could otherwise
// still dispatch concurrently, each claiming one of poolLLM's own
// maxAttributionInFlight slots. This caps attribution specifically to a
// single in-flight task at a time, deliberately independent of poolLLM's
// own overall capacity - maxAttributionInFlight can still be >1 so
// characterization/direction/music-scoring keep genuine concurrency with
// each other (and with the one active attribution task), rather than
// tying that down to attribution's own narrower constraint.
//
// Deliberately FindInFlight, not Find: two attribution tasks merely queued
// alongside each other (neither dispatched yet - e.g. attributionOrderDependency's
// own lazily-created chapter-0 task sitting next to an already-queued
// chapter-1 task for the same book) would otherwise each find the other as
// a "match" under Find and mutually deadlock, since neither has actually
// started yet. Restricting this to in-flight tasks only breaks that
// symmetry - see FindInFlight's own doc comment - letting the queue's own
// ordering (plus attributionOrderDependency's explicit same-book chain)
// pick one to dispatch first; every other still-queued attribution task
// then finds that one in flight and correctly waits.
//
// Never lazily creates a task the way attributionOrderDependency does
// (there's nothing to conjure here - either some other attribution task is
// already in flight, or there isn't), so this is a pure lq.FindInFlight, no
// pushDependency call.
func (m *Manager) globalAttributionSlotDependency(lq *taskqueue.LockedQueue, t *task) taskqueue.Task {
	found, ok := lq.FindInFlight(func(c taskqueue.Task) bool {
		other, ok := c.(*task)
		return ok && other.kind == KindSpeakerAttribution && other.Key() != t.Key()
	})
	if !ok {
		return nil
	}
	return found
}
