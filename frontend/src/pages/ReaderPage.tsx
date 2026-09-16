import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react'
import { useParams } from '@tanstack/react-router'
import {
  useAttributeSpeakers,
  useAttributingChapters,
  useBook,
  useBookmarks,
  useChapterMusicRange,
  useChapterRange,
  useCreateBookmark,
  useCustomVoicePresets,
  useDeleteBookmark,
  useDeleteChapterAudio,
  useDirectingChapters,
  useGenerateChapter,
  useGenerateParagraphSFX,
  useGeneratingChapters,
  useLookahead,
  useRegenerateParagraph,
  useRegenerateMusicRegion,
  useRetagDescriptions,
  useRetagScareQuotes,
  useScoreChapterMusic,
  useScoringMusicChapters,
  useSetParagraphDescription,
  useSetParagraphScareQuote,
  useSetParagraphSFXPrompt,
  useSetParagraphSpeaker,
  useSpeakers,
  useTagDirections,
  useUpdatePosition,
  useVoice,
  useVoicePresets,
} from '../api/queries'
import { ApiError } from '../api/client'
import { DEFAULT_CLONE_MODEL } from '../components/VoiceEditorForm'
import { usePlayback } from '../hooks/usePlayback'
import { useBackgroundMusic } from '../hooks/useBackgroundMusic'
import { useInView } from '../hooks/useInView'
import { useClickOutside } from '../hooks/useClickOutside'
import { useBookUpdates } from '../hooks/useBookUpdates'
import { useMediaSession } from '../hooks/useMediaSession'
import { READER_FONT_FAMILIES, useReaderTypography } from '../hooks/useReaderTypography'
import { PlayerBar } from '../components/PlayerBar'
import { ChapterSection } from '../components/ChapterSection'
import { AnnotationTooltip } from '../components/AnnotationTooltip'
import type { AnnotationTooltipHandle } from '../components/AnnotationTooltip'
import type { ChapterDetail, MusicRegion, Paragraph } from '../api/types'
import { matchesSelectedSpeaker } from '../utils/annotations'

// Wraps `fn` in a stable function identity (never changes across renders)
// that always calls whatever the latest `fn` actually is - the same
// "latest ref" pattern usePlayback's own stateRef uses, applied here so
// callbacks handed down to memoized children (ChapterSection/
// ParagraphGroup) don't force them to re-render just because the
// underlying handler closed over some value that changed (e.g. a mutation
// object TanStack Query re-creates each render).
function useStableCallback<A extends unknown[], R>(fn: (...args: A) => R): (...args: A) => R {
  const fnRef = useRef(fn)
  fnRef.current = fn
  return useCallback((...args: A) => fnRef.current(...args), [])
}

// How many loaded chapters on each side of whichever one is actually on
// screen stay fully mounted - see the chapterHeights/registerHeight
// windowing below. Generous enough that ordinary scrolling (including a
// fast flick) never visibly pops a chapter from placeholder to full
// content mid-view; chapters already fetched and rendered once, so
// expanding a placeholder back to full content is a synchronous re-render,
// not a network wait.
const RENDER_WINDOW_RADIUS = 2

// A stable empty-array reference for a chapter with no music regions (or
// while annotations view is off, so none were fetched at all) - handing
// ChapterSection a fresh `[]` literal every render would defeat its own
// memo (a new array reference always compares as "changed"), the same
// reasoning the stable-identity useStableCallback wrappers below exist for.
const EMPTY_MUSIC_REGIONS: MusicRegion[] = []

