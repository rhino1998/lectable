import {
  ROUTE_RESPONSE_KINDS,
  type BulkAction,
  type BulkScope,
  type CustomVoicePresetInput,
  type ErrorCode,
  type GenerateSFXRequest,
  type JobTier,
  type Position,
  type ResetPass,
  type Routes,
  type TestLLMRequest,
  type VoiceSettingsUpdate,
} from './types'

class ApiError extends Error {
  status: number
  // Machine-readable error code, when the backend sends one (see
  // httpapi.writeErrorCode) - for errors the UI can recover from.
  code?: ErrorCode
  constructor(status: number, message: string, code?: ErrorCode) {
    super(message)
    this.status = status
    this.code = code
  }
}

type RouteKey = keyof Routes

// One route's arguments, from the generated route table: path params when
// it has any, optional query params, and its body when it takes one.
export type CallArgs<K extends RouteKey> = ([keyof Routes[K]['path']] extends [never]
  ? { path?: undefined }
  : { path: Routes[K]['path'] }) &
  ([keyof Routes[K]['query']] extends [never] ? { query?: undefined } : { query?: Routes[K]['query'] }) &
  ([Routes[K]['body']] extends [never] ? { body?: undefined } : { body: Routes[K]['body'] })

type CallRest<K extends RouteKey> = Record<never, never> extends CallArgs<K> ? [args?: CallArgs<K>] : [args: CallArgs<K>]

// Calls one backend route, typed end to end by the generated route table
// (Routes): the "<METHOD> <path>" key picks the params, body, and
// response type, and ROUTE_RESPONSE_KINDS how the body is read (JSON,
// nothing for a 204, or a Blob for a file).
export async function call<K extends RouteKey>(route: K, ...[args]: CallRest<K>): Promise<Routes[K]['response']> {
  const [method, template] = route.split(' ') as [string, string]
  const a = (args ?? {}) as {
    path?: Record<string, string | number>
    query?: Record<string, string | number | boolean | undefined>
    body?: unknown
  }
  let url = template.replace(/\{(\w+)\}/g, (_, name: string) => encodeURIComponent(String(a.path?.[name])))
  const qs = new URLSearchParams()
  for (const [k, v] of Object.entries(a.query ?? {})) if (v !== undefined) qs.set(k, String(v))
  if (qs.size > 0) url += `?${qs}`
  // Belt-and-suspenders alongside the backend's own Cache-Control:
  // no-store (see httpapi.writeJSON) - a browser serving a cached response
  // instead of actually asking the network would return stale state.
  const init: RequestInit = { method, cache: 'no-store' }
  if (a.body instanceof FormData) {
    init.body = a.body
  } else if (a.body !== undefined) {
    init.headers = { 'Content-Type': 'application/json' }
    init.body = JSON.stringify(a.body)
  }
  const res = await fetch(url, init)
  if (!res.ok) {
    let message = `HTTP ${res.status}`
    let code: ErrorCode | undefined
    try {
      const body = await res.json()
      if (body?.error) message = body.error
      if (typeof body?.code === 'string') code = body.code
    } catch {
      // ignore non-JSON error bodies
    }
    throw new ApiError(res.status, message, code)
  }
  switch (ROUTE_RESPONSE_KINDS[route]) {
    case 'none':
      return undefined as Routes[K]['response']
    case 'binary':
      return (await res.blob()) as Routes[K]['response']
    default:
      return (await res.json()) as Routes[K]['response']
  }
}

