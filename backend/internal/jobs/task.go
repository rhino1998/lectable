package jobs

import (
	"cmp"
	"context"
	"time"

	"github.com/rhino1998/lectable/backend/internal/pronounce"
	"github.com/rhino1998/lectable/backend/internal/store"
	"github.com/rhino1998/lectable/backend/internal/ttsproto"
)

// Kind identifies what sort of work a task represents. The job system
// generalizes over several kinds of work rather than hardcoding "TTS
// generation" as the only thing this package's heap/worker dispatches -
// every Kind is queued through the same priority heap and dispatched into
// one of the worker's slots (see task.kind, QueueTask.Kind, poolFor,
// popTask/startTask/handleResult's own per-kind branches). Adding a new
// kind of job means extending those switches (and picking a taskPool for
// it), not inventing a parallel ad hoc tracking mechanism the way
// attribution's own tracking used to be a wholly separate, hardcoded
// map/struct pair before this type existed.
type Kind string

const (
	// KindVoiceClone is a paragraph rendered by cloning a preset's
	// reference clip (the common case - see Manager.generate).
	KindVoiceClone Kind = "voice_clone"
	// KindVoiceDesign is a paragraph rendered straight from a fully
	// custom instruct via VoiceDesign, with no reference clip/preset
	// backing it. poolDesign, not poolGeneration, despite being ordinary
	// paragraph audio like KindVoiceClone - see poolDesign's own doc
	// comment for why every VoiceDesign-model call shares that one slot
	// regardless of which Kind triggers it.
	KindVoiceDesign Kind = "voice_design"
	// KindSpeakerAttribution is an LLM speaker-attribution run for one
	// chapter (internal/speakerattr) - see EnqueueAttribution, the entry
	// point httpapi calls. Needs a caller-supplied AttributionFunc (this
	// package has no Store/Speaker access of its own), but is otherwise
	// fire-and-forget exactly like KindVoiceClone/KindVoiceDesign's own
	// Enqueue* methods - an earlier version blocked the caller until the
	// task finished (see EnqueueAttribution's own doc comment for why
	// that changed).
	KindSpeakerAttribution Kind = "speaker_attribution"
	// KindSpeakerCharacterization is an LLM voice-characterization run
	// for one character (speakerattr.Client.CharacterizeVoice) - see
	// RunCharacterization. Split out from KindSpeakerAttribution as its
	// own kind (rather than running inline as part of an attribution
	// task, the way auto-discovery's characterization step used to)
	// specifically so a reader can trigger/retry it for one character on
	// its own, visibly, through the same queue - see
	// httpapi.handleCharacterizeSpeaker.
	KindSpeakerCharacterization Kind = "speaker_characterization"
	// KindSpeechDirection is an LLM speech-direction-tagging run for one
	// chapter (internal/speakerattr.Client.DirectChapter) - see
	// EnqueueDirection, the entry point httpapi calls. Shares
	// KindSpeakerAttribution's exact shape: fire-and-forget, poolLLM,
	// requires a caller-supplied DirectionFunc since this package has no
	// Store/Speaker access of its own. A separate Kind (not folded into
	// KindSpeakerAttribution itself) so it shows up distinctly on the Jobs
	// dashboard and can be triggered/tracked independently of attribution -
	// unlike Describe, this isn't chained automatically after a successful
	// attribution run (see httpapi.directChapter's own doc comment for why).
	KindSpeechDirection Kind = "speech_direction"
	// KindSpeakerReattribution is an "Auto Split" run for one chapter
	// (httpapi.reattributeChapterSpeaker, reusing
	// speakerattr.Client.AttributeChapter itself rather than a second
	// prompt): re-judges every paragraph currently credited to one
	// specific speaker - a character believed not to be a real, distinct
	// individual, or literally "Unknown" (see EnqueueReattribution) - and
	// persists only the paragraphs that actually belonged to that speaker,
	// so a false-positive character's lines get redistributed to whichever
	// real character/Narrator/Unknown they actually belong to instead of
	// staying stuck under a name that should never have existed. Shares
	// KindSpeakerAttribution's exact shape (fire-and-forget, poolLLM,
	// caller-supplied ReattributionFunc, cooperative pause/requeue between
	// batches) but is its own Kind, both so it's visible/cancelable
	// distinctly on the Jobs dashboard and so its dedup key can be scoped
	// per (chapter, speaker) rather than per chapter alone (see
	// EnqueueReattribution's own doc comment).
	KindSpeakerReattribution Kind = "speaker-reattribute"
	// KindVoiceDesignPreview is a one-off VoiceDesign render with no
	// backing paragraph - the voice editor's "Test" button (see
	// RunVoiceDesignPreview, httpapi.handleTestVoiceDesign). Shares
	// KindVoiceDesign's underlying ttsworker call and poolDesign slot
	// pool/concurrency limit (see poolFor), but is its own Kind rather
	// than reusing KindVoiceDesign directly: a real KindVoiceDesign task is
	// always paragraph-scoped (chapterIdx/paragraphIdx meaningful, gets
	// persisted to the store on completion - see
	// Manager.handleResult), none of which applies to a preview that was
	// never asked to be saved anywhere. Blocks its caller like
	// RunCharacterization, not fire-and-forget like KindVoiceClone/
	// KindVoiceDesign's own Enqueue* methods - see RunVoiceDesignPreview's
	// own doc comment for why blocking is the right shape here.
	KindVoiceDesignPreview Kind = "voice_design_preview"
	// KindVoiceProvision is one character's voice provisioning or forced
	// regeneration (see EnqueueVoiceProvision, httpapi.
	// handleGenerateCharacterVoices/handleRegenerateCharacterVoices) -
	// creating (or, for regeneration, force-rerendering) a character's
	// voice preset and its reference clip. Shares KindVoiceDesignPreview's
	// general shape: no backing paragraph, fire-and-forget via
	// EnqueueVoiceProvision rather than blocking like RunCharacterization/
	// RunVoiceDesignPreview, since SpeakersPage's "Generate/Regenerate all
	// voices" buttons queue a whole roster's worth at once and don't want
	// to hold a request open per character - but poolDesign (shared with
	// KindVoiceDesign/KindVoiceDesignPreview - see its own doc comment for
	// why all three share one slot - maxDesignInFlight), not poolGeneration,
	// specifically so a whole roster's worth (or runBookPipeline's own
	// concurrent per-phase burst) queued at once can't claim every one of
	// poolGeneration's own maxInFlight slots and starve actual paragraph
	// audio a reader is waiting to hear. Still not
	// poolLLM, even though the underlying work (httpapi.
	// provisionCharacterVoice/regenerateCharacterVoice) can itself enqueue
	// a KindSpeakerCharacterization (poolLLM) task internally when a
	// character isn't characterized yet - queuing this as a second poolLLM
	// task instead would have it permanently occupy poolLLM's own slot(s)
	// waiting for its own inner call to get one, deadlocking (see
	// handleGenerateCharacterVoices' own doc comment, written before this
	// kind existed, for the fuller version of that reasoning). Using a TTS-
	// family pool also means this correctly participates in the worker's
	// own TTS/poolLLM mutual-exclusion (see worker's own fill)
	// instead of bypassing it the way a bare goroutine calling the TTS
	// worker directly would - the whole reason this kind exists rather than
	// staying a detached goroutine.
	KindVoiceProvision Kind = "voice_provision"
	// KindLengthEstimate is EnqueueLengthEstimate's own one-off, throwaway
	// PocketTTS-cloned sample (see task.skipDependencies/task.estimateChars
	// and Manager.estimateLength) - shares KindVoiceClone's exact
	// generate() path (same clone-a-reference-clip call) and poolGeneration
	// slot pool, since it's the same kind of real GPU/model work and
	// should count against that same concurrency budget, but has no
	// backing paragraph at all (see startGenerationTask/handleResult's own
	// early branches for this kind) and always sets skipDependencies -
	// none of characterizationDependency/speechDirectionDependency/
	// referenceClipDependency apply to a self-contained sample with its
	// own already-resolved preset and no chapter pipeline state to wait
	// on.
	KindLengthEstimate Kind = "length_estimate"
	// KindSFXGeneration renders and persists one paragraph's own
	// Stable Audio SFX sound-effect clip (see EnqueueSFXGeneration,
	// httpapi.handleGenerateParagraphSFX) - poolSFX, fire-and-forget like
	// KindVoiceProvision's own explicit callers (the closure httpapi
	// supplies does its own Store/audiopath persistence;
	// this package has none of that access itself), dedup-keyed by
	// paragraph.ID like an ordinary KindVoiceClone task (the default
	// dedupKey case) since a paragraph can only usefully have one SFX
	// render in flight at a time.
	KindSFXGeneration Kind = "sfx_generation"
	// KindSFXPreview is a one-off render with no backing paragraph, from
	// any of this app's generation-only "gen task" engines (ACE-Step,
	// Stable Audio - Music/SFX/Medium) - the standalone
	// POST /api/sfx/generate test surface (see RunSFXPreview,
	// httpapi.handleGenerateSFX). One shared kind across every engine, not
	// one per engine: KindVoiceDesignPreview's own exact shape (reusing
	// task.runPreview/previewWaiters verbatim - both are just "run a
	// caller-supplied closure through a pool slot, return audio bytes"),
	// and which engine actually ran is just whatever the caller's own
	// closure happened to call - this package never needs to know.
	KindSFXPreview Kind = "sfx_preview"
	// KindLLMPreview is a one-off raw system+user prompt call against this
	// app's own embedded speaker-attribution GGUF model, with no backing
	// paragraph/chapter - the standalone POST /api/llm/test surface (see
	// RunLLMPreview, httpapi.handleTestLLM). poolLLM, not a dedicated pool:
	// llmworker already serves concurrent calls natively via multi-sequence
	// batching (see backend/CLAUDE.md's "ttsworker / audioworker /
	// llmworker" section), so this mainly buys Jobs-dashboard visibility
	// and TierUrgent priority over queued background attribution/
	// characterization/direction-tagging work, not GPU-residency mutual
	// exclusion the way poolSFX exists for. KindSFXPreview's own exact
	// shape, reusing runPreviewText/previewTextWaiters instead of
	// runPreview/previewWaiters (a text result, not audio bytes).
	KindLLMPreview Kind = "llm_preview"
	// KindAmbience is one chapter's ambience pass - LLM tone-region music
	// scoring (internal/speakerattr.Client.ScoreMusic) and then its
	// lighting sub-pass (Client.DescribeLighting) - the entry point behind
	// the reader's per-chapter background-music toggle - see
	// EnqueueAmbience, httpapi.ambienceChapter. Was "music_scoring" before
	// lighting joined it. KindSpeechDirection's exact shape
	// (fire-and-forget, poolLLM, defaultAttributionTier, dispatched through
	// the same generic runLLM/llmWaiters machinery handleResult's own
	// poolLLM branch already handles for every kind in that pool) - see its
	// own doc comment for the reasoning, which applies unchanged here.
	KindAmbience Kind = "ambience"
	// KindMusicGeneration renders and persists every one of a chapter's
	// currently-eligible-in-order background-music regions in one task
	// (internal/musicgen.GenerateRegion, once per region - see
	// EnqueueMusicGeneration/generateChapterMusicBatch) - one task per
	// chapter, not one per region: a region's own clip generates fast
	// (Stable Audio Medium, not a long-running LLM batch) and a
	// "continuation" region needs its own immediately-preceding region's
	// clip anyway, so there's no benefit to splitting a chapter's ready
	// regions across several separately-dispatched/retried tasks the way
	// an earlier per-region version did - see MaybeAdvanceChapterMusic's
	// own doc comment. poolSFX, not a dedicated pool - poolSFX's own doc
	// comment already covers why every Stable Audio engine (SFX or Medium)
	// shares one slot pool regardless of which persisted-vs-preview kind
	// dispatches it. Fire-and-forget like KindSFXGeneration's own explicit
	// caller (jobs.Manager.MaybeAdvanceChapterMusic, not an HTTP handler -
	// a region's generation is entirely background-triggered once its own
	// paragraphs' narration is ready, never a reader-initiated "generate
	// now" click the way KindSFXGeneration's TierUrgent is for), but
	// TierBackground rather than KindSFXGeneration's TierUrgent - nothing
	// the reader is looking at blocks on a region's music actually being
	// ready, the same reasoning TierBackground already carries for
	// KindSpeechDirection/KindSpeakerAttribution.
	KindMusicGeneration Kind = "music_generation"
	// KindMusicLiveGeneration is KindMusicGeneration's reader-driven
	// sibling: one music region at a time, starting from the region the
	// reader is in and chaining forward region by region, each generated
	// as soon as its own paragraphs are voiced rather than waiting for the
	// whole chapter the way KindMusicGeneration does - so live listening
	// gets music while the rest of the chapter's narration is still
	// generating. Same batch function (generateChapterMusicBatch, with a
	// one-region batch), same poolSFX, TierUrgent rather than
	// TierBackground since it's what the reader is hearing right now (the
	// reader's own playback is waiting on it, the same as a paragraph they
	// just jumped to). Keyed by
	// region; the moment one dispatches it queues the next region's task,
	// which depends on it (Manager.needs) so it's ready to go the
	// instant this one finishes - see advanceLiveMusic. Both kinds can
	// reach the same region, so each region is claimed in memory
	// (Manager.claimMusicRegion) before generating and only ever generated
	// once. See MaybeAdvanceChapterMusic.
	KindMusicLiveGeneration Kind = "music_live_generation"
	// KindScareQuote is an LLM scare-quote-tagging run for one chapter
	// (internal/speakerattr.Client.ScareQuoteChapter, via
	// httpapi.scareQuoteChapterForJob) - see EnqueueScareQuote.
	// KindSpeechDirection's exact shape (fire-and-forget, poolLLM,
	// defaultAttributionTier, caller-supplied ScareQuoteFunc). Every
	// KindSpeakerAttribution task for a chapter depends on this one
	// (scareQuoteDependency): attribution needs to know which quoted spans
	// aren't dialogue before judging speakers.
	KindScareQuote Kind = "scare_quote_tagging"
	// KindPronunciation is an LLM pronunciation-resolution run for one
	// chapter (internal/speakerattr.Client.ResolvePronunciation, via
	// httpapi.pronounceChapter) - see EnqueuePronunciation. Split out of
	// KindSpeechDirection (which used to run it as a third sub-pass) since
	// a plain word substitution applies under every clone model, while
	// direction tagging is Higgs-only.
	KindPronunciation Kind = "pronunciation"
	// KindSummary is a chapter's summary pass (internal/speakerattr.Client.
	// SummarizeChapter, via httpapi.summarizeChapter) - its summary, scenes
	// and cast. KindScareQuote's shape; attribution and the ambience pass
	// depend on it (summaryDependency).
	KindSummary Kind = "chapter_summary"
	// KindEmbedding embeds a chapter's paragraphs for search by meaning
	// (internal/semantic.Service.EmbedChapter, via httpapi.embedChapter).
	// KindScareQuote's shape, and LLM-pool work though no LLM call: the
	// embedding model sits on the GPU beside the speaker LLM (llmworker.
	// Embedder), so it has to run while that pool holds the card.
	KindEmbedding Kind = "chapter_embedding"
	// KindBookSetting is a book's setting pass (internal/speakerattr.Client.
	// DescribeSetting, via httpapi.deriveBookSetting) - one per book, keyed
	// by it. The ambience, characterization and summary tasks that read the
	// setting depend on it (bookSettingDependency) when the book has none.
	KindBookSetting Kind = "book_setting"
	// KindRecap writes a book's "previously on" recap
	// (internal/speakerattr.Client.Recap/SeriesRecap, via
	// httpapi.writeRecap) - one per book, keyed by it; see RunRecap.
	KindRecap Kind = "recap"
	// KindRosterReview reviews one roster (a series', or a lone book's)
	// for duplicate and mislabeled characters and stores what it finds as
	// proposals (httpapi.ReviewRoster) - rules first, then one short LLM
	// call per pair of names that may be one person. Keyed by roster
	// scope, so two books of a series share one run.
	KindRosterReview Kind = "roster_review"
	// KindRecapNarration speaks that recap in the book's narrator voice
	// (RunRecapNarration) - KindVoiceProvision's shape, in poolGeneration.
	KindRecapNarration Kind = "recap_narration"
)

