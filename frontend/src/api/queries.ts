import { useEffect, useMemo, useRef } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { api } from './client'
import { useLive, useLiveMany } from './live'
import type { LiveResult } from './live'
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
  SFXTestOptions,
  Speaker,
  SpeakerAppearance,
  VoicePresets,
  VoiceSettings,
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

export function useDeleteBook() {
  return useMutation({ mutationFn: (bookId: string) => api.deleteBook(bookId) })
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
  return useMutation({ mutationFn: (chapterIdx: number) => api.deleteChapterAudio(bookId, chapterIdx) })
}

// Chapter-header "Re-import" - see api.reimportChapter.
export function useReimportChapter(bookId: string) {
  return useMutation({
    mutationFn: ({ chapterIdx, file, force }: { chapterIdx: number; file?: File; force?: boolean }) =>
      api.reimportChapter(bookId, chapterIdx, { file, force }),
  })
}

export function useChapter(bookId: string, idx: number): LiveResult<ChapterDetail> {
  return useLive('chapter', idx >= 0 ? { bookId, chapterIdx: idx } : null)
}

// A contiguous, inclusive range of chapters [start, end] for
// continuous-scroll reading - one live subscription per chapter, so a
// paragraph finishing in any loaded chapter patches just that chapter.
export function useChapterRange(bookId: string, start: number, end: number): LiveResult<ChapterDetail>[] {
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
export function useChapterMusic(bookId: string, chapterIdx: number, enabled: boolean): LiveResult<ChapterMusic> {
  return useLive('chapterMusic', enabled && chapterIdx >= 0 ? { bookId, chapterIdx } : null)
}

// useChapterRange's own counterpart for background-music regions - backs
// the annotations view's region boundary markers (ChapterSection.
// MusicRegionBoundary) across every chapter currently loaded in the
// reader's scroll window. `enabled` gates the whole range at once -
// ReaderPage passes annotationsView, so switching annotations off drops
// every loaded chapter's music subscription instead of leaving them live
// unseen.
export function useChapterMusicRange(bookId: string, start: number, end: number, enabled: boolean): LiveResult<ChapterMusic>[] {
  const params = useMemo(() => {
    const out: { bookId: string; chapterIdx: number }[] = []
    if (enabled) for (let i = Math.max(start, 0); i <= end; i++) out.push({ bookId, chapterIdx: i })
    return out
  }, [bookId, start, end, enabled])
  return useLiveMany('chapterMusic', params)
}

// Triggers background-music tone-region scoring for one chapter - fire and
// forget (see api.scoreChapterMusic/useScoringMusicChapters for tracking
// completion).
export function useScoreChapterMusic(bookId: string) {
  return useMutation({ mutationFn: (chapterIdx: number) => api.scoreChapterMusic(bookId, chapterIdx) })
}

export function useGenerateChapterMusic(bookId: string) {
  return useMutation({ mutationFn: (chapterIdx: number) => api.generateChapterMusic(bookId, chapterIdx) })
}

// Generates/regenerates one music region. chapterIdx isn't needed by the
// request itself (which addresses the region directly by id) - kept in the
// variables so existing call sites don't change shape.
export function useRegenerateMusicRegion(_bookId: string) {
  return useMutation({
    mutationFn: ({ regionId }: { regionId: string; chapterIdx: number }) => api.regenerateMusicRegion(regionId),
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
    mutationFn: ({ chapterIdx, paragraphIdx, speaker }: { chapterIdx: number; paragraphIdx: number; speaker: string }) =>
      api.setParagraphSpeaker(bookId, chapterIdx, paragraphIdx, speaker),
  })
}

// setParagraphSpeaker's own counterpart for a description line - reassigns
// one paragraph's description from one character to another.
export function useSetParagraphDescription(bookId: string) {
  return useMutation({
    mutationFn: ({
      chapterIdx,
      paragraphIdx,
      from,
      to,
    }: {
      chapterIdx: number
      paragraphIdx: number
      from: string
      to: string
    }) => api.setParagraphDescription(bookId, chapterIdx, paragraphIdx, from, to),
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

// Manual per-line emotion override - see api.setParagraphEmotion.
export function useSetParagraphEmotion(bookId: string) {
  return useMutation({
    mutationFn: ({ chapterIdx, paragraphIdx, emotion }: { chapterIdx: number; paragraphIdx: number; emotion: string }) =>
      api.setParagraphEmotion(bookId, chapterIdx, paragraphIdx, emotion),
  })
}

// Re-renders one speaker's emotion variant - see api.regenerateVariant.
export function useRegenerateVariant(bookId: string) {
  return useMutation({
    mutationFn: ({ speaker, emotion }: { speaker: string; emotion: string }) =>
      api.regenerateVariant(bookId, speaker, emotion),
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
  return useMutation({ mutationFn: (chapterIdx: number) => api.retagScareQuotes(bookId, chapterIdx) })
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
  return useMutation({ mutationFn: (settings: VoiceSettings) => api.updateVoice(bookId, settings) })
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
  return useMutation({ mutationFn: (chapterIdx: number) => api.attributeSpeakers(bookId, chapterIdx) })
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
  return useMutation({ mutationFn: (chapterIdx: number) => api.resolvePronunciation(bookId, chapterIdx) })
}

// The library page's "Preprocess" button (attribution -> characterization
// -> voice provisioning -> direction tagging, for the whole book - see
// api.preprocessBook). BookSummary.preprocessing flips on the books topic
// as soon as the run is enqueued.
export function usePreprocessBook() {
  return useMutation({ mutationFn: (bookId: string) => api.preprocessBook(bookId) })
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

// Every queued/in-flight task of `kind` for bookId in the live job queue.
function useBookJobs(bookId: string, kind: QueueTask['kind']): QueueTask[] {
  const jobs = useJobsSnapshot().data
  return useMemo(
    () => [...(jobs?.inFlight ?? []), ...(jobs?.queued ?? [])].filter((t) => t.kind === kind && t.bookId === bookId),
    [jobs, bookId, kind],
  )
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

// useAttributingChapters' own counterpart for pronunciation resolution.
export function usePronouncingChapters(bookId: string): Set<number> {
  return useChapterIdxSet(bookId, 'pronunciation')
}

// useAttributingChapters' own counterpart for scare-quote tagging.
export function useScareQuotingChapters(bookId: string): Set<number> {
  return useChapterIdxSet(bookId, 'scare_quote_tagging')
}

// useAttributingChapters' own counterpart for description tagging.
export function useDescribingChapters(bookId: string): Set<number> {
  return useChapterIdxSet(bookId, 'description_tagging')
}

// useAttributingChapters' own counterpart for background-music tone-region
// scoring. Doesn't track "music_generation" - that's region-scoped, not
// chapter-scoped, and its progress is already visible on the chapterMusic
// topic wherever that's mounted (the reader).
export function useScoringMusicChapters(bookId: string): Set<number> {
  return useChapterIdxSet(bookId, 'music_scoring')
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

export function useSetCharacterInvalid(bookId: string) {
  return useMutation({
    mutationFn: ({ characterId, invalid }: { characterId: string; invalid: boolean }) =>
      api.setCharacterInvalid(bookId, characterId, invalid),
  })
}

export function useDeleteCharacter(bookId: string) {
  return useMutation({ mutationFn: (characterId: string) => api.deleteCharacter(bookId, characterId) })
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
  return useMutation({ mutationFn: (characterId: string) => api.generateCharacterVoice(bookId, characterId) })
}

// SpeakersPage's "Generate all voices" - fire-and-forget batch enqueue via
// httpapi.handleGenerateCharacterVoices (onSuccess only means "queued";
// each character's voice shows up on the speakers topic as it lands).
export function useGenerateCharacterVoices(bookId: string) {
  return useMutation({ mutationFn: (characterIds: string[]) => api.generateCharacterVoices(bookId, characterIds) })
}

// SpeakersPage's "Regenerate all voices" - same fire-and-forget shape as
// useGenerateCharacterVoices.
export function useRegenerateCharacterVoices(bookId: string) {
  return useMutation({ mutationFn: (characterIds: string[]) => api.regenerateCharacterVoices(bookId, characterIds) })
}

export function useCharacterizeSpeaker(bookId: string) {
  return useMutation({ mutationFn: (characterId: string) => api.characterizeSpeaker(bookId, characterId) })
}

// SpeakersPage's "Recharacterize all" - see api.characterizeSpeakers' own
// doc comment for why this is a single batch request rather than firing
// useCharacterizeSpeaker's mutation once per character from the component.
// onSuccess only means "the batch was queued" (202, fire-and-forget) - see
// useCharacterizingCharacters for per-row progress.
export function useCharacterizeSpeakers(bookId: string) {
  return useMutation({ mutationFn: (characterIds: string[]) => api.characterizeSpeakers(bookId, characterIds) })
}

// Which of bookId's characters (by name - QueueTask.label is the only
// identifier a speaker_characterization task carries, see its own doc
// comment) currently have a characterization task queued/in-flight in the
// shared backend job queue - useCharacterizeSpeakers (the batch
// "Recharacterize all" endpoint) is fire-and-forget, so there's no
// mutation-lifecycle moment that means "this one character is actually
// done" for it to drive a per-row spinner from. useCharacterizeSpeaker (the
// single-row "Regenerate" button) is still a genuinely blocking request, so
// it keeps tracking its own in-flight character id locally instead of
// through this - SpeakersPage combines both.
//
// onFinished (optional) is called with exactly the names that just
// dropped out of the tracked set - i.e. finished, successfully or not.
// SpeakersPage uses it to bump audioVersion for those characters, the
// same cache-busting useCharacterizeSpeaker's own onSuccess already does
// for the single-row "Regenerate" button (see withCacheBust): the backend
// really does delete a recharacterized character's old cached reference
// clip (voicerefs.Delete, inside recharacterizeAndInvalidate) regardless
// of which endpoint triggered it, but the URL itself never changes, so a
// still-open <audio src> has no reason to refetch it unless something
// explicitly watches for it.
export function useCharacterizingCharacters(bookId: string, onFinished?: (names: string[]) => void): Set<string> {
  const characterizing = useLabelSet(bookId, 'speaker_characterization')
  const prevRef = useRef<Set<string>>(characterizing)
  const onFinishedRef = useRef(onFinished)
  onFinishedRef.current = onFinished
  useEffect(() => {
    const finished = [...prevRef.current].filter((name) => !characterizing.has(name))
    prevRef.current = characterizing
    if (finished.length > 0) onFinishedRef.current?.(finished)
  }, [characterizing])
  return characterizing
}

export function useCharacterAppearances(bookId: string, characterId: string | null): LiveResult<SpeakerAppearance[]> {
  return useLive('characterAppearances', characterId !== null ? { bookId, characterId } : null)
}

export function useCharacterDescriptions(bookId: string, characterId: string | null): LiveResult<SpeakerAppearance[]> {
  return useLive('characterDescriptions', characterId !== null ? { bookId, characterId } : null)
}

// Fire-and-forget (see api.retagDescriptions's own doc comment) - progress
// comes from useDescribingChapters.
export function useRetagDescriptions(bookId: string) {
  return useMutation({ mutationFn: (chapterIdx: number) => api.retagDescriptions(bookId, chapterIdx) })
}

export function useUpdatePosition(bookId: string) {
  return useMutation({ mutationFn: (position: Position) => api.updatePosition(bookId, position) })
}

// query is expected to already be debounced by the caller (BookSearch) -
// this hook just skips firing while it's too short to be a useful search.
// A one-off request/response, not live state.
export function useSearchBook(bookId: string, query: string) {
  return useQuery({
    queryKey: ['books', bookId, 'search', query],
    queryFn: () => api.searchBook(bookId, query),
    enabled: query.trim().length > 1,
  })
}

export function useBookmarks(bookId: string): LiveResult<Bookmark[]> {
  return useLive('bookmarks', { bookId })
}

export function useCreateBookmark(bookId: string) {
  return useMutation({
    mutationFn: ({ chapterIdx, paragraphIdx, note }: { chapterIdx: number; paragraphIdx: number; note?: string }) =>
      api.createBookmark(bookId, chapterIdx, paragraphIdx, note),
  })
}

export function useUpdateBookmarkNote(_bookId: string) {
  return useMutation({ mutationFn: ({ id, note }: { id: string; note: string }) => api.updateBookmarkNote(id, note) })
}

export function useDeleteBookmark(_bookId: string) {
  return useMutation({ mutationFn: (id: string) => api.deleteBookmark(id) })
}

export function useVoicePresets(): LiveResult<VoicePresets> {
  return useLive('voicePresets', {})
}

export function useDefaultVoice(): LiveResult<VoiceSettings> {
  return useLive('defaultVoice', {})
}

export function useUpdateDefaultVoice() {
  return useMutation({ mutationFn: (settings: VoiceSettings) => api.updateDefaultVoice(settings) })
}

// Compiled into the backend, never changes - a plain one-off fetch.
export function useVoiceLanguages() {
  return useQuery({ queryKey: ['voice-languages'], queryFn: api.voiceLanguages, staleTime: Infinity })
}

export function useCustomVoicePresets(): LiveResult<CustomVoicePreset[]> {
  return useLive('customVoicePresets', {})
}

export function useCreateCustomVoicePreset() {
  return useMutation({ mutationFn: (input: CustomVoicePresetInput) => api.createCustomVoicePreset(input) })
}

export function useUpdateCustomVoicePreset() {
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: CustomVoicePresetInput }) => api.updateCustomVoicePreset(id, input),
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
  return useMutation({ mutationFn: (opts: SFXTestOptions) => api.testSFX(opts) })
}

// Standalone LLM test page - see api.testLLM's own doc comment (stateless,
// nothing persisted).
export function useTestLLM() {
  return useMutation({ mutationFn: (opts: LLMTestOptions) => api.testLLM(opts) })
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

// The shared backend job queue. Mounted by the Jobs page and, indirectly
// (useAttributingChapters and friends), by the reader and Speakers page -
// they all share one subscription.
export function useJobsSnapshot(): LiveResult<JobsSnapshot> {
  return useLive('jobs', {})
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
