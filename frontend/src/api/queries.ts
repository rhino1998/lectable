import { useEffect, useRef } from 'react'
import { useMutation, useQueries, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from './client'
import type { BookSummary, ChapterDetail, ChapterMusic, CustomVoicePresetInput, LLMTestOptions, Position, QueueTask, SFXTestOptions, VoiceSettings } from './types'
import type { UseQueryResult } from '@tanstack/react-query'

// Polls while any book is mid-preprocess (BookSummary.preprocessing - see
// backend httpapi.isPreprocessing) so the library page's per-book spinner
// clears on its own once a run finishes, same "poll only while something's
// actually unsettled" shape as chapterQueryOptions' own refetchInterval
// below. Stops entirely once nothing's running, rather than polling the
// whole library list on a fixed timer regardless.
export function useBooks() {
  return useQuery({
    queryKey: ['books'],
    queryFn: api.listBooks,
    refetchInterval: (query: { state: { data?: BookSummary[] } }) => {
      const data = query.state.data
      if (!data) return false
      return data.some((b) => b.preprocessing) ? 3000 : false
    },
  })
}

export function useBook(bookId: string) {
  return useQuery({ queryKey: ['books', bookId], queryFn: () => api.getBook(bookId) })
}

export function useUploadBook() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (file: File) => api.uploadBook(file),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['books'] }),
  })
}

export function useDeleteBook() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (bookId: string) => api.deleteBook(bookId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['books'] }),
  })
}

// VoicePanel's "Delete generated audio" - wipes every paragraph's audio
// for this book (see api.deleteBookAudio/httpapi.handleDeleteBookAudio)
// without deleting the book itself. Doesn't go through jobs.Manager, so
// unlike a normal generation-driven change there's no live wshub push for
// already-open reader tabs to pick this up from - invalidating every
// chapter query for this book (not just whichever one's currently loaded)
// plus the book detail/list queries (readyCount/generatedPercent) is the
// most this can do from here; a tab that doesn't refetch on its own won't
// see paragraphs flip back to pending until it does.
export function useDeleteBookAudio(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.deleteBookAudio(bookId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['books', bookId] })
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters'] })
      qc.invalidateQueries({ queryKey: ['books'] })
    },
  })
}

// Chapter-header "Clear generation" - useDeleteBookAudio's single-chapter
// counterpart. Only invalidates this one chapter's query (plus the book
// detail/list queries, for readyCount/generatedPercent) rather than every
// chapter query for the book.
export function useDeleteChapterAudio(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (chapterIdx: number) => api.deleteChapterAudio(bookId, chapterIdx),
    onSuccess: (_data, chapterIdx) => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters', chapterIdx] })
      qc.invalidateQueries({ queryKey: ['books', bookId] })
      qc.invalidateQueries({ queryKey: ['books'] })
    },
  })
}

// Paragraph audio status/urls update live via useBookUpdates' WebSocket
// push (see hooks/useBookUpdates.ts) - this poll is now just a slow safety
// net in case that connection is down or briefly reconnecting, not the
// primary update path, hence the much longer interval than it used to be.
// Stops once every paragraph is settled (ready or error).
function chapterQueryOptions(bookId: string, idx: number) {
  return {
    queryKey: ['books', bookId, 'chapters', idx] as const,
    queryFn: () => api.getChapter(bookId, idx),
    enabled: idx >= 0,
    refetchInterval: (query: { state: { data?: ChapterDetail } }) => {
      const data = query.state.data
      if (!data) return false
      // SFX generation (see useGenerateParagraphSFX) has no websocket push
      // of its own - unlike narration's audioStatus, this poll is its only
      // update path, so it gets a much tighter interval while any
      // paragraph is actively generating one.
      if (data.paragraphs.some((p) => p.sfxStatus === 'generating')) return 2000
      const settled = data.paragraphs.every((p) => p.audioStatus === 'ready' || p.audioStatus === 'error')
      return settled ? false : 15000
    },
  }
}

export function useChapter(bookId: string, idx: number) {
  return useQuery(chapterQueryOptions(bookId, idx))
}

// Fetches a contiguous, inclusive range of chapters [start, end] for
// continuous-scroll reading. Each chapter still polls independently while
// it has unsettled paragraphs, same as useChapter.
export function useChapterRange(
  bookId: string,
  start: number,
  end: number,
): UseQueryResult<ChapterDetail>[] {
  const indices: number[] = []
  for (let i = start; i <= end; i++) indices.push(i)
  return useQueries({
    queries: indices.map((idx) => chapterQueryOptions(bookId, idx)),
  })
}

