import { useEffect, useMemo, useRef } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { api } from './client'
import { useLive, useLiveMany } from './live'
import { useStableCallback } from '../hooks/useStableCallback'
import type { LiveResult } from './live'
import type {
  Bookmark,
  BookDetail,
  BookExport,
  BookSummary,
  BulkAction,
  BulkScope,
  ResetPass,
  ChapterDetail,
  ChapterMusic,
  CreateExportRequest,
  CustomVoicePreset,
  CustomVoicePresetInput,
  JobsSnapshot,
  JobsSummary,
  GlobalLexiconEntry,
  LexiconEntry,
  LexiconSuggestion,
  UpsertLexiconRequest,
  UpsertWorldFactRequest,
  WorldFact,
  TestLLMRequest,
  Position,
  QueueTask,
  GenerateSFXRequest,
  Speaker,
  SpeakerAppearance,
  VoicePresets,
  VoiceSettings,
  VoiceSettingsUpdate,
  BookSetting,
  BookRecapResponse,
  RecapSettings,
  SourceSettings,
  SourceTitleRule,
  BookSettingResponse,
  RosterReviewResponse,
  TimeJump,
  ChapterNotes,
  ChapterNotesResponse,
} from './types'

// Server state lives in live topics (see ./live.ts and backend
// httpapi.registerLiveTopics): each hook below subscribes for as long as
// its component is mounted and re-renders whenever the backend pushes a
// change, whatever caused it - this tab's own mutation, another tab, or
// background generation/attribution. That's why none of the mutations
// further down invalidate or refetch anything: the write itself is what
// produces the update. TanStack Query is still used for the mutations
// themselves and for the two plain request/response reads (search, the
// static language list).

export function useBooks(): LiveResult<BookSummary[]> {
  return useLive('books', {})
}

export function useBook(bookId: string): LiveResult<BookDetail> {
  return useLive('book', { bookId })
}

export function useUploadBook() {
  return useMutation({ mutationFn: (file: File) => api.uploadBook(file) })
}

export function useImportBookUrl() {
  return useMutation({
    mutationFn: (args: {
      url: string
      previewId?: string
      chapters?: string[]
      titleRule?: SourceTitleRule
    }) => api.importBookUrl(args.url, args.previewId, args.chapters, args.titleRule),
  })
}

export function usePreviewImport() {
  return useMutation({
    mutationFn: (opts: { url?: string; previewId?: string; archive?: boolean; file?: File }) =>
      api.previewImport(opts),
  })
}

export function useBackfillBookSource() {
  return useMutation({
    mutationFn: (args: { bookId: string; file: File; first?: number }) =>
      api.backfillBookSource(args.bookId, args.file, args.first),
  })
}

export function useListEpubChapters(bookId: string) {
  return useMutation({ mutationFn: (file: File) => api.listEpubChapters(file, bookId) })
}

export function useMatchEpubChapter(bookId: string) {
  return useMutation({
    mutationFn: (args: { chapterIdx: number; file: File; epubChapter: number }) =>
      api.matchEpubChapter(bookId, args.chapterIdx, args.file, args.epubChapter),
  })
}

export function useReviewBookSource() {
  return useMutation({
    mutationFn: (args: { bookId: string; importIds: string[]; skipIds: string[] }) =>
      api.reviewBookSource(args.bookId, args.importIds, args.skipIds),
  })
}

export function useAddBookSource(bookId: string) {
  return useMutation({ mutationFn: (url: string) => api.addBookSource(bookId, url) })
}

export function useSetSourceTitleRule(bookId: string) {
  return useMutation({
    mutationFn: (args: { sourceId: string; rule: SourceTitleRule }) =>
      api.setSourceTitleRule(bookId, args.sourceId, args.rule),
  })
}

export function useRemoveBookSource(bookId: string) {
  return useMutation({
    mutationFn: (sourceId: string) => api.removeBookSource(bookId, sourceId),
  })
}

export function useSyncBookSource() {
  return useMutation({ mutationFn: (bookId: string) => api.syncBookSource(bookId) })
}

export function useRenameBook(bookId: string) {
  return useMutation({
    mutationFn: (args: { title: string; author: string }) =>
      api.renameBook(bookId, args.title, args.author),
  })
}

export function useDeleteBook() {
  return useMutation({ mutationFn: (bookId: string) => api.deleteBook(bookId) })
}

// Every export of a book - built files, builds in progress, failures (see
// backend httpapi.buildExports).
export function useBookExports(bookId: string): LiveResult<BookExport[]> {
  return useLive('bookExports', { bookId })
}

export function useCreateExport(bookId: string) {
  return useMutation({ mutationFn: (req: CreateExportRequest) => api.createExport(bookId, req) })
}

export function useDeleteExport(bookId: string) {
  return useMutation({ mutationFn: (exportId: string) => api.deleteExport(bookId, exportId) })
}

// VoicePanel's "Delete generated audio" - wipes every paragraph's audio
// for this book (see api.deleteBookAudio/httpapi.handleDeleteBookAudio)
// without deleting the book itself.
export function useDeleteBookAudio(bookId: string) {
  return useMutation({ mutationFn: () => api.deleteBookAudio(bookId) })
}

// Chapter-header "Clear generation" - useDeleteBookAudio's single-chapter
// counterpart.
export function useDeleteChapterAudio(bookId: string) {
  return useMutation({
    mutationFn: (chapterIdx: number) => api.deleteChapterAudio(bookId, chapterIdx),
  })
}

// Chapter-header "Re-import" - see api.reimportChapter.
export function useReimportChapter(bookId: string) {
  return useMutation({
    mutationFn: ({
      chapterIdx,
      file,
      force,
    }: {
      chapterIdx: number
      file?: File
      force?: boolean
    }) => api.reimportChapter(bookId, chapterIdx, { file, force }),
  })
}

