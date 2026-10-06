# internal/jobs - the job queue

Moved out of `../CLAUDE.md`. What each job kind does, how jobs are
enqueued, ordered and retried, the reader rules, and pause/drain. The
scheduler underneath is `internal/sched` (design: `../../docs/scheduler-design.md`).

`internal/jobs/` is a single priority queue shared by every
chapter/paragraph/character-scoped background job, generalized over a
`Kind` (`KindVoiceClone`, `KindVoiceDesign`, `KindSpeakerAttribution`,
`KindSpeakerCharacterization`, `KindSpeechDirection`, `KindPronunciation`,
among others) rather than
one mechanism per feature. **Pause** (`POST /api/jobs/pause`) stops new
dispatch; in-flight work finishes, then resident models unload
(`unloadAllOnceIdle`) and the queue is *drained* (`Manager.Drained`,
`drained` in `GET /api/jobs`). A paused, drained queue still dispatches
explicit previews (`isExplicitPreview`: LLM, voice-design and SFX
previews, including the attribution/scare-quote previews) - pause is
the scheduler's admission gate (`Manager.gate`), so held-back work just
stays queued.
`?drain=1` makes the pause call wait for the drain (`WaitDrained`);
`backend/attreval` does that around every run so a batch of previews
runs alone rather than swapping models against generation, and resumes
after unless the queue was already paused. Files: `manager.go` (the `Manager`, pools
and in-flight limits, pause/resume, model unloading),
`task.go` (`Kind`, tiers, `task`, dedup keys and ordering), `schedtask.go`
(`task` as a `sched.Task`: run, retry, merge, how it ends), `hooks.go`
(the httpapi-supplied hook types and their setters), `enqueue.go`
(paragraph resolution, merge groups, lookahead, `pushTask`),
`dependencies.go` (every `*Dependency`: what a task needs first),
`llmtasks.go` (LLM-pass enqueue/run), `provision.go` (voice
provisioning, SFX, previews), `music.go` (region music), `control.go`
(`Snapshot`, cancel, promote), `generation.go` (running a clone task
and saving its result), `pipeline.go` (bulk groups). (`throughput.go`: successful narration
completions feed a characters-per-second rate over the latest continuous
run, `Manager.Throughput`, which the jobs snapshot exposes as
`narrationCharsPerSecond` beside each narration task's `chars`, for the
Jobs views' time-left estimate; the same run's clip lengths give
`Manager.RealtimeFactor`, audio seconds per wall-clock second, exposed as
`narrationRealtimeFactor`.)

