package jobs

import (
	"context"

	"github.com/rhino1998/lectable/backend/internal/store"
)

// AttributionFunc performs one chapter's speaker attribution and reports
// how many paragraphs were (re)attributed - the actual work behind a
// KindSpeakerAttribution task. Supplied by the caller at EnqueueAttribution
// time rather than implemented in this package, since running it needs
// Store/Speaker access this package doesn't have (see
// internal/speakerattr, httpapi.Server.attributeChapter). ctx is always
// the scheduler's own long-lived context (see worker/startTask), not
// tied to any particular HTTP request - the whole point of
// EnqueueAttribution being fire-and-forget is that this keeps running
// regardless of whether the request that triggered it is still around.
//
// requeue is non-nil when this implementation checked HasHigherPriorityWork
// between batches, found some, and stopped before processing every
// paragraph it was given - calling it later (see taskResult.requeue's own
// doc comment on exactly when) queues a follow-up task to pick up whatever
// was left, effectively pausing this (TierBackground) run behind whatever
// more urgent work just showed up rather than making it wait out however
// many batches remain. nil means every paragraph was actually
// processed (successfully or not) - see httpapi.attributeChapter, the one
// implementation of this type, for how it decides.
type AttributionFunc func(ctx context.Context) (attributed int, requeue func(), err error)

// ReattributionFunc performs one chapter's "Auto Split" pass for a single
// speaker and reports how many of that speaker's own paragraphs actually
// got reassigned - the actual work behind a KindSpeakerReattribution task.
// Supplied by the caller at EnqueueReattribution time (see
// httpapi.reattributeChapterSpeaker), since this package has no Store/
// Speaker access of its own. Shares AttributionFunc's exact shape
// (requeue non-nil only when the implementation paused - see
// speakerattr.Client.AttributeChapter's own shouldPause - before judging
// every one of that speaker's paragraphs in this chapter) but is its own
// type, the same way DirectionFunc is kept distinct from AttributionFunc
// despite an identical shape, so the two stay independently documented and
// free to diverge later.
type ReattributionFunc func(ctx context.Context) (reattributed int, requeue func(), err error)

// DirectionFunc performs one chapter's speech-direction tagging and
// reports how many paragraphs were tagged - the actual work behind a
// KindSpeechDirection task. Supplied by the caller at EnqueueDirection
// time (see httpapi.directChapter) for the same reason AttributionFunc is:
// running it needs Store/Speaker access this package doesn't have. Shares
// AttributionFunc's exact shape (requeue non-nil only if the
// implementation paused before finishing) even though httpapi's current
// implementation never actually pauses mid-chapter yet (always nil
// requeue) - kept as a real function type of its own, not a reuse of
// AttributionFunc, so the two stay independently documented and free to
// diverge later.
type DirectionFunc func(ctx context.Context) (tagged int, requeue func(), err error)

// AmbienceFunc performs one chapter's background-music tone-region
// scoring and reports how many regions were produced - the actual work
// behind a KindAmbience task. Supplied by the caller at
// EnqueueAmbience time (see httpapi.ambienceChapter), for the same
// reason AttributionFunc/DirectionFunc are - running it needs Store/
// Speaker access this package doesn't have. DirectionFunc's exact shape.
type AmbienceFunc func(ctx context.Context) (scored int, requeue func(), err error)

// ScareQuoteFunc performs one chapter's scare-quote tagging and reports how
// many paragraphs' flags changed - the actual work behind a KindScareQuote
// task (see httpapi.scareQuoteChapterForJob). DirectionFunc's exact shape.
type ScareQuoteFunc func(ctx context.Context) (changed int, requeue func(), err error)

// PronunciationFunc performs one chapter's pronunciation resolution - the
// actual work behind a KindPronunciation task (see
// httpapi.pronounceChapter). DirectionFunc's exact shape.
type PronunciationFunc func(ctx context.Context) (resolved int, requeue func(), err error)

// CharacterizationFunc performs one character's re-characterization (an
// LLM call - speakerattr.Client.CharacterizeVoice - plus persisting the
// result) - the actual work behind a KindSpeakerCharacterization task.
// Supplied by the caller at RunCharacterization time, since this package
// has no Store/Speaker access of its own; unlike AttributionFunc,
// RunCharacterization still blocks its caller on the real result (a single
// character's characterization is one short LLM call, not a whole
// chapter's worth of batches, so holding a request open for it is fine -
// see EnqueueAttribution's own doc comment for why attribution itself
// stopped doing this).
type CharacterizationFunc func(ctx context.Context) error