export function useChapter(bookId: string, idx: number): LiveResult<ChapterDetail> {
  return useLive('chapter', idx >= 0 ? { bookId, chapterIdx: idx } : null)
}

// A contiguous, inclusive range of chapters [start, end] for
// continuous-scroll reading - one live subscription per chapter, so a
// paragraph finishing in any loaded chapter patches just that chapter.
export function useChapterRange(
  bookId: string,
  start: number,
  end: number,
): LiveResult<ChapterDetail>[] {
  const params = useMemo(() => {
    const out: { bookId: string; chapterIdx: number }[] = []
    for (let i = Math.max(start, 0); i <= end; i++) out.push({ bookId, chapterIdx: i })
    return out
  }, [bookId, start, end])
  return useLiveMany('chapter', params)
}

// `enabled` gates the subscription itself - useBackgroundMusic passes the
// book's own musicEnabled through (no point watching regions for a chapter
// whose music will never play), while annotations view
// (useChapterMusicRange below) passes true unconditionally, since it's
// showing scored-region info as a diagnostic regardless of whether the
// book-wide toggle is even on.
export function useChapterMusic(
  bookId: string,
  chapterIdx: number,
  enabled: boolean,
): LiveResult<ChapterMusic> {
  return useLive('chapterMusic', enabled && chapterIdx >= 0 ? { bookId, chapterIdx } : null)
}

// useChapterRange's own counterpart for background-music regions - backs
// the annotations view's region boundary markers (ChapterSection.
// MusicRegionBoundary) across every chapter currently loaded in the
// reader's scroll window. `enabled` gates the whole range at once -
// ReaderPage passes annotationsView, so switching annotations off drops
// every loaded chapter's music subscription instead of leaving them live
// unseen.
export function useChapterMusicRange(
  bookId: string,
  start: number,
  end: number,
  enabled: boolean,
): LiveResult<ChapterMusic>[] {
  const params = useMemo(() => {
    const out: { bookId: string; chapterIdx: number }[] = []
    if (enabled) for (let i = Math.max(start, 0); i <= end; i++) out.push({ bookId, chapterIdx: i })
    return out
  }, [bookId, start, end, enabled])
  return useLiveMany('chapterMusic', params)
}

// useChapterMusicRange's counterpart for chapter summary notes - backs the
// annotations view's summary block and scene markers (ChapterSection) for
// every chapter in the reader's scroll window, subscribed only while
// `enabled` (annotations view) is on.
export function useChapterNotesRange(
  bookId: string,
  start: number,
  end: number,
  enabled: boolean,
): LiveResult<ChapterNotesResponse>[] {
  const params = useMemo(() => {
    const out: { bookId: string; chapterIdx: number }[] = []
    if (enabled) for (let i = Math.max(start, 0); i <= end; i++) out.push({ bookId, chapterIdx: i })
    return out
  }, [bookId, start, end, enabled])
  return useLiveMany('chapterNotes', params)
}

// Triggers background-music tone-region scoring for one chapter - fire and
// forget (see api.runChapterAmbience/useAmbienceChapters for tracking
// completion).
export function useRunChapterAmbience(bookId: string) {
  return useMutation({
    mutationFn: (chapterIdx: number) => api.runChapterAmbience(bookId, chapterIdx),
  })
}

export function useGenerateChapterMusic(bookId: string) {
  return useMutation({
    mutationFn: (chapterIdx: number) => api.generateChapterMusic(bookId, chapterIdx),
  })
}

// Generates/regenerates one music region. chapterIdx isn't needed by the
// request itself (which addresses the region directly by id) - kept in the
// variables so existing call sites don't change shape.
export function useRegenerateMusicRegion(_bookId: string) {
  return useMutation({
    mutationFn: ({ regionId }: { regionId: string; chapterIdx: number }) =>
      api.regenerateMusicRegion(regionId),
  })
}

export function useGenerateChapter(bookId: string) {
  return useMutation({ mutationFn: (idx: number) => api.generateChapter(bookId, idx) })
}

export function useRegenerateParagraph(bookId: string) {
  return useMutation({
    mutationFn: ({ chapterIdx, paragraphIdx }: { chapterIdx: number; paragraphIdx: number }) =>
      api.regenerateParagraph(bookId, chapterIdx, paragraphIdx),
  })
}

// Reassigns one paragraph to a different speaker.
export function useSetParagraphSpeaker(bookId: string) {
  return useMutation({
    mutationFn: ({
      chapterIdx,
      paragraphIdx,
      speaker,
      isNew,
    }: {
      chapterIdx: number
      paragraphIdx: number
      speaker: string
      isNew?: boolean
    }) => api.setParagraphSpeaker(bookId, chapterIdx, paragraphIdx, speaker, isNew),
  })
}

// Marks/unmarks one paragraph as a scare quote live - the backend call
// itself also invalidates that paragraph's already-generated audio (see
// api.setParagraphScareQuote's own doc comment), which the chapter topic
// reflects on its own.
export function useSetParagraphScareQuote(bookId: string) {
  return useMutation({
    mutationFn: ({
      chapterIdx,
      paragraphIdx,
      scareQuote,
    }: {
      chapterIdx: number
      paragraphIdx: number
      scareQuote: boolean
    }) => api.setParagraphScareQuote(bookId, chapterIdx, paragraphIdx, scareQuote),
  })
}