// Polls while this chapter's background music is still being worked on -
// scoring not finished yet, or any region still pending/generating - and
// stops once everything has settled (ready or error), same shape as
// chapterQueryOptions' own refetchInterval. `enabled` gates the query
// itself - useBackgroundMusic passes the book's own musicEnabled through
// (no point polling regions for a chapter whose music will never play),
// while annotations view (useChapterMusicRange below) passes true
// unconditionally, since it's showing scored-region info as a diagnostic
// regardless of whether the book-wide toggle is even on.
function chapterMusicQueryOptions(bookId: string, chapterIdx: number, enabled: boolean) {
  return {
    queryKey: ['books', bookId, 'chapters', chapterIdx, 'music'] as const,
    queryFn: () => api.getChapterMusic(bookId, chapterIdx),
    enabled: enabled && chapterIdx >= 0,
    refetchInterval: (query: { state: { data?: ChapterMusic } }) => {
      const data = query.state.data
      if (!data) return false
      if (!data.scored) return 3000
      const settled = data.regions.every((r) => r.status === 'ready' || r.status === 'error')
      return settled ? false : 3000
    },
  }
}

export function useChapterMusic(bookId: string, chapterIdx: number, enabled: boolean) {
  return useQuery(chapterMusicQueryOptions(bookId, chapterIdx, enabled))
}

// useChapterRange's own counterpart for background-music regions - backs
// the annotations view's region boundary markers (ChapterSection.
// MusicRegionBoundary) across every chapter currently loaded in the
// reader's scroll window, not just whichever one is actively playing (that
// narrower case is useChapterMusic/useBackgroundMusic's job). `enabled`
// gates the whole range at once - ReaderPage passes annotationsView, so
// switching annotations off stops polling every loaded chapter's music
// query instead of leaving them running unseen.
export function useChapterMusicRange(bookId: string, start: number, end: number, enabled: boolean): UseQueryResult<ChapterMusic>[] {
  const indices: number[] = []
  for (let i = start; i <= end; i++) indices.push(i)
  return useQueries({
    queries: indices.map((idx) => chapterMusicQueryOptions(bookId, idx, enabled)),
  })
}

// Triggers background-music tone-region scoring for one chapter -
// useAttributeSpeakers/useTagDirections' own exact fire-and-forget shape
// (see api.scoreChapterMusic/useScoringMusicChapters for tracking
// completion the same way useAttributingChapters/useDirectingChapters do).
export function useScoreChapterMusic(bookId: string) {
  return useMutation({
    mutationFn: (chapterIdx: number) => api.scoreChapterMusic(bookId, chapterIdx),
  })
}

// Generates/regenerates one music region - useRegenerateParagraph's own
// shape, just invalidating the chapter's music query (regions/status
// live there) instead of its chapter detail one. chapterIdx is only
// needed for that invalidation, not by api.regenerateMusicRegion itself
// (which addresses the region directly by id).
export function useRegenerateMusicRegion(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ regionId }: { regionId: string; chapterIdx: number }) => api.regenerateMusicRegion(regionId),
    onSuccess: (_data, { chapterIdx }) =>
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters', chapterIdx, 'music'] }),
  })
}

export function useGenerateChapter(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (idx: number) => api.generateChapter(bookId, idx),
    onSuccess: (_data, idx) => qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters', idx] }),
  })
}

// Live status updates arrive over useBookUpdates' WebSocket the same as
// any other generation - this invalidation is just a safety net in case
// that connection is down, same reasoning as useGenerateChapter's own.
export function useRegenerateParagraph(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ chapterIdx, paragraphIdx }: { chapterIdx: number; paragraphIdx: number }) =>
      api.regenerateParagraph(bookId, chapterIdx, paragraphIdx),
    onSuccess: (_data, { chapterIdx }) => qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters', chapterIdx] }),
  })
}

// Reassigns one paragraph to a different speaker - invalidates the speaker
// table (paragraph/ready counts moved between rows), every character's
// appearances list for this book (the paragraph left one and may have
// joined another - a broad prefix match rather than tracking exactly which
// two characterIds were affected), and the chapter it lives in (its own
// Speaker field, shown in the reader).
export function useSetParagraphSpeaker(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ chapterIdx, paragraphIdx, speaker }: { chapterIdx: number; paragraphIdx: number; speaker: string }) =>
      api.setParagraphSpeaker(bookId, chapterIdx, paragraphIdx, speaker),
    onSuccess: (_data, { chapterIdx }) => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      qc.invalidateQueries({ queryKey: ['books', bookId, 'characters'] })
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters', chapterIdx] })
    },
  })
}

