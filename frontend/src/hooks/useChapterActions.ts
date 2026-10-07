import { useCallback, useState } from 'react'
import {
  useAmbienceChapters,
  useAttributeAroundEdits,
  useAttributeSpeakers,
  useAttributingChapters,
  useDeleteChapterAudio,
  useDirectingChapters,
  useGenerateChapter,
  useGeneratingChapters,
  usePronouncingChapters,
  useRegenerateChapterNotes,
  useRegenerateMusicRegion,
  useResolvePronunciation,
  useRetagScareQuotes,
  useRunChapterAmbience,
  useSaveChapterNotes,
  useScareQuotingChapters,
  useTagDirections,
} from '../api/queries'
import { ApiError } from '../api/client'
import type { ChapterNotes } from '../api/types'
import { useReimportChapterAction } from './useReimportChapterAction'

// Chapter-header actions: clear a chapter's own generated audio (a
// scalpel next to VoicePanel's whole-book "Delete generated audio"), and
// run/re-run speaker attribution for just that chapter without going to
// the Book settings page - see BookSettingsPage's own "Attribute speakers" button
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
  const attributeAroundEdits = useAttributeAroundEdits(bookId)
  const saveChapterNotes = useSaveChapterNotes(bookId)
  const regenerateChapterNotes = useRegenerateChapterNotes(bookId)
  const attributingIdxs = useAttributingChapters(bookId)
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
  // Scare-quote tagging is a queued job, derived from the live
  // job-queue topic like attributingIdxs/directingIdxs above.
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

  // Re-asks only the dialogue near lines the reader corrected, showing the
  // model those corrections - the follow-up to fixing a few speakers.
  const runAttributeAroundEdits = useCallback(
    (idx: number) => {
      setError(null)
      attributeAroundEdits.mutate(idx, {
        onError: (err) =>
          setError(
            err instanceof ApiError && err.code === 'no_edits'
              ? 'No corrected speakers in this chapter yet — fix a line first (right-click it)'
              : err instanceof ApiError
                ? err.message
                : 'Speaker attribution failed',
          ),
      })
    },
    [attributeAroundEdits],
  )

  // Saves the reader's notes; resolves once stored, and rejects with the
  // backend's message (a group name, a scene listing someone not in the
  // cast) for the editor to show beside its fields.
  const saveNotes = useCallback(
    (idx: number, notes: ChapterNotes) =>
      saveChapterNotes.mutateAsync({ chapterIdx: idx, notes }).then(() => undefined),
    [saveChapterNotes],
  )

  const runRegenerateNotes = useCallback(
    (idx: number) => {
      if (!confirm("Regenerate this chapter's notes? Your edits to them will be replaced.")) return
      setError(null)
      regenerateChapterNotes.mutate(idx, {
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Could not regenerate the notes'),
      })
    },
    [regenerateChapterNotes],
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
    runAttributeAroundEdits,
    saveNotes,
    runRegenerateNotes,
    runRetagScareQuotesChapter,
    runTagDirectionsChapter,
    runResolvePronunciationChapter,
    runAmbienceChapter,
    runGenerateChapter,
    runRegenerateMusic,
  }
}