// Tier is shared across every chapter/paragraph-scoped Kind - a
// KindSpeakerAttribution task carries one of these same values (see
// EnqueueAttribution/defaultAttributionTier) so it sorts through the
// shared heap by the same Urgent/Lookahead/Background rules a queued
// voice-clone/design task does, rather than a separate ad hoc ordering.
const (
	// TierUrgent is a single paragraph the reader explicitly asked for
	// right now - "regenerate this one" - not just "queue it high because
	// it's coming up soon" (TierLookahead). Always dispatched next, ahead
	// of even TierLookahead's own distance-from-current-position sort
	// (see task.Less): there's no "distance" to weigh for an explicit
	// one-off request, it just needs to go first. Negative, not 0, so
	// TierLookahead/TierBackground's own relative ordering (and every
	// existing `tier < tier` comparison between them) doesn't need to
	// change at all.
	TierUrgent = -1
	// TierLookahead is paragraphs the reader is (or will imminently be)
	// waiting on. Always preempts TierBackground work still sitting in the
	// queue - it never interrupts a paragraph tts-service has already
	// started, only picks the next one to dispatch.
	TierLookahead = 0
	// TierNormal sits between TierLookahead and TierBackground - nothing
	// is enqueued at it by default; it's reached only by a reader
	// explicitly promoting a task (PromoteTier, the Jobs page), for work
	// that should jump ahead of the bulk TierBackground backlog without
	// competing with what the reader is actually waiting on right now.
	// Counts as "higher priority" for HasHigherPriorityWork, same as any
	// tier more urgent than TierBackground.
	TierNormal = 1
	// TierBackground is bulk "generate this whole chapter" / upload-sample
	// work - useful to have done eventually, but never worth making the
	// reader wait behind.
	TierBackground = 2
)