// Marks one paragraph as dialogue or narration - see api.setParagraphQuote.
export function useSetParagraphQuote(bookId: string) {
  return useMutation({
    mutationFn: ({
      chapterIdx,
      paragraphIdx,
      quote,
    }: {
      chapterIdx: number
      paragraphIdx: number
      quote: boolean
    }) => api.setParagraphQuote(bookId, chapterIdx, paragraphIdx, quote),
  })
}

// Manual per-line emotion override - see api.setParagraphEmotion.
export function useSetParagraphEmotion(bookId: string) {
  return useMutation({
    mutationFn: ({
      chapterIdx,
      paragraphIdx,
      emotion,
    }: {
      chapterIdx: number
      paragraphIdx: number
      emotion: string
    }) => api.setParagraphEmotion(bookId, chapterIdx, paragraphIdx, emotion),
  })
}

// Re-renders one speaker's emotion variant - see api.regenerateVariant.
export function useRegenerateVariant(bookId: string) {
  return useMutation({
    mutationFn: ({
      speaker,
      emotion,
      speakerIndex,
      stageId,
    }: {
      speaker: string
      // '' for the voice's neutral clip.
      emotion: string
      // Which of the speaker's voices (SpeakerVoice.number / .stage).
      speakerIndex?: number
      stageId?: string
    }) => api.regenerateVariant(bookId, speaker, emotion, { speakerIndex, stageId }),
  })
}

// Saves a paragraph's own sound-effect test prompt and/or trigger word -
// see api.setParagraphSFXPrompt's own doc comment.
export function useSetParagraphSFXPrompt(bookId: string) {
  return useMutation({
    mutationFn: ({
      chapterIdx,
      paragraphIdx,
      prompt,
      triggerWord,
    }: {
      chapterIdx: number
      paragraphIdx: number
      prompt: string
      triggerWord?: number
    }) => api.setParagraphSFXPrompt(bookId, chapterIdx, paragraphIdx, prompt, triggerWord),
  })
}

// Renders (or re-renders) one paragraph's sound effect - see
// api.generateParagraphSFX's own doc comment. Async (a pooled backend job,
// not a blocking call) - this mutation's own isPending only covers the
// enqueue request itself; the chapter topic's sfxStatus is what reports
// the clip actually reaching 'ready'/'error'.
export function useGenerateParagraphSFX(bookId: string) {
  return useMutation({
    mutationFn: ({
      chapterIdx,
      paragraphIdx,
      prompt,
      durationSeconds,
    }: {
      chapterIdx: number
      paragraphIdx: number
      prompt?: string
      durationSeconds?: number
    }) => api.generateParagraphSFX(bookId, chapterIdx, paragraphIdx, prompt, durationSeconds),
  })
}

// Fire-and-forget, same as useRetagDescriptions below - progress comes
// from useScareQuotingChapters.
export function useRetagScareQuotes(bookId: string) {
  return useMutation({
    mutationFn: (chapterIdx: number) => api.retagScareQuotes(bookId, chapterIdx),
  })
}

// Fire-and-forget: keeps generation running ahead of the reader's current
// position (see api.lookahead).
export function useLookahead(bookId: string) {
  return useMutation({
    mutationFn: ({ chapterIdx, paragraphIdx }: { chapterIdx: number; paragraphIdx: number }) =>
      api.lookahead(bookId, chapterIdx, paragraphIdx),
  })
}

export function useVoice(bookId: string): LiveResult<VoiceSettings> {
  return useLive('voice', { bookId })
}

export function useUpdateVoice(bookId: string) {
  return useMutation({
    mutationFn: (settings: VoiceSettingsUpdate) => api.updateVoice(bookId, settings),
  })
}

export function useSpeakers(bookId: string): LiveResult<Speaker[]> {
  return useLive('speakers', { bookId })
}

export function useDeleteSpeakerData(bookId: string) {
  return useMutation({ mutationFn: () => api.deleteSpeakerData(bookId) })
}

// Only enqueues (see api.attributeSpeakers) - see useAttributingChapters
// for tracking the run itself.
export function useAttributeSpeakers(bookId: string) {
  return useMutation({
    mutationFn: (chapterIdx: number) => api.attributeSpeakers(bookId, chapterIdx),
  })
}

// useAttributeSpeakers limited to the lines around the chapter's corrected
// speakers (api.attributeSpeakersAroundEdits) - the same attribution job,
// so useAttributingChapters tracks it too.
export function useAttributeAroundEdits(bookId: string) {
  return useMutation({
    mutationFn: (chapterIdx: number) => api.attributeSpeakersAroundEdits(bookId, chapterIdx),
  })
}

// Same fire-and-forget shape as useAttributeSpeakers (see
// api.tagDirections) - see useDirectingChapters for tracking the run.
export function useTagDirections(bookId: string) {
  return useMutation({ mutationFn: (chapterIdx: number) => api.tagDirections(bookId, chapterIdx) })
}

// Same fire-and-forget shape as useTagDirections (see
// api.resolvePronunciation) - see usePronouncingChapters for tracking the
// run.
export function useResolvePronunciation(bookId: string) {
  return useMutation({
    mutationFn: (chapterIdx: number) => api.resolvePronunciation(bookId, chapterIdx),
  })
}

// The library page's "Preprocess" button (attribution -> characterization
// -> direction tagging, for the whole book - see
// api.preprocessBook). BookSummary.preprocessing flips on the books topic
// as soon as the run is enqueued.
export function usePreprocessBook() {
  return useMutation({ mutationFn: (bookId: string) => api.preprocessBook(bookId) })
}

// The library's series title-bar "Preprocess series" button: every book of
// bookId's series, one at a time in order.
export function usePreprocessSeries() {
  return useMutation({ mutationFn: (bookId: string) => api.preprocessSeries(bookId) })
}