// setParagraphSpeaker's own counterpart for a description line - reassigns
// one paragraph's description from one character to another. Invalidates
// the speaker table (this can register a brand-new character, the same as
// useSetParagraphSpeaker), every character's own appearances/descriptions
// lists for this book (broad prefix match - the paragraph left `from`'s
// descriptions list and may have joined `to`'s), and the chapter it lives
// in (ReaderPage's own annotationKind reads describesCharacters directly).
export function useSetParagraphDescription(bookId: string) {
  const qc = useQueryClient()
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
    onSuccess: (_data, { chapterIdx }) => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      qc.invalidateQueries({ queryKey: ['books', bookId, 'characters'] })
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters', chapterIdx] })
    },
  })
}

// Marks/unmarks one paragraph as a scare quote live - unlike
// useSetParagraphSpeaker/useSetParagraphDescription above, the backend call
// itself invalidates that paragraph's own already-generated audio (see
// api.setParagraphScareQuote's own doc comment), so this also invalidates
// the chapter query (audioStatus there needs to catch up to "pending"
// again) alongside the annotation-relevant fields.
export function useSetParagraphScareQuote(bookId: string) {
  const qc = useQueryClient()
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
    onSuccess: (_data, { chapterIdx }) => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters', chapterIdx] })
    },
  })
}

// Saves a paragraph's own sound-effect test prompt and/or trigger word -
// see api.setParagraphSFXPrompt's own doc comment. Doesn't touch any
// already-generated clip, so invalidating the chapter query here is just
// keeping the saved-prompt text field in sync (useful if the same chapter
// is open in more than one place), not catching up an audio status.
export function useSetParagraphSFXPrompt(bookId: string) {
  const qc = useQueryClient()
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
    onSuccess: (_data, { chapterIdx }) => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters', chapterIdx] })
    },
  })
}

// Renders (or re-renders) one paragraph's sound effect - see
// api.generateParagraphSFX's own doc comment. Async (a pooled backend job,
// not a blocking call) - this mutation's own isPending only covers the
// enqueue request itself; chapterQueryOptions' own poll (api/queries.ts)
// is what picks up sfxStatus actually reaching 'ready'/'error'.
export function useGenerateParagraphSFX(bookId: string) {
  const qc = useQueryClient()
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
    onSuccess: (_data, { chapterIdx }) => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters', chapterIdx] })
    },
  })
}

// Blocking, same shape/reasoning as useRetagDescriptions above.
export function useRetagScareQuotes(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (chapterIdx: number) => api.retagScareQuotes(bookId, chapterIdx),
    onSuccess: (_data, chapterIdx) => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters', chapterIdx] })
    },
  })
}

// Fire-and-forget: keeps generation running ahead of the reader's current
// position (see api.lookahead). No cache invalidation here - the polling
// already active on loaded chapters (chapterQueryOptions) is what actually
// surfaces newly-ready audio as it lands.
export function useLookahead(bookId: string) {
  return useMutation({
    mutationFn: ({ chapterIdx, paragraphIdx }: { chapterIdx: number; paragraphIdx: number }) =>
      api.lookahead(bookId, chapterIdx, paragraphIdx),
  })
}

export function useVoice(bookId: string) {
  return useQuery({ queryKey: ['books', bookId, 'voice'], queryFn: () => api.getVoice(bookId) })
}

export function useUpdateVoice(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (settings: VoiceSettings) => api.updateVoice(bookId, settings),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'voice'] })
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters'] })
      qc.invalidateQueries({ queryKey: ['books', bookId] })
    },
  })
}

export function useSpeakers(bookId: string) {
  return useQuery({ queryKey: ['books', bookId, 'speakers'], queryFn: () => api.listSpeakers(bookId) })
}

export function useDeleteSpeakerData(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.deleteSpeakerData(bookId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters'] })
      qc.invalidateQueries({ queryKey: ['custom-voice-presets'] })
    },
  })
}

// The mutation itself now only enqueues (see api.attributeSpeakers) - its
// own onSuccess fires once the enqueue is acked, well before attribution
// has actually run, so it's not a useful point to invalidate the speaker
// table from any more. See useAttributingChapters for that.
export function useAttributeSpeakers(bookId: string) {
  return useMutation({
    mutationFn: (chapterIdx: number) => api.attributeSpeakers(bookId, chapterIdx),
  })
}

// Same fire-and-forget shape as useAttributeSpeakers, for the same reason
// (see api.tagDirections) - see useDirectingChapters for tracking
// completion the same way useAttributingChapters does for attribution.
export function useTagDirections(bookId: string) {
  return useMutation({
    mutationFn: (chapterIdx: number) => api.tagDirections(bookId, chapterIdx),
  })
}