// task is one unit of dispatched work - a paragraph's worth of generation
// (KindVoiceClone/KindVoiceDesign, resolved once - book voice settings,
// ref text, etc. - when the task is created rather than re-looked-up by
// the worker), one chapter's speaker attribution, or one character's
// voice characterization (the two LLM kinds - see EnqueueAttribution/
// RunCharacterization). Only the fields relevant to t.kind are populated;
// see generate/handleResult's own per-kind branches for which.
type task struct {
	kind Kind
	// tier is the priority the task was asked for at. Its effective
	// priority (see internal/sched) can be more urgent; Run sets tier to
	// that as it starts, so everything downstream of a dispatch reads the
	// tier the task actually ran at.
	tier       int
	chapterIdx int // this task's chapter's index in the book

	// m is the Manager the task was pushed to (set by pushTask and by the
	// constructors of dependency tasks).
	m *Manager
	// parent is the key of the running group that asked for this task, or
	// "" - see Manager.pushTask.
	parent string
	// source is who asks for this task when it isn't a group: "" for a
	// direct request, or a reader's source (readerSource), whose request
	// is withdrawn once that reader has moved on.
	source string
	// resume is a paused LLM pass's requeue callback: the next Run calls it
	// to pick up the remainder (see task.Run). attributed sums the pass's
	// count across its runs.
	resume     func()
	attributed int
	// settled is set once the task's outcome has reached its waiters and
	// the store, so Ended doesn't deliver a second one.
	settled bool

	// pushedAt/startedAt time this task's queue wait and run for
	// handleResult's per-task timing log line. Set before the task is
	// visible to the queue (pushedAt) or to its runner (startedAt), so
	// never read concurrently with a write.
	pushedAt  time.Time
	startedAt time.Time
	// ranBefore is how long t ran in earlier stretches of a pass that
	// paused and resumed under this task (see kindDurations).
	ranBefore time.Duration

	// skipDependencies opts t out of Manager.needs entirely (see
	// that function's own early check) - set on KindLengthEstimate tasks,
	// whose sample has no chapter-pipeline state (characterization/
	// speech-direction/reference-clip) for those dependencies to
	// meaningfully check in the first place: it resolves and reads its own
	// reference clip itself (EnqueueLengthEstimate), before ever building
	// this task. Never set on any other Kind today, but deliberately a
	// plain field on task rather than a KindLengthEstimate-only special
	// case inside Manager.needs, so a future one-off kind with the
	// same "self-contained, nothing to wait on" shape can opt in the same
	// way without Manager.needs growing another kind check.
	skipDependencies bool

	// estimateChars is KindLengthEstimate's own sample's character count -
	// handleResult divides the generated clip's duration by this to get
	// the seconds-per-character Store.SetLengthEstimate persists. Unused
	// by every other Kind.
	estimateChars int

	// attempt counts genuine failures this exact unit of work has already
	// suffered, starting at 0 for a task's first ever dispatch - see
	// maxTaskAttempts/the scheduler's retry. handleResult checks it on a real
	// failure (not a cooperative pause, which doesn't count against it) and,
	// while budget remains, retries by pushing a fresh copy back onto this
	// same priority queue rather than
	// looping inside generate/runLLM/etc. itself: a retry this way is still
	// subject to the same tier/kindPriority ordering, pool mutual-exclusion,
	// and Jobs-dashboard visibility as any other queued task, instead of
	// silently blocking a worker slot in a private retry loop invisible to
	// everything else waiting on that slot.
	attempt int

	// Cancelling a dispatched task (Manager.Cancel) cancels the context its
	// Run was given. A generation task's ttsworker HTTP call aborts
	// promptly; an LLM task's decode loop only notices between calls
	// (llama.cpp's decode is synchronous), so it ends once its current
	// batch does. Either way the task's own result handling delivers the
	// error to its waiters; only a task that never ran is told by Ended.

	bookID    string
	chapterID string

	// seriesName/seriesIndex are poolLLM ("pre-processing") tasks only -
	// KindSpeakerAttribution/KindSpeakerCharacterization/KindSpeechDirection
	// - resolved from the book at enqueue time (see Manager.seriesFor) and
	// used by task.Less to order same-tier pre-processing tasks by a
	// book's place in its series rather than the arbitrary bookID ordering
	// generation tasks use. seriesName is "" for a book with no series, in
	// which case ordering falls back to that same arbitrary bookID rule -
	// there's nothing to sequence a standalone book's own pre-processing
	// against. Never set on a generation task (KindVoiceClone/KindVoiceDesign/
	// KindVoiceDesignPreview/KindVoiceProvision) - see task.Less's own
	// doc comment for why generation intentionally keeps its existing,
	// non-series-aware ordering.
	seriesName  string
	seriesIndex float64

	// KindVoiceClone/KindVoiceDesign only:
	voiceID   string
	paragraph store.Paragraph
	// mergeParagraphs is non-nil (len >= 2) when this task covers a scare-
	// quote merge group (see store.Paragraph.ScareQuote/
	// pushResolvedTask/scareQuoteMergeGroup) rather than one paragraph -
	// every member, including paragraph itself (always mergeParagraphs[0],
	// the group's anchor: dedupKey below keys off paragraph.ID, so every
	// push for any member of the same group has to agree on the same
	// anchor to actually dedup together). nil (the overwhelming common
	// case) means paragraph is generated alone exactly as before this
	// feature existed - see generate/handleMergedResult for the two
	// branches this drives.
	mergeParagraphs []store.Paragraph

	// shared marks a paragraph whose clip lives in the shared store (see
	// sharedclips.go), as its take'th reading (clipkey.TakeAt). fresh is
	// an explicit "regenerate this paragraph": the clip is rendered anew
	// even though it exists, and replaces it for every paragraph using it.
	shared bool
	fresh  bool
	take   int
	// clipKey is the paragraph's content key, set by generate once it
	// knows the reference clip and text it is rendering; clipHit is set
	// with it when a shared clip already existed and generate skipped
	// rendering (finishResult then puts the paragraph on that clip).
	clipKey string
	clipHit bool
	// claimed is the content key t is the one task rendering, if any (see
	// sharedClips.claim).
	claimed string
	// again is the latest push of t's key that arrived while t was already
	// running (Merge), guarded by Manager.mu: t's result may not be what
	// that push asked for - see runAgain.
	again *task

	// genLexicon is the pronunciation lexicon generate actually rendered
	// with (genLexiconSet once it has run) - refreshStaleGeneration
	// compares the paragraph's current text against *that*, not the
	// lexicon as it stands when the clip comes back: resolving both sides
	// with the current rules would hide a rule edited mid-render, and save
	// audio of the old pronunciation over the edit's invalidation.
	genLexicon      []pronounce.LexiconEntry
	genLexiconSet   bool
	instruct        string
	presetID        string
	language        string
	refText         string
	speedMultiplier float64
	seed            int
	cloneModel      string
	designModel     string
	// cloneInstruct is a clone-time style instruction sent alongside the
	// reference clip on the actual /generate call (see narration.
	// ResolvedVoice.CloneInstruct's own doc comment for when this is
	// non-empty - only store.Book.InstructCharacterVoices' own fallback
	// path sets it today) - distinct from instruct above, which is a
	// design-time instruction only ever used to render a preset's own
	// standalone reference clip from scratch. Only honored by a family
	// with its own instructOption (breeze_tts - see audioworker.
	// cloneFamily.instructOption), silently ignored otherwise.
	cloneInstruct string

	// KindSpeakerAttribution/KindSpeakerCharacterization only (the two LLM
	// kinds - see poolFor): the actual call (httpapi supplies this, since
	// it needs Store/Speaker access this package doesn't have), llmKey
	// (this task's dedupKey suffix - a chapterID for attribution, a
	// characterID for characterization), label (a human-readable
	// dashboard identifier for a kind that isn't chapter/paragraph-scoped
	// - "" for attribution, which already has a chapter to show), and
	// every caller currently blocked on this task's result - characterization
	// only, now that attribution is fire-and-forget too (EnqueueAttribution
	// never populates this). llmWaiters is usually one entry, but pushTask's
	// dedup can attach more: if a second RunCharacterization call for the
	// same character arrives while this one is still queued or in flight,
	// its own waiter is appended here (see pushTask) rather than
	// silently dropped, since every RunCharacterization caller needs a real
	// result, not just "someone will eventually handle this." handleResult
	// sends to every entry (a no-op for an attribution task, which has
	// none).
	runLLM     func(ctx context.Context) (attributed int, requeue func(), err error)
	llmKey     string
	label      string
	llmWaiters []chan llmOutcome

	// KindVoiceDesignPreview only: the actual render call (httpapi supplies
	// this, since it needs the exact instruct/text/seed/language a caller
	// typed in - see RunVoiceDesignPreview) and every caller blocked on its
	// result - mirrors llmWaiters' shape exactly, just carrying audio bytes
	// instead of an attributed count, since a preview render has no
	// meaningful "paragraphs attributed" to report. llmKey still doubles as
	// this kind's dedup suffix (see dedupKey) - a unique-per-call value
	// (RunVoiceDesignPreview never reuses one), so previewWaiters is always
	// exactly one entry in practice; the slice shape is kept anyway so
	// pushTask's own generic dedup/merge machinery doesn't need a special
	// case for this kind.
	runPreview     func(ctx context.Context) (audio []byte, err error)
	previewWaiters []chan previewOutcome

	// KindLLMPreview only: previewWaiters' own sibling for a kind that
	// returns raw text instead of audio bytes (see RunLLMPreview,
	// httpapi.handleTestLLM) - same reasoning/shape as runPreview/
	// previewWaiters above, just for a different result type.
	runPreviewText     func(ctx context.Context) (text string, err error)
	previewTextWaiters []chan previewTextOutcome

	// KindVoiceProvision only: the actual provision/regenerate call
	// (httpapi supplies this - see EnqueueVoiceProvision) and every caller
	// currently blocked on its result - mirrors llmWaiters/previewWaiters'
	// shape exactly. Usually empty (EnqueueVoiceProvision's own explicit,
	// batch-friendly callers - the Speakers page's "Generate/Regenerate
	// all voices" buttons, and httpapi.recharacterizeAndInvalidate's own
	// follow-up - are deliberately fire-and-forget, same reasoning as
	// KindSpeakerAttribution's own), but RunVoiceProvision (the lazy path,
	// provisionMissingCharacterVoices) blocks on one so a character's
	// voice resolution can use the freshly provisioned presetID right
	// away - and, as a side effect, makes that otherwise-invisible lazy
	// provisioning show up on the Jobs dashboard the same as an explicit
	// one does, addressing a real gap: it used to call m.provision
	// directly, bypassing the task queue (and so this dashboard) entirely.
	// runProvision's attempt param is this task's own t.attempt at dispatch
	// time (0 on the first try, incremented by the scheduler's retry on each retry) -
	// see EnqueueVoiceProvision/RunVoiceProvision's own doc comments for why
	// callers need it: VoiceDesign is fully deterministic on (instruct,
	// seed, refText, language), so a caller whose seed is fixed (a
	// preset's own stored one) needs to perturb it by attempt on a retry,
	// or a numerically-unstable crash ("no finite logits") just reproduces
	// identically on every attempt instead of actually getting a second
	// try.
	runProvision     func(ctx context.Context, attempt int) (presetID string, err error)
	provisionWaiters []chan provisionOutcome

	// KindSFXGeneration only: the actual generate-and-persist call
	// (httpapi supplies this - see EnqueueSFXGeneration; it needs Store/
	// TTS/audiopath access this package doesn't have, same reasoning
	// runProvision's own doc comment gives) and every caller currently
	// blocked on its result - mirrors provisionWaiters' shape exactly,
	// usually empty since EnqueueSFXGeneration's only caller
	// (httpapi.handleGenerateParagraphSFX) is fire-and-forget.
	runSFXGen     func(ctx context.Context, attempt int) error
	sfxGenWaiters []chan error

	// KindMusicGeneration only: runSFXGen/sfxGenWaiters' own exact sibling
	// for one chapter's own batch generate-and-persist call, covering every
	// region MaybeAdvanceChapterMusic found eligible at dispatch time (this
	// package builds and supplies the closure itself - see
	// EnqueueMusicGeneration) - always fire-and-forget in practice
	// (MaybeAdvanceChapterMusic never blocks on a waiter), kept as its own
	// field pair rather than reusing runSFXGen/sfxGenWaiters so Jobs-
	// dashboard/log messages naming "music generation" specifically don't
	// have to share a generic "sfx" label with paragraph-scoped
	// KindSFXGeneration tasks.
	runMusicGen     func(ctx context.Context, attempt int) error
	musicGenWaiters []chan error
	// KindMusicLiveGeneration only: the paragraph idx its region starts at
	// (where the chain continues from once it dispatches - see
	// continueLiveMusic), and the region right before it in the chain,
	// whose own still-queued/in-flight task it depends on (see
	// liveMusicDependency) - normally also its seed, except for a
	// chapter's first region, whose chain predecessor is the previous
	// chapter's last region (musicPrevChapterID), depended on only to keep
	// the chain in order.
	musicRegionStart   int
	musicPrevRegionID  string
	musicPrevChapterID string
}

