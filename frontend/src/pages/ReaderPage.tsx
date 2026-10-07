import { useCallback, useDeferredValue, useEffect, useMemo, useRef, useState } from 'react'
import { useParams } from '@tanstack/react-router'
import {
  useBook,
  useBookmarks,
  useChapterMusicRange,
  useChapterNotesRange,
  useChapterRange,
  useCreateBookmark,
  useDeleteBookmark,
  useGenerateParagraphSFX,
  useLookahead,
  useRegenerateParagraph,
  useSetParagraphSFXPrompt,
  useSpeakers,
  useUpdatePosition,
} from '../api/queries'
import { usePlayback } from '../hooks/usePlayback'
import { useBackgroundMusic } from '../hooks/useBackgroundMusic'
import { useLighting } from '../hooks/useLighting'
import { useLightingState } from '../lighting/controller'
import { useInView } from '../hooks/useInView'
import { useMediaSession } from '../hooks/useMediaSession'
import { READER_FONT_FAMILIES, useReaderTypography } from '../hooks/useReaderTypography'
import { useStableCallback } from '../hooks/useStableCallback'
import { useChapterActions } from '../hooks/useChapterActions'
import { useChapterWindowing } from '../hooks/useChapterWindowing'
import { useAutoFollow } from '../hooks/useAutoFollow'
import { useSFXOverlay } from '../hooks/useSFXOverlay'
import { remotePosition, useRemotePosition } from '../hooks/useRemotePosition'
import { useReaderShortcuts } from '../hooks/useReaderShortcuts'
import { ParagraphContextMenu } from '../components/ParagraphContextMenu'
import type { ParagraphContextMenuHandle } from '../components/ParagraphContextMenu'
import { RecapPanel, type RecapTransport } from '../components/RecapPanel'
import { PlayerBar } from '../components/PlayerBar'
import { ChapterSection } from '../components/ChapterSection'
import { AnnotationTooltip } from '../components/AnnotationTooltip'
import type { AnnotationTooltipHandle } from '../components/AnnotationTooltip'
import type { ChapterDetail, ChapterNotes, MusicRegion, Paragraph } from '../api/types'
import { matchesSelectedSpeaker } from '../utils/annotations'

// How many loaded chapters on each side of whichever one is actually on
// screen stay fully mounted - see useChapterWindowing's chapterHeights
// (per-chapter measured heights). Generous enough that ordinary scrolling (including a
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

const ANNOTATIONS_VIEW_KEY = 'lectable.annotationsView'

// Storage can be unavailable (private window, blocked site data) - the
// toggle then just starts off each visit, as it always used to.
function loadAnnotationsView(): boolean {
  try {
    return window.localStorage.getItem(ANNOTATIONS_VIEW_KEY) === '1'
  } catch {
    return false
  }
}

function saveAnnotationsView(on: boolean) {
  try {
    window.localStorage.setItem(ANNOTATIONS_VIEW_KEY, on ? '1' : '0')
  } catch {
    // Not remembered this time - nothing else depends on it.
  }
}