// llmOutcome is an LLM-kind task's (KindSpeakerAttribution or
// KindSpeakerCharacterization) real result, delivered to every entry in
// task.llmWaiters by handleResult so each RunCharacterization caller can
// block on it - unlike KindVoiceClone/KindVoiceDesign tasks, which are
// dispatched fire-and-forget by the Enqueue* methods and whose outcome
// only ever needs to reach the store, not a waiting caller.
// KindSpeakerAttribution tasks are fire-and-forget too now (see
// EnqueueAttribution) and so never populate llmWaiters, but still report
// attributed through this same struct on their way to handleResult's
// shared LLM-kind branch. attributed is always 0 for characterization.
type llmOutcome struct {
	attributed int
	err        error
}

// CharacterVoiceProvisioner lazily ensures char has an assigned voice for
// cloneModel - characterizing them first if they aren't yet, then creating
// and assigning a freshly-cloned preset - the first time their voice is
// actually needed for generation (see provisionMissingCharacterVoices,
// called from paragraphsNeedingGeneration/enqueueParagraphRegenerate)
// rather than eagerly the moment attribution discovers them. Supplied once
// at startup (see SetCharacterVoiceProvisioner), since it needs Store/
// Speaker/TTS access this package doesn't have (see
// httpapi.Server.provisionCharacterVoice, which also serializes concurrent
// calls for the same character - this package's own per-characterID
// dedup on KindSpeakerCharacterization tasks, alone, only covers the LLM
// characterization step, not the preset-creation step after it). A never-
// configured provisioner (nil) just means every character-scoped
// generation keeps falling back to the book's own voice until one is
// assigned some other way (attribution's old auto-assign behavior, or a
// reader assigning one manually) - the same as before this existed.
type CharacterVoiceProvisioner func(ctx context.Context, bookID string, char store.Character, cloneModel string) (presetID string, err error)

// CharacterCharacterizer lazily runs one character's own characterization
// (the LLM step behind CharacterizationFunc/RunCharacterization, persisting
// char.Summary/RefLine - see store.Character's own doc comment) the moment
// characterizationDependency discovers a KindVoiceClone/KindVoiceDesign/
// KindVoiceProvision task depends on a speaker who isn't characterized yet
// (char.Summary == "", the same "stored fact, not a task-completion signal"
// finished-state check referenceClipDependency already uses for reference
// clips - see its own doc comment) and finds no characterization task
// already queued or in flight for them either. Supplied once at startup
// (see SetCharacterCharacterizer), since it needs Store/Speaker access this
// package doesn't have - httpapi.Server wires in a wrapper around the same
// characterizeVoice/recharacterizeAndInvalidate pair RunCharacterization's
// own explicit callers use. A never-configured characterizer (nil) just
// means a clone/design/provision task for an uncharacterized speaker is
// never auto-blocked on one materializing - the same as before this
// existed, when only an explicit "Characterize"/"Regenerate" click or
// provisionMissingCharacterVoices' own lazy provisioning path (a different,
// blocking mechanism - see CharacterVoiceProvisioner's own doc comment)
// ever characterized anyone.
//
// A character with no attributed quotes gathered yet
// (characterizeVoice's own "wait for more material" no-op, returning ("",
// nil) rather than an error) leaves char.Summary empty even after this
// runs successfully - characterizationDependency has no way to distinguish
// that from "never attempted," so a speaker stuck in that state keeps
// getting a fresh (fast, no-op) characterization task recreated every time
// something next depends on them, until enough material actually exists.
// Accepted as the same tradeoff CharacterVoiceProvisioner's own lazy
// provisioning already makes rather than tracking a separate "attempted
// and gave up" marker for what should be a self-resolving, rare, and cheap
// case in practice.
type CharacterCharacterizer func(ctx context.Context, bookID string, char store.Character) error

// ChapterDirector lazily runs one chapter's own speech-direction tagging
// (the work behind DirectionFunc/EnqueueDirection, persisting per-paragraph
// tags and - on a full, uninterrupted pass - setting Passes.Direction) the
// moment speechDirectionDependency discovers a KindVoiceClone/
// KindVoiceDesign task depends on a chapter that isn't tagged yet
// (!ch.Passes.Direction, the same stored-fact finished-state check
// CharacterCharacterizer/referenceClipDependency use) and finds no
// direction task already queued or in flight for it either.
// Supplied once at startup (see SetChapterDirector), since it needs Store/
// Speaker access this package doesn't have - httpapi.Server wires in a
// wrapper around the same directChapter explicit "Tag directions" clicks
// already use, with onlyIdx always nil (a fresh, full-chapter run - see
// directChapter's own doc comment for what a non-nil onlyIdx means, never
// applicable here since this is never itself a paused continuation).
// directChapter's own pause/resume handling (its own requeue closure,
// re-invoking EnqueueDirection directly) is unaffected by being reached
// this way instead of through an explicit click - it doesn't know or care
// who queued the task it's running as. A never-configured director (nil)
// just means a clone/design task for an untagged chapter is never
// auto-blocked on one materializing - the same as before this existed.
type ChapterDirector func(ctx context.Context, book *store.Book, ch *store.Chapter) (tagged int, requeue func(), err error)