// provisionOutcome is a KindVoiceProvision task's real result, delivered to
// every entry in task.provisionWaiters by handleResult - llmOutcome/
// previewOutcome's sibling for a kind that returns a voice preset id.
type provisionOutcome struct {
	presetID string
	err      error
}

// previewOutcome is a KindVoiceDesignPreview task's real result, delivered
// to every entry in task.previewWaiters by handleResult - llmOutcome's
// sibling for a kind that returns audio bytes instead of an attributed
// count.
type previewOutcome struct {
	audio []byte
	err   error
}

// previewTextOutcome is a KindLLMPreview task's real result, delivered to
// every entry in task.previewTextWaiters by handleResult - previewOutcome's
// own sibling for a kind that returns raw text instead of audio bytes.
type previewTextOutcome struct {
	text string
	err  error
}

// dedupKey identifies t for queue dedup and in-flight tracking (both owned by
// internal/sched now, keyed by this) -
// a paragraph can only usefully be queued once (its own ID); a chapter's
// attribution, or a character's characterization, can each only usefully
// run once at a time (llmKey - a chapterID or characterID respectively,
// distinctly namespaced per kind so the different id spaces can never
// collide with each other or with a paragraph ID).
func (t *task) dedupKey() string {
	switch t.kind {
	case KindSpeakerAttribution:
		return "attr:" + t.llmKey
	case KindSpeakerCharacterization:
		return "characterize:" + t.llmKey
	case KindSpeechDirection:
		return "direction:" + t.llmKey
	case KindSpeakerReattribution:
		// t.llmKey already namespaces both the chapter and the speaker
		// being eliminated (see EnqueueReattribution) - two different
		// speakers' own "Auto Split" runs against the same chapter must
		// dedup independently of each other, unlike attribution's own
		// chapterID-only key, which only ever has one attribution run per
		// chapter to dedup against in the first place.
		return "reattr:" + t.llmKey
	case KindVoiceDesignPreview:
		// Always unique (see RunVoiceDesignPreview) - two different "Test"
		// clicks are two different, unrelated renders with nothing to
		// usefully dedup against each other, unlike a chapter or character.
		return "design_preview:" + t.llmKey
	case KindSFXPreview:
		// Same "always unique" reasoning as KindVoiceDesignPreview above.
		return "sfx_preview:" + t.llmKey
	case KindLLMPreview:
		return "llm_preview:" + t.llmKey
	case KindVoiceProvision:
		// llmKey already encodes both the character id and which mode
		// ("generate" vs "regenerate" - see EnqueueVoiceProvision) so a
		// generate call for a character and a regenerate call for that
		// same character never collide into one task (they mean different
		// things: idempotent-skip vs force-rerender), while two of the
		// same mode for the same character still correctly join.
		return "voice_provision:" + t.llmKey
	case KindLengthEstimate:
		// Keyed by book, not paragraph.ID (this task has none) - a second
		// upload-triggered estimate for the same book while one's still in
		// flight (shouldn't happen in practice, EnqueueLengthEstimate has
		// exactly one call site, but harmless either way) just dedups
		// instead of double-dispatching.
		return "length_estimate:" + t.bookID
	case KindAmbience:
		return "ambience:" + t.llmKey
	case KindMusicGeneration:
		return "music_gen:" + t.llmKey
	case KindMusicLiveGeneration:
		return "music_live:" + t.llmKey
	case KindScareQuote:
		return "scare_quote:" + t.llmKey
	case KindPronunciation:
		return "pronunciation:" + t.llmKey
	case KindSummary:
		return "summary:" + t.llmKey
	case KindEmbedding:
		return "embedding:" + t.llmKey
	case KindBookSetting:
		return "book_setting:" + t.llmKey
	case KindRecap:
		return "recap:" + t.llmKey
	case KindRosterReview:
		return "roster_review:" + t.llmKey
	case KindRecapNarration:
		return "recap_narration:" + t.llmKey
	default:
		return t.paragraph.ID
	}
}

