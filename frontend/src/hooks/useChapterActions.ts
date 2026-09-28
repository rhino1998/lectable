import { useCallback, useState } from 'react'
import {
  useAmbienceChapters,
  useAttributeSpeakers,
  useAttributingChapters,
  useDeleteChapterAudio,
  useDescribingChapters,
  useDirectingChapters,
  useGenerateChapter,
  useGeneratingChapters,
  usePronouncingChapters,
  useRegenerateMusicRegion,
  useResolvePronunciation,
  useRetagDescriptions,
  useRetagScareQuotes,
  useRunChapterAmbience,
  useScareQuotingChapters,
  useTagDirections,
} from '../api/queries'
import { ApiError } from '../api/client'
import { useReimportChapterAction } from './useReimportChapterAction'

// Chapter-header actions: clear a chapter's own generated audio (a
// scalpel next to VoicePanel's whole-book "Delete generated audio"), and
// run/re-run speaker attribution for just that chapter without going to
// the Speakers page - see SpeakersPage's own "Attribute speakers" button
// for the fuller roster-management version of the same call.
//
// Every run* callback is useCallback-stable (keyed on its own mutation
// object) so ChapterSection's memo holds, and every failure lands in the one
// shared `error` - the reader shows it under its header. `setError` is
// exposed for other reader actions (the paragraph context menu) that report
// into the same slot.
export function useChapterActions(bookId: string) {
  const deleteChapterAudio = useDeleteChapterAudio(bookId)
  const attributeSpeakers = useAttributeSpeakers(bookId)
  const attributingIdxs = useAttributingChapters(bookId)
  const retagDescriptions = useRetagDescriptions(bookId)
  const retagScareQuotes = useRetagScareQuotes(bookId)
  const tagDirections = useTagDirections(bookId)
  const directingIdxs = useDirectingChapters(bookId)
  const resolvePronunciation = useResolvePronunciation(bookId)
  const pronouncingIdxs = usePronouncingChapters(bookId)
  const runChapterAmbience = useRunChapterAmbience(bookId)
  const ambienceIdxs = useAmbienceChapters(bookId)
  const generateChapter = useGenerateChapter(bookId)
  const generatingIdxs = useGeneratingChapters(bookId)
  const regenerateMusicRegion = useRegenerateMusicRegion(bookId)
  const [error, setError] = useState<string | null>(null)
  // Description/scare-quote tagging are queued jobs, derived from the live
  // job-queue topic like attributingIdxs/directingIdxs above.
  const describingIdxs = useDescribingChapters(bookId)
  const scareQuotingIdxs = useScareQuotingChapters(bookId)

  const runClearChapterAudio = useCallback(
    (idx: number, title: string) => {
      if (
        !confirm(
          `Clear all generated audio for "${title}"? Every paragraph in this chapter will need to be regenerated.`,
        )
      ) {
        return
      }
      setError(null)
      deleteChapterAudio.mutate(idx, {
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : "Could not clear this chapter's audio"),
      })
    },
    [deleteChapterAudio],
  )

  const { run: runReimportChapter, reimportingIdx } = useReimportChapterAction(bookId, setError)

  const runAttributeChapter = useCallback(
    (idx: number) => {
      setError(null)
      attributeSpeakers.mutate(idx, {
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Speaker attribution failed'),
      })
    },
    [attributeSpeakers],
  )

  const runRetagDescriptionsChapter = useCallback(
    (idx: number) => {
      setError(null)
      retagDescriptions.mutate(idx, {
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Description tagging failed'),
      })
    },
    [retagDescriptions],
  )

  const runRetagScareQuotesChapter = useCallback(
    (idx: number) => {
      setError(null)
      retagScareQuotes.mutate(idx, {
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Scare-quote tagging failed'),
      })
    },
    [retagScareQuotes],
  )

  const runTagDirectionsChapter = useCallback(
    (idx: number) => {
      setError(null)
      tagDirections.mutate(idx, {
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Speech-direction tagging failed'),
      })
    },
    [tagDirections],
  )

  const runResolvePronunciationChapter = useCallback(
    (idx: number) => {
      setError(null)
      resolvePronunciation.mutate(idx, {
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Pronunciation resolution failed'),
      })
    },
    [resolvePronunciation],
  )

  const runAmbienceChapter = useCallback(
    (idx: number) => {
      setError(null)
      runChapterAmbience.mutate(idx, {
        onError: (err) => setError(err instanceof ApiError ? err.message : 'Ambience pass failed'),
      })
    },
    [runChapterAmbience],
  )

  const runGenerateChapter = useCallback(
    (idx: number) => {
      setError(null)
      generateChapter.mutate(idx, {
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Audio generation failed'),
      })
    },
    [generateChapter],
  )

  const runRegenerateMusic = useCallback(
    (chapterIdx: number, regionId: string) => {
      setError(null)
      regenerateMusicRegion.mutate(
        { regionId, chapterIdx },
        {
          onError: (err) =>
            setError(err instanceof ApiError ? err.message : 'Could not generate background music'),
        },
      )
    },
    [regenerateMusicRegion],
  )

  return {
    error,
    setError,
    attributingIdxs,
    describingIdxs,
    scareQuotingIdxs,
    directingIdxs,
    pronouncingIdxs,
    ambienceIdxs,
    generatingIdxs,
    reimportingIdx,
    clearingIdx: deleteChapterAudio.isPending ? deleteChapterAudio.variables : undefined,
    runChapterAmbience,
    runClearChapterAudio,
    runReimportChapter,
    runAttributeChapter,
    runRetagDescriptionsChapter,
    runRetagScareQuotesChapter,
    runTagDirectionsChapter,
    runResolvePronunciationChapter,
    runAmbienceChapter,
    runGenerateChapter,
    runRegenerateMusic,
  }
}