// The library page's "Preprocess" button (attribution -> characterization
// -> voice provisioning -> direction tagging, for the whole book - see
// api.preprocessBook). Invalidates the book list on success so it refetches
// right away and picks up BookSummary.preprocessing=true immediately,
// rather than waiting out useBooks' own poll interval (which only starts
// once something's already seen to be running) to show the spinner.
export function usePreprocessBook() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (bookId: string) => api.preprocessBook(bookId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['books'] }),
  })
}

// The library page's "Generate audio" hover button (see api.generateBook) -
// enqueues background TTS generation for the whole book at once. Same
// invalidation reasoning as usePreprocessBook: refetches the book list
// right away so BookSummary.generatedPercent starts climbing as soon as
// paragraphs actually complete, rather than waiting out useBooks' own poll
// interval.
export function useGenerateBook() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (bookId: string) => api.generateBook(bookId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['books'] }),
  })
}

// The "Remaining" option on the same menu useGenerateBook's "All" option
// sits in - see api.generateRemaining. Same shape/invalidation reasoning.
export function useGenerateRemaining() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (bookId: string) => api.generateRemaining(bookId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['books'] }),
  })
}

// Tracks which of bookId's chapters currently have a speaker-attribution
// task queued/in-flight in the shared backend job queue (polling
// useJobsSnapshot), and invalidates the book's speaker table whenever a
// previously-tracked chapter drops out of that set - i.e. its attribution
// just finished (successfully or not; either way there's nothing left to
// wait on, and the speaker table needs a refetch to show whatever changed).
// Replaces the old per-mutation onSuccess invalidation now that
// attributeSpeakers is fire-and-forget (backend jobs.Manager.
// EnqueueAttribution) rather than blocking until the run completes - there's
// no mutation-lifecycle moment that means "this chapter is actually done"
// any more, so this fills that gap from the outside instead.
export function useAttributingChapters(bookId: string): Set<number> {
  const qc = useQueryClient()
  const jobsQuery = useJobsSnapshot()
  const attributing = new Set(
    [...(jobsQuery.data?.inFlight ?? []), ...(jobsQuery.data?.queued ?? [])]
      .filter((t) => t.kind === 'speaker_attribution' && t.bookId === bookId)
      .map((t) => t.chapterIdx),
  )
  const prevRef = useRef<Set<number>>(attributing)
  useEffect(() => {
    const finished = [...prevRef.current].some((idx) => !attributing.has(idx))
    prevRef.current = attributing
    if (finished) {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      // Refreshes each chapter's attributedCount (see store.ChapterSummary/
      // chapterSummaryDTO) so the Speakers/Reader pages' own "attributed"
      // status stops showing stale pre-attribution counts once this run
      // actually lands.
      qc.invalidateQueries({ queryKey: ['books', bookId] })
    }
    // Only the polled snapshot should re-run this - `attributing` is
    // derived fresh from it every render, not an independent dependency.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [jobsQuery.data, bookId])
  return attributing
}

// useAttributingChapters' own exact counterpart for speech-direction
// tagging (jobs kind "speech_direction") - see its doc comment for the
// full reasoning (fire-and-forget mutation, no lifecycle moment to
// invalidate from, so this fills the gap from the polled job queue
// instead). Invalidates the book query (not the speaker table - direction
// tags have nothing to do with speakers) so each chapter's `directed` map
// (chapterSummaryDTO) picks up the just-finished run.
export function useDirectingChapters(bookId: string): Set<number> {
  const qc = useQueryClient()
  const jobsQuery = useJobsSnapshot()
  const directing = new Set(
    [...(jobsQuery.data?.inFlight ?? []), ...(jobsQuery.data?.queued ?? [])]
      .filter((t) => t.kind === 'speech_direction' && t.bookId === bookId)
      .map((t) => t.chapterIdx),
  )
  const prevRef = useRef<Set<number>>(directing)
  useEffect(() => {
    const finished = [...prevRef.current].some((idx) => !directing.has(idx))
    prevRef.current = directing
    if (finished) {
      qc.invalidateQueries({ queryKey: ['books', bookId] })
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [jobsQuery.data, bookId])
  return directing
}

// useAttributingChapters/useDirectingChapters' own counterpart for
// background-music tone-region scoring (jobs kind "music_scoring") - the
// Speakers page's chapter table uses this to show a chapter as "Scoring…"
// and invalidate book.chapters (musicScored lives there) once a run
// finishes. Doesn't track "music_generation" - that's region-scoped, not
// chapter-scoped (no single chapterIdx-keyed "is this chapter done"
// question to answer the way scoring/attribution/direction have), and its
// own progress is already visible via useChapterMusic's own poll wherever
// that's mounted (the reader).
export function useScoringMusicChapters(bookId: string): Set<number> {
  const qc = useQueryClient()
  const jobsQuery = useJobsSnapshot()
  const scoring = new Set(
    [...(jobsQuery.data?.inFlight ?? []), ...(jobsQuery.data?.queued ?? [])]
      .filter((t) => t.kind === 'music_scoring' && t.bookId === bookId)
      .map((t) => t.chapterIdx),
  )
  const prevRef = useRef<Set<number>>(scoring)
  useEffect(() => {
    const finished = [...prevRef.current].some((idx) => !scoring.has(idx))
    prevRef.current = scoring
    if (finished) {
      qc.invalidateQueries({ queryKey: ['books', bookId] })
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [jobsQuery.data, bookId])
  return scoring
}

// useAttributingChapters/useDirectingChapters/useScoringMusicChapters' own
// counterpart for a chapter's own narration audio generation (job kind
// "pipeline_generate_chapter" - see backend jobs.EnqueueChapter/
// toPipelineQueueTask) - the Speakers page's chapter table uses this the
// same way, to show a chapter as "Generating…" and invalidate book.chapters
// (readyCount/paragraphCount live there) once a run finishes. Unlike
// attribution/direction/music-scoring, useGenerateChapter (the mutation
// this tracks completion for) already does its own onSuccess invalidation
// of the one chapter's own detail query - this additionally covers the
// bulk "Generate all"/"Generate remaining" case, which fires many chapters'
// worth of pipeline_generate_chapter tasks at once with no per-chapter
// mutation callback of its own to invalidate from.
export function useGeneratingChapters(bookId: string): Set<number> {
  const qc = useQueryClient()
  const jobsQuery = useJobsSnapshot()
  const generating = new Set(
    [...(jobsQuery.data?.inFlight ?? []), ...(jobsQuery.data?.queued ?? [])]
      .filter((t) => t.kind === 'pipeline_generate_chapter' && t.bookId === bookId)
      .map((t) => t.chapterIdx),
  )
  const prevRef = useRef<Set<number>>(generating)
  useEffect(() => {
    const finished = [...prevRef.current].some((idx) => !generating.has(idx))
    prevRef.current = generating
    if (finished) {
      qc.invalidateQueries({ queryKey: ['books', bookId] })
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [jobsQuery.data, bookId])
  return generating
}

export function useSetCharacterVoice(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ characterId, voicePresetId }: { characterId: string; voicePresetId: string }) =>
      api.setCharacterVoice(bookId, characterId, voicePresetId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters'] })
    },
  })
}

export function useDeleteCharacter(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (characterId: string) => api.deleteCharacter(bookId, characterId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters'] })
      qc.invalidateQueries({ queryKey: ['custom-voice-presets'] })
    },
  })
}

export function useMergeCharacter(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ characterId, targetName }: { characterId: string; targetName: string }) =>
      api.mergeCharacter(bookId, characterId, targetName),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters'] })
      qc.invalidateQueries({ queryKey: ['custom-voice-presets'] })
    },
  })
}