export const api = {
  uploadBook: (file: File) => {
    const body = new FormData()
    body.append('file', file)
    return call('POST /api/books', { body })
  },

  deleteBook: (id: string) => call('DELETE /api/books/{id}', { path: { id } }),

  // Deletes every generated audio file for this book (and the DB's own
  // bookkeeping about them, across every voice it's ever been generated
  // under - see store.DeleteBookAudio) without touching its text,
  // chapters, speaker attribution, or voice settings - see
  // httpapi.handleDeleteBookAudio. Every paragraph reports pending again
  // afterward, so reopening the book/regenerating starts completely fresh.
  deleteBookAudio: (id: string) => call('DELETE /api/books/{id}/audio', { path: { id } }),

  // Chapter-header "Clear generation" - deleteBookAudio's single-chapter
  // counterpart, see httpapi.handleDeleteChapterAudio/store.DeleteChapterAudio.
  deleteChapterAudio: (bookId: string, chapterIdx: number) =>
    call('DELETE /api/books/{id}/chapters/{idx}/audio', { path: { id: bookId, idx: chapterIdx } }),

  // Chapter-header "Re-import" - re-parses one chapter from the book's
  // stored source epub and replaces its content (httpapi.
  // handleReimportChapter). file uploads the epub too (and keeps it as the
  // stored copy); force skips the chapter-title check.
  reimportChapter: (bookId: string, chapterIdx: number, opts: { file?: File; force?: boolean } = {}) => {
    const body = new FormData()
    if (opts.file) body.append('file', opts.file)
    return call('POST /api/books/{id}/chapters/{idx}/reimport', {
      path: { id: bookId, idx: chapterIdx },
      query: { force: opts.force ? true : undefined },
      body,
    })
  },

  generateChapter: (bookId: string, idx: number) =>
    call('POST /api/books/{id}/chapters/{idx}/generate', { path: { id: bookId, idx } }),

  // Re-renders one already-generated paragraph from scratch (the reader
  // didn't like how it came out), not the "hasn't been generated yet"
  // path generateChapter covers.
  regenerateParagraph: (bookId: string, chapterIdx: number, paragraphIdx: number) =>
    call('POST /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/regenerate', { path: { id: bookId, idx: chapterIdx, pidx: paragraphIdx } }),

  // Corrects one paragraph's speaker attribution directly - the per-line
  // counterpart of mergeCharacter's whole-character reassignment, used by
  // the Speakers page's per-character "appearances" list to fix one
  // individually misattributed line without touching any other paragraph
  // sharing the same (wrong) speaker. "" and "Narrator" both mean "no
  // character"; any other name is registered as a real character if it
  // wasn't one already.
  setParagraphSpeaker: (bookId: string, chapterIdx: number, paragraphIdx: number, speaker: string) =>
    call('PUT /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/speaker', { path: { id: bookId, idx: chapterIdx, pidx: paragraphIdx }, body: { speaker } }),

  // setParagraphSpeaker's own counterpart for a *description* line (a
  // narration paragraph the Describe pass tagged as describing a
  // character's appearance/personality, not a spoken line) - used by the
  // Speakers page's per-character "descriptions" list to move one
  // individually misattributed description from `from` to `to`, without
  // touching any other character that same paragraph might also describe
  // (a paragraph can describe more than one person). "" and "Narrator" for
  // `to` both mean "describes no one now"; any other name is registered as
  // a real character if it wasn't one already.
  setParagraphDescription: (bookId: string, chapterIdx: number, paragraphIdx: number, from: string, to: string) =>
    call('PUT /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/description', { path: { id: bookId, idx: chapterIdx, pidx: paragraphIdx }, body: { from, to } }),

  // Marks (or unmarks) one quoted paragraph as a scare quote live - a
  // reader's own manual correction of speakerattr's ScareQuoteChapter pass.
  // Unlike setParagraphSpeaker/setParagraphDescription, this immediately
  // invalidates the paragraph's own already-generated audio server-side
  // (see httpapi.handleSetParagraphScareQuote), so it re-generates -
  // merged with its narration neighbors, if any - next time it's needed.
  setParagraphScareQuote: (bookId: string, chapterIdx: number, paragraphIdx: number, scareQuote: boolean) =>
    call('PUT /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/scare-quote', { path: { id: bookId, idx: chapterIdx, pidx: paragraphIdx }, body: { scareQuote } }),

  // Manually overrides one dialogue line's emotion (an id from
  // utils/emotions.ts, or '' for neutral) - see
  // httpapi.handleSetParagraphEmotion. The line's audio regenerates
  // server-side when its effective emotion changes. 400 for narration.
  setParagraphEmotion: (bookId: string, chapterIdx: number, paragraphIdx: number, emotion: string) =>
    call('PUT /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/emotion', { path: { id: bookId, idx: chapterIdx, pidx: paragraphIdx }, body: { emotion } }),

  // Re-renders the emotion variant `speaker`'s lines clone from, resetting
  // this book's audio for their lines in that emotion - see
  // httpapi.handleRegenerateVariant. Fire-and-forget; the speakers topic
  // reports the new status once it renders.
  regenerateVariant: (bookId: string, speaker: string, emotion: string) =>
    call('POST /api/books/{id}/speakers/variants/regenerate', { path: { id: bookId }, body: { speaker, emotion } }),

  // Sound-effect (Stable Audio SFX) test surface (see backend/CLAUDE.md's
  // "SFX sound effects" section) - saves a paragraph's own text-to-audio
  // prompt and/or trigger word without generating anything yet.
  setParagraphSFXPrompt: (
    bookId: string,
    chapterIdx: number,
    paragraphIdx: number,
    prompt: string,
    triggerWord?: number,
  ) =>
    call('PUT /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/sfx-prompt', { path: { id: bookId, idx: chapterIdx, pidx: paragraphIdx }, body: { prompt, triggerWord } }),

  // Renders (or re-renders) one paragraph's sound effect - prompt, if
  // given, is saved first (same as calling setParagraphSFXPrompt just
  // before this), so the test UI can save+generate in one click; omit it
  // to regenerate from whatever prompt is already saved. Dispatched
  // through the backend's own pooled job queue (see httpapi.
  // handleGenerateParagraphSFX's own doc comment) - returns 202
  // immediately, not the finished clip; completion arrives on the chapter
  // topic (sfxStatus - see api/queries.ts).
  generateParagraphSFX: (
    bookId: string,
    chapterIdx: number,
    paragraphIdx: number,
    prompt?: string,
    durationSeconds?: number,
  ) =>
    call('POST /api/books/{id}/chapters/{idx}/paragraphs/{pidx}/generate-sfx', {
      path: { id: bookId, idx: chapterIdx, pidx: paragraphIdx },
      body: { prompt: prompt ?? '', durationSeconds },
    }),

  // Speakers page's "Retag scare quotes" - enqueues a scare-quote tagging
  // job for one chapter (fire-and-forget, 202), mirroring
  // retagDescriptions below.
  retagScareQuotes: (bookId: string, chapterIdx: number) =>
    call('POST /api/books/{id}/chapters/{idx}/retag-scare-quotes', { path: { id: bookId, idx: chapterIdx } }),

  // Keeps some runway of generated audio ahead of (chapterIdx, paragraphIdx),
  // spanning into later chapters as needed - see jobs.Manager.EnqueueLookahead.
  lookahead: (bookId: string, chapterIdx: number, paragraphIdx: number) =>
    call('POST /api/books/{id}/lookahead', { path: { id: bookId }, body: { chapterIdx, paragraphIdx } }),

  // Triggers background-music tone-region scoring for one chapter -
  // attributeSpeakers/tagDirections' own exact fire-and-forget shape, see
  // backend httpapi.handleScoreChapterMusic. Explicit and always allowed
  // regardless of the book-wide musicEnabled toggle (VoiceSettings) - that
  // only gates whether a scored region actually goes on to generate.
  scoreChapterMusic: (bookId: string, chapterIdx: number) =>
    call('POST /api/books/{id}/chapters/{idx}/score-music', { path: { id: bookId, idx: chapterIdx } }),

  // Generates every music region in one chapter that doesn't have a clip
  // yet, retrying failed ones - see backend httpapi.handleGenerateChapterMusic.
  // Runs regardless of the book-wide musicEnabled toggle. queued is how many
  // regions went out (0 if all already have music); 409 if the chapter
  // isn't scored or its narration isn't fully generated yet.
  generateChapterMusic: (bookId: string, chapterIdx: number) =>
    call('POST /api/books/{id}/chapters/{idx}/generate-music', { path: { id: bookId, idx: chapterIdx } }),

  // Generates (or re-generates) one music region's own clip - the
  // annotations-view boundary marker's own "Generate"/"Regenerate"
  // button, see backend httpapi.handleRegenerateMusicRegion. Always
  // allowed regardless of the region's current status.
  regenerateMusicRegion: (regionId: string) =>
    call('POST /api/music-regions/{id}/regenerate', { path: { id: regionId } }),

  updateVoice: (bookId: string, settings: VoiceSettingsUpdate) =>
    call('PUT /api/books/{id}/voice', { path: { id: bookId }, body: settings }),

  // Clears this book's own paragraph attribution and deletes the whole
  // series scope's character roster - identity, voice assignments, and
  // auto-created voice presets + cached clips - see
  // httpapi.handleDeleteBookSpeakerData.
  deleteSpeakerData: (bookId: string) => call('DELETE /api/books/{id}/speakers', { path: { id: bookId } }),

  // Enqueues LLM speaker attribution for one chapter (503 if the backend
  // has no SPEAKER_LLM_MODEL_PATH configured) - registers any
  // newly-discovered character; their voice is provisioned lazily later,
  // the first time it's actually needed for generation. Fire-and-forget,
  // same {queued} shape as generateChapter/enqueueParagraphRegenerate:
  // attribution runs a whole chapter through the LLM in batches and can
  // take minutes, so this no longer blocks on the actual run finishing -
  // see backend jobs.Manager.EnqueueAttribution's own doc comment.
  // Progress/completion is observed via useAttributingChapters (the live
  // jobs topic), not this call's own response.
  attributeSpeakers: (bookId: string, chapterIdx: number) =>
    call('POST /api/books/{id}/chapters/{idx}/attribute-speakers', { path: { id: bookId, idx: chapterIdx } }),

  // Enqueues a description-tagging job for one chapter (503 if the backend
  // has no SPEAKER_LLM_MODEL_PATH configured) - fire-and-forget like
  // attributeSpeakers; the job waits on the chapter's scare-quote tagging
  // first. See httpapi.handleRetagDescriptions.
  retagDescriptions: (bookId: string, chapterIdx: number) =>
    call('POST /api/books/{id}/chapters/{idx}/retag-descriptions', { path: { id: bookId, idx: chapterIdx } }),

  // Enqueues emotion labeling (each dialogue line gets an emotion from
  // utils/emotions.ts, or stays neutral) for one chapter - fire-and-forget,
  // same {queued} shape as attributeSpeakers, for the same reason: it runs
  // the LLM over a whole chapter in batches and can take a while. Any
  // clone model. 503 if the backend has no SPEAKER_LLM_MODEL_PATH
  // configured - see httpapi.handleTagDirections. Progress/completion is observed the same
  // way attribution's is (GET /api/jobs), not this call's own response.
  tagDirections: (bookId: string, chapterIdx: number) =>
    call('POST /api/books/{id}/chapters/{idx}/tag-directions', { path: { id: bookId, idx: chapterIdx } }),

  // Enqueues pronunciation resolution (ambiguous abbreviations like "Dr." ->
  // "Doctor") for one chapter - fire-and-forget, same {queued} shape as
  // tagDirections. 503 without SPEAKER_LLM_MODEL_PATH - see
  // httpapi.handleResolvePronunciation.
  resolvePronunciation: (bookId: string, chapterIdx: number) =>
    call('POST /api/books/{id}/chapters/{idx}/resolve-pronunciation', { path: { id: bookId, idx: chapterIdx } }),

  // Kicks off the whole-book meta-task: attribution, then characterization,
  // then voice provisioning, then direction-tagging, for every chapter/
  // character in the book, in the backend, without needing the Speakers
  // page's four individual "all" buttons clicked in order. Fire-and-forget,
  // same {queued} shape as attributeSpeakers/tagDirections - a whole book
  // can take a long time. 503 if SPEAKER_LLM_MODEL_PATH is unconfigured,
  // 409 if a run is already in progress for this book - see
  // httpapi.handlePreprocessBook. Progress is observable via
  // BookSummary.preprocessing (the books topic) and the jobs topic, same
  // as the individual buttons.
  preprocessBook: (bookId: string) => call('POST /api/books/{id}/preprocess', { path: { id: bookId } }),

  // Enqueues background TTS generation for every chapter in the book at
  // once - the library page's hover "Generate audio" button. Fire-and-
  // forget, same {queued} shape as preprocessBook - see
  // httpapi.handleGenerateBook. Idempotent: safe to call again while a
  // previous call is still generating, or on a book that's already fully
  // generated (each chapter's own enqueue is a no-op once ready).
  generateBook: (bookId: string) => call('POST /api/books/{id}/generate', { path: { id: bookId } }),

  // "Remaining" option alongside generateBook's "All": enqueues background
  // TTS generation for every paragraph from the book's current stored
  // reading position through the end, instead of the whole book from
  // chapter 0 - see httpapi.handleGenerateRemaining. Same fire-and-forget
  // {queued} shape; no body needed, position is read server-side from the
  // book itself.
  generateRemaining: (bookId: string) => call('POST /api/books/{id}/generate-remaining', { path: { id: bookId } }),

  // One whole-book Speakers-page action, queued server-side as a single
  // cancelable Jobs row ("pipeline_bulk_<action>") wrapping every
  // per-chapter/per-character task it fans out into - see
  // httpapi.handleBulkAction. scope "rest" skips what's already done, "all"
  // re-runs everything. queued is how many chapters/characters it covers.
  bulkAction: (bookId: string, action: BulkAction, scope: BulkScope) =>
    call('POST /api/books/{id}/bulk/{action}', { path: { id: bookId, action }, query: { scope } }),

  // Clears one pass across the whole book as though it never ran, deleting
  // whatever generated audio it made stale - see httpapi.handleResetPass.
  // pass uses bulkAction's names ("generate" = every narration clip).
  resetPass: (bookId: string, pass: ResetPass) =>
    call('POST /api/books/{id}/reset/{pass}', { path: { id: bookId, pass } }),

  // Assigns (or, with "", clears) a character's own narration voice.
  setCharacterVoice: (bookId: string, characterId: string, voicePresetId: string) =>
    call('PUT /api/books/{id}/characters/{characterId}/voice', { path: { id: bookId, characterId }, body: { voicePresetId } }),

  // Marks (or unmarks) a character as not a real speaker, so attribution
  // stops creating/assigning the name - non-destructive: their current
  // lines, summary, and voice stay as they are. Auto Split sets this too.
  // See httpapi.handleSetCharacterInvalid.
  setCharacterInvalid: (bookId: string, characterId: string, invalid: boolean) =>
    call('PUT /api/books/{id}/characters/{characterId}/invalid', { path: { id: bookId, characterId }, body: { invalid } }),

  // Replaces one character's aliases - see Speaker.aliases. 400 for a
  // group ("Bert and Sid") or non-name, 409 for another character's name
  // or alias (merge them instead).
  setCharacterAliases: (bookId: string, characterId: string, aliases: string[]) =>
    call('PUT /api/books/{id}/characters/{characterId}/aliases', { path: { id: bookId, characterId }, body: { aliases } }),

  // Removes one character entirely - this book's own paragraphs
  // attributed to them revert to Unknown (still real dialogue, just no
  // longer attributed to this character - "Narrator" is reserved for
  // actual narration), their identity/voice assignments/auto-created
  // voice presets are deleted for their whole series scope - see
  // httpapi.handleDeleteCharacter.
  deleteCharacter: (bookId: string, characterId: string) =>
    call('DELETE /api/books/{id}/characters/{characterId}', { path: { id: bookId, characterId } }),

  // Folds one character into another (or into "Narrator") - this book's
  // own paragraphs attributed to them are reattributed to targetName
  // instead of blanked, and their own identity/voice/presets are deleted
  // for their whole series scope - see httpapi.handleMergeCharacter.
  mergeCharacter: (bookId: string, characterId: string, targetName: string) =>
    call('POST /api/books/{id}/characters/{characterId}/merge', { path: { id: bookId, characterId }, body: { targetName } }),

  // "Auto Split": for a speaker believed not to be a real, distinct
  // individual (a false-positive character attribution, or the literal
  // "Unknown" sentinel itself - unlike every other character action, this
  // is addressed by name, not id, since Unknown has no characters-table
  // row of its own), re-runs LLM attribution over every chapter
  // containing one of their paragraphs and redistributes each one to
  // whichever real character/Narrator/Unknown actually speaks it - see
  // httpapi.handleReattributeSpeaker. Fire-and-forget, same {queued}
  // shape as attributeSpeakers/tagDirections (queued here counts chapters
  // dispatched, not paragraphs) - 503 if SPEAKER_LLM_MODEL_PATH is
  // unconfigured, 404 if name isn't "Unknown" and isn't a real character
  // either. Progress is observable via the jobs topic
  // (kind: 'speaker-reattribute'), and the speakers topic reflects each
  // chapter's result as its task clears. A real character (not Unknown)
  // is also marked invalid (see setCharacterInvalid) so attribution won't
  // hand their lines straight back. Deliberately doesn't delete
  // the character's own identity/voice afterward even if it ends up
  // empty - a reader can follow up with deleteCharacter once they see
  // the row is actually empty.
  reattributeSpeaker: (bookId: string, name: string) =>
    call('POST /api/books/{id}/speakers/reattribute', { path: { id: bookId }, body: { name } }),

  // Force-provisions a voice for one character right now instead of
  // waiting for the lazy path to trigger it at generation time - see
  // httpapi.handleGenerateCharacterVoice. 409 if there still isn't enough
  // attributed dialogue to characterize them from yet.
  generateCharacterVoice: (bookId: string, characterId: string) =>
    call('POST /api/books/{id}/characters/{characterId}/generate-voice', { path: { id: bookId, characterId } }),

  // Batch-forces a fresh reference-clip render for every given character
  // id in one request - fire-and-forget (202, no per-character result) via
  // httpapi.handleRegenerateCharacterVoices, regardless of whether a
  // character already has a cached voice.
  regenerateCharacterVoices: (bookId: string, characterIds: string[]) =>
    call('POST /api/books/{id}/characters/regenerate-voice', { path: { id: bookId }, body: { characterIds } }),

  // Force re-runs characterization for one character (503 if the backend
  // has no SPEAKER_LLM_MODEL_PATH configured), even if it's already been
  // characterized - unlike attributeSpeakers' own auto-discovery, which
  // only ever characterizes once. Invalidates (not re-renders) this
  // book's own resolved clone model's assigned voice preset for the
  // character, if any - see httpapi.handleCharacterizeSpeaker.
  characterizeSpeaker: (bookId: string, characterId: string) =>
    call('POST /api/books/{id}/characters/{characterId}/characterize', { path: { id: bookId, characterId } }),

  getPosition: (bookId: string) => call('GET /api/books/{id}/position', { path: { id: bookId } }),

  updatePosition: (bookId: string, position: Position) =>
    call('PUT /api/books/{id}/position', { path: { id: bookId }, body: position }),

  searchBook: (bookId: string, q: string) =>
    call('GET /api/books/{id}/search', { path: { id: bookId }, query: { q } }),

  // Flags (chapterIdx, paragraphIdx) as bookmarked, or updates its note if
  // it already is - see backend Store.UpsertBookmark.
  createBookmark: (bookId: string, chapterIdx: number, paragraphIdx: number, note = '') =>
    call('POST /api/books/{id}/bookmarks', { path: { id: bookId }, body: { chapterIdx, paragraphIdx, note } }),

  updateBookmarkNote: (id: string, note: string) =>
    call('PUT /api/bookmarks/{id}', { path: { id }, body: { note } }),

  deleteBookmark: (id: string) => call('DELETE /api/bookmarks/{id}', { path: { id } }),

  // Cancels one job-queue task by id (QueueTask.id) - see
  // httpapi.handleCancelJob. encodeURIComponent since an LLM-kind task's id
  // carries a ":" (e.g. "attr:<chapterId>") - harmless unescaped in a path
  // segment, but escaping it is the correct general habit regardless of
  // which ids happen to need it today.
  cancelJob: (id: string) => call('DELETE /api/jobs/{id}', { path: { id } }),

  // Raises one job-queue task's own priority tier - see
  // httpapi.handleSetJobTier/jobs.Manager.PromoteTier. Upgrade only: the
  // backend rejects (409) a tier that isn't strictly more urgent than the
  // task's current one, so this is never a general "set to anything" -
  // the Jobs dashboard's own priority menu only ever offers tiers more
  // urgent than a row's current one for exactly that reason (see
  // JobsPage's own TIER_ORDER/upgradeOptions).
  setJobTier: (id: string, tier: JobTier) =>
    call('PUT /api/jobs/{id}/tier', { path: { id }, body: { tier } }),

  // Cancels every queued and in-flight task at once - see
  // httpapi.handleCancelAllJobs.
  cancelAllJobs: () => call('DELETE /api/jobs'),

  // Stops the queue from dispatching any *new* task - see
  // httpapi.handlePauseJobs. Not a cancel: whatever's already in flight
  // keeps running to completion.
  pauseJobs: () => call('POST /api/jobs/pause'),

  // Undoes pauseJobs - see httpapi.handleResumeJobs.
  resumeJobs: () => call('POST /api/jobs/resume'),

  // Forces an immediate ttsworker restart - see httpapi.handleRestartWorker.
  // Blocks until the new worker is confirmed healthy, so this can take a
  // few seconds.
  restartWorker: () => call('POST /api/jobs/restart-worker'),

  voiceLanguages: () => call('GET /api/voices/languages'),

  updateDefaultVoice: (settings: VoiceSettingsUpdate) => call('PUT /api/voices/default', { body: settings }),

  createCustomVoicePreset: (input: CustomVoicePresetInput) => call('POST /api/voices/custom-presets', { body: input }),

  updateCustomVoicePreset: (id: string, input: CustomVoicePresetInput) =>
    call('PUT /api/voices/custom-presets/{id}', { path: { id }, body: input }),

  deleteCustomVoicePreset: (id: string) => call('DELETE /api/voices/custom-presets/{id}', { path: { id } }),

  // Force re-renders a preset's reference clip from its currently-saved
  // recipe, without changing any saved fields - see
  // httpapi.handleRegenerateCustomVoicePreset. Useful to retry after a
  // refError, or just to pick up a fresh render.
  regenerateCustomVoicePreset: (id: string) => call('POST /api/voices/custom-presets/{id}/regenerate', { path: { id } }),

  // Synthesizes arbitrary text with an existing custom preset's actual
  // saved voice, for previewing in the editor. Returns a playable blob
  // rather than JSON, so it bypasses the request() helper. `cloneModel`
  // is which clone model to preview through (a voice has none of its own);
  // omitted, the backend uses the default clone model new books get.
  testCustomVoicePreset: (id: string, text: string, cloneModel?: string, temperature?: number) =>
    call('POST /api/voices/custom-presets/{id}/test', { path: { id }, body: { text, cloneModel, temperature } }),

  // Same, for a curated built-in preset.
  testPreset: (id: string, text: string, cloneModel?: string, temperature?: number) =>
    call('POST /api/voices/presets/{id}/test', { path: { id }, body: { text, cloneModel, temperature } }),

  // Force re-renders a built-in preset's reference clip from its fixed,
  // compiled-in recipe - see httpapi.handleRegeneratePreset.
  regeneratePreset: (id: string) => call('POST /api/voices/presets/{id}/regenerate', { path: { id } }),

  // Previews a voice design directly (no preset id at all) - the only way
  // to hear an instruct before it's been saved. Passing seed lets the
  // backend cache the render by its exact (instruct, seed, text,
  // designModel) recipe, so saving the preset right after testing it,
  // unchanged, reuses this exact clip instead of rendering again - see
  // httpapi.handleTestVoiceDesign / handleCreateCustomVoicePreset.
  // designModel previews a specific VoiceDesign engine - "" defers to the
  // worker's own process-wide default.
  testVoiceDesign: (
    instruct: string,
    text: string,
    seed?: number,
    designModel?: string,
    guidanceScale?: number,
    temperature?: number,
  ) =>
    call('POST /api/voices/design-test', { body: { instruct, text, seed, designModel, guidanceScale, temperature } }),

  // Standalone sound-effect/music test (the SFX page) - stateless, not
  // attached to any book/paragraph; one endpoint for every generation-only
  // engine (see SFX_ENGINES), selected by opts.engine ("stable_audio_music"
  // if omitted) - see httpapi.handleGenerateSFX. Every field but prompt is
  // optional and defers to that engine's own built-in default when
  // omitted.
  testSFX: (opts: GenerateSFXRequest) => call('POST /api/sfx/generate', { body: opts }),

  // Standalone raw system+user prompt test against this app's own
  // embedded speaker-attribution GGUF model - stateless, no
  // speakerattr-specific framing; see httpapi.handleTestLLM.
  testLLM: (opts: TestLLMRequest) => call('POST /api/llm/test', { body: opts }),

  // Previews "instructed voice cloning" - clones baseId's own already-
  // rendered reference clip while applying instruct as a clone-time style
  // instruction (only actually honored by INSTRUCTED_CLONE_MODEL) - the
  // voice/character editor's "test with another voice as a base" control.
  // baseId is any existing voice id (built-in or custom); cloneModel is
  // which clone model to preview through (the default clone model new
  // books get, if omitted). guidanceScale, when
  // given, overrides the backend's own configured instruct-time
  // guidance_scale (LECTABLE_AUDIOCPP_BREEZE_CLONE_GUIDANCE_SCALE) for this
  // one preview call, letting a reader experiment with adherence strength
  // live - see httpapi.handleTestCloneInstruct.
  testCloneInstruct: (baseId: string, instruct: string, text: string, cloneModel?: string, guidanceScale?: string) =>
    call('POST /api/voices/clone-instruct-test', { body: { baseId, instruct, text, cloneModel, guidanceScale } }),
}

export { ApiError }