// The library header's "Preprocess library" button: every book, a few
// series at a time (api.preprocessLibrary). One row on the Jobs page.
export function usePreprocessLibrary() {
  return useMutation({ mutationFn: () => api.preprocessLibrary() })
}

// The library page's "Generate audio" hover button (see api.generateBook).
export function useGenerateBook() {
  return useMutation({ mutationFn: (bookId: string) => api.generateBook(bookId) })
}

// The "Remaining" option on the same menu useGenerateBook's "All" option
// sits in - see api.generateRemaining.
export function useGenerateRemaining() {
  return useMutation({ mutationFn: (bookId: string) => api.generateRemaining(bookId) })
}

// A Speakers-page whole-book action (see api.bulkAction) - fire-and-forget;
// see useBulkActionsRunning for the "still running" signal.
export function useBulkAction(bookId: string) {
  return useMutation({
    mutationFn: ({ action, scope }: { action: BulkAction; scope: BulkScope }) =>
      api.bulkAction(bookId, action, scope),
  })
}

// The Book settings page's per-pass reset (see api.resetPass). What it cleared
// arrives on the chapter/speakers topics by itself.
export function useResetPass(bookId: string) {
  return useMutation({ mutationFn: (pass: ResetPass) => api.resetPass(bookId, pass) })
}

// Which bulk actions currently have a row queued/in flight for bookId.
// "generate" maps to the pipeline_generate_book row it queues.
export function useBulkActionsRunning(bookId: string): Set<BulkAction> {
  const jobs = useLive('bookJobs', { bookId }).data
  return useMemo(() => {
    const running = new Set<BulkAction>()
    for (const t of [...(jobs?.inFlight ?? []), ...(jobs?.queued ?? [])]) {
      if (t.kind === 'pipeline_generate_book') running.add('generate')
      else if (t.kind.startsWith('pipeline_bulk_'))
        running.add(t.kind.slice('pipeline_bulk_'.length) as BulkAction)
    }
    return running
  }, [jobs])
}

// Every queued/in-flight task of `kind` for bookId in the live job queue.
// Reads the book's own bookJobs topic, which has no narration tasks
// (voice_clone/voice_design) - the whole queue is tens of megabytes with a
// few books queued for generation.
function useBookJobs(bookId: string, kind: QueueTask['kind']): QueueTask[] {
  const jobs = useLive('bookJobs', { bookId }).data
  return useMemo(
    () => [...(jobs?.inFlight ?? []), ...(jobs?.queued ?? [])].filter((t) => t.kind === kind),
    [jobs, kind],
  )
}

// A book's own queue for its Jobs tab: everything queued or running for it
// except narration tasks, which the bookJobs topic leaves out.
export function useBookJobsSnapshot(bookId: string): LiveResult<JobsSnapshot> {
  return useLive('bookJobs', { bookId })
}

function useChapterIdxSet(bookId: string, kind: QueueTask['kind']): Set<number> {
  const tasks = useBookJobs(bookId, kind)
  return useMemo(() => new Set(tasks.map((t) => t.chapterIdx)), [tasks])
}

function useLabelSet(bookId: string, kind: QueueTask['kind']): Set<string> {
  const tasks = useBookJobs(bookId, kind)
  return useMemo(() => new Set(tasks.filter((t) => t.label).map((t) => t.label!)), [tasks])
}

// Which of bookId's chapters currently have a speaker-attribution task
// queued/in-flight in the shared backend job queue - attributeSpeakers is
// fire-and-forget, so this is the only "still running" signal there is.
// What a finished run changed (speaker table, chapter passes) arrives on
// those topics by itself.
export function useAttributingChapters(bookId: string): Set<number> {
  return useChapterIdxSet(bookId, 'speaker_attribution')
}

// useAttributingChapters' own counterpart for speech-direction tagging.
export function useDirectingChapters(bookId: string): Set<number> {
  return useChapterIdxSet(bookId, 'speech_direction')
}

// useAttributingChapters' own counterpart for the summary pass.
export function useSummarizingChapters(bookId: string): Set<number> {
  return useChapterIdxSet(bookId, 'chapter_summary')
}

// Queues one chapter's summary pass - fire-and-forget like
// useResolvePronunciation; see useSummarizingChapters for tracking the run.
export function useSummarizeChapter(bookId: string) {
  return useMutation({
    mutationFn: (chapterIdx: number) => api.summarizeChapter(bookId, chapterIdx),
  })
}

// Re-runs a chapter's summary pass over notes the reader edited,
// replacing them (api.summarizeChapter's overwriteEdits).
export function useRegenerateChapterNotes(bookId: string) {
  return useMutation({
    mutationFn: (chapterIdx: number) => api.summarizeChapter(bookId, chapterIdx, true),
  })
}

// Saves the reader's version of a chapter's notes; the chapterNotes live
// topic carries the result back.
// The chapter's mark arrives on the live book topic.
export function useSetChapterMatter(bookId: string) {
  return useMutation({
    mutationFn: ({ chapterIdx, matter }: { chapterIdx: number; matter: boolean }) =>
      api.setChapterMatter(bookId, chapterIdx, matter),
  })
}

// Queues the book's matter pass (a book_matter job) - fire-and-forget; the
// marks arrive on the live book topic.
export function useDetectBookMatter(bookId: string) {
  return useMutation({ mutationFn: () => api.detectBookMatter(bookId) })
}

export function useDetectingBookMatter(bookId: string): boolean {
  return useBookJobs(bookId, 'book_matter').length > 0
}

export function useSaveChapterNotes(bookId: string) {
  return useMutation({
    mutationFn: ({ chapterIdx, notes }: { chapterIdx: number; notes: ChapterNotes }) =>
      api.putChapterNotes(bookId, chapterIdx, notes),
  })
}