// "Auto Split" - fire-and-forget, same shape/caveats as
// useCharacterizeSpeakers (onSuccess only means "queued," not "every
// paragraph reattributed yet" - see api.reattributeSpeaker's own doc
// comment). name is the speaker being eliminated (a real character's exact
// name, or literally "Unknown") - see useReattributingSpeakers for the
// per-row "in progress" signal this alone doesn't give.
export function useReattributeSpeaker(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (name: string) => api.reattributeSpeaker(bookId, name),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters'] })
    },
  })
}

// Tracks which of bookId's speakers (by name - QueueTask.label is the only
// identifier a speaker-reattribute task carries, same as
// speaker_characterization's own label) currently have an "Auto Split" task
// queued/in-flight - useCharacterizingCharacters' own sibling, for the same
// reason: reattributeSpeaker fans out into several fire-and-forget
// per-chapter tasks with no single mutation-lifecycle moment that means
// "this speaker is actually done." Invalidates the speaker/chapter caches
// once a speaker's last chapter task clears, so the table picks up its
// (possibly now much smaller, or gone) paragraph count without a manual
// refetch.
export function useReattributingSpeakers(bookId: string): Set<string> {
  const qc = useQueryClient()
  const jobsQuery = useJobsSnapshot()
  const reattributing = new Set(
    [...(jobsQuery.data?.inFlight ?? []), ...(jobsQuery.data?.queued ?? [])]
      .filter((t) => t.kind === 'speaker-reattribute' && t.bookId === bookId && t.label)
      .map((t) => t.label!),
  )
  const prevRef = useRef<Set<string>>(reattributing)
  useEffect(() => {
    const finished = [...prevRef.current].filter((name) => !reattributing.has(name))
    prevRef.current = reattributing
    if (finished.length > 0) {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      qc.invalidateQueries({ queryKey: ['books', bookId, 'chapters'] })
    }
    // Only the polled snapshot should re-run this - `reattributing` is
    // derived fresh from it every render, not an independent dependency.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [jobsQuery.data, bookId])
  return reattributing
}

export function useGenerateCharacterVoice(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (characterId: string) => api.generateCharacterVoice(bookId, characterId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      qc.invalidateQueries({ queryKey: ['custom-voice-presets'] })
    },
  })
}