export function ReaderPage() {
  const { bookId } = useParams({ from: '/books/$bookId' })
  const bookQuery = useBook(bookId)
  const updatePosition = useUpdatePosition(bookId)
  const lookahead = useLookahead(bookId)
  useBookUpdates(bookId)
  const typography = useReaderTypography()

  const bookmarksQuery = useBookmarks(bookId)
  const createBookmark = useCreateBookmark(bookId)
  const deleteBookmark = useDeleteBookmark(bookId)
  const bookmarkByKey = useMemo(() => {
    const map = new Map<string, string>() // "chapterIdx:paragraphIdx" -> bookmark id
    for (const b of bookmarksQuery.data ?? []) map.set(`${b.chapterIdx}:${b.paragraphIdx}`, b.id)
    return map
  }, [bookmarksQuery.data])
  // Stable identity (see useStableCallback) so passing it down through
  // ChapterSection/ParagraphGroup doesn't defeat their memoization every
  // time a bookmark is added/removed and bookmarkByKey gets a new Map.
  const toggleBookmark = useStableCallback((chapterIdx: number, paragraphIdx: number) => {
    const existingId = bookmarkByKey.get(`${chapterIdx}:${paragraphIdx}`)
    if (existingId) deleteBookmark.mutate(existingId)
    else createBookmark.mutate({ chapterIdx, paragraphIdx })
  })

  const regenerateParagraph = useRegenerateParagraph(bookId)
  const regenerateParagraphAt = useStableCallback((chapterIdx: number, paragraphIdx: number) => {
    regenerateParagraph.mutate({ chapterIdx, paragraphIdx })
  })

  // Sound-effect (Stable Audio SFX) test surface - see backend/CLAUDE.md's
  // "SFX sound effects" section and ChapterSection's own SFX panel.
  // onGenerateSFX returns a promise (mutateAsync) rather than firing-and-
  // forgetting like the other handlers here, since ParagraphGroup's own
  // panel drives its busy/error state directly off it - the promise only
  // covers the enqueue request itself (generation is a pooled backend
  // job), so the panel still separately watches sfxStatus for real
  // completion (see chapterQueryOptions' own tighter poll while it's
  // 'generating').
  const setSFXPrompt = useSetParagraphSFXPrompt(bookId)
  const generateSFX = useGenerateParagraphSFX(bookId)
  const setSFXPromptAt = useStableCallback((chapterIdx: number, paragraphIdx: number, prompt: string, triggerWord?: number) => {
    setSFXPrompt.mutate({ chapterIdx, paragraphIdx, prompt, triggerWord })
  })
  const generateSFXAt = useStableCallback(async (chapterIdx: number, paragraphIdx: number, prompt: string) => {
    await generateSFX.mutateAsync({ chapterIdx, paragraphIdx, prompt })
  })
  // lastSFXKeyRef backs the sound-effect overlay-playback effect below -
  // declared here (before playback/chapters exist yet) since hooks must
  // run unconditionally in the same order every render; the effect itself
  // is defined further down, once both are in scope.
  const lastSFXKeyRef = useRef<string | null>(null)

  // Chapter-header actions: clear a chapter's own generated audio (a
  // scalpel next to VoicePanel's whole-book "Delete generated audio"), and
  // run/re-run speaker attribution for just that chapter without going to
  // the Speakers page - see SpeakersPage's own "Attribute speakers" button
  // for the fuller roster-management version of the same call.
  const deleteChapterAudio = useDeleteChapterAudio(bookId)
  const attributeSpeakers = useAttributeSpeakers(bookId)
  const attributingIdxs = useAttributingChapters(bookId)
  const retagDescriptions = useRetagDescriptions(bookId)
  const retagScareQuotes = useRetagScareQuotes(bookId)
  const tagDirections = useTagDirections(bookId)
  const directingIdxs = useDirectingChapters(bookId)
  const scoreChapterMusic = useScoreChapterMusic(bookId)
  const scoringMusicIdxs = useScoringMusicChapters(bookId)
  const generateChapter = useGenerateChapter(bookId)
  const generatingIdxs = useGeneratingChapters(bookId)
  const regenerateMusicRegion = useRegenerateMusicRegion(bookId)
  const [chapterActionError, setChapterActionError] = useState<string | null>(null)
  // useRetagDescriptions/useRetagScareQuotes are blocking (see SpeakersPage's
  // own retaggingIdx/retaggingScareQuoteIdx doc comment for why) - unlike
  // attributingIdxs/directingIdxs/scoringMusicIdxs/generatingIdxs above,
  // which are all derived from the shared job-queue poll, "which chapter is
  // retagging right now" needs its own locally-tracked id per button.
  const [retaggingDescriptionsIdx, setRetaggingDescriptionsIdx] = useState<number | null>(null)
  const [retaggingScareQuotesIdx, setRetaggingScareQuotesIdx] = useState<number | null>(null)

  // Mirrors SpeakersPage's own effectiveCloneModel/directionSupported: the
  // chapter-header direction-tagging button is only offered when the
  // book's resolved voice actually clones through Higgs, the only clone
  // model whose tokenizer understands this tag vocabulary - see
  // SpeakersPage's own doc comment on directionSupported for the fuller
  // reasoning (the backend enforces the same check server-side regardless,
  // this is just so the button isn't offered when it can't work).
  const voiceQuery = useVoice(bookId)
  const builtinPresetsQuery = useVoicePresets()
  const customPresetsQuery = useCustomVoicePresets()
  const effectiveCloneModel =
    builtinPresetsQuery.data?.presets.find((p) => p.id === voiceQuery.data?.presetId)?.cloneModel ??
    customPresetsQuery.data?.find((p) => p.id === voiceQuery.data?.presetId)?.cloneModel ??
    DEFAULT_CLONE_MODEL
  const directionSupported = effectiveCloneModel === DEFAULT_CLONE_MODEL

  // Colors narrator/speaker/description segments in the paragraph list
  // instead of leaving that distinction to the speaker-hint badge alone
  // (see annotationKind above) - off by default since the coloring adds
  // visual noise a reader may not want while just listening.
  const [annotationsView, setAnnotationsView] = useState(false)
  // Clicking a name in PlayerBar's speaker list selects it here, which
  // adds an extra highlight (on top of the normal narrator/speaker/
  // description coloring) to every segment attributed to them - lets a
  // reader visually scan for one character's lines specifically instead
  // of just "this is dialogue" in general. Cleared whenever annotations
  // view itself is turned off, since the selection has no visible effect
  // (and no way to change it) while the toggle's off anyway.
  const [selectedSpeaker, setSelectedSpeaker] = useState<string | null>(null)

  // Right-click "reassign speaker"/"reassign description" menu (annotations
  // view only) - see openSpeakerMenu/reassignSpeaker/reassignDescription
  // below and their render near the end of this component. speakersQuery
  // backs the menu's own "reassign to…" list, same query
  // useSetParagraphSpeaker/useSetParagraphDescription's own onSuccess
  // invalidates, so a name just typed in via UpsertCharacter (a brand-new
  // speaker) shows up here next time the menu opens without a separate
  // fetch.
  const speakersQuery = useSpeakers(bookId)
  const setParagraphSpeaker = useSetParagraphSpeaker(bookId)
  const setParagraphDescription = useSetParagraphDescription(bookId)
  const setParagraphScareQuote = useSetParagraphScareQuote(bookId)
  const [contextMenu, setContextMenu] = useState<{
    chapterIdx: number
    paragraphIdx: number
    isQuote: boolean
    currentSpeaker: string
    describesCharacters: string[]
    scareQuote: boolean
    x: number
    y: number
  } | null>(null)
  const contextMenuRef = useRef<HTMLDivElement>(null)
  useClickOutside([contextMenuRef], () => setContextMenu(null), contextMenu !== null)

  const openSpeakerMenu = useCallback(
    (e: React.MouseEvent, chapterIdx: number, p: Paragraph) => {
      if (!annotationsView) return
      e.preventDefault()
      e.stopPropagation()
      setContextMenu({
        chapterIdx,
        paragraphIdx: p.idx,
        isQuote: !!p.isQuote,
        currentSpeaker: p.speaker ?? '',
        describesCharacters: p.describesCharacters ?? [],
        scareQuote: !!p.scareQuote,
        x: e.clientX,
        y: e.clientY,
      })
    },
    [annotationsView],
  )

  // Corrects the menu's raw click-point position (contextMenu.x/y) to keep
  // it fully on screen - clamped against both the viewport edges and
  // .player-bar's own top edge, since the raw position alone can otherwise
  // render the menu (or its lower rows) behind the fixed bottom bar or off
  // the right/bottom of the window when the paragraph clicked sits near
  // either. Runs via useLayoutEffect (not useEffect) so the corrected
  // position is committed before the browser ever paints the menu at its
  // raw, unclamped spot - no visible jump. null until measured, so the
  // menu's own style falls back to the raw click point for that first,
  // pre-measurement render.
  const [menuPos, setMenuPos] = useState<{ left: number; top: number } | null>(null)
  useLayoutEffect(() => {
    if (!contextMenu) {
      setMenuPos(null)
      return
    }
    const el = contextMenuRef.current
    if (!el) return
    const margin = 8
    const rect = el.getBoundingClientRect()
    const playerBar = document.querySelector('.player-bar')
    const bottomBoundary = playerBar ? playerBar.getBoundingClientRect().top : window.innerHeight
    const left = Math.min(contextMenu.x, window.innerWidth - rect.width - margin)
    const top = Math.min(contextMenu.y, bottomBoundary - rect.height - margin)
    setMenuPos({ left: Math.max(margin, left), top: Math.max(margin, top) })
  }, [contextMenu])

  const reassignTargets = useMemo(
    () => [...new Set((speakersQuery.data ?? []).map((s) => s.name).filter((n) => n !== '' && n !== 'Narrator'))],
    [speakersQuery.data],
  )

  const reassignSpeaker = useCallback(
    (speaker: string) => {
      if (!contextMenu) return
      setChapterActionError(null)
      setParagraphSpeaker.mutate(
        { chapterIdx: contextMenu.chapterIdx, paragraphIdx: contextMenu.paragraphIdx, speaker },
        { onError: (err) => setChapterActionError(err instanceof ApiError ? err.message : 'Could not reassign this paragraph') },
      )
      setContextMenu(null)
    },
    [contextMenu, setParagraphSpeaker],
  )

  // reassignSpeaker's own counterpart for a *description* paragraph - moves
  // it off `from`'s own describes-list and onto `to`'s instead ("" clears
  // it, describing no one), leaving any other character this same
  // paragraph also describes untouched (a description paragraph can name
  // more than one - see the menu's own per-name rows below).
  const reassignDescription = useCallback(
    (from: string, to: string) => {
      if (!contextMenu) return
      setChapterActionError(null)
      setParagraphDescription.mutate(
        { chapterIdx: contextMenu.chapterIdx, paragraphIdx: contextMenu.paragraphIdx, from, to },
        { onError: (err) => setChapterActionError(err instanceof ApiError ? err.message : 'Could not reassign this description') },
      )
      setContextMenu(null)
    },
    [contextMenu, setParagraphDescription],
  )

  // Live "mark/unmark as scare quote" toggle - see
  // handleSetParagraphScareQuote's own doc comment for why this (unlike
  // reassignSpeaker/reassignDescription above) immediately invalidates the
  // paragraph's own audio server-side rather than requiring a separate
  // explicit regenerate.
  const toggleScareQuote = useCallback(() => {
    if (!contextMenu) return
    setChapterActionError(null)
    setParagraphScareQuote.mutate(
      { chapterIdx: contextMenu.chapterIdx, paragraphIdx: contextMenu.paragraphIdx, scareQuote: !contextMenu.scareQuote },
      { onError: (err) => setChapterActionError(err instanceof ApiError ? err.message : 'Could not update scare-quote status') },
    )
    setContextMenu(null)
  }, [contextMenu, setParagraphScareQuote])

  const runClearChapterAudio = useCallback(
    (idx: number, title: string) => {
      if (!confirm(`Clear all generated audio for "${title}"? Every paragraph in this chapter will need to be regenerated.`)) {
        return
      }
      setChapterActionError(null)
      deleteChapterAudio.mutate(idx, {
        onError: (err) =>
          setChapterActionError(err instanceof ApiError ? err.message : 'Could not clear this chapter\'s audio'),
      })
    },
    [deleteChapterAudio],
  )

  const runAttributeChapter = useCallback(
    (idx: number) => {
      setChapterActionError(null)
      attributeSpeakers.mutate(idx, {
        onError: (err) =>
          setChapterActionError(err instanceof ApiError ? err.message : 'Speaker attribution failed'),
      })
    },
    [attributeSpeakers],
  )

  const runRetagDescriptionsChapter = useCallback(
    (idx: number) => {
      setChapterActionError(null)
      setRetaggingDescriptionsIdx(idx)
      retagDescriptions.mutate(idx, {
        onError: (err) =>
          setChapterActionError(err instanceof ApiError ? err.message : 'Description tagging failed'),
        onSettled: () => setRetaggingDescriptionsIdx(null),
      })
    },
    [retagDescriptions],
  )

  const runRetagScareQuotesChapter = useCallback(
    (idx: number) => {
      setChapterActionError(null)
      setRetaggingScareQuotesIdx(idx)
      retagScareQuotes.mutate(idx, {
        onError: (err) =>
          setChapterActionError(err instanceof ApiError ? err.message : 'Scare-quote tagging failed'),
        onSettled: () => setRetaggingScareQuotesIdx(null),
      })
    },
    [retagScareQuotes],
  )

  const runTagDirectionsChapter = useCallback(
    (idx: number) => {
      setChapterActionError(null)
      tagDirections.mutate(idx, {
        onError: (err) =>
          setChapterActionError(err instanceof ApiError ? err.message : 'Speech-direction tagging failed'),
      })
    },
    [tagDirections],
  )

  const runScoreMusicChapter = useCallback(
    (idx: number) => {
      setChapterActionError(null)
      scoreChapterMusic.mutate(idx, {
        onError: (err) =>
          setChapterActionError(err instanceof ApiError ? err.message : 'Background-music scoring failed'),
      })
    },
    [scoreChapterMusic],
  )

  const runGenerateChapter = useCallback(
    (idx: number) => {
      setChapterActionError(null)
      generateChapter.mutate(idx, {
        onError: (err) =>
          setChapterActionError(err instanceof ApiError ? err.message : 'Audio generation failed'),
      })
    },
    [generateChapter],
  )

  const runRegenerateMusic = useCallback(
    (chapterIdx: number, regionId: string) => {
      setChapterActionError(null)
      regenerateMusicRegion.mutate(
        { regionId, chapterIdx },
        {
          onError: (err) =>
            setChapterActionError(err instanceof ApiError ? err.message : 'Could not generate background music'),
        },
      )
    },
    [regenerateMusicRegion],
  )

  // The window of chapters currently mounted in the scroll list. Grows
  // downward/upward as the reader scrolls near either edge, instead of
  // requiring a chapter to be explicitly selected.
  const [range, setRange] = useState<{ start: number; end: number } | null>(null)

  useEffect(() => {
    if (bookQuery.data && range === null) {
      setRange({ start: bookQuery.data.posChapterIdx, end: bookQuery.data.posChapterIdx })
    }
  }, [bookQuery.data, range])

  const totalChapters = bookQuery.data?.chapters.length ?? 0
  const chapterResults = useChapterRange(bookId, range?.start ?? 0, range?.end ?? -1)

  const chapters = useMemo(() => {
    const map = new Map<number, ChapterDetail>()
    for (const r of chapterResults) {
      if (r.data) map.set(r.data.idx, r.data)
    }
    return map
  }, [chapterResults])

  // Background-music regions for every currently-loaded chapter - backs
  // annotations view's region boundary markers (ChapterSection.
  // MusicRegionBoundary). Only actually polled while annotations view is
  // on (see useChapterMusicRange's own `enabled` doc comment) - otherwise
  // every loaded chapter would pay for a music-regions fetch a reader who
  // never opens annotations view will never see. ChapterMusic itself
  // carries no chapter idx of its own (unlike ChapterDetail), so this maps
  // by the range's own position rather than reading one back off the data.
  const musicResults = useChapterMusicRange(bookId, range?.start ?? 0, range?.end ?? -1, annotationsView)
  const chapterMusicRegions = useMemo(() => {
    const map = new Map<number, MusicRegion[]>()
    const start = range?.start ?? 0
    musicResults.forEach((r, i) => {
      if (r.data) map.set(start + i, r.data.regions)
    })
    return map
  }, [musicResults, range])

  // Speakers already narrating paragraphs near contextMenu's own paragraph,
  // within the same chapter (the same scene/conversation), closest
  // occurrence first - mirrors android's nearbySpeakersFor/
  // SpeakerPickerSheet exactly, including the "Narrator/blank excluded,
  // dialogue paragraphs only" filter (a misattributed line is usually
  // meant for whoever else is already talking around it, not some
  // character from a completely different part of the book). Distance is
  // measured in paragraph index, either direction. Only ever backs the
  // "Reassign speaker" branch below - android doesn't do this for
  // description reassignment either (DescriptionPickerSheet stays a flat
  // roster list), so neither does this.
  const nearbySpeakers = useMemo(() => {
    if (!contextMenu || !contextMenu.isQuote) return []
    const chapter = chapters.get(contextMenu.chapterIdx)
    if (!chapter) return []
    const targetIdx = contextMenu.paragraphIdx
    return [
      ...new Set(
        chapter.paragraphs
          .filter((p) => p.idx !== targetIdx && p.speaker && p.speaker !== 'Narrator')
          .sort((a, b) => Math.abs(a.idx - targetIdx) - Math.abs(b.idx - targetIdx))
          .map((p) => p.speaker as string),
      ),
    ]
  }, [contextMenu, chapters])

  // Hoists nearbySpeakers ahead of the rest of the book's full roster in
  // the "Reassign speaker" menu, under its own section header - same
  // "restrict to the known roster, then split into nearby/rest" shape
  // android's own SpeakerPickerSheet uses. With nothing nearby, this just
  // falls back to reassignTargets as a flat list, same as it always was.
  const nearbyReassignTargets = useMemo(
    () => nearbySpeakers.filter((name) => reassignTargets.includes(name)),
    [nearbySpeakers, reassignTargets],
  )
  const restReassignTargets = useMemo(
    () => reassignTargets.filter((name) => !nearbyReassignTargets.includes(name)),
    [reassignTargets, nearbyReassignTargets],
  )

  const expandDown = useCallback(() => {
    setRange((r) => (r ? { ...r, end: Math.min(r.end + 1, totalChapters - 1) } : r))
  }, [totalChapters])
  const expandUp = useCallback(() => {
    setRange((r) => (r ? { ...r, start: Math.max(r.start - 1, 0) } : r))
  }, [])

  const handlePosition = useCallback(
    (chIdx: number, paraIdx: number, seconds: number) => {
      updatePosition.mutate({ chapterIdx: chIdx, paragraphIdx: paraIdx, seconds })
    },
    [updatePosition],
  )

  const playback = usePlayback({
    chapters,
    totalChapters,
    initialChapterIdx: bookQuery.data?.posChapterIdx ?? 0,
    initialParagraphIdx: bookQuery.data?.posParagraphIdx ?? 0,
    initialSeconds: bookQuery.data?.posSeconds ?? 0,
    ready: bookQuery.data !== undefined,
    onPosition: handlePosition,
    onNeedChapter: expandDown,
  })

  // Chapter background music - an entirely separate, parallel Web Audio
  // graph mixed in underneath playback's own plain <audio> narration (see
  // useBackgroundMusic's own doc comment). musicEnabled is book-wide (see
  // BookSummary.musicEnabled/VoiceSettings.musicEnabled) - not gated on
  // chapters actually being loaded into the `chapters` map the way
  // playback itself is, since it only ever needs the book's own toggle
  // plus the currently-playing chapter's own regions (and, for a
  // cross-chapter pre-roll near a chapter's end, the next one's - see the
  // hook's own doc comment on chapterParagraphCount/totalChapters).
  useBackgroundMusic({
    bookId,
    chapterIdx: playback.chapterIdx,
    chapterParagraphCount: chapters.get(playback.chapterIdx)?.paragraphs.length ?? 0,
    totalChapters,
    paragraphIdx: playback.paragraphIdx,
    paragraphCurrentTime: playback.currentTime,
    paragraphDuration: playback.duration,
    musicEnabled: !!bookQuery.data?.musicEnabled,
    isPlaying: playback.isPlaying,
    playbackRate: playback.playbackRate,
  })

  // Lazily scores whichever chapter the reader is currently on, the first
  // time it's needed - background music is a book-wide toggle
  // (BookSummary.musicEnabled) but scoring itself is real per-chapter LLM
  // work (store.Passes.Music), so turning the toggle on doesn't eagerly
  // score every chapter in the book at once; instead each chapter gets
  // scored the first time playback actually reaches it, mirroring how
  // narration itself only generates lookahead-bounded runway rather than
  // the whole book up front. Re-fires as playback advances into a new
  // chapter; api.scoreChapterMusic dedups server-side (one in-flight
  // scoring task per chapter - see jobs.Manager's dedup), so calling this
  // opportunistically on every chapter change is safe. Skipped entirely
  // once already scored (passes.music) or already in flight
  // (scoringMusicIdxs, from the shared job queue).
  useEffect(() => {
    if (!bookQuery.data?.musicEnabled) return
    const chapterSummary = bookQuery.data.chapters[playback.chapterIdx]
    if (!chapterSummary || chapterSummary.passes.music) return
    if (scoringMusicIdxs.has(playback.chapterIdx)) return
    scoreChapterMusic.mutate(playback.chapterIdx)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [bookQuery.data?.musicEnabled, bookQuery.data?.chapters, playback.chapterIdx, scoringMusicIdxs])

  // Stable-identity wrappers around playback's own playAt/seek - playback
  // itself is a new object every time usePlayback's internal state ticks
  // (e.g. every 'timeupdate'), which would otherwise defeat ChapterSection/
  // ParagraphGroup's memoization on every such tick even though the actual
  // behavior these close over hasn't changed.
  const playAt = useStableCallback((chapterIdx: number, paragraphIdx: number) => playback.playAt(chapterIdx, paragraphIdx))
  const seek = useStableCallback((seconds: number) => playback.seek(seconds))

  // Sound-effect overlay playback: a brand-new, independent <audio> (via
  // the Audio() constructor, not playback's own audioElement) fired once
  // playback actually reaches this paragraph's own chosen trigger word
  // (sfxTriggerWord, a 0-based index into words - see ChapterSection's own
  // word-picker), not merely once the paragraph starts - deliberately
  // never stopped/tracked past that point, so a clip longer than the rest
  // of its own paragraph keeps playing to completion even once playback
  // has moved on to a later one (see backend/CLAUDE.md's "SFX sound
  // effects" section: overlap with later paragraphs is fine, an explicit
  // stop on advance is not). Re-checked on every playback.currentTime
  // update (usePlayback's own 'timeupdate'-driven state, a few times a
  // second - plenty of precision for triggering a sound cue, unlike
  // per-frame word highlighting) rather than only on paragraph change, so
  // landing mid-paragraph past the trigger word (a seek, or a paragraph
  // entered already past it) still fires immediately instead of never.
  // Keyed off (chapterIdx, paragraphIdx) so this fires at most once per
  // paragraph visit even though the effect re-runs many times while
  // playing it, and gated on isPlaying so a paragraph merely landed on
  // while paused never triggers one - matching how narration itself only
  // plays on Play.
  useEffect(() => {
    if (!playback.isPlaying) return
    const key = `${playback.chapterIdx}:${playback.paragraphIdx}`
    if (lastSFXKeyRef.current === key) return
    const p = chapters.get(playback.chapterIdx)?.paragraphs[playback.paragraphIdx]
    if (p?.sfxStatus !== 'ready' || !p.sfxAudioUrl) return
    const triggerTime = p.words[p.sfxTriggerWord ?? 0]?.start ?? 0
    if (playback.currentTime < triggerTime) return
    lastSFXKeyRef.current = key
    new Audio(p.sfxAudioUrl).play().catch(() => {
      // Autoplay blocked (no user gesture on this tab yet) - nothing to
      // recover from; a later paragraph's own overlay isn't affected.
    })
  }, [playback.isPlaying, playback.chapterIdx, playback.paragraphIdx, playback.currentTime, chapters])

  // ParagraphText only ever reads the audioElement prop while its own
  // `active` is true (see that component), so a non-active chapter's
  // ChapterSection has no actual use for the real, ever-swapping
  // playback.audioElement - handing it out to every loaded chapter anyway
  // would just make all of them "change props" (and thus re-render) on
  // every paragraph transition's active/standby swap. This inert element
  // (never given a src, so no network/decoder activity) is a stable,
  // never-changing stand-in for every chapter that isn't the one playing.
  const inertAudioRef = useRef<HTMLAudioElement | null>(null)
  if (inertAudioRef.current === null) inertAudioRef.current = new Audio()

  // Keeps generation running ~10 paragraphs ahead of wherever the reader
  // currently is, spanning into later chapters as needed rather than
  // stopping at the current chapter's end. Re-fires as playback advances;
  // the backend dedups (one in-flight lookahead per book), so this is safe
  // to call opportunistically on every position change.
  //
  // Suppressed entirely while any of this book's chapters has a speaker
  // attribution task in flight (attributingIdxs, below): attribution can
  // reassign a paragraph's Speaker, and with multiVoice on that changes
  // which voice it resolves to (internal/narration.Resolver) - audio
  // generated from a pre-attribution guess would just get invalidated
  // and regenerated once attribution lands, wasting a real TTS render.
  // Also re-fires the moment attributingIdxs drains back to empty (it's a
  // dependency here too, not just position), so lookahead picks back up
  // immediately once attribution finishes rather than waiting for the
  // reader's next position change.
  useEffect(() => {
    if (attributingIdxs.size > 0) return
    lookahead.mutate({ chapterIdx: playback.chapterIdx, paragraphIdx: playback.paragraphIdx })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [playback.chapterIdx, playback.paragraphIdx, attributingIdxs.size])

  // Recomputed client-side from the book's chapter/paragraph counts plus
  // live playback position, so it updates in real time as the reader
  // listens rather than only refreshing when the book query refetches.
  // Mirrors the backend's toBookSummary calculation exactly.
  const { liveProgressPercent, liveFinished } = useMemo(() => {
    const bookChapters = bookQuery.data?.chapters ?? []
    let total = 0
    let completed = 0
    let finished = false
    bookChapters.forEach((c, i) => {
      total += c.paragraphCount
      if (c.idx < playback.chapterIdx) {
        completed += c.paragraphCount
      } else if (c.idx === playback.chapterIdx) {
        completed += Math.min(playback.paragraphIdx, c.paragraphCount)
        if (i === bookChapters.length - 1 && playback.paragraphIdx >= c.paragraphCount - 1) {
          finished = true
        }
      }
    })
    return {
      liveProgressPercent: total > 0 ? Math.min(100, (completed / total) * 100) : 0,
      liveFinished: finished,
    }
  }, [bookQuery.data?.chapters, playback.chapterIdx, playback.paragraphIdx])

  const topSentinelRef = useRef<HTMLDivElement>(null)
  const bottomSentinelRef = useRef<HTMLDivElement>(null)
  useInView(topSentinelRef, expandUp, '800px', [range?.start])
  useInView(bottomSentinelRef, expandDown, '800px', [range?.end])

  const paragraphRefs = useRef(new Map<string, HTMLElement>())
  const registerParagraphRef = useCallback((key: string, el: HTMLElement | null) => {
    if (el) paragraphRefs.current.set(key, el)
    else paragraphRefs.current.delete(key)
  }, [])

  // Which loaded chapter is actually scrolled into view - distinct from
  // playback.chapterIdx (which chapter is currently *playing*, possibly
  // off-screen if the reader scrolled ahead/back manually - see
  // autoFollow). Backs PlayerBar's "this chapter only" speaker-list scope
  // (scopeChapter below), which should reflect what's on screen, not
  // what's playing. Tracks intersection ratios across every mounted
  // chapter-block (chapterSectionRefs) and keeps whichever one is
  // currently most visible - entries only report elements whose
  // intersection *changed* on a given callback, so ratios are accumulated
  // in a ref across calls rather than recomputed from just that batch.
  const chapterSectionRefs = useRef(new Map<number, HTMLElement>())
  const registerChapterSectionRef = useCallback((idx: number, el: HTMLElement | null) => {
    if (el) chapterSectionRefs.current.set(idx, el)
    else chapterSectionRefs.current.delete(idx)
  }, [])
  const chapterVisibilityRef = useRef(new Map<number, number>())
  const [visibleChapterIdx, setVisibleChapterIdx] = useState<number | null>(null)
  useEffect(() => {
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          const idx = Number((entry.target as HTMLElement).dataset.chapterIdx)
          chapterVisibilityRef.current.set(idx, entry.isIntersecting ? entry.intersectionRatio : 0)
        }
        let best: number | null = null
        let bestRatio = 0
        for (const [idx, ratio] of chapterVisibilityRef.current) {
          if (ratio > bestRatio) {
            bestRatio = ratio
            best = idx
          }
        }
        if (best !== null) setVisibleChapterIdx(best)
      },
      { threshold: [0, 0.1, 0.25, 0.5, 0.75, 1] },
    )
    for (const el of chapterSectionRefs.current.values()) observer.observe(el)
    return () => observer.disconnect()
  }, [range?.start, range?.end])

  // Windowing: which loaded chapters actually get their full DOM mounted
  // (heading + entire paragraph list) versus collapsed to a single sized
  // spacer - see ChapterSection's renderFull/placeholderHeight and the
  // render loop below. `range` only ever grows (a chapter, once loaded,
  // stays in the map for the rest of the session - see expandUp/
  // expandDown), so without this a long reading session accumulates every
  // chapter scrolled through, all still fully mounted, and every one of
  // them pays layout/paint cost on every render regardless of whether
  // ChapterSection's own memoization skips reconciling its React tree.
  // Caches each chapter's own last-measured height (via the ResizeObserver
  // below) so collapsing it to a placeholder doesn't change the scroll
  // list's total height - the reader's scroll position stays put even
  // though most of what they've already read is no longer real DOM.
  const [chapterHeights, setChapterHeights] = useState<Map<number, number>>(() => new Map())
  const registerHeight = useCallback((idx: number, height: number) => {
    setChapterHeights((prev) => {
      const existing = prev.get(idx)
      if (existing !== undefined && Math.abs(existing - height) < 1) return prev
      const next = new Map(prev)
      next.set(idx, height)
      return next
    })
  }, [])
  useEffect(() => {
    const observer = new ResizeObserver((entries) => {
      for (const entry of entries) {
        const el = entry.target as HTMLElement
        // Skip a chapter that hasn't loaded its real data yet (still
        // showing "Loading chapter…") - see ChapterSection's data-loaded -
        // caching that skeleton's own tiny height would wrongly become
        // this chapter's placeholder size forever if it falls out of the
        // render window before its real content ever gets a chance to
        // mount and be measured.
        if (el.dataset.loaded !== '1') continue
        const idx = Number(el.dataset.chapterIdx)
        const height = entry.borderBoxSize?.[0]?.blockSize ?? entry.contentRect.height
        if (Number.isFinite(idx) && height > 0) registerHeight(idx, height)
      }
    })
    for (const el of chapterSectionRefs.current.values()) observer.observe(el)
    return () => observer.disconnect()
  }, [range?.start, range?.end, registerHeight])

  // Keeps the currently-playing paragraph centered as playback advances
  // (and does the initial resume-position scroll the same way, since that's
  // just "the active paragraph" at mount). Suspends itself the moment the
  // reader scrolls *by their own input*, so following doesn't fight someone
  // reading ahead or back; the "Jump to current" button in the player bar
  // both recenters immediately and turns following back on.
  //
  // Detecting "explicit" scrolling means listening for the input gestures
  // that cause it (wheel, touch drag, scroll-relevant keys) rather than the
  // ambient 'scroll' event - the page's height keeps changing as chapters
  // and images load in, and those layout shifts alone can move the
  // scroll position (and fire 'scroll') with no user input at all.
  const [autoFollow, setAutoFollow] = useState(true)
  const hasScrolledOnceRef = useRef(false)

  const scrollToActiveParagraph = useCallback(
    (behavior: ScrollBehavior) => {
      const key = `${playback.chapterIdx}:${playback.paragraphIdx}`
      const el = paragraphRefs.current.get(key)
      if (!el) return
      el.scrollIntoView({ block: 'center', behavior })
      hasScrolledOnceRef.current = true
    },
    [playback.chapterIdx, playback.paragraphIdx],
  )

  useEffect(() => {
    if (!autoFollow) return
    scrollToActiveParagraph(hasScrolledOnceRef.current ? 'smooth' : 'auto')
    // Re-runs when `chapters` changes too, so it retries once the resumed
    // chapter's paragraphs actually render (first tick after load, the ref
    // won't exist yet).
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [playback.chapterIdx, playback.paragraphIdx, chapters, autoFollow])

  useEffect(() => {
    const SCROLL_KEYS = new Set([
      'ArrowUp',
      'ArrowDown',
      'PageUp',
      'PageDown',
      'Home',
      'End',
      ' ',
      'Spacebar',
    ])
    const breakFollow = () => setAutoFollow(false)
    const onKeyDown = (e: KeyboardEvent) => {
      if (SCROLL_KEYS.has(e.key)) breakFollow()
    }
    window.addEventListener('wheel', breakFollow, { passive: true })
    window.addEventListener('touchmove', breakFollow, { passive: true })
    window.addEventListener('keydown', onKeyDown)
    return () => {
      window.removeEventListener('wheel', breakFollow)
      window.removeEventListener('touchmove', breakFollow)
      window.removeEventListener('keydown', onKeyDown)
    }
  }, [])

  // Moves playback to the adjacent paragraph, crossing into the
  // next/previous chapter when the current one is exhausted - but only if
  // that neighboring chapter happens to already be loaded (same
  // conservative check usePlayback's own end-of-chapter handling uses);
  // otherwise this just does nothing rather than fetching on the fly.
  const skipParagraph = useCallback(
    (direction: 1 | -1) => {
      const chapter = chapters.get(playback.chapterIdx)
      if (!chapter) return
      const nextParagraphIdx = playback.paragraphIdx + direction
      if (chapter.paragraphs[nextParagraphIdx]) {
        playback.playAt(playback.chapterIdx, nextParagraphIdx)
        return
      }
      const neighborChapterIdx = playback.chapterIdx + direction
      const neighborChapter = chapters.get(neighborChapterIdx)
      if (!neighborChapter || neighborChapter.paragraphs.length === 0) return
      playback.playAt(neighborChapterIdx, direction === 1 ? 0 : neighborChapter.paragraphs.length - 1)
    },
    [chapters, playback],
  )

  // OS-level play/pause/skip (lock screen, headset/hardware media keys,
  // the browser tab's own media indicator) - see useMediaSession. A
  // chapter is this book's own "track" granularity (skip = skipParagraph's
  // same paragraph-level move, but metadata is per-chapter, matching
  // android's own MediaSessionService). Falls back to the book's own
  // chapter-summary title before the full chapter has loaded (e.g. right
  // after a jump), same fallback chain the reader-header title elsewhere
  // in this component already leans on.
  useMediaSession({
    title: chapters.get(playback.chapterIdx)?.title ?? bookQuery.data?.chapters[playback.chapterIdx]?.title ?? '',
    artist: bookQuery.data?.author ?? '',
    album: bookQuery.data?.title ?? '',
    artworkUrl: bookQuery.data?.coverUrl,
    isPlaying: playback.isPlaying,
    onPlay: playback.play,
    onPause: playback.pause,
    onNextTrack: () => skipParagraph(1),
    onPreviousTrack: () => skipParagraph(-1),
    currentTime: playback.currentTime,
    duration: playback.duration,
    playbackRate: playback.playbackRate,
  })

  // Jumps to the next segment attributed to selectedSpeaker, searching
  // forward from just after the current playback position - same
  // "currently-loaded chapters only" scope skipParagraph above already
  // uses (chapters is only ever the reader's own loaded window, see
  // useChapterRange), rather than fetching further chapters on the fly or
  // going through the appearances endpoint (which spans the whole series,
  // not just this book's own reading position). A no-op if nothing
  // matches within what's already loaded.
  // Takes the target name directly rather than reading selectedSpeaker
  // from closure - the "jump to next appearance" button shows on every
  // speaker-list row now, not just the selected one, so a click can jump
  // to someone who isn't (yet) selected; selecting them here too keeps
  // the reader's own highlight in sync with whoever was just jumped to.
  const jumpToNextAppearance = useCallback(
    (speakerName: string) => {
      if (range === null) return
      setSelectedSpeaker(speakerName)
      for (let chIdx = playback.chapterIdx; chIdx <= range.end; chIdx++) {
        const chapter = chapters.get(chIdx)
        if (!chapter) break
        const startIdx = chIdx === playback.chapterIdx ? playback.paragraphIdx + 1 : 0
        const match = chapter.paragraphs.find((p) => p.idx >= startIdx && matchesSelectedSpeaker(p, speakerName))
        if (match) {
          playback.playAt(chIdx, match.idx, { autoplay: false })
          setAutoFollow(true)
          return
        }
      }
      // Nothing further ahead (within what's loaded) - wrap around to
      // this speaker's very first appearance instead of dead-ending, same
      // "cycle through" convention a next-track button usually follows.
      // Scans from range.start up through (and including) the current
      // chapter, stopping short of the current paragraph itself in that
      // last chapter so this doesn't just re-select whatever's already
      // playing.
      for (let chIdx = range.start; chIdx <= playback.chapterIdx; chIdx++) {
        const chapter = chapters.get(chIdx)
        if (!chapter) break
        const endIdx = chIdx === playback.chapterIdx ? playback.paragraphIdx : Infinity
        const match = chapter.paragraphs.find((p) => p.idx < endIdx && matchesSelectedSpeaker(p, speakerName))
        if (match) {
          playback.playAt(chIdx, match.idx, { autoplay: false })
          setAutoFollow(true)
          return
        }
      }
    },
    [range, chapters, playback],
  )

  // Custom hover tooltip for annotation segments, replacing the native
  // title="..." attribute (a browser default alt-text-style tooltip) with
  // a styled floating one (AnnotationTooltip, rendered near the end of
  // this component). Anchored to the specific word under the cursor
  // (ParagraphText wraps each word in its own .word span - see that
  // component), not the cursor position itself or the whole segment's
  // rect - e.target/closest walks up from wherever the mousemove/
  // mouseenter actually landed to find it, falling back to the whole
  // segment (e.currentTarget) when the pointer's over inter-word space
  // rather than a word itself. Wired to onMouseMove as well as
  // onMouseEnter (below) since moving from one word to the next within
  // the same segment doesn't re-fire mouseenter on the outer span.
  //
  // Driven through an imperative ref (AnnotationTooltipHandle) rather than
  // state owned here - mousemove over annotated text fires far more often
  // than any other reader interaction, and ReaderPage's own render covers
  // every loaded chapter's full paragraph list, so routing this through
  // ReaderPage state would re-render that entire list on every pixel of
  // mouse movement. Only the tiny AnnotationTooltip component re-renders.
  const tooltipRef = useRef<AnnotationTooltipHandle>(null)
  const showAnnotationTooltip = useCallback((e: React.MouseEvent<HTMLElement>, text: string) => {
    const wordEl = (e.target as HTMLElement).closest('.word') as HTMLElement | null
    const rect = (wordEl ?? e.currentTarget).getBoundingClientRect()
    tooltipRef.current?.show(text, rect)
  }, [])
  const hideAnnotationTooltip = useCallback(() => tooltipRef.current?.hide(), [])

  // Play/pause/skip/bookmark keyboard shortcuts - ignored while a form
  // field has focus (typing " "/arrow keys/"b" in the search box, a
  // voice-editor textarea, etc. must behave normally, not hijack playback)
  // or while a modifier is held (so e.g. Cmd+ArrowLeft browser-back still
  // works).
  useEffect(() => {
    const isTypingTarget = (el: EventTarget | null) => {
      if (!(el instanceof HTMLElement)) return false
      return el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT' || el.isContentEditable
    }
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.metaKey || e.ctrlKey || e.altKey || isTypingTarget(e.target)) return
      if (e.key === ' ' || e.key === 'Spacebar') {
        e.preventDefault()
        if (playback.isPlaying) playback.pause()
        else playback.play()
      } else if (e.key === 'ArrowRight') {
        e.preventDefault()
        skipParagraph(1)
      } else if (e.key === 'ArrowLeft') {
        e.preventDefault()
        skipParagraph(-1)
      } else if (e.key === 'b' || e.key === 'B') {
        e.preventDefault()
        toggleBookmark(playback.chapterIdx, playback.paragraphIdx)
      } else if ((e.key === 'n' || e.key === 'N') && selectedSpeaker) {
        e.preventDefault()
        jumpToNextAppearance(selectedSpeaker)
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [playback, skipParagraph, toggleBookmark, selectedSpeaker, jumpToNextAppearance])

  const jumpToCurrentParagraph = useCallback(() => {
    setAutoFollow(true)
    scrollToActiveParagraph('smooth')
  }, [scrollToActiveParagraph])

  const jumpToChapter = (idx: number) => {
    setRange({ start: idx, end: idx })
    playback.playAt(idx, 0)
  }

  const jumpToParagraph = (chapterIdx: number, paragraphIdx: number) => {
    setRange({ start: chapterIdx, end: chapterIdx })
    playback.playAt(chapterIdx, paragraphIdx)
    setAutoFollow(true)
  }

  if (bookQuery.isLoading || range === null) return <p>Loading…</p>
  if (bookQuery.isError || !bookQuery.data) return <p className="error-text">Could not load this book.</p>

  const book = bookQuery.data
  const currentChapter = chapters.get(playback.chapterIdx)
  // Whichever chapter is actually scrolled into view (visibleChapterIdx),
  // falling back to the playing chapter before the observer's first
  // measurement lands - both what "this chapter only" scopes to in
  // PlayerBar's speaker list (scopeChapter) and the center of the render
  // window below (RENDER_WINDOW_RADIUS chapters to each side stay fully
  // mounted; everything else collapses to a sized placeholder once it's
  // been measured once).
  const focusChapterIdx = visibleChapterIdx ?? playback.chapterIdx
  const scopeChapter = chapters.get(focusChapterIdx) ?? currentChapter

  return (
    <div className="reader">
      <div className="reader-header">
        <div className="reader-header-row">
          <div>
            <h1>{book.title}</h1>
            <p className="muted">
              {book.author || 'Unknown author'}
              {book.seriesName && ` · ${book.seriesName}${book.seriesIndex ? ` #${book.seriesIndex}` : ''}`}
            </p>
          </div>
        </div>
        {chapterActionError && <p className="error-text">{chapterActionError}</p>}
      </div>

      <div
        className="scroll-reader"
        style={
          {
            '--reader-font-size': `${typography.fontSize}px`,
            '--reader-font-family': READER_FONT_FAMILIES.find((f) => f.value === typography.fontFamily)?.stack,
          } as React.CSSProperties
        }
      >
        {range.start > 0 && <div ref={topSentinelRef} className="scroll-sentinel" />}

        {Array.from({ length: range.end - range.start + 1 }, (_, i) => range.start + i).map((idx) => {
          const chapter = chapters.get(idx)
          const chapterSummary = book.chapters[idx]
          const chapterTitle = chapter?.title ?? chapterSummary?.title ?? ''
          const isActiveChapter = idx === playback.chapterIdx
          const measuredHeight = chapterHeights.get(idx)
          const renderFull = Math.abs(idx - focusChapterIdx) <= RENDER_WINDOW_RADIUS || measuredHeight === undefined
          return (
            <ChapterSection
              key={idx}
              idx={idx}
              chapter={chapter}
              chapterTitle={chapterTitle}
              renderFull={renderFull}
              placeholderHeight={measuredHeight}
              chapterAttributing={attributingIdxs.has(idx)}
              chapterClearing={deleteChapterAudio.isPending && deleteChapterAudio.variables === idx}
              hasAttribution={!!chapterSummary?.passes.attribution}
              chapterRetaggingDescriptions={retaggingDescriptionsIdx === idx}
              chapterRetaggingScareQuotes={retaggingScareQuotesIdx === idx}
              chapterDirecting={directingIdxs.has(idx)}
              chapterScoringMusic={scoringMusicIdxs.has(idx)}
              chapterGenerating={generatingIdxs.has(idx)}
              hasDirection={!!chapterSummary?.passes.direction}
              hasMusic={!!chapterSummary?.passes.music}
              isGenerated={!!chapterSummary && chapterSummary.readyCount >= chapterSummary.paragraphCount}
              directionSupported={directionSupported}
              annotationsView={annotationsView}
              selectedSpeaker={selectedSpeaker}
              isActiveChapter={isActiveChapter}
              activeParagraphIdx={isActiveChapter ? playback.paragraphIdx : null}
              audioElement={isActiveChapter ? playback.audioElement : inertAudioRef.current!}
              bookmarkByKey={bookmarkByKey}
              onClearAudio={runClearChapterAudio}
              onAttribute={runAttributeChapter}
              onRetagDescriptions={runRetagDescriptionsChapter}
              onRetagScareQuotes={runRetagScareQuotesChapter}
              onTagDirections={runTagDirectionsChapter}
              onScoreMusic={runScoreMusicChapter}
              onGenerate={runGenerateChapter}
              musicRegions={chapterMusicRegions.get(idx) ?? EMPTY_MUSIC_REGIONS}
              onRegenerateMusic={runRegenerateMusic}
              registerSectionRef={registerChapterSectionRef}
              onSeek={seek}
              onPlayAt={playAt}
              onToggleBookmark={toggleBookmark}
              onRegenerate={regenerateParagraphAt}
              onOpenSpeakerMenu={openSpeakerMenu}
              onShowTooltip={showAnnotationTooltip}
              onHideTooltip={hideAnnotationTooltip}
              registerParagraphRef={registerParagraphRef}
              onSetSFXPrompt={setSFXPromptAt}
              onGenerateSFX={generateSFXAt}
            />
          )
        })}

        {range.end < totalChapters - 1 && <div ref={bottomSentinelRef} className="scroll-sentinel" />}
      </div>

      <PlayerBar
        bookId={bookId}
        chapter={currentChapter}
        bookChapters={book.chapters}
        playback={playback}
        autoFollow={autoFollow}
        onJumpToCurrent={jumpToCurrentParagraph}
        onJumpToChapter={jumpToChapter}
        onJumpToParagraph={jumpToParagraph}
        progressPercent={liveProgressPercent}
        estimatedTotalSeconds={book.estimatedTotalSeconds}
        finished={liveFinished}
        annotationsView={annotationsView}
        onToggleAnnotationsView={() =>
          setAnnotationsView((v) => {
            if (v) setSelectedSpeaker(null)
            return !v
          })
        }
        speakers={speakersQuery.data ?? []}
        selectedSpeaker={selectedSpeaker}
        onSelectSpeaker={setSelectedSpeaker}
        scopeChapter={scopeChapter}
        onJumpToNextAppearance={jumpToNextAppearance}
        readerFontSize={typography.fontSize}
        readerFontFamily={typography.fontFamily}
        onReaderFontSizeChange={typography.setFontSize}
        onReaderFontFamilyChange={typography.setFontFamily}
      />

      {contextMenu && (
        <div
          ref={contextMenuRef}
          className="speaker-context-menu"
          style={menuPos ? { top: menuPos.top, left: menuPos.left } : { top: contextMenu.y, left: contextMenu.x }}
        >
          {contextMenu.isQuote ? (
            <>
              <div className="speaker-context-menu-title">Reassign speaker</div>
              <div className="speaker-context-menu-current">
                Currently: <strong>{contextMenu.currentSpeaker || 'Narrator'}</strong>
              </div>
              <button
                className="speaker-context-menu-item"
                disabled={contextMenu.currentSpeaker === ''}
                onClick={() => reassignSpeaker('')}
              >
                Narrator
              </button>
              <div className="speaker-context-menu-section">Scare quote</div>
              <button className="speaker-context-menu-item" onClick={toggleScareQuote}>
                {contextMenu.scareQuote ? 'Unmark as scare quote' : 'Mark as scare quote'}
              </button>
              {nearbyReassignTargets.length > 0 && <div className="speaker-context-menu-section">Nearby</div>}
              {nearbyReassignTargets.map((name) => (
                <button
                  key={name}
                  className="speaker-context-menu-item"
                  disabled={name === contextMenu.currentSpeaker}
                  onClick={() => reassignSpeaker(name)}
                >
                  {name}
                </button>
              ))}
              {nearbyReassignTargets.length > 0 && <div className="speaker-context-menu-section">All characters</div>}
              {restReassignTargets.map((name) => (
                <button
                  key={name}
                  className="speaker-context-menu-item"
                  disabled={name === contextMenu.currentSpeaker}
                  onClick={() => reassignSpeaker(name)}
                >
                  {name}
                </button>
              ))}
            </>
          ) : contextMenu.describesCharacters.length > 0 ? (
            <>
              <div className="speaker-context-menu-title">Reassign description</div>
              {contextMenu.describesCharacters.map((from) => (
                <div key={from} className="speaker-context-menu-group">
                  <div className="speaker-context-menu-current">
                    Describes: <strong>{from}</strong>
                  </div>
                  <button className="speaker-context-menu-item" onClick={() => reassignDescription(from, '')}>
                    Remove
                  </button>
                  {reassignTargets
                    .filter((name) => name !== from)
                    .map((name) => (
                      <button key={name} className="speaker-context-menu-item" onClick={() => reassignDescription(from, name)}>
                        {name}
                      </button>
                    ))}
                </div>
              ))}
            </>
          ) : (
            <>
              <div className="speaker-context-menu-title">Reassign speaker</div>
              <div className="speaker-context-menu-current">
                Currently: <strong>Narrator</strong>
              </div>
              <div className="speaker-context-menu-empty">Only dialogue can be reassigned to a character</div>
            </>
          )}
        </div>
      )}

      <AnnotationTooltip ref={tooltipRef} />
    </div>
  )
}