// One chapter's summary-pass notes (live topic), or nothing while
// chapterIdx is null.
export function useChapterNotes(
  bookId: string,
  chapterIdx: number | null,
): LiveResult<ChapterNotesResponse> {
  return useLive('chapterNotes', chapterIdx === null ? null : { bookId, chapterIdx })
}

// useAttributingChapters' own counterpart for pronunciation resolution.
export function usePronouncingChapters(bookId: string): Set<number> {
  return useChapterIdxSet(bookId, 'pronunciation')
}

// useAttributingChapters' own counterpart for scare-quote tagging.
export function useScareQuotingChapters(bookId: string): Set<number> {
  return useChapterIdxSet(bookId, 'scare_quote_tagging')
}

// useAttributingChapters' own counterpart for the ambience pass (music
// scoring plus lighting). Doesn't track "music_generation" - that's region-scoped, not
// chapter-scoped, and its progress is already visible on the chapterMusic
// topic wherever that's mounted (the reader).
export function useAmbienceChapters(bookId: string): Set<number> {
  return useChapterIdxSet(bookId, 'ambience')
}

// Chapters with background-music clips being generated right now - either
// the whole-chapter run or the live one around the reader's position.
export function useGeneratingMusicChapters(bookId: string): Set<number> {
  const whole = useChapterIdxSet(bookId, 'music_generation')
  const live = useChapterIdxSet(bookId, 'music_live_generation')
  return useMemo(() => new Set([...whole, ...live]), [whole, live])
}

// useAttributingChapters' own counterpart for a chapter's own narration
// audio generation (job kind "pipeline_generate_chapter" - see backend
// jobs.EnqueueChapter/toPipelineQueueTask), including the bulk "Generate
// all"/"Generate remaining" case.
export function useGeneratingChapters(bookId: string): Set<number> {
  return useChapterIdxSet(bookId, 'pipeline_generate_chapter')
}

export function useSetCharacterVoice(bookId: string) {
  return useMutation({
    mutationFn: ({ characterId, voicePresetId }: { characterId: string; voicePresetId: string }) =>
      api.setCharacterVoice(bookId, characterId, voicePresetId),
  })
}

export function useDedupeCharacters(bookId: string) {
  return useMutation({ mutationFn: () => api.dedupeCharacters(bookId) })
}

export function useSetCharacterRole(bookId: string) {
  return useMutation({
    mutationFn: ({ characterId, role }: { characterId: string; role: boolean }) =>
      api.setCharacterRole(bookId, characterId, role),
  })
}

export function useSetCharacterInvalid(bookId: string) {
  return useMutation({
    mutationFn: ({ characterId, invalid }: { characterId: string; invalid: boolean }) =>
      api.setCharacterInvalid(bookId, characterId, invalid),
  })
}

// See api.renameCharacter.
export function useRenameCharacter(bookId: string) {
  return useMutation({
    mutationFn: ({ characterId, name }: { characterId: string; name: string }) =>
      api.renameCharacter(bookId, characterId, name),
  })
}

// See api.setCharacterAliases.
export function useSetCharacterAliases(bookId: string) {
  return useMutation({
    mutationFn: ({ characterId, aliases }: { characterId: string; aliases: string[] }) =>
      api.setCharacterAliases(bookId, characterId, aliases),
  })
}

// A book's time jumps, live - see api.createTimeJump.
export function useTimeJumps(bookId: string): LiveResult<TimeJump[]> {
  return useLive('timeJumps', { bookId })
}

export function useCreateTimeJump(bookId: string) {
  return useMutation({
    mutationFn: ({ chapterIdx, years }: { chapterIdx: number; years: number }) =>
      api.createTimeJump(bookId, chapterIdx, years),
  })
}

export function useDeleteTimeJump(bookId: string) {
  return useMutation({ mutationFn: (jumpId: string) => api.deleteTimeJump(bookId, jumpId) })
}

// See api.setCharacterStage.
export function useSetCharacterStage(bookId: string) {
  return useMutation({
    mutationFn: ({
      characterId,
      ...stage
    }: {
      characterId: string
      chapterIdx: number
      age: number
      instruct?: string
    }) => api.setCharacterStage(bookId, characterId, stage),
  })
}

// See api.updateCharacterStage.
export function useUpdateCharacterStage(bookId: string) {
  return useMutation({
    mutationFn: ({
      characterId,
      stageId,
      ...stage
    }: {
      characterId: string
      stageId: string
      age: number
      instruct?: string
    }) => api.updateCharacterStage(bookId, characterId, stageId, stage),
  })
}

// See api.deleteCharacterStage.
export function useDeleteCharacterStage(bookId: string) {
  return useMutation({
    mutationFn: ({ characterId, stageId }: { characterId: string; stageId: string }) =>
      api.deleteCharacterStage(bookId, characterId, stageId),
  })
}

// See api.setCharacterPronouns.
export function useSetCharacterPronouns(bookId: string) {
  return useMutation({
    mutationFn: ({ characterId, pronouns }: { characterId: string; pronouns: string }) =>
      api.setCharacterPronouns(bookId, characterId, pronouns),
  })
}

export function useDeleteCharacter(bookId: string) {
  return useMutation({
    mutationFn: (characterId: string) => api.deleteCharacter(bookId, characterId),
  })
}

export function useMergeCharacter(bookId: string) {
  return useMutation({
    mutationFn: ({ characterId, targetName }: { characterId: string; targetName: string }) =>
      api.mergeCharacter(bookId, characterId, targetName),
  })
}