// SpeakersPage's "Generate all voices" - fire-and-forget batch enqueue via
// httpapi.handleGenerateCharacterVoices, same shape/caveats as
// useCharacterizeSpeakers (onSuccess only means "queued," not "every
// character has a voice yet"). There's no job-queue kind of its own to
// poll for per-row progress the way useCharacterizingCharacters does for
// recharacterization (see that handler's own doc comment on why - each
// character's provisioning runs in a detached goroutine, not a tracked
// task) - useCharacterizingCharacters' own invalidation still fires for
// whichever characters this triggers a fresh characterization for, which
// covers the common case of provisioning a brand-new character's voice for
// the first time.
export function useGenerateCharacterVoices(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (characterIds: string[]) => api.generateCharacterVoices(bookId, characterIds),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      qc.invalidateQueries({ queryKey: ['custom-voice-presets'] })
    },
  })
}

// SpeakersPage's "Regenerate all voices" - fire-and-forget batch enqueue
// via httpapi.handleRegenerateCharacterVoices, same shape/caveats as
// useGenerateCharacterVoices (onSuccess only means "queued," and there's
// no job-queue kind of its own to poll per-row progress from - see that
// handler's own doc comment). Unlike useGenerateCharacterVoices, most
// characters this targets are already characterized, so even the partial
// signal useCharacterizingCharacters gives that one doesn't reliably fire
// here either - the speaker table/voice previews only catch up once
// something else (a manual refetch, navigating away and back) refreshes
// them.
export function useRegenerateCharacterVoices(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (characterIds: string[]) => api.regenerateCharacterVoices(bookId, characterIds),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      qc.invalidateQueries({ queryKey: ['custom-voice-presets'] })
    },
  })
}

export function useCharacterizeSpeaker(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (characterId: string) => api.characterizeSpeaker(bookId, characterId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      // The character's assigned voice preset's instruct may have changed
      // (see httpapi.handleCharacterizeSpeaker) - refresh the Voices page
      // too if it's mounted.
      qc.invalidateQueries({ queryKey: ['custom-voice-presets'] })
    },
  })
}

// SpeakersPage's "Recharacterize all" - see api.characterizeSpeakers' own
// doc comment for why this is a single batch request rather than firing
// useCharacterizeSpeaker's mutation once per character from the component.
// onSuccess only means "the batch was queued," not "every character is
// done" (202, fire-and-forget) - the speaker table won't reflect finished
// work until its own poll/refetch picks up each task clearing, same as
// attribution.
export function useCharacterizeSpeakers(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (characterIds: string[]) => api.characterizeSpeakers(bookId, characterIds),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      qc.invalidateQueries({ queryKey: ['custom-voice-presets'] })
    },
  })
}