**Scheduling is `internal/sched`** (rules and reasons:
`../../docs/scheduler-design.md`; replaced `internal/taskqueue` and this
package's second pipeline queue on 2026-10-03). What this package
supplies on top:
  - **Tiers** (`TierUrgent`/`TierLookahead`/`TierNormal`/`TierBackground`)
    are request priorities. A task's *effective* tier is derived: the
    most urgent of its own requests and of whatever needs it, so a pass
    an urgent paragraph waits on is urgent, and stops being so when that
    paragraph is cancelled. `task.tier` is only what it was asked for at;
    `Snapshot` and `task.Run` use the effective one.
  - **Pools are scheduler contexts** (`kindPool`, `poolCapacity`): one of
    `poolGeneration`/`poolDesign`/`poolSFX`/`poolLLM` runs at a time,
    since they share ttsworker's VRAM. `task.CanStart` holds each to its
    capacity and attribution to one task at a time. `Manager.onSwitch`
    unloads the family being left on a TTS<->LLM crossing.
  - **Dependencies are declared, not polled**: `Manager.needs`
    (`task.Prepare`) runs when a task is queued and again when what it
    waited for has ended. Each `*Dependency` checks the persisted state
    first (pass flag, clip on disk) and names a task to create only if
    it's missing; a task already queued under that key is joined. A
    dependency that fails for good (3 attempts) or is cancelled **fails
    what waited for it** - a paragraph gets an audio error, a waiter the
    error (`task.Ended`). Nothing is recreated forever any more.
  - **Retries are the scheduler's**: `handleResult` returns the error and
    the same task runs again (`task.Retryable` = `retryEligible`);
    waiters and the store only hear of the last attempt. A paused LLM
    pass (non-nil `requeue`) and a stale render return `sched.ErrYield`:
    same task, no attempt counted. On its next run a paused pass calls
    its `requeue`, and `pushTask` hands the remainder it pushes to that
    task (`Manager.resumeInto`) rather than queueing a second one.
  - **Groups** (`pipeline.go`: preprocessing phases, bulk groups) are
    tasks with no context, so they run beside any pool. What a group
    asks for through the context its run is given (`withParent` -
    every blocking `Run*` call) is *adopted*: it takes the group's tier,
    follows a promotion, and is cancelled with the group unless
    something else asked for it. Paragraph audio a group queues is not
    adopted yet; `waitForChapterTasks` and `cascadePromotion`
    (`ChildFilter`) still cover it. A cancelled phase fails the phases
    waiting on it.
  - **Yielding**: `HasHigherPriorityWork` (checked between batches by
    long LLM passes) is `sched.ShouldYield` for the running LLM tasks -
    true only when a strictly more urgent *ready* task is blocked by
    them.
  - `PromoteTier` is `sched.Bump` (one more request for the task);
    `Cancel`/`CancelAll`/`CancelChapter` are `sched.Cancel`/`CancelWhere`.
  - Tests step dispatch by hand (`newTestManager`: `sched.Config.Manual`,
    `mgr.sched.Pop`/`Finish`, `complete`) until `Start` is called.

**Reader rules** (`reader.go`):
  - Each reader is its own request source (`readerSource`: book plus the
    `clientId` its lookahead calls carry). `enqueueLookahead` makes that
    source's requests exactly the current window and withdraws the rest
    (`sched.Retain`): what only that reader wanted is cancelled, in
    flight included; what a bulk run or another reader also wants falls
    back to their priority.
  - **Prework**: every lookahead also requests the emotion and
    pronunciation passes (and, under strict order, attribution) of the
    next `preworkChapters` (3) chapters at `TierLookahead`.
  - **Window passes**: a `TierUrgent` paragraph whose chapter's emotion or
    pronunciation pass hasn't run waits for a run over just its
    30-paragraph window (`windowDependency`, key
    `direction:<chapterID>@<start>`, `httpapi.directChapterRange` with
    `partial`), not the whole chapter. The whole pass then runs over what
    windows left (`passRun`; covered idxs are kept in memory in
    `Manager.passWindows`, so after a restart a whole pass relabels them).
    A chapter's pass and its windows never run at once (`task.CanStart`).
  - **Strict order** (`store.Book.StrictOrder`, `strictBlocked`): a
    multi-voice book's chapter isn't narrated until it is attributed -
    lookahead asks for the attribution instead and queues the paragraphs
    on a later call; chapter/book generation waits for it
    (`ensureAttributed`). An explicit single-paragraph regenerate ignores
    it. Off (reader-first) by default.
  - **Setting required** (`store.Book.SettingRequired`): off, the setting
    pass is waited for but its failure fails nothing (a member need); on,
    it's a real dependency.

Not built yet: removing a bump from the Jobs page (`sched.Unbump`
exists), adopting paragraph audio into its group, the two settings in the
Android app (web Speakers page only), and persisting requests across a
restart.

The kinds:
  - `KindVoiceClone`/`KindVoiceDesign` (`poolGeneration`) walk a
    chapter's not-yet-ready paragraphs and generate each one (via
    `internal/voicerefs` + `internal/ttsworker`), writing Ogg Opus clips
    (`audiopath.WriteClip`) and updating paragraph status. Clone generations are force-aligned
    inline as a completeness check (`jobs.generateCloneChecked`):
    audio.cpp's aligner places words missing from the audio *past the
    clip's end*, and Higgs occasionally reaches EOC early and drops a
    paragraph's final sentence (measured: 16 of 3838 paragraphs in one
    real book, 3-17 words each). >=2 such words triggers a regeneration
    with a smaller audio.cpp `text_chunk_size` request option (half,
    then a quarter of the text's length, floored at 80 chars - see
    `retryChunkSizes`), keeping the most complete attempt; its word
    timings are saved directly. The same check also transcribes the
    clip (`/transcribe`) and diffs it word by word against the text
    (`extraWordRun`): a run of >=3 consecutive words the text doesn't
    contain (a repeated phrase, a leaked reference line) triggers the
    same retries - the forced aligner can't catch these itself, since it
    stretches a neighboring word over extra speech rather than leaving
    a gap. Measured on 405 real paragraphs: none had a run over 1, so
    ASR slips alone don't trip it. Retries keep the attempt with the
    fewest missing plus extra words. Everything else (VoiceDesign, or a
    failed check) still aligns as a detached follow-up. Each paragraph resolves its own voice via
    `internal/narration.Resolver` before being queued, so a chapter's
    paragraphs can dispatch to, and cache audio under, several different
    `voice_id`s in one generation run. An **unvoiced** paragraph -
    nothing but an ellipsis (`store.Paragraph.Unvoiced`: "…", "“...”") -
    is a beat of silence, not a line: its task waits on nothing and
    `generate` returns 0.8s of silence made in Go (`silentClip`), with
    empty word timings, instead of calling the worker. Attribution and
    the emotion pass treat it as narration (speaker `Narrator`, no
    emotion), so Brightblade's "…" replies no longer land on whoever
    spoke last; migration `00007` cleared the ones already attributed.
  - `KindScareQuote` (`poolLLM`) runs one chapter's scare-quote
    tagging - `KindSpeechDirection`'s shape (`EnqueueScareQuote`/
    `RunScareQuote`). `KindSpeakerAttribution` depends on the chapter's
    `KindScareQuote` (see "Scare-quote tagging" in `speakerattr.md`).
  - `KindSpeakerAttribution` (`poolLLM`) runs one chapter's speaker
    attribution, triggered via `Manager.EnqueueAttribution` - unlike the
    generation kinds, this package has no `Store`/`Speaker` access of
    its own, so the actual work is a caller-supplied `AttributionFunc`
    (`httpapi.Server.attributeChapter`). Fire-and-forget, defaulting to
    `TierBackground` - attribution never blocks anything the reader is
    looking at, and SpeakersPage's "Attribute all" can enqueue a whole
    book at once; that bulk work shouldn't preempt a more urgent
    `poolLLM` task (`RunCharacterization`, still `TierUrgent`) queued
    behind it. Also doesn't block its caller (`202` returned
    immediately) on a `poolLLM` slot actually dispatching it. An earlier
    version did block the caller; changed because attribution runs a
    whole chapter through the LLM in batches and can take minutes, and
    "Attribute all" fires one request per unattributed chapter at once -
    for a long book that meant dozens of HTTP requests sitting open
    behind `poolLLM`'s single slot for as long as the whole book took, a
    page reload aborting every not-yet-sent request outright (so that
    chapter's attribution never ran at all, despite the dispatched task
    itself never actually being tied to the request's context - it
    always ran via the worker loop's own long-lived context). Progress
    is observable via `GET /api/jobs` rather than the enqueuing
    response. `attributeChapter`'s own closure never calls
    `RunCharacterization` - it used to, and since `poolLLM` only had 1
    slot, that nested call could never dispatch, hanging both it and
    every other attribution/characterization request server-wide until
    the original request's context was cancelled. Fixed by moving
    characterization out of attribution entirely - `poolLLM`'s 1-slot
    size (as it was then) was safe once nothing dispatched into it ever
    called back into it.
  - `KindSpeakerCharacterization` (`poolLLM`) runs one character's
    voice characterization (`Client.CharacterizeVoice`, triggered via
    `Manager.RunCharacterization`, which - unlike `EnqueueAttribution` -
    still blocks its caller until a `poolLLM` slot dispatches it: a
    single character's characterization is one short LLM call, so
    holding a request open for it is fine), same join/`TierUrgent`
    behavior, keyed by characterID instead of chapterID, reporting only
    an error. Its only callers are `httpapi.provisionCharacterVoice`
    (the lazy path, called from `jobs.Manager`'s own background
    goroutines, never from inside a worker slot) and
    `httpapi.handleCharacterizeSpeaker` (Speakers page's "Regenerate"
    button), so it shows up on the Jobs dashboard and a reader can
    explicitly retrigger it even if already characterized.
    `handleCharacterizeSpeaker` then invalidates (updates `Instruct`,
    deletes the cached reference clip, does *not* eagerly re-render)
    that book's resolved clone model's assigned voice preset for the
    character, so it's lazily rebuilt next time it's actually needed.