// kindPriority ranks Kind for tie-breaking same-tier tasks against each
// other, inside task.Less - embedding first (see below), then characterization, then voice
// provisioning, then actual clone/design generation, the same dependency
// order this package's Manager.needs enforces as a hard rule
// between pools (a character's voice needs characterizing before it's
// worth provisioning, and provisioned before paragraphs in it generate)
// applied here as a soft default among same-tier candidates more broadly,
// not just when they'd otherwise collide on the same character. Kinds not
// listed (attribution, design previews) rank last (0), tied with each
// other by Less's own further tiebreaks. Higher returned value sorts
// first.
func kindPriority(k Kind) int {
	switch k {
	case KindEmbedding:
		// A chapter embeds in under a second, and casting reads the
		// result: behind a few hundred characterizations it waited hours.
		return 4
	case KindSpeakerCharacterization:
		return 3
	case KindVoiceProvision:
		return 2
	case KindVoiceClone, KindVoiceDesign:
		return 1
	default:
		return 0
	}
}

// kindCompare maps each Kind to its own tie-break ordering function, used
// by Less once kindPriority is already tied - a plain per-kind registry
// (like kindPool) rather than one shared function switching on every
// kind's own rule inline, so a future Kind with a genuinely different
// ordering need just registers its own function here instead of growing
// a shared if/else chain everyone else's ordering also has to read past.
// Each returns a cmp.Compare-style int (negative if a sorts first, zero
// if tied, positive if b does), the shape cmp.Or below needs to chain
// several of these as one ordered sequence of tiebreaks. Every kind
// registered below happens to share the same actual behavior today
// (comparePosition) - that's a statement about what these kinds currently
// need, not a reason to collapse them into one shared case; a kind with
// no entry (KindVoiceDesignPreview/KindVoiceProvision) has no
// finer-grained ordering of its own at all, falling straight through to
// the scheduler's own arrival-order tiebreak.
var kindCompare = map[Kind]func(a, b *task, priority int) int{
	KindVoiceClone:              compareClone,
	KindVoiceDesign:             comparePosition,
	KindSpeakerAttribution:      comparePosition,
	KindSpeakerCharacterization: comparePosition,
	KindSpeechDirection:         comparePosition,
	KindSpeakerReattribution:    comparePosition,
	KindScareQuote:              comparePosition,
	KindPronunciation:           comparePosition,
	KindSummary:                 comparePosition,
	KindEmbedding:               comparePosition,
	KindBookSetting:             comparePosition,
	KindRecap:                   comparePosition,
	KindRosterReview:            comparePosition,
}