// Tracks which of bookId's characters (by name - QueueTask.label is the
// only identifier a speaker_characterization task carries, see its own
// doc comment) currently have a characterization task queued/in-flight in
// the shared backend job queue - useAttributingChapters' own sibling, for
// the exact same reason: useCharacterizeSpeakers (the batch "Recharacterize
// all" endpoint) is fire-and-forget, so there's no mutation-lifecycle
// moment that means "this one character is actually done" for it to drive
// a per-row spinner from. useCharacterizeSpeaker (the single-row
// "Regenerate" button) is still a genuinely blocking request, so it keeps
// tracking its own in-flight character id locally instead of through
// this - SpeakersPage combines both.
//
// onFinished (optional) is called with exactly the names that just
// dropped out of the tracked set - i.e. finished, successfully or not -
// on top of the unconditional speaker-table/preset invalidation below.
// SpeakersPage uses it to bump audioVersion for those characters, the
// same cache-busting useCharacterizeSpeaker's own onSuccess already does
// for the single-row "Regenerate" button (see withCacheBust): the backend
// really does delete a recharacterized character's old cached reference
// clip (voicerefs.Delete, inside recharacterizeAndInvalidate) regardless
// of which endpoint triggered it, but the batch endpoint's fire-and-forget
// shape means nothing here reacts to "this specific character's voice was
// just invalidated" unless something explicitly watches for it - without
// that, a still-open <audio src> pointing at the old preset id can look
// exactly like the delete never happened, since the URL itself never
// changes and the browser has no reason to refetch it.
export function useCharacterizingCharacters(bookId: string, onFinished?: (names: string[]) => void): Set<string> {
  const qc = useQueryClient()
  const jobsQuery = useJobsSnapshot()
  const characterizing = new Set(
    [...(jobsQuery.data?.inFlight ?? []), ...(jobsQuery.data?.queued ?? [])]
      .filter((t) => t.kind === 'speaker_characterization' && t.bookId === bookId && t.label)
      .map((t) => t.label!),
  )
  const prevRef = useRef<Set<string>>(characterizing)
  const onFinishedRef = useRef(onFinished)
  onFinishedRef.current = onFinished
  useEffect(() => {
    const finished = [...prevRef.current].filter((name) => !characterizing.has(name))
    prevRef.current = characterizing
    if (finished.length > 0) {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
      qc.invalidateQueries({ queryKey: ['custom-voice-presets'] })
      onFinishedRef.current?.(finished)
    }
    // Only the polled snapshot should re-run this - `characterizing` is
    // derived fresh from it every render, not an independent dependency.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [jobsQuery.data, bookId])
  return characterizing
}

export function useCharacterAppearances(bookId: string, characterId: string | null) {
  return useQuery({
    queryKey: ['books', bookId, 'characters', characterId, 'appearances'],
    queryFn: () => api.characterAppearances(bookId, characterId as string),
    enabled: characterId !== null,
  })
}

export function useCharacterDescriptions(bookId: string, characterId: string | null) {
  return useQuery({
    queryKey: ['books', bookId, 'characters', characterId, 'descriptions'],
    queryFn: () => api.characterDescriptions(bookId, characterId as string),
    enabled: characterId !== null,
  })
}

// Blocking (see api.retagDescriptions's own doc comment) - callers track
// their own in-flight/error state the same way useCharacterizeSpeaker's
// single-row "Regenerate" button does, rather than through the job-queue
// poll useAttributingChapters uses for attribution itself.
export function useRetagDescriptions(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (chapterIdx: number) => api.retagDescriptions(bookId, chapterIdx),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['books', bookId, 'characters'] })
      qc.invalidateQueries({ queryKey: ['books', bookId, 'speakers'] })
    },
  })
}

export function useUpdatePosition(bookId: string) {
  return useMutation({
    mutationFn: (position: Position) => api.updatePosition(bookId, position),
  })
}

// query is expected to already be debounced by the caller (BookSearch) -
// this hook just skips firing while it's too short to be a useful search.
export function useSearchBook(bookId: string, query: string) {
  return useQuery({
    queryKey: ['books', bookId, 'search', query],
    queryFn: () => api.searchBook(bookId, query),
    enabled: query.trim().length > 1,
  })
}

export function useBookmarks(bookId: string) {
  return useQuery({ queryKey: ['books', bookId, 'bookmarks'], queryFn: () => api.listBookmarks(bookId) })
}

export function useCreateBookmark(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ chapterIdx, paragraphIdx, note }: { chapterIdx: number; paragraphIdx: number; note?: string }) =>
      api.createBookmark(bookId, chapterIdx, paragraphIdx, note),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['books', bookId, 'bookmarks'] }),
  })
}

export function useUpdateBookmarkNote(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, note }: { id: string; note: string }) => api.updateBookmarkNote(id, note),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['books', bookId, 'bookmarks'] }),
  })
}

export function useDeleteBookmark(bookId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.deleteBookmark(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['books', bookId, 'bookmarks'] }),
  })
}

export function useVoicePresets() {
  return useQuery({ queryKey: ['voice-presets'], queryFn: api.voicePresets, staleTime: Infinity })
}

export function useDefaultVoice() {
  return useQuery({ queryKey: ['voice-default'], queryFn: api.getDefaultVoice })
}

export function useUpdateDefaultVoice() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (settings: VoiceSettings) => api.updateDefaultVoice(settings),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['voice-default'] }),
  })
}