// ChapterAttributor lazily runs one chapter's own speaker attribution (the
// work behind AttributionFunc/EnqueueAttribution, persisting per-paragraph
// speakers and - on a full, uninterrupted pass - setting Passes.Attribution)
// the moment attributionOrderDependency discovers a later chapter's own
// KindSpeakerAttribution task depends on an earlier one in the same book
// that isn't attributed yet (!ch.Passes.Attribution, the same stored-fact
// finished-state check CharacterCharacterizer/ChapterDirector use) and finds
// no attribution task already queued or in flight for it either. Supplied
// once at startup (see SetChapterAttributor), since it needs Store/Speaker
// access this package doesn't have - httpapi.Server wires in a wrapper
// around the same attributeChapter explicit "Attribute speakers" clicks
// already use, with onlyUnattributed always false (a fresh, full-chapter
// run, never itself a paused continuation this way). attributeChapter's own
// pause/resume handling (its own requeue closure, re-invoking
// EnqueueAttribution directly) is unaffected by being reached this way
// instead of through an explicit click - it doesn't know or care who queued
// the task it's running as. A never-configured attributor (nil) just means
// a later chapter's attribution is never auto-blocked on an earlier one
// materializing - the same as before this existed.
type ChapterAttributor func(ctx context.Context, book *store.Book, ch *store.Chapter) (attributed int, requeue func(), err error)

// AmbienceRunner lazily runs one chapter's own background-music tone-region
// scoring (the work behind AmbienceFunc/EnqueueAmbience, persisting
// new store.MusicRegion rows and - via MaybeAdvanceChapterMusic, called
// from the same real implementation this wraps - dispatching whatever
// region generation that scoring just made eligible) the moment
// maybeRunChapterAmbience discovers a chapter about to have its own
// narration audio generated (enqueueChapter, i.e. every "generate this
// chapter's voice" entry point - EnqueueChapter/EnqueueBookGenerate/
// EnqueueRemaining) hasn't finished its ambience pass yet
// (!ch.Passes.AmbienceDone(), the same
// stored-fact finished-state check CharacterCharacterizer/ChapterDirector/
// ChapterAttributor use) for a book with store.Book.MusicEnabled on.
// Supplied once at startup (see SetAmbienceRunner), since it needs Store/
// Speaker access this package doesn't have - httpapi.Server wires in the
// same ambienceChapter explicit "Score music" clicks already use.
// Unlike ChapterDirector/ChapterAttributor, this has no pause/resume
// continuation of its own to worry about being reached a different way -
// ambienceChapter's own requeue closure (same as directChapter/
// attributeChapter's) re-invokes EnqueueAmbience directly regardless
// of who queued the task it's running as. A never-configured scorer (nil)
// just means generating a chapter's voice never auto-triggers scoring for
// it - the same as before this existed, when a reader had to visit the
// Speakers page and score it (or wait for a bulk "Score all music" run)
// separately from generating its audio.
type AmbienceRunner func(ctx context.Context, book *store.Book, ch *store.Chapter) (scored int, requeue func(), err error)

// ChapterScareQuoter lazily runs one chapter's own scare-quote tagging (the
// work behind ScareQuoteFunc/EnqueueScareQuote, setting Passes.ScareQuote
// on a full pass) the moment scareQuoteDependency discovers a
// KindSpeakerAttribution task for a chapter that isn't
// tagged yet and finds no scare-quote task already queued or in flight for
// it - ChapterDirector's exact role for speechDirectionDependency. A
// never-configured quoter (nil) just means attribution never waits on
// scare-quote tagging.
type ChapterScareQuoter func(ctx context.Context, book *store.Book, ch *store.Chapter) (changed int, requeue func(), err error)

// ChapterPronouncer lazily runs one chapter's own pronunciation resolution
// (the work behind PronunciationFunc/EnqueuePronunciation, setting
// Passes.Pronunciation on a full pass) the moment pronunciationDependency
// discovers a KindVoiceClone/KindVoiceDesign task for a chapter that isn't
// resolved yet - ChapterDirector's exact role for
// speechDirectionDependency. A never-configured pronouncer (nil) just
// means generation never waits on pronunciation resolution.
// ChapterSummarizer lazily runs one chapter's summary pass (setting
// Passes.Summary) the moment summaryDependency finds a KindSpeakerAttribution
// or KindAmbience task for a chapter that has none yet - ChapterScareQuoter's
// role for scareQuoteDependency. Never configured (nil) means nothing waits
// on summaries.
type ChapterSummarizer func(ctx context.Context, book *store.Book, ch *store.Chapter) (int, func(), error)