// lessTask breaks a tie between two tasks of equal effective priority:
// kindPriority first (characterization before provisioning before plain
// generation, the order Manager.needs enforces as a hard dependency,
// applied as a soft preference more broadly), then whatever kindCompare has
// registered for t's own kind. A remaining tie is left to the scheduler's
// arrival order.
func lessTask(t, o *task, priority int) bool {
	compare := kindCompare[t.kind]
	if compare == nil {
		compare = func(*task, *task, int) int { return 0 }
	}
	return cmp.Or(
		cmp.Compare(kindPriority(o.kind), kindPriority(t.kind)), // higher kindPriority sorts first
		compare(t, o, priority),
	) < 0
}

// comparePosition orders by the books' own series first (name, then
// index - see task.seriesName's own doc comment), checked unconditionally
// for every Kind that uses this, not gated to any particular pool: a
// series relationship between two books is a real fact about them
// regardless of what kind of work either task represents. Two tasks with
// no series at all (both "") or belonging to different series still get
// a perfectly well-defined, stable order out of this (alphabetically by
// name, "" sorting first) - there's no need to special-case "doesn't
// apply" separately from "tied", since ordering by name costs nothing
// when it's genuinely a tie (equal names) and gives a harmless, arbitrary
// answer when it isn't (see this function's own doc comment on the "no
// specific ordering guaranteed between series" contract that satisfies).
// Falls back to plain book/chapter/paragraph position once the series
// axis is exhausted too - not raw enqueue order, so a chapter whose
// "generate" request happens to reach the backend later (client-side
// scheduling, retries, network races) never jumps ahead of an earlier
// chapter's still-queued work. A genuine remaining tie (0) falls through
// to the scheduler's own arrival-order tiebreak.
func comparePosition(a, b *task, _ int) int {
	return cmp.Or(
		cmp.Compare(a.seriesName, b.seriesName),
		cmp.Compare(a.seriesIndex, b.seriesIndex),
		cmp.Compare(a.bookID, b.bookID),
		cmp.Compare(a.chapterIdx, b.chapterIdx),
		cmp.Compare(a.paragraph.Idx, b.paragraph.Idx),
	)
}