// "Auto Split" - fire-and-forget (onSuccess only means "queued," not
// "every paragraph reattributed yet" - see api.reattributeSpeaker's own doc
// comment). name is the speaker being eliminated (a real character's exact
// name, or literally "Unknown") - see useReattributingSpeakers for the
// per-row "in progress" signal.
export function useReattributeSpeaker(bookId: string) {
  return useMutation({ mutationFn: (name: string) => api.reattributeSpeaker(bookId, name) })
}

// Which of bookId's speakers (by name - QueueTask.label is the only
// identifier a speaker-reattribute task carries, same as
// speaker_characterization's own label) currently have an "Auto Split"
// task queued/in-flight - either the whole-click pipeline_auto_split
// wrapper or any of its per-chapter children (a paused chapter's own
// continuation can outlive the wrapper - see backend
// jobs.Manager.RunReattribution).
export function useReattributingSpeakers(bookId: string): Set<string> {
  const wrappers = useLabelSet(bookId, 'pipeline_auto_split')
  const chapters = useLabelSet(bookId, 'speaker-reattribute')
  return useMemo(() => new Set([...wrappers, ...chapters]), [wrappers, chapters])
}

export function useGenerateCharacterVoice(bookId: string) {
  return useMutation({
    mutationFn: (characterId: string) => api.generateCharacterVoice(bookId, characterId),
  })
}

// Batch "regenerate these characters' voices" (PlayerBar's per-character
// action) - fire-and-forget, see api.regenerateCharacterVoices.
export function useRegenerateCharacterVoices(bookId: string) {
  return useMutation({
    mutationFn: (characterIds: string[]) => api.regenerateCharacterVoices(bookId, characterIds),
  })
}

export function useCharacterizeSpeaker(bookId: string) {
  return useMutation({
    mutationFn: (characterId: string) => api.characterizeSpeaker(bookId, characterId),
  })
}

// Which of bookId's characters (by name - QueueTask.label is the only
// identifier a speaker_characterization task carries, see its own doc
// comment) currently have a characterization task queued/in-flight in the
// shared backend job queue - "Recharacterize all" (a bulk action, see
// useBulkAction) is fire-and-forget, so there's no
// mutation-lifecycle moment that means "this one character is actually
// done" for it to drive a per-row spinner from. useCharacterizeSpeaker (the
// single-row "Regenerate" button) is still a genuinely blocking request, so
// it keeps tracking its own in-flight character id locally instead of
// through this - BookSettingsPage combines both.
//
// onFinished (optional) is called with exactly the names that just
// dropped out of the tracked set - i.e. finished, successfully or not.
// BookSettingsPage uses it to bump audioVersion for those characters, the
// same cache-busting useCharacterizeSpeaker's own onSuccess already does
// for the single-row "Regenerate" button (see withCacheBust): the backend
// really does delete a recharacterized character's old cached reference
// clip (voicerefs.Delete, inside recharacterizeAndInvalidate) regardless
// of which endpoint triggered it, but the URL itself never changes, so a
// still-open <audio src> has no reason to refetch it unless something
// explicitly watches for it.
export function useCharacterizingCharacters(
  bookId: string,
  onFinished?: (names: string[]) => void,
): Set<string> {
  const characterizing = useLabelSet(bookId, 'speaker_characterization')
  const prevRef = useRef<Set<string>>(characterizing)
  const notifyFinished = useStableCallback((names: string[]) => onFinished?.(names))
  useEffect(() => {
    const finished = [...prevRef.current].filter((name) => !characterizing.has(name))
    prevRef.current = characterizing
    if (finished.length > 0) notifyFinished(finished)
  }, [characterizing, notifyFinished])
  return characterizing
}

export function useCharacterAppearances(
  bookId: string,
  characterId: string | null,
): LiveResult<SpeakerAppearance[]> {
  return useLive('characterAppearances', characterId !== null ? { bookId, characterId } : null)
}

export function useUpdatePosition(bookId: string) {
  return useMutation({ mutationFn: (position: Position) => api.updatePosition(bookId, position) })
}

// The library card's change arrives on the live books topic.
export function useMarkRead() {
  return useMutation({
    mutationFn: ({ bookId, read }: { bookId: string; read: boolean }) => api.markRead(bookId, read),
  })
}

// query is expected to already be debounced by the caller (BookSearch) -
// this hook just skips firing while it's too short to be a useful search.
// A one-off request/response, not live state.
export function useSearchBook(bookId: string, query: string, byMeaning = false) {
  return useQuery({
    queryKey: ['books', bookId, 'search', query, byMeaning],
    queryFn: () => api.searchBook(bookId, query, byMeaning),
    enabled: query.trim().length > 1,
  })
}

export function useBookmarks(bookId: string): LiveResult<Bookmark[]> {
  return useLive('bookmarks', { bookId })
}

export function useCreateBookmark(bookId: string) {
  return useMutation({
    mutationFn: ({
      chapterIdx,
      paragraphIdx,
      note,
    }: {
      chapterIdx: number
      paragraphIdx: number
      note?: string
    }) => api.createBookmark(bookId, chapterIdx, paragraphIdx, note),
  })
}

export function useUpdateBookmarkNote(_bookId: string) {
  return useMutation({
    mutationFn: ({ id, note }: { id: string; note: string }) => api.updateBookmarkNote(id, note),
  })
}

export function useDeleteBookmark(_bookId: string) {
  return useMutation({ mutationFn: (id: string) => api.deleteBookmark(id) })
}

// The book's "previously on" recap (live topic: the recap, and whether one
// is being prepared) - see backend httpapi/recap.go.
export function useBookRecap(bookId: string): LiveResult<BookRecapResponse> {
  return useLive('bookRecap', { bookId })
}