// BookSettingDeriver runs one book's setting pass (setting its stored
// setting profile) - the work behind a KindBookSetting task that
// bookSettingDependency creates. Never configured (nil) means nothing waits
// on a setting.
type BookSettingDeriver func(ctx context.Context, book *store.Book) error

// SummaryFunc performs one chapter's summary pass - the work behind a
// KindSummary task. DirectionFunc's shape.
type SummaryFunc func(ctx context.Context) (int, func(), error)

// EmbeddingFunc embeds one chapter's paragraphs - the work behind a
// KindEmbedding task - and reports how many it embedded. SummaryFunc's
// shape; it never pauses, so its requeue is always nil.
type EmbeddingFunc func(ctx context.Context) (int, func(), error)

type ChapterPronouncer func(ctx context.Context, book *store.Book, ch *store.Chapter) (resolved int, requeue func(), err error)

// SetCharacterVoiceProvisioner wires fn in as the lazy voice provisioner -
// see CharacterVoiceProvisioner's own doc comment. Called once from
// cmd/server/main.go after both this Manager and httpapi.Server exist
// (the provisioner needs the latter's Store/Speaker/TTS access), before
// the HTTP server starts accepting requests.
func (m *Manager) SetCharacterVoiceProvisioner(fn CharacterVoiceProvisioner) {
	m.provision = fn
}

// SetCharacterCharacterizer wires fn in as the lazy characterizer - see
// CharacterCharacterizer's own doc comment. Called once from
// httpapi.NewRouter, the same place/timing as SetCharacterVoiceProvisioner.
func (m *Manager) SetCharacterCharacterizer(fn CharacterCharacterizer) {
	m.characterize = fn
}

// SetChapterDirector wires fn in as the lazy director - see
// ChapterDirector's own doc comment. Called once from httpapi.NewRouter,
// the same place/timing as SetCharacterVoiceProvisioner.
func (m *Manager) SetChapterDirector(fn ChapterDirector) {
	m.direct = fn
}

// SetChapterAttributor wires fn in as the lazy attributor - see
// ChapterAttributor's own doc comment. Called once from httpapi.NewRouter,
// the same place/timing as SetCharacterVoiceProvisioner.
func (m *Manager) SetChapterAttributor(fn ChapterAttributor) {
	m.attribute = fn
}

// SetAmbienceRunner wires fn in as the lazy scorer - see AmbienceRunner's own
// doc comment. Called once from httpapi.NewRouter, the same place/timing
// as SetCharacterVoiceProvisioner.
func (m *Manager) SetAmbienceRunner(fn AmbienceRunner) {
	m.runAmbience = fn
}

// SetChapterScareQuoter wires fn in as the lazy scare-quote tagger - see
// ChapterScareQuoter's own doc comment. Called once from
// httpapi.NewRouter, the same place/timing as SetChapterDirector.
func (m *Manager) SetChapterScareQuoter(fn ChapterScareQuoter) {
	m.scareQuote = fn
}

// SetBookSettingDeriver wires fn in as the lazy setting pass - see
// BookSettingDeriver. Called once from httpapi.NewRouter.
func (m *Manager) SetBookSettingDeriver(fn BookSettingDeriver) {
	m.deriveSetting = fn
}

// SetChapterSummarizer wires fn in as the lazy summarizer - see
// ChapterSummarizer. Called once from httpapi.NewRouter.
func (m *Manager) SetChapterSummarizer(fn ChapterSummarizer) {
	m.summarize = fn
}

// SetChapterPronouncer wires fn in as the lazy pronunciation resolver - see
// ChapterPronouncer's own doc comment. Called once from httpapi.NewRouter,
// the same place/timing as SetChapterDirector.
func (m *Manager) SetChapterPronouncer(fn ChapterPronouncer) {
	m.pronounce = fn
}

// ChapterWindowPass runs a chapter pass (emotion labeling, pronunciation)
// over only the paragraphs whose idx is in only. partial marks a window
// run ahead of the rest of the chapter: it must not mark the chapter's
// pass done, and never requeues. A non-partial run over only is the rest
// of a chapter whose windows are already done, and does mark it.
type ChapterWindowPass func(ctx context.Context, book *store.Book, ch *store.Chapter, only map[int]bool, partial bool) (int, func(), error)

// SetChapterWindowDirector and SetChapterWindowPronouncer wire in the
// windowed forms of the emotion and pronunciation passes - see
// Manager.windowDependency. Without them those passes always run whole.
func (m *Manager) SetChapterWindowDirector(fn ChapterWindowPass)   { m.directWindow = fn }
func (m *Manager) SetChapterWindowPronouncer(fn ChapterWindowPass) { m.pronounceWindow = fn }