// compareClone is comparePosition, except that within one chapter a
// TierBackground/TierNormal clone task orders by its reference clip
// (refKey) before its paragraph index. The TTS worker keeps each recent
// reference's prompt-prefix KV resident (audio.cpp's
// higgs_audio_tts.reference_kv_slots), so running one voice's lines back
// to back turns almost every call after that voice's first into a cheap
// restore instead of a full reference prefill, however many voices and
// emotion variants a chapter uses. TierLookahead/TierUrgent keep strict
// reading order - a reader is waiting on those in sequence.
func compareClone(a, b *task, priority int) int {
	if priority < TierNormal || a.bookID != b.bookID || a.chapterIdx != b.chapterIdx {
		return comparePosition(a, b, priority)
	}
	return cmp.Or(
		cmp.Compare(a.refKey(), b.refKey()),
		cmp.Compare(a.paragraph.Idx, b.paragraph.Idx),
	)
}

// refKey identifies the reference clip a clone task clones from: its
// preset plus the emotion variant its line uses (each variant is a
// separate clip - see variantClipDependency, which applies the same
// merged-group exception).
func (t *task) refKey() string {
	if len(t.mergeParagraphs) > 1 {
		return t.presetID
	}
	return t.presetID + ":" + t.paragraph.EffectiveEmotion()
}