// Asks for the recap of where the book's reader is now - fire-and-forget;
// the bookRecap topic carries both the wait and the result.
export function usePrepareBookRecap(bookId: string) {
  return useMutation({
    mutationFn: (force: boolean) => api.prepareBookRecap(bookId, force),
  })
}

// How long away brings the recap offer - one library-wide setting (live
// topic), shared by every client.
export function useSourceSettings(): LiveResult<SourceSettings> {
  return useLive('sourceSettings', {})
}

export function useSetSourceSettings() {
  return useMutation({
    mutationFn: (checkEveryHours: number) => api.setSourceSettings(checkEveryHours),
  })
}

export function useSetPatreonSession() {
  return useMutation({ mutationFn: (sessionId: string) => api.setPatreonSession(sessionId) })
}

export function useCheckSources() {
  return useMutation({ mutationFn: () => api.checkSources() })
}

// The library header's "Prepare recaps" button.
export function useCheckRecaps() {
  return useMutation({ mutationFn: () => api.checkRecaps() })
}

export function useRecapSettings(): LiveResult<RecapSettings> {
  return useLive('recapSettings', {})
}

export function useSetRecapSettings() {
  return useMutation({
    mutationFn: (afterMinutes: number) => api.setRecapSettings(afterMinutes),
  })
}

// The book's setting profile (live topic) and its edits - see backend
// httpapi/setting.go.
export function useBookSetting(bookId: string): LiveResult<BookSettingResponse> {
  return useLive('bookSetting', { bookId })
}

export function useSetBookSetting(bookId: string) {
  return useMutation({
    mutationFn: (body: BookSetting) => api.setBookSetting(bookId, body),
  })
}

// Queues the book's setting pass (a book_setting job) - fire-and-forget;
// useBookSettingDeriving tracks the run, the bookSetting topic its result.
export function useDeriveBookSetting(bookId: string) {
  return useMutation({ mutationFn: () => api.deriveBookSetting(bookId) })
}

// Whether bookId's setting pass is queued or running.
export function useBookSettingDeriving(bookId: string): boolean {
  return useBookJobs(bookId, 'book_setting').length > 0
}

// The proposed fixes to the book's roster still waiting for a say (live
// topic), and the review that finds them - see backend
// httpapi/roster_review.go.
export function useRosterReview(bookId: string): LiveResult<RosterReviewResponse> {
  return useLive('rosterReview', { bookId })
}

export function useRunRosterReview(bookId: string) {
  return useMutation({ mutationFn: () => api.runRosterReview(bookId) })
}

// Whether bookId's roster review is queued or running.
export function useRosterReviewRunning(bookId: string): boolean {
  return useBookJobs(bookId, 'roster_review').length > 0
}

export function useDecideRosterProposal(bookId: string) {
  return useMutation({
    mutationFn: (args: { proposalId: string; accept: boolean }) =>
      args.accept
        ? api.acceptRosterProposal(bookId, args.proposalId)
        : api.skipRosterProposal(bookId, args.proposalId),
  })
}

// The book's series pronunciation lexicon (live topic) and its edits.
export function useLexicon(bookId: string): LiveResult<LexiconEntry[]> {
  return useLive('lexicon', { bookId })
}

export function useUpsertLexiconEntry(bookId: string) {
  return useMutation({
    mutationFn: (body: UpsertLexiconRequest) => api.upsertLexiconEntry(bookId, body),
  })
}

// Roster name words without a rule yet, unusual spellings first (live).
export function useLexiconSuggestions(bookId: string): LiveResult<LexiconSuggestion[]> {
  return useLive('lexiconSuggestions', { bookId })
}

// Live "what would this rule respell?" check for the lexicon form - a plain
// query (not a topic: it's about an unsaved rule), refetched as the rule
// changes; the previous answer stays up while the next one loads.
export function useLexiconMatches(bookId: string, rule: UpsertLexiconRequest) {
  const term = rule.term.trim()
  return useQuery({
    queryKey: ['lexiconMatches', bookId, term, rule.prefix, rule.caseSensitive],
    queryFn: () => api.lexiconMatches(bookId, rule),
    enabled: term.length > 0,
    placeholderData: (prev) => prev,
  })
}

export function usePreviewLexiconEntry(bookId: string) {
  return useMutation({
    mutationFn: (body: UpsertLexiconRequest) => api.previewLexiconEntry(bookId, body),
  })
}

export function useDeleteLexiconEntry(bookId: string) {
  return useMutation({ mutationFn: (entryId: string) => api.deleteLexiconEntry(bookId, entryId) })
}

// The rules that apply to every book (live topic) and their edits, for
// the Settings page.
export function useGlobalLexicon(): LiveResult<GlobalLexiconEntry[]> {
  return useLive('globalLexicon', {})
}

export function useUpsertGlobalLexiconEntry() {
  return useMutation({
    mutationFn: (body: UpsertLexiconRequest) => api.upsertGlobalLexiconEntry(body),
  })
}

export function useDeleteGlobalLexiconEntry() {
  return useMutation({ mutationFn: (entryId: string) => api.deleteGlobalLexiconEntry(entryId) })
}

// The facts that hold in every book (live topic) and their edits.
export function useGlobalFacts(): LiveResult<WorldFact[]> {
  return useLive('globalFacts', {})
}

export function useUpsertGlobalFact() {
  return useMutation({
    mutationFn: (body: UpsertWorldFactRequest) => api.upsertGlobalFact(body),
  })
}

export function useDeleteGlobalFact() {
  return useMutation({ mutationFn: (factId: string) => api.deleteGlobalFact(factId) })
}

// The book's series' own facts, then the global ones (live topic), and
// edits to the series' own.
export function useBookFacts(bookId: string): LiveResult<WorldFact[]> {
  return useLive('bookFacts', { bookId })
}