export function useVoiceLanguages() {
  return useQuery({ queryKey: ['voice-languages'], queryFn: api.voiceLanguages, staleTime: Infinity })
}

export function useCustomVoicePresets() {
  return useQuery({ queryKey: ['custom-voice-presets'], queryFn: api.customVoicePresets })
}

export function useCreateCustomVoicePreset() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (input: CustomVoicePresetInput) => api.createCustomVoicePreset(input),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['custom-voice-presets'] }),
  })
}

export function useUpdateCustomVoicePreset() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, input }: { id: string; input: CustomVoicePresetInput }) =>
      api.updateCustomVoicePreset(id, input),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['custom-voice-presets'] }),
  })
}

export function useDeleteCustomVoicePreset() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.deleteCustomVoicePreset(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['custom-voice-presets'] }),
  })
}

export function useRegenerateCustomVoicePreset() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.regenerateCustomVoicePreset(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['custom-voice-presets'] }),
  })
}

export function useTestCustomVoicePreset() {
  return useMutation({
    mutationFn: ({ id, text, cloneModel }: { id: string; text: string; cloneModel?: string }) =>
      api.testCustomVoicePreset(id, text, cloneModel),
  })
}

export function useTestPreset() {
  return useMutation({
    mutationFn: ({ id, text, cloneModel }: { id: string; text: string; cloneModel?: string }) =>
      api.testPreset(id, text, cloneModel),
  })
}

export function useRegeneratePreset() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.regeneratePreset(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['voice-presets'] }),
  })
}

// Standalone SFX test page - see api.testSFX's own doc comment. No
// invalidation needed (stateless, nothing persisted).
export function useTestSFX() {
  return useMutation({
    mutationFn: (opts: SFXTestOptions) => api.testSFX(opts),
  })
}

// Standalone LLM test page - see api.testLLM's own doc comment. No
// invalidation needed (stateless, nothing persisted).
export function useTestLLM() {
  return useMutation({
    mutationFn: (opts: LLMTestOptions) => api.testLLM(opts),
  })
}

export function useTestVoiceDesign() {
  return useMutation({
    mutationFn: ({
      instruct,
      text,
      seed,
      designModel,
    }: {
      instruct: string
      text: string
      seed?: number
      designModel?: string
    }) => api.testVoiceDesign(instruct, text, seed, designModel),
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

// Live-updated via useJobsUpdates' WebSocket (mounted once at the app
// root, routes.tsx), which replaces this query's cache entry wholesale on
// every queue change - this poll is now just a slow safety net in case
// that connection is down or briefly reconnecting, not the primary update
// path, same reasoning as chapterQueryOptions' own poll next to
// useBookUpdates.
export function useJobsSnapshot() {
  return useQuery({ queryKey: ['jobs'], queryFn: api.jobsSnapshot, refetchInterval: 15000 })
}

// Jobs dashboard's per-row "Cancel" button. Doesn't wait for the next 2s
// poll to reflect the change - invalidates immediately so a canceled
// queued task disappears (and an in-flight one's cancellation-in-progress
// state, if the backend ever surfaces one) right away.
export function useCancelJob() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => api.cancelJob(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['jobs'] }),
  })
}

// Jobs dashboard's per-row priority menu - same immediate-invalidation
// rationale as useCancelJob, so a promoted row's new tier (and its
// resulting position in the queue) shows up right away rather than
// waiting for the next poll/WS push.
export function useSetJobTier() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, tier }: { id: string; tier: QueueTask['tier'] }) => api.setJobTier(id, tier),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['jobs'] }),
  })
}

// Jobs dashboard's "Cancel all" button - same immediate-invalidation
// rationale as useCancelJob.
export function useCancelAllJobs() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.cancelAllJobs(),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['jobs'] }),
  })
}

// Jobs dashboard's "Pause"/"Resume" toggle button - same immediate-
// invalidation rationale as useCancelJob, though the websocket already
// pushes the new paused state right away too (jobs.Manager.Pause/Resume
// both call notifyChanged) - this just covers the gap before that
// push arrives, or a connection that's briefly down.
export function usePauseJobs() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.pauseJobs(),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['jobs'] }),
  })
}

export function useResumeJobs() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.resumeJobs(),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['jobs'] }),
  })
}

// Jobs dashboard's "Restart ttsworker" button - forces the same kind of
// restart the watchdog does automatically on an RSS breach/crash, for a
// reader who wants to recover a stuck or visibly-leaking worker right now
// instead of waiting for it. The request itself blocks until the new
// worker is healthy, so isPending covers that whole window.
export function useRestartWorker() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.restartWorker(),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['jobs'] }),
  })
}