type taskResult struct {
	audio []byte // KindVoiceClone/KindVoiceDesign/KindVoiceDesignPreview/KindSFXPreview only
	// latents is audio's decoder input when the clone model has one (see
	// ttsworker.Audio) - written beside the clip as its sidecar.
	latents *ttsproto.Latents
	// words is audio's own word timings when generate already aligned it
	// for its completeness check (see generateCloneChecked) - nil
	// otherwise, leaving alignment to saveParagraphAudio's own detached
	// pass.
	words      []ttsproto.Word
	text       string // KindLLMPreview only - the raw generated text
	presetID   string // KindVoiceProvision only - the character's (created or reused) voice preset id
	attributed int    // KindSpeakerAttribution only - paragraphs (re)attributed
	// requeue is KindSpeakerAttribution-only: non-nil when AttributionFunc
	// stopped early because HasHigherPriorityWork reported more urgent
	// work waiting (see AttributionFunc's own doc comment), with
	// paragraphs left it never got to. Calling it re-enqueues a follow-up
	// task for just those - handleResult calls it after the scheduler ending the task has
	// cleared this task's own dedup entry, so the follow-up (same
	// dedupKey) doesn't get silently merged into this now-finishing task
	// instead of actually queuing (see pushTask's own dedup/inFlight
	// check). nil for every other outcome (finished normally, failed, or
	// any non-attribution kind).
	requeue func()
	err     error
}