export function useUpsertBookFact(bookId: string) {
  return useMutation({
    mutationFn: (body: UpsertWorldFactRequest) => api.upsertBookFact(bookId, body),
  })
}

export function useDeleteBookFact(bookId: string) {
  return useMutation({ mutationFn: (factId: string) => api.deleteBookFact(bookId, factId) })
}

export function useVoicePresets(): LiveResult<VoicePresets> {
  return useLive('voicePresets', {})
}

export function useDefaultVoice(): LiveResult<VoiceSettings> {
  return useLive('defaultVoice', {})
}

export function useUpdateDefaultVoice() {
  return useMutation({
    mutationFn: (settings: VoiceSettingsUpdate) => api.updateDefaultVoice(settings),
  })
}

// Compiled into the backend, never changes - a plain one-off fetch.
export function useVoiceLanguages() {
  return useQuery({
    queryKey: ['voice-languages'],
    queryFn: api.voiceLanguages,
    staleTime: Infinity,
  })
}

export function useCustomVoicePresets(): LiveResult<CustomVoicePreset[]> {
  return useLive('customVoicePresets', {})
}

export function useCreateCustomVoicePreset() {
  return useMutation({
    mutationFn: (input: CustomVoicePresetInput) => api.createCustomVoicePreset(input),
  })
}

export function useUpdateCustomVoicePreset() {
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: CustomVoicePresetInput }) =>
      api.updateCustomVoicePreset(id, input),
  })
}

export function useDeleteCustomVoicePreset() {
  return useMutation({ mutationFn: (id: string) => api.deleteCustomVoicePreset(id) })
}

export function useRegenerateCustomVoicePreset() {
  return useMutation({ mutationFn: (id: string) => api.regenerateCustomVoicePreset(id) })
}

export function useTestCustomVoicePreset() {
  return useMutation({
    mutationFn: ({
      id,
      text,
      cloneModel,
      temperature,
    }: {
      id: string
      text: string
      cloneModel?: string
      temperature?: number
    }) => api.testCustomVoicePreset(id, text, cloneModel, temperature),
  })
}

export function useTestPreset() {
  return useMutation({
    mutationFn: ({
      id,
      text,
      cloneModel,
      temperature,
    }: {
      id: string
      text: string
      cloneModel?: string
      temperature?: number
    }) => api.testPreset(id, text, cloneModel, temperature),
  })
}

export function useRegeneratePreset() {
  return useMutation({ mutationFn: (id: string) => api.regeneratePreset(id) })
}

// Standalone SFX test page - see api.testSFX's own doc comment (stateless,
// nothing persisted).
export function useTestSFX() {
  return useMutation({ mutationFn: (opts: GenerateSFXRequest) => api.testSFX(opts) })
}

// Standalone LLM test page - see api.testLLM's own doc comment (stateless,
// nothing persisted).
export function useTestLLM() {
  return useMutation({ mutationFn: (opts: TestLLMRequest) => api.testLLM(opts) })
}

export function useTestVoiceDesign() {
  return useMutation({
    mutationFn: ({
      instruct,
      text,
      seed,
      designModel,
      guidanceScale,
      temperature,
    }: {
      instruct: string
      text: string
      seed?: number
      designModel?: string
      guidanceScale?: number
      temperature?: number
    }) => api.testVoiceDesign(instruct, text, seed, designModel, guidanceScale, temperature),
  })
}

export function useTestCloneInstruct() {
  return useMutation({
    mutationFn: ({
      baseId,
      instruct,
      text,
      cloneModel,
      guidanceScale,
    }: {
      baseId: string
      instruct: string
      text: string
      cloneModel?: string
      guidanceScale?: string
    }) => api.testCloneInstruct(baseId, instruct, text, cloneModel, guidanceScale),
  })
}

// The shared backend job queue for the Jobs page: everything in flight, the
// head of the queue (JobsSnapshot.queuedTotal counts the rest), and
// per-book narration sums. Never the "jobs" topic, which lists every task.
export function useJobsQueue(): LiveResult<JobsSnapshot> {
  return useLive('jobsQueue', {})
}

// The queue's counts, pause state and narration rates, without any tasks.
export function useJobsSummary(): LiveResult<JobsSummary> {
  return useLive('jobsSummary', {})
}

// Jobs dashboard's per-row "Cancel" button.
export function useCancelJob() {
  return useMutation({ mutationFn: (id: string) => api.cancelJob(id) })
}

// Jobs dashboard's per-row priority menu.
export function useSetJobTier() {
  return useMutation({
    mutationFn: ({ id, tier }: { id: string; tier: QueueTask['tier'] }) => api.setJobTier(id, tier),
  })
}

// The library card's "Cancel jobs" button.
export function useCancelBookJobs() {
  return useMutation({ mutationFn: (bookId: string) => api.cancelBookJobs(bookId) })
}

// Jobs dashboard's "Cancel all" button.
export function useCancelAllJobs() {
  return useMutation({ mutationFn: () => api.cancelAllJobs() })
}

// Jobs dashboard's "Pause"/"Resume" toggle button.
export function usePauseJobs() {
  return useMutation({ mutationFn: () => api.pauseJobs() })
}

export function useResumeJobs() {
  return useMutation({ mutationFn: () => api.resumeJobs() })
}

// Jobs dashboard's "Restart ttsworker" button - forces the same kind of
// restart the watchdog does automatically on an RSS breach/crash, for a
// reader who wants to recover a stuck or visibly-leaking worker right now
// instead of waiting for it. The request itself blocks until the new
// worker is healthy, so isPending covers that whole window.
export function useRestartWorker() {
  return useMutation({ mutationFn: () => api.restartWorker() })
}
