import type {
  Bookmark,
  BookDetail,
  BookSummary,
  ChapterDetail,
  ChapterMusic,
  CustomVoicePreset,
  CustomVoicePresetInput,
  JobsSnapshot,
  LLMTestOptions,
  Position,
  QueueTask,
  SearchResult,
  SFXTestOptions,
  Speaker,
  SpeakerAppearance,
  VoicePreset,
  VoicePresets,
  VoiceSettings,
} from './types'

class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  // Belt-and-suspenders alongside the backend's own Cache-Control:
  // no-store (see httpapi.writeJSON) - several of these hit a fixed URL
  // on a short poll (the Jobs page, a chapter with pending paragraphs),
  // so a browser serving a cached response instead of actually asking
  // the network would make a live view look frozen even while the
  // backend keeps progressing.
  const res = await fetch(path, { cache: 'no-store', ...init })
  if (!res.ok) {
    let message = `HTTP ${res.status}`
    try {
      const body = await res.json()
      if (body?.error) message = body.error
    } catch {
      // ignore non-JSON error bodies
    }
    throw new ApiError(res.status, message)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

function json(method: string, body: unknown): RequestInit {
  return {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }
}

async function requestBlob(path: string, init: RequestInit): Promise<Blob> {
  const res = await fetch(path, init)
  if (!res.ok) {
    let message = `HTTP ${res.status}`
    try {
      const body = await res.json()
      if (body?.error) message = body.error
    } catch {
      // ignore non-JSON error bodies
    }
    throw new ApiError(res.status, message)
  }
  return res.blob()
}

export const api = {
  listBooks: () => request<BookSummary[]>('/api/books'),

  getBook: (id: string) => request<BookDetail>(`/api/books/${id}`),

  uploadBook: async (file: File): Promise<BookSummary> => {
    const form = new FormData()
    form.append('file', file)
    return request<BookSummary>('/api/books', { method: 'POST', body: form })
  },

  deleteBook: (id: string) => request<void>(`/api/books/${id}`, { method: 'DELETE' }),

  // Deletes every generated audio file for this book (and the DB's own
  // bookkeeping about them, across every voice it's ever been generated
  // under - see store.DeleteBookAudio) without touching its text,
  // chapters, speaker attribution, or voice settings - see
  // httpapi.handleDeleteBookAudio. Every paragraph reports pending again
  // afterward, so reopening the book/regenerating starts completely fresh.
  deleteBookAudio: (id: string) => request<{ ok: boolean }>(`/api/books/${id}/audio`, { method: 'DELETE' }),

  // Chapter-header "Clear generation" - deleteBookAudio's single-chapter
  // counterpart, see httpapi.handleDeleteChapterAudio/store.DeleteChapterAudio.
  deleteChapterAudio: (bookId: string, chapterIdx: number) =>
    request<{ ok: boolean }>(`/api/books/${bookId}/chapters/${chapterIdx}/audio`, { method: 'DELETE' }),

  getChapter: (bookId: string, idx: number) =>
    request<ChapterDetail>(`/api/books/${bookId}/chapters/${idx}`),

  generateChapter: (bookId: string, idx: number) =>
    request<{ queued: boolean }>(`/api/books/${bookId}/chapters/${idx}/generate`, { method: 'POST' }),

  // Re-renders one already-generated paragraph from scratch (the reader
  // didn't like how it came out), not the "hasn't been generated yet"
  // path generateChapter covers.
  regenerateParagraph: (bookId: string, chapterIdx: number, paragraphIdx: number) =>
    request<{ queued: boolean }>(
      `/api/books/${bookId}/chapters/${chapterIdx}/paragraphs/${paragraphIdx}/regenerate`,
      { method: 'POST' },
    ),

  // Corrects one paragraph's speaker attribution directly - the per-line
  // counterpart of mergeCharacter's whole-character reassignment, used by
  // the Speakers page's per-character "appearances" list to fix one
  // individually misattributed line without touching any other paragraph
  // sharing the same (wrong) speaker. "" and "Narrator" both mean "no
  // character"; any other name is registered as a real character if it
  // wasn't one already.
  setParagraphSpeaker: (bookId: string, chapterIdx: number, paragraphIdx: number, speaker: string) =>
    request<{ ok: boolean }>(
      `/api/books/${bookId}/chapters/${chapterIdx}/paragraphs/${paragraphIdx}/speaker`,
      json('PUT', { speaker }),
    ),

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
    request<{ ok: boolean }>(
      `/api/books/${bookId}/chapters/${chapterIdx}/paragraphs/${paragraphIdx}/description`,
      json('PUT', { from, to }),
    ),

  // Marks (or unmarks) one quoted paragraph as a scare quote live - a
  // reader's own manual correction of speakerattr's ScareQuoteChapter pass.
  // Unlike setParagraphSpeaker/setParagraphDescription, this immediately
  // invalidates the paragraph's own already-generated audio server-side
  // (see httpapi.handleSetParagraphScareQuote), so it re-generates -
  // merged with its narration neighbors, if any - next time it's needed.
  setParagraphScareQuote: (bookId: string, chapterIdx: number, paragraphIdx: number, scareQuote: boolean) =>
    request<{ ok: boolean }>(
      `/api/books/${bookId}/chapters/${chapterIdx}/paragraphs/${paragraphIdx}/scare-quote`,
      json('PUT', { scareQuote }),
    ),

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
    request<{ ok: boolean }>(
      `/api/books/${bookId}/chapters/${chapterIdx}/paragraphs/${paragraphIdx}/sfx-prompt`,
      json('PUT', { prompt, triggerWord }),
    ),

  // Renders (or re-renders) one paragraph's sound effect - prompt, if
  // given, is saved first (same as calling setParagraphSFXPrompt just
  // before this), so the test UI can save+generate in one click; omit it
  // to regenerate from whatever prompt is already saved. Dispatched
  // through the backend's own pooled job queue (see httpapi.
  // handleGenerateParagraphSFX's own doc comment) - returns 202
  // immediately, not the finished clip; the chapter query's own poll
  // (see api/queries.ts) picks up completion.
  generateParagraphSFX: (
    bookId: string,
    chapterIdx: number,
    paragraphIdx: number,
    prompt?: string,
    durationSeconds?: number,
  ) =>
    request<{ queued: boolean }>(
      `/api/books/${bookId}/chapters/${chapterIdx}/paragraphs/${paragraphIdx}/generate-sfx`,
      json('POST', { prompt, durationSeconds }),
    ),

  // Speakers page's "Retag scare quotes" - force re-runs scare-quote
  // tagging for one chapter, mirroring retagDescriptions below.
  retagScareQuotes: (bookId: string, chapterIdx: number) =>
    request<{ ok: boolean }>(`/api/books/${bookId}/chapters/${chapterIdx}/retag-scare-quotes`, { method: 'POST' }),

  // Keeps some runway of generated audio ahead of (chapterIdx, paragraphIdx),
  // spanning into later chapters as needed - see jobs.Manager.EnqueueLookahead.
  lookahead: (bookId: string, chapterIdx: number, paragraphIdx: number) =>
    request<{ queued: boolean }>(
      `/api/books/${bookId}/lookahead`,
      json('POST', { chapterIdx, paragraphIdx }),
    ),

  // Background-music regions for one chapter - see backend
  // httpapi.handleGetChapterMusic. useBackgroundMusic polls this while
  // playing a chapter with the book's own musicEnabled turned on.
  getChapterMusic: (bookId: string, chapterIdx: number) =>
    request<ChapterMusic>(`/api/books/${bookId}/chapters/${chapterIdx}/music`),

  // Triggers background-music tone-region scoring for one chapter -
  // attributeSpeakers/tagDirections' own exact fire-and-forget shape, see
  // backend httpapi.handleScoreChapterMusic. Explicit and always allowed
  // regardless of the book-wide musicEnabled toggle (VoiceSettings) - that
  // only gates whether a scored region actually goes on to generate.
  scoreChapterMusic: (bookId: string, chapterIdx: number) =>
    request<{ queued: boolean }>(`/api/books/${bookId}/chapters/${chapterIdx}/score-music`, { method: 'POST' }),

  // Generates (or re-generates) one music region's own clip - the
  // annotations-view boundary marker's own "Generate"/"Regenerate"
  // button, see backend httpapi.handleRegenerateMusicRegion. Always
  // allowed regardless of the region's current status.
  regenerateMusicRegion: (regionId: string) =>
    request<{ queued: boolean }>(`/api/music-regions/${regionId}/regenerate`, { method: 'POST' }),

  getVoice: (bookId: string) => request<VoiceSettings>(`/api/books/${bookId}/voice`),

  updateVoice: (bookId: string, settings: VoiceSettings) =>
    request<VoiceSettings>(`/api/books/${bookId}/voice`, json('PUT', settings)),

  listSpeakers: (bookId: string) => request<Speaker[]>(`/api/books/${bookId}/speakers`),

  // Clears this book's own paragraph attribution and deletes the whole
  // series scope's character roster - identity, voice assignments, and
  // auto-created voice presets + cached clips - see
  // httpapi.handleDeleteBookSpeakerData.
  deleteSpeakerData: (bookId: string) => request<{ ok: boolean }>(`/api/books/${bookId}/speakers`, { method: 'DELETE' }),

  // Enqueues LLM speaker attribution for one chapter (503 if the backend
  // has no SPEAKER_LLM_MODEL_PATH configured) - registers any
  // newly-discovered character; their voice is provisioned lazily later,
  // the first time it's actually needed for generation. Fire-and-forget,
  // same {queued} shape as generateChapter/enqueueParagraphRegenerate:
  // attribution runs a whole chapter through the LLM in batches and can
  // take minutes, so this no longer blocks on the actual run finishing -
  // see backend jobs.Manager.EnqueueAttribution's own doc comment.
  // Progress/completion is observed via useAttributingChapters (polls
  // GET /api/jobs), not this call's own response.
  attributeSpeakers: (bookId: string, chapterIdx: number) =>
    request<{ queued: boolean }>(`/api/books/${bookId}/chapters/${chapterIdx}/attribute-speakers`, {
      method: 'POST',
    }),

  // Force re-runs description-tagging for one chapter (503 if the backend
  // has no SPEAKER_LLM_MODEL_PATH configured) - unlike attributeSpeakers,
  // this blocks until the run actually finishes: it's a single chapter's
  // worth of small batches, fast enough not to need the fire-and-forget/
  // job-queue treatment attribution itself needs - see
  // httpapi.handleRetagDescriptions.
  retagDescriptions: (bookId: string, chapterIdx: number) =>
    request<{ ok: boolean }>(`/api/books/${bookId}/chapters/${chapterIdx}/retag-descriptions`, {
      method: 'POST',
    }),

  // Enqueues speech-direction tagging (Higgs's own inline delivery tags,
  // e.g. <|emotion:anger|>) for one chapter - fire-and-forget, same
  // {queued} shape as attributeSpeakers, for the same reason: it runs the
  // LLM over a whole chapter in batches and can take a while. 400 if the
  // book's resolved clone model isn't Higgs's (audiocpp-higgs-4b), 503 if
  // the backend has no SPEAKER_LLM_MODEL_PATH configured - see
  // httpapi.handleTagDirections. Progress/completion is observed the same
  // way attribution's is (GET /api/jobs), not this call's own response.
  tagDirections: (bookId: string, chapterIdx: number) =>
    request<{ queued: boolean }>(`/api/books/${bookId}/chapters/${chapterIdx}/tag-directions`, {
      method: 'POST',
    }),

  // Kicks off the whole-book meta-task: attribution, then characterization,
  // then voice provisioning, then direction-tagging, for every chapter/
  // character in the book, in the backend, without needing the Speakers
  // page's four individual "all" buttons clicked in order. Fire-and-forget,
  // same {queued} shape as attributeSpeakers/tagDirections - a whole book
  // can take a long time. 503 if SPEAKER_LLM_MODEL_PATH is unconfigured,
  // 409 if a run is already in progress for this book - see
  // httpapi.handlePreprocessBook. Progress is observable via
  // BookSummary.preprocessing (see useBooks' own polling) and GET
  // /api/jobs, same as the individual buttons.
  preprocessBook: (bookId: string) =>
    request<{ queued: boolean }>(`/api/books/${bookId}/preprocess`, { method: 'POST' }),

  // Enqueues background TTS generation for every chapter in the book at
  // once - the library page's hover "Generate audio" button. Fire-and-
  // forget, same {queued} shape as preprocessBook - see
  // httpapi.handleGenerateBook. Idempotent: safe to call again while a
  // previous call is still generating, or on a book that's already fully
  // generated (each chapter's own enqueue is a no-op once ready).
  generateBook: (bookId: string) => request<{ queued: boolean }>(`/api/books/${bookId}/generate`, { method: 'POST' }),

  // "Remaining" option alongside generateBook's "All": enqueues background
  // TTS generation for every paragraph from the book's current stored
  // reading position through the end, instead of the whole book from
  // chapter 0 - see httpapi.handleGenerateRemaining. Same fire-and-forget
  // {queued} shape; no body needed, position is read server-side from the
  // book itself.
  generateRemaining: (bookId: string) =>
    request<{ queued: boolean }>(`/api/books/${bookId}/generate-remaining`, { method: 'POST' }),

  // Assigns (or, with "", clears) a character's own narration voice.
  setCharacterVoice: (bookId: string, characterId: string, voicePresetId: string) =>
    request<{ ok: boolean }>(`/api/books/${bookId}/characters/${characterId}/voice`, json('PUT', { voicePresetId })),

  // Removes one character entirely - this book's own paragraphs
  // attributed to them revert to Unknown (still real dialogue, just no
  // longer attributed to this character - "Narrator" is reserved for
  // actual narration), their identity/voice assignments/auto-created
  // voice presets are deleted for their whole series scope - see
  // httpapi.handleDeleteCharacter.
  deleteCharacter: (bookId: string, characterId: string) =>
    request<{ ok: boolean }>(`/api/books/${bookId}/characters/${characterId}`, { method: 'DELETE' }),

  // Folds one character into another (or into "Narrator") - this book's
  // own paragraphs attributed to them are reattributed to targetName
  // instead of blanked, and their own identity/voice/presets are deleted
  // for their whole series scope - see httpapi.handleMergeCharacter.
  mergeCharacter: (bookId: string, characterId: string, targetName: string) =>
    request<{ ok: boolean }>(`/api/books/${bookId}/characters/${characterId}/merge`, json('POST', { targetName })),

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
  // either. Progress is observable via GET /api/jobs
  // (kind: 'speaker-reattribute') and by refetching this book's speaker
  // table once each chapter's task clears. Deliberately doesn't delete
  // the character's own identity/voice afterward even if it ends up
  // empty - a reader can follow up with deleteCharacter once they see
  // the row is actually empty.
  reattributeSpeaker: (bookId: string, name: string) =>
    request<{ queued: number }>(`/api/books/${bookId}/speakers/reattribute`, json('POST', { name })),

  // Force-provisions a voice for one character right now instead of
  // waiting for the lazy path to trigger it at generation time - see
  // httpapi.handleGenerateCharacterVoice. 409 if there still isn't enough
  // attributed dialogue to characterize them from yet.
  generateCharacterVoice: (bookId: string, characterId: string) =>
    request<{ voicePresetId: string; audioUrl: string }>(
      `/api/books/${bookId}/characters/${characterId}/generate-voice`,
      { method: 'POST' },
    ),

  // Batch-enqueues voice provisioning for every given character id in one
  // request - fire-and-forget (202, no per-character result) via
  // httpapi.handleGenerateCharacterVoices, same shape as
  // characterizeSpeakers above. SpeakersPage's "Generate all voices".
  generateCharacterVoices: (bookId: string, characterIds: string[]) =>
    request<{ queued: number }>(
      `/api/books/${bookId}/characters/generate-voice`,
      json('POST', { characterIds }),
    ),

  // Batch-forces a fresh reference-clip render for every given character
  // id in one request - fire-and-forget (202, no per-character result) via
  // httpapi.handleRegenerateCharacterVoices. Unlike generateCharacterVoices
  // above, this re-renders regardless of whether a character already has a
  // cached voice - SpeakersPage's "Regenerate all voices".
  regenerateCharacterVoices: (bookId: string, characterIds: string[]) =>
    request<{ queued: number }>(
      `/api/books/${bookId}/characters/regenerate-voice`,
      json('POST', { characterIds }),
    ),

  characterAppearances: (bookId: string, characterId: string) =>
    request<SpeakerAppearance[]>(`/api/books/${bookId}/characters/${characterId}/appearances`),

  // Every paragraph tagged as describing this character's physical
  // appearance or personality (see httpapi.handleCharacterDescriptions) -
  // characterAppearances' own description-tagging counterpart, same DTO
  // shape (SpeakerAppearance is reused as-is; a description paragraph just
  // never has an audioUrl tied to *this* character's own voice, since they
  // don't speak it - see the backend's own doc comment).
  characterDescriptions: (bookId: string, characterId: string) =>
    request<SpeakerAppearance[]>(`/api/books/${bookId}/characters/${characterId}/descriptions`),

  // Force re-runs characterization for one character (503 if the backend
  // has no SPEAKER_LLM_MODEL_PATH configured), even if it's already been
  // characterized - unlike attributeSpeakers' own auto-discovery, which
  // only ever characterizes once. Invalidates (not re-renders) this
  // book's own resolved clone model's assigned voice preset for the
  // character, if any - see httpapi.handleCharacterizeSpeaker.
  characterizeSpeaker: (bookId: string, characterId: string) =>
    request<{ summary: string; voiceInvalidated: boolean }>(
      `/api/books/${bookId}/characters/${characterId}/characterize`,
      { method: 'POST' },
    ),

  // Batch-enqueues re-characterization for every given character id in one
  // request - fire-and-forget (202, no per-character result) via
  // httpapi.handleCharacterizeSpeakers/jobs.Manager.EnqueueCharacterization,
  // unlike characterizeSpeaker above which blocks until that one
  // character's result comes back. SpeakersPage's "Recharacterize all" -
  // see that handler's own doc comment for why a batch endpoint exists
  // instead of just firing characterizeSpeaker once per character from
  // here (the browser's own per-origin connection cap, plus a page reload
  // before every request even went out, could otherwise silently drop
  // whichever characters hadn't been dispatched yet). Progress shows up by
  // refetching this book's speaker table, same as any other
  // characterization.
  characterizeSpeakers: (bookId: string, characterIds: string[]) =>
    request<{ queued: number }>(
      `/api/books/${bookId}/characters/characterize`,
      json('POST', { characterIds }),
    ),

  getPosition: (bookId: string) => request<Position>(`/api/books/${bookId}/position`),

  updatePosition: (bookId: string, position: Position) =>
    request<void>(`/api/books/${bookId}/position`, json('PUT', position)),

  searchBook: (bookId: string, q: string) =>
    request<SearchResult[]>(`/api/books/${bookId}/search?q=${encodeURIComponent(q)}`),

  listBookmarks: (bookId: string) => request<Bookmark[]>(`/api/books/${bookId}/bookmarks`),

  // Flags (chapterIdx, paragraphIdx) as bookmarked, or updates its note if
  // it already is - see backend Store.UpsertBookmark.
  createBookmark: (bookId: string, chapterIdx: number, paragraphIdx: number, note = '') =>
    request<{ id: string }>(`/api/books/${bookId}/bookmarks`, json('POST', { chapterIdx, paragraphIdx, note })),

  updateBookmarkNote: (id: string, note: string) =>
    request<{ ok: boolean }>(`/api/bookmarks/${id}`, json('PUT', { note })),

  deleteBookmark: (id: string) => request<void>(`/api/bookmarks/${id}`, { method: 'DELETE' }),

  // Live TTS generation queue state - what's dispatched to tts-service
  // right now vs. still waiting - for the jobs dashboard.
  jobsSnapshot: () => request<JobsSnapshot>('/api/jobs'),

  // Cancels one job-queue task by id (QueueTask.id) - see
  // httpapi.handleCancelJob. encodeURIComponent since an LLM-kind task's id
  // carries a ":" (e.g. "attr:<chapterId>") - harmless unescaped in a path
  // segment, but escaping it is the correct general habit regardless of
  // which ids happen to need it today.
  cancelJob: (id: string) => request<{ canceled: boolean }>(`/api/jobs/${encodeURIComponent(id)}`, { method: 'DELETE' }),

  // Raises one job-queue task's own priority tier - see
  // httpapi.handleSetJobTier/jobs.Manager.PromoteTier. Upgrade only: the
  // backend rejects (409) a tier that isn't strictly more urgent than the
  // task's current one, so this is never a general "set to anything" -
  // the Jobs dashboard's own priority menu only ever offers tiers more
  // urgent than a row's current one for exactly that reason (see
  // JobsPage's own TIER_ORDER/upgradeOptions).
  setJobTier: (id: string, tier: QueueTask['tier']) =>
    request<{ promoted: boolean }>(`/api/jobs/${encodeURIComponent(id)}/tier`, json('PUT', { tier })),

  // Cancels every queued and in-flight task at once - see
  // httpapi.handleCancelAllJobs.
  cancelAllJobs: () => request<{ canceled: number }>('/api/jobs', { method: 'DELETE' }),

  // Stops the queue from dispatching any *new* task - see
  // httpapi.handlePauseJobs. Not a cancel: whatever's already in flight
  // keeps running to completion.
  pauseJobs: () => request<{ paused: boolean }>('/api/jobs/pause', { method: 'POST' }),

  // Undoes pauseJobs - see httpapi.handleResumeJobs.
  resumeJobs: () => request<{ paused: boolean }>('/api/jobs/resume', { method: 'POST' }),

  // Forces an immediate ttsworker restart - see httpapi.handleRestartWorker.
  // Blocks until the new worker is confirmed healthy, so this can take a
  // few seconds.
  restartWorker: () => request<{ restarted: boolean }>('/api/jobs/restart-worker', { method: 'POST' }),

  voicePresets: () => request<VoicePresets>('/api/voices/presets'),

  voiceLanguages: () => request<{ languages: string[] }>('/api/voices/languages'),

  getDefaultVoice: () => request<VoiceSettings>('/api/voices/default'),

  updateDefaultVoice: (settings: VoiceSettings) => request<VoiceSettings>('/api/voices/default', json('PUT', settings)),

  customVoicePresets: () => request<CustomVoicePreset[]>('/api/voices/custom-presets'),

  createCustomVoicePreset: (input: CustomVoicePresetInput) =>
    request<CustomVoicePreset>('/api/voices/custom-presets', json('POST', input)),

  updateCustomVoicePreset: (id: string, input: CustomVoicePresetInput) =>
    request<CustomVoicePreset>(`/api/voices/custom-presets/${id}`, json('PUT', input)),

  deleteCustomVoicePreset: (id: string) =>
    request<void>(`/api/voices/custom-presets/${id}`, { method: 'DELETE' }),

  // Force re-renders a preset's reference clip from its currently-saved
  // recipe, without changing any saved fields - see
  // httpapi.handleRegenerateCustomVoicePreset. Useful to retry after a
  // refError, or just to pick up a fresh render.
  regenerateCustomVoicePreset: (id: string) =>
    request<CustomVoicePreset>(`/api/voices/custom-presets/${id}/regenerate`, { method: 'POST' }),

  // Synthesizes arbitrary text with an existing custom preset's actual
  // saved voice, for previewing in the editor. Returns a playable blob
  // rather than JSON, so it bypasses the request() helper. `cloneModel`,
  // when given, overrides the preset's saved model - lets the editor
  // preview the model currently selected in the form before it's saved.
  testCustomVoicePreset: (id: string, text: string, cloneModel?: string) =>
    requestBlob(`/api/voices/custom-presets/${id}/test`, json('POST', { text, cloneModel })),

  // Same, for a curated built-in preset.
  testPreset: (id: string, text: string, cloneModel?: string) =>
    requestBlob(`/api/voices/presets/${id}/test`, json('POST', { text, cloneModel })),

  // Force re-renders a built-in preset's reference clip from its fixed,
  // compiled-in recipe - see httpapi.handleRegeneratePreset.
  regeneratePreset: (id: string) => request<VoicePreset>(`/api/voices/presets/${id}/regenerate`, { method: 'POST' }),

  // Previews a voice design directly (no preset id at all) - the only way
  // to hear an instruct before it's been saved. Passing seed lets the
  // backend cache the render by its exact (instruct, seed, text,
  // designModel) recipe, so saving the preset right after testing it,
  // unchanged, reuses this exact clip instead of rendering again - see
  // httpapi.handleTestVoiceDesign / handleCreateCustomVoicePreset.
  // designModel previews a specific VoiceDesign engine - "" defers to the
  // worker's own process-wide default.
  testVoiceDesign: (instruct: string, text: string, seed?: number, designModel?: string) =>
    requestBlob('/api/voices/design-test', json('POST', { instruct, text, seed, designModel })),

  // Standalone sound-effect/music test (the SFX page) - stateless, not
  // attached to any book/paragraph; one endpoint for every generation-only
  // engine (see SFX_ENGINES), selected by opts.engine ("stable_audio_music"
  // if omitted) - see httpapi.handleGenerateSFX. Every field but prompt is
  // optional and defers to that engine's own built-in default when
  // omitted.
  testSFX: (opts: SFXTestOptions) => requestBlob('/api/sfx/generate', json('POST', opts)),

  // Standalone raw system+user prompt test against this app's own
  // embedded speaker-attribution GGUF model - stateless, no
  // speakerattr-specific framing; see httpapi.handleTestLLM.
  testLLM: (opts: LLMTestOptions) => request<{ text: string }>('/api/llm/test', json('POST', opts)),

  // Previews "instructed voice cloning" - clones baseId's own already-
  // rendered reference clip while applying instruct as a clone-time style
  // instruction (only actually honored by INSTRUCTED_CLONE_MODEL) - the
  // voice/character editor's "test with another voice as a base" control.
  // baseId is any existing voice id (built-in or custom); cloneModel, when
  // given, overrides baseId's own resolved clone model. guidanceScale, when
  // given, overrides the backend's own configured instruct-time
  // guidance_scale (LECTABLE_AUDIOCPP_BREEZE_CLONE_GUIDANCE_SCALE) for this
  // one preview call, letting a reader experiment with adherence strength
  // live - see httpapi.handleTestCloneInstruct.
  testCloneInstruct: (baseId: string, instruct: string, text: string, cloneModel?: string, guidanceScale?: string) =>
    requestBlob('/api/voices/clone-instruct-test', json('POST', { baseId, instruct, text, cloneModel, guidanceScale })),
}

export { ApiError }