export function ReaderPage() {
  const { bookId } = useParams({ from: '/books/$bookId' })
  const bookQuery = useBook(bookId)
  const updatePosition = useUpdatePosition(bookId)
  const lookahead = useLookahead(bookId)
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
  // completion (pushed on the chapter topic).
  const setSFXPrompt = useSetParagraphSFXPrompt(bookId)
  const generateSFX = useGenerateParagraphSFX(bookId)
  const setSFXPromptAt = useStableCallback(
    (chapterIdx: number, paragraphIdx: number, prompt: string, triggerWord?: number) => {
      setSFXPrompt.mutate({ chapterIdx, paragraphIdx, prompt, triggerWord })
    },
  )
  const generateSFXAt = useStableCallback(
    async (chapterIdx: number, paragraphIdx: number, prompt: string) => {
      await generateSFX.mutateAsync({ chapterIdx, paragraphIdx, prompt })
    },
  )

  const {
    error: chapterActionError,
    setError: setChapterActionError,
    attributingIdxs,
    scareQuotingIdxs,
    directingIdxs,
    pronouncingIdxs,
    ambienceIdxs,
    generatingIdxs,
    reimportingIdx,
    clearingIdx,
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
  } = useChapterActions(bookId)

  // Colors narrator/speaker segments in the paragraph list
  // instead of leaving that distinction to the speaker-hint badge alone
  // (see annotationKind above) - off by default since the coloring adds
  // visual noise a reader may not want while just listening.
  // Remembered per browser across reader visits/reloads (see
  // loadAnnotationsView) - android keeps the same toggle in its reading
  // settings.
  const [annotationsView, setAnnotationsView] = useState(loadAnnotationsView)
  // Clicking a name in PlayerBar's speaker list selects it here, which
  // adds an extra highlight (on top of the normal narrator/speaker/
  // description coloring) to every segment attributed to them - lets a
  // reader visually scan for one character's lines specifically instead
  // of just "this is dialogue" in general. Cleared whenever annotations
  // view itself is turned off, since the selection has no visible effect
  // (and no way to change it) while the toggle's off anyway.
  const [selectedSpeaker, setSelectedSpeaker] = useState<string | null>(null)

  // Right-click "reassign speaker" menu (annotations
  // view only) - see ParagraphContextMenu. speakersQuery also backs
  // PlayerBar's speaker list.
  const speakersQuery = useSpeakers(bookId)
  const contextMenuRef = useRef<ParagraphContextMenuHandle>(null)

  const openSpeakerMenu = useCallback(
    (e: React.MouseEvent, chapterIdx: number, p: Paragraph) => {
      if (!annotationsView) return
      e.preventDefault()
      e.stopPropagation()
      contextMenuRef.current?.open(chapterIdx, p, e.clientX, e.clientY)
    },
    [annotationsView],
  )

  // The window of chapters currently mounted in the scroll list. Grows
  // downward/upward as the reader scrolls near either edge, instead of
  // requiring a chapter to be explicitly selected.
  const [range, setRange] = useState<{ start: number; end: number } | null>(null)

  // The "previously on" recap - see RecapPanel. Open, it sits in the text
  // above the paragraph the reader was at when it opened (recapAnchor),
  // rendered into the slot that paragraph's ChapterSection provides.
  const [recapOpen, setRecapOpenState] = useState(false)
  const [recapAnchor, setRecapAnchor] = useState<{ chapterIdx: number; paragraphIdx: number }>()
  const [recapSlot, setRecapSlot] = useState<HTMLElement | null>(null)
  // Set while the recap is open with a clip: the player bar's play/pause,
  // the keyboard shortcut and the sleep timer then act on it, not the book.
  const [recapTransport, setRecapTransport] = useState<RecapTransport | null>(null)

  const remotePositionState = useRemotePosition()
  const { noteOwnSave } = remotePositionState

  useEffect(() => {
    if (bookQuery.data && range === null) {
      setRange({ start: bookQuery.data.posChapterIdx, end: bookQuery.data.posChapterIdx })
      // Where this reader resumed from is its own, not another device's.
      noteOwnSave({
        chapterIdx: bookQuery.data.posChapterIdx,
        paragraphIdx: bookQuery.data.posParagraphIdx,
      })
    }
  }, [bookQuery.data, range, noteOwnSave])

  const totalChapters = bookQuery.data?.chapters.length ?? 0
  const chapterResults = useChapterRange(bookId, range?.start ?? 0, range?.end ?? -1)

  const chapters = useMemo(() => {
    const map = new Map<number, ChapterDetail>()
    for (const r of chapterResults) {
      if (r.data) map.set(r.data.idx, r.data)
    }
    return map
  }, [chapterResults])
  // What the scroll list itself renders from. Live topic updates arrive
  // through useSyncExternalStore, which always renders urgently - so a
  // newly-loaded chapter (a couple hundred paragraphs mounting at once)
  // blocked the main thread mid-scroll. Deferred, React mounts it in an
  // interruptible background render instead. Playback, lookahead, and
  // everything else logic-side keep reading the live `chapters`.
  const renderedChapters = useDeferredValue(chapters)

  // Background-music regions for every currently-loaded chapter - backs
  // annotations view's region boundary markers (ChapterSection.
  // MusicRegionBoundary). Only actually subscribed while annotations view
  // is on (see useChapterMusicRange's own `enabled` doc comment) -
  // otherwise every loaded chapter would pay for a music-regions topic a
  // reader who never opens annotations view will never see. ChapterMusic itself
  // carries no chapter idx of its own (unlike ChapterDetail), so this maps
  // by the range's own position rather than reading one back off the data.
  const musicResults = useChapterMusicRange(
    bookId,
    range?.start ?? 0,
    range?.end ?? -1,
    annotationsView,
  )
  const chapterMusicRegions = useMemo(() => {
    const map = new Map<number, MusicRegion[]>()
    const start = range?.start ?? 0
    musicResults.forEach((r, i) => {
      if (r.data) map.set(start + i, r.data.regions)
    })
    return map
  }, [musicResults, range])

  // Summary notes (summary, cast, scenes) for every loaded chapter -
  // annotations view's chapter summary block and scene markers
  // (ChapterSection), subscribed only while annotations view is on, like
  // the music regions above.
  const notesResults = useChapterNotesRange(
    bookId,
    range?.start ?? 0,
    range?.end ?? -1,
    annotationsView,
  )
  const chapterNotesByIdx = useMemo(() => {
    const map = new Map<number, ChapterNotes>()
    const start = range?.start ?? 0
    notesResults.forEach((r, i) => {
      if (r.data?.notes) map.set(start + i, r.data.notes)
    })
    return map
  }, [notesResults, range])

  const expandDown = useCallback(() => {
    setRange((r) => (r ? { ...r, end: Math.min(r.end + 1, totalChapters - 1) } : r))
  }, [totalChapters])
  const expandUp = useCallback(() => {
    setRange((r) => (r ? { ...r, start: Math.max(r.start - 1, 0) } : r))
  }, [])

  const handlePosition = useCallback(
    (chIdx: number, paraIdx: number, seconds: number) => {
      noteOwnSave({ chapterIdx: chIdx, paragraphIdx: paraIdx })
      updatePosition.mutate({ chapterIdx: chIdx, paragraphIdx: paraIdx, seconds })
    },
    [updatePosition, noteOwnSave],
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
  // What the player bar and shortcuts drive - see recapTransport.
  const barPlayback = useMemo(
    () =>
      recapTransport ? { ...playback, ...recapTransport, isWaitingForAudio: false } : playback,
    [playback, recapTransport],
  )

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

  // Room lighting from the same regions' lighting cues - see useLighting.
  useLighting({
    bookId,
    chapterIdx: playback.chapterIdx,
    paragraphIdx: playback.paragraphIdx,
    isPlaying: playback.isPlaying,
  })
  const lightingEnabled = useLightingState().settings.enabled

  // Lazily scores whichever chapter the reader is currently on, the first
  // time it's needed - background music is a book-wide toggle
  // (BookSummary.musicEnabled) but scoring itself is real per-chapter LLM
  // work (store.Passes.Ambience/Lighting), so turning the toggle on doesn't eagerly
  // score every chapter in the book at once; instead each chapter gets
  // scored the first time playback actually reaches it, mirroring how
  // narration itself only generates lookahead-bounded runway rather than
  // the whole book up front. Re-fires as playback advances into a new
  // chapter; api.runChapterAmbience dedups server-side (one in-flight
  // scoring task per chapter - see jobs.Manager's dedup), so calling this
  // opportunistically on every chapter change is safe. Skipped entirely
  // once the ambience pass is done (passes.ambience and passes.lighting)
  // or already in flight
  // (ambienceIdxs, from the shared job queue).
  useEffect(() => {
    // Lighting needs the ambience pass too, even with music off.
    if (!bookQuery.data?.musicEnabled && !lightingEnabled) return
    if (!bookQuery.data) return
    const chapterSummary = bookQuery.data.chapters[playback.chapterIdx]
    if (!chapterSummary || (chapterSummary.passes.ambience && chapterSummary.passes.lighting))
      return
    if (ambienceIdxs.has(playback.chapterIdx)) return
    runChapterAmbience.mutate(playback.chapterIdx)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [
    bookQuery.data?.musicEnabled,
    lightingEnabled,
    bookQuery.data?.chapters,
    playback.chapterIdx,
    ambienceIdxs,
  ])

  // Stable-identity wrappers around playback's own playAt/seek - playback
  // itself is a new object every time usePlayback's internal state ticks
  // (e.g. every 'timeupdate'), which would otherwise defeat ChapterSection/
  // ParagraphGroup's memoization on every such tick even though the actual
  // behavior these close over hasn't changed.
  const playAt = useStableCallback((chapterIdx: number, paragraphIdx: number) =>
    playback.playAt(chapterIdx, paragraphIdx),
  )
  const seek = useStableCallback((seconds: number) => playback.seek(seconds))

  useSFXOverlay({
    chapters,
    chapterIdx: playback.chapterIdx,
    paragraphIdx: playback.paragraphIdx,
    currentTime: playback.currentTime,
    isPlaying: playback.isPlaying,
  })

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
  // listens rather than only when the book topic's saved position changes.
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
  // Each sentinel only exists once the chapter at its edge of the range
  // has actually arrived (or failed). Until then that chapter is just a
  // short "Loading chapter…" block, so the sentinel beyond it stays inside
  // its 800px margin and would expand again, and again - on a book resumed
  // mid-way (or after a jump resets the range) that chained through a
  // couple dozen chapters above the resume point, all fetched and fully
  // mounted at once, before real content finally pushed the sentinel away.
  const edgeSettled = (idx: number) =>
    renderedChapters.has(idx) || !!(range && chapterResults[idx - range.start]?.isError)
  const startSettled = range !== null && edgeSettled(range.start)
  const endSettled = range !== null && edgeSettled(range.end)
  useInView(topSentinelRef, expandUp, '800px', [range?.start, startSettled])
  useInView(bottomSentinelRef, expandDown, '800px', [range?.end, endSettled])

  const { registerChapterSectionRef, visibleChapterIdx, chapterHeights } = useChapterWindowing(
    range?.start,
    range?.end,
  )
  const { autoFollow, setAutoFollow, registerParagraphRef, jumpToCurrentParagraph } = useAutoFollow(
    {
      chapterIdx: playback.chapterIdx,
      paragraphIdx: playback.paragraphIdx,
      renderedChapters,
      visibleChapterIdx,
    },
  )

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
      playback.playAt(
        neighborChapterIdx,
        direction === 1 ? 0 : neighborChapter.paragraphs.length - 1,
      )
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
    title:
      chapters.get(playback.chapterIdx)?.title ??
      bookQuery.data?.chapters[playback.chapterIdx]?.title ??
      '',
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
        const match = chapter.paragraphs.find(
          (p) => p.idx >= startIdx && matchesSelectedSpeaker(p, speakerName),
        )
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
        const match = chapter.paragraphs.find(
          (p) => p.idx < endIdx && matchesSelectedSpeaker(p, speakerName),
        )
        if (match) {
          playback.playAt(chIdx, match.idx, { autoplay: false })
          setAutoFollow(true)
          return
        }
      }
    },
    [range, chapters, playback, setAutoFollow],
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

  useReaderShortcuts({
    isPlaying: barPlayback.isPlaying,
    play: barPlayback.play,
    pause: barPlayback.pause,
    chapterIdx: playback.chapterIdx,
    paragraphIdx: playback.paragraphIdx,
    skipParagraph,
    toggleBookmark,
    selectedSpeaker,
    jumpToNextAppearance,
  })

  const jumpToChapter = (idx: number) => {
    setRange({ start: idx, end: idx })
    playback.playAt(idx, 0)
  }

  const jumpToParagraph = (chapterIdx: number, paragraphIdx: number) => {
    setRange({ start: chapterIdx, end: chapterIdx })
    playback.playAt(chapterIdx, paragraphIdx)
    setAutoFollow(true)
  }

  const setRecapOpen = useCallback(
    (open: boolean) => {
      if (open) {
        setRecapAnchor({ chapterIdx: playback.chapterIdx, paragraphIdx: playback.paragraphIdx })
      }
      setRecapOpenState(open)
    },
    [playback.chapterIdx, playback.paragraphIdx],
  )
  const recapPos = useMemo(
    () => ({ chapterIdx: playback.chapterIdx, paragraphIdx: playback.paragraphIdx }),
    [playback.chapterIdx, playback.paragraphIdx],
  )

  if (bookQuery.isLoading || range === null) return <p>Loading…</p>
  if (bookQuery.isError || !bookQuery.data)
    return <p className="error-text">Could not load this book.</p>

  const book = bookQuery.data
  const currentChapter = chapters.get(playback.chapterIdx)
  // Moved on another device while this reader sat paused - see
  // useRemotePosition.
  const remote = playback.isPlaying
    ? null
    : remotePosition(
        { chapterIdx: book.posChapterIdx, paragraphIdx: book.posParagraphIdx },
        { chapterIdx: playback.chapterIdx, paragraphIdx: playback.paragraphIdx },
        remotePositionState.ownSaves,
        remotePositionState.dismissed,
      )
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
              {book.seriesName &&
                ` · ${book.seriesName}${book.seriesIndex ? ` #${book.seriesIndex}` : ''}`}
            </p>
          </div>
        </div>
        {chapterActionError && <p className="error-text">{chapterActionError}</p>}
        {remote && (
          <p className="remote-position-banner">
            Continued on another device:{' '}
            {book.chapters[remote.chapterIdx]?.title ?? `Chapter ${remote.chapterIdx + 1}`},
            paragraph {remote.paragraphIdx + 1}.{' '}
            <button
              className="text-button"
              onClick={() => {
                setRange({ start: remote.chapterIdx, end: remote.chapterIdx })
                playback.playAt(remote.chapterIdx, remote.paragraphIdx, { autoplay: false })
                setAutoFollow(true)
              }}
            >
              Jump there
            </button>{' '}
            <button className="text-button" onClick={() => remotePositionState.dismiss(remote)}>
              Stay here
            </button>
          </p>
        )}
      </div>

      <div
        className="scroll-reader"
        style={
          {
            '--reader-font-size': `${typography.fontSize}px`,
            '--reader-font-family': READER_FONT_FAMILIES.find(
              (f) => f.value === typography.fontFamily,
            )?.stack,
          } as React.CSSProperties
        }
      >
        {range.start > 0 && startSettled && (
          <div ref={topSentinelRef} className="scroll-sentinel" />
        )}

        {Array.from({ length: range.end - range.start + 1 }, (_, i) => range.start + i).map(
          (idx) => {
            const chapter = renderedChapters.get(idx)
            const chapterSummary = book.chapters[idx]
            const chapterTitle = chapter?.title ?? chapterSummary?.title ?? ''
            const isActiveChapter = idx === playback.chapterIdx
            const measuredHeight = chapterHeights.get(idx)
            // The playing chapter always stays fully mounted too, however
            // far the reader has scrolled away from it - otherwise its
            // paragraph refs (paragraphRefs) unregister once it collapses to
            // a placeholder, and "Jump to current"/autoFollow's
            // scrollToActiveParagraph would have nothing to scroll to.
            const renderFull =
              isActiveChapter ||
              Math.abs(idx - focusChapterIdx) <= RENDER_WINDOW_RADIUS ||
              measuredHeight === undefined
            return (
              <ChapterSection
                key={idx}
                idx={idx}
                chapter={chapter}
                chapterTitle={chapterTitle}
                renderFull={renderFull}
                placeholderHeight={measuredHeight}
                chapterAttributing={attributingIdxs.has(idx)}
                chapterClearing={clearingIdx === idx}
                chapterReimporting={reimportingIdx === idx}
                hasAttribution={!!chapterSummary?.passes.attribution}
                chapterRetaggingScareQuotes={scareQuotingIdxs.has(idx)}
                chapterDirecting={directingIdxs.has(idx)}
                chapterPronouncing={pronouncingIdxs.has(idx)}
                chapterRunningAmbience={ambienceIdxs.has(idx)}
                chapterGenerating={generatingIdxs.has(idx)}
                hasDirection={!!chapterSummary?.passes.direction}
                hasPronunciation={!!chapterSummary?.passes.pronunciation}
                hasMusic={!!chapterSummary?.passes.ambience}
                isGenerated={
                  !!chapterSummary && chapterSummary.readyCount >= chapterSummary.paragraphCount
                }
                annotationsView={annotationsView}
                selectedSpeaker={selectedSpeaker}
                isActiveChapter={isActiveChapter}
                activeParagraphIdx={isActiveChapter ? playback.paragraphIdx : null}
                audioElement={isActiveChapter ? playback.audioElement : inertAudioRef.current!}
                bookmarkByKey={bookmarkByKey}
                onClearAudio={runClearChapterAudio}
                onReimport={runReimportChapter}
                onAttribute={runAttributeChapter}
                onAttributeAroundEdits={runAttributeAroundEdits}
                onSaveNotes={saveNotes}
                onRegenerateNotes={runRegenerateNotes}
                onRetagScareQuotes={runRetagScareQuotesChapter}
                onTagDirections={runTagDirectionsChapter}
                onResolvePronunciation={runResolvePronunciationChapter}
                onRunAmbience={runAmbienceChapter}
                onGenerate={runGenerateChapter}
                musicRegions={chapterMusicRegions.get(idx) ?? EMPTY_MUSIC_REGIONS}
                notes={chapterNotesByIdx.get(idx) ?? null}
                recapSlotIdx={
                  recapOpen && recapAnchor?.chapterIdx === idx ? recapAnchor.paragraphIdx : null
                }
                registerRecapSlot={setRecapSlot}
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
          },
        )}

        {range.end < totalChapters - 1 && endSettled && (
          <div ref={bottomSentinelRef} className="scroll-sentinel" />
        )}
      </div>

      <PlayerBar
        bookId={bookId}
        chapter={currentChapter}
        bookChapters={book.chapters}
        playback={barPlayback}
        statusOverride={recapTransport ? 'Previously on…' : undefined}
        autoFollow={autoFollow}
        onJumpToCurrent={jumpToCurrentParagraph}
        onJumpToChapter={jumpToChapter}
        onJumpToParagraph={jumpToParagraph}
        progressPercent={liveProgressPercent}
        estimatedTotalSeconds={book.estimatedTotalSeconds}
        finished={liveFinished}
        recap={
          <RecapPanel
            bookId={bookId}
            lastReadAt={book.lastReadAt}
            seriesIndex={book.seriesIndex ?? 0}
            pos={recapPos}
            isPlaying={playback.isPlaying}
            open={recapOpen}
            onOpenChange={setRecapOpen}
            container={recapSlot}
            playbackRate={playback.playbackRate}
            onTransport={setRecapTransport}
            onPause={playback.pause}
            onPlay={playback.play}
          />
        }
        recapOpen={recapOpen}
        onToggleRecap={() => setRecapOpen(!recapOpen)}
        annotationsView={annotationsView}
        onToggleAnnotationsView={() =>
          setAnnotationsView((v) => {
            if (v) setSelectedSpeaker(null)
            saveAnnotationsView(!v)
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

      <ParagraphContextMenu
        ref={contextMenuRef}
        bookId={bookId}
        chapters={chapters}
        speakers={speakersQuery.data ?? []}
        onError={setChapterActionError}
      />

      <AnnotationTooltip ref={tooltipRef} />
    </div>
  )
}
