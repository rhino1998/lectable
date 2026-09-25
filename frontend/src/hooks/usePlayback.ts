import { useEffect, useMemo, useRef, useState } from 'react'
import type { ChapterDetail } from '../api/types'

const POSITION_REPORT_INTERVAL_MS = 3000
const PLAYBACK_RATE_STORAGE_KEY = 'lectable:playback-rate'

// Playback speed is a per-listener preference, not per-book state, so it
// lives in localStorage rather than going through the backend.
function loadStoredPlaybackRate(): number {
  try {
    const raw = window.localStorage.getItem(PLAYBACK_RATE_STORAGE_KEY)
    const parsed = raw ? Number(raw) : NaN
    return Number.isFinite(parsed) && parsed > 0 ? parsed : 1
  } catch {
    return 1
  }
}

function storePlaybackRate(rate: number): void {
  try {
    window.localStorage.setItem(PLAYBACK_RATE_STORAGE_KEY, String(rate))
  } catch {
    // Private browsing / blocked storage - the rate just won't persist.
  }
}

interface UsePlaybackArgs {
  // Currently loaded chapters, keyed by chapter idx. Playback reads
  // straight out of this map by (chapterIdx, paragraphIdx) rather than a
  // flat array index, since infinite scroll can prepend earlier chapters
  // at any time and a flat index would shift out from under it.
  chapters: Map<number, ChapterDetail>
  totalChapters: number
  initialChapterIdx: number
  initialParagraphIdx: number
  initialSeconds: number
  // False while the caller's own saved-position data (e.g. a book query)
  // is still loading. initialChapterIdx/initialParagraphIdx/initialSeconds
  // aren't meaningful yet at that point (ReaderPage has to pass some
  // value - 0/0/0 - since this hook, like any, must be called
  // unconditionally rather than only once real data exists) - this is
  // what lets the hook tell "genuinely position 0" apart from "the real
  // position just hasn't arrived yet" and apply it exactly once, whenever
  // it actually does. See the effect below.
  ready: boolean
  onPosition: (chapterIdx: number, paragraphIdx: number, seconds: number) => void
  // Called when playback reaches the end of a loaded chapter and the next
  // one isn't loaded yet, so the caller can grow its loaded range.
  onNeedChapter: (chapterIdx: number) => void
}

export interface PlaybackController {
  chapterIdx: number
  paragraphIdx: number
  isPlaying: boolean
  isWaitingForAudio: boolean
  currentTime: number
  duration: number
  playbackRate: number
  // The currently-active <audio> element - exposed so consumers needing
  // per-frame precision (word-highlighting) can poll audioElement.currentTime
  // directly via requestAnimationFrame instead of waiting on the
  // 'timeupdate' event, which only fires a few times a second. This swaps
  // identity across a gapless paragraph transition (see usePlayback's
  // double-buffering) - consumers should treat it as "whichever element is
  // playing right now," not a fixed object.
  audioElement: HTMLAudioElement
  play: () => void
  pause: () => void
  // opts.autoplay (default true) starts/resumes playback at the new
  // position, matching every existing "click to play from here" call
  // site (paragraph click, search results, bookmarks, chapter jump).
  // Pass { autoplay: false } to only move position/scroll - used by
  // ReaderPage's "jump to next appearance" so it doesn't start playing
  // audio the reader had paused, or start playing at all if they were
  // already paused before the jump.
  playAt: (chapterIdx: number, paragraphIdx: number, opts?: { autoplay?: boolean }) => void
  // Jumps to a timestamp within the currently-active paragraph's own clip
  // (word-click-to-seek) - resumes playback if paused, otherwise just
  // relocates the already-playing audio.
  seek: (seconds: number) => void
  setPlaybackRate: (rate: number) => void
}

// Given a playback position within a paragraph's own clip, walks forward
// through however many scare-quote merge-group members (see api/types.ts's
// own Paragraph.audioPointerSeconds) that position has already reached -
// not just the immediate next one - so a caller that couldn't check every
// single boundary in real time (a 'timeupdate'/'ended' fallback catching up
// after the per-frame loop below missed several while backgrounded, or one
// coarse 'timeupdate' tick spanning more than one very short merge-group
// member) still lands on whichever paragraph is actually playing right now.
function advanceMergeGroup(chapter: ChapterDetail | undefined, fromIdx: number, atTime: number): number {
  if (!chapter) return fromIdx
  let idx = fromIdx
  for (;;) {
    const current = chapter.paragraphs[idx]
    const currentUrl = current?.audioStatus === 'ready' ? current.audioUrl : undefined
    const next = chapter.paragraphs[idx + 1]
    if (
      !currentUrl ||
      !next ||
      next.audioStatus !== 'ready' ||
      next.audioUrl !== currentUrl ||
      atTime < (next.audioPointerSeconds ?? 0)
    ) {
      return idx
    }
    idx += 1
  }
}

// The clip that plays after paragraph (chapterIdx, paragraphIdx)'s own: the
// first paragraph past its scare-quote merge group (whose members all share
// one clip - see advanceMergeGroup), crossing into the next loaded chapter.
// Preloading paragraphIdx + 1 instead re-fetched the clip already playing
// while inside a group, and only reached the real next clip once playback
// got to the group's last member - often a single word, too late to buffer,
// so the swap in handleEnded fell back to a cold load. Undefined when that
// paragraph isn't ready, or doesn't start its own clip.
function resolveNextClipUrl(
  chapters: Map<number, ChapterDetail>,
  chapterIdx: number,
  paragraphIdx: number,
): string | undefined {
  const current = chapters.get(chapterIdx)?.paragraphs[paragraphIdx]
  const currentUrl = current?.audioStatus === 'ready' ? current.audioUrl : undefined
  let ci = chapterIdx
  let pi = paragraphIdx + 1
  for (;;) {
    const chapter = chapters.get(ci)
    if (!chapter) return undefined
    if (pi >= chapter.paragraphs.length) {
      ci += 1
      pi = 0
      continue
    }
    const p = chapter.paragraphs[pi]
    if (p.audioStatus !== 'ready' || !p.audioUrl) return undefined
    if (p.audioUrl !== currentUrl) return (p.audioPointerSeconds ?? 0) === 0 ? p.audioUrl : undefined
    pi += 1
  }
}

// Resolves paragraph (chapterIdx, paragraphIdx)'s ready audio URL, crossing
// into the next loaded chapter for paragraphIdx one past the current
// chapter's end - same "what comes next" logic 'ended' uses to advance.
function resolveReadyUrl(
  chapters: Map<number, ChapterDetail>,
  chapterIdx: number,
  paragraphIdx: number,
): string | undefined {
  const chapter = chapters.get(chapterIdx)
  const paragraph = chapter?.paragraphs[paragraphIdx]
  if (paragraph) {
    return paragraph.audioStatus === 'ready' ? paragraph.audioUrl : undefined
  }
  if (chapter && paragraphIdx >= chapter.paragraphs.length) {
    const next = chapters.get(chapterIdx + 1)?.paragraphs[0]
    return next?.audioStatus === 'ready' ? next.audioUrl : undefined
  }
  return undefined
}

export function usePlayback({
  chapters,
  totalChapters,
  initialChapterIdx,
  initialParagraphIdx,
  initialSeconds,
  ready,
  onPosition,
  onNeedChapter,
}: UsePlaybackArgs): PlaybackController {
  // Two <audio> elements, alternating which is "active" (playing, exposed
  // as audioElement) and which is "standby". The standby element preloads
  // the *next* paragraph's clip while the active one is still playing, so
  // advancing on 'ended' is a fast swap-and-play instead of a cold
  // src/load/play cycle - that cold cycle (network fetch plus the
  // browser's own decoder setup for a brand new element source) is what
  // causes a noticeable gap between paragraphs with a single shared
  // element.
  const elementsRef = useRef<[HTMLAudioElement, HTMLAudioElement] | null>(null)
  if (elementsRef.current === null) {
    elementsRef.current = [new Audio(), new Audio()]
  }
  const elements = elementsRef.current

  const [activeIndex, setActiveIndex] = useState(0)
  const activeIndexRef = useRef(activeIndex)
  activeIndexRef.current = activeIndex

  const [chapterIdx, setChapterIdx] = useState(initialChapterIdx)
  const [paragraphIdx, setParagraphIdx] = useState(initialParagraphIdx)
  const [isPlaying, setIsPlaying] = useState(false)
  const [currentTime, setCurrentTime] = useState(0)
  const [duration, setDuration] = useState(0)
  const [playbackRate, setPlaybackRateState] = useState(loadStoredPlaybackRate)

  const appliedInitialSeek = useRef(false)
  const appliedInitialPosition = useRef(false)
  const lastReportRef = useRef(0)
  // Set when 'ended' fires but the next chapter isn't loaded yet; consumed
  // by the effect below once `chapters` grows to include it.
  const pendingAdvanceRef = useRef(false)

  // Catches up chapterIdx/paragraphIdx once the caller's real saved
  // position actually arrives (ready flips true), rather than being
  // permanently stuck at whatever placeholder (0/0) it had to pass on
  // this hook's very first call - useState(initialChapterIdx) above only
  // ever reads its argument on that first call, same as any React state
  // initializer, so without this a book opened straight into (not
  // navigated to from somewhere the data was already loaded) would always
  // resume from the very start regardless of the saved position.
  useEffect(() => {
    if (!ready || appliedInitialPosition.current) return
    appliedInitialPosition.current = true
    setChapterIdx(initialChapterIdx)
    setParagraphIdx(initialParagraphIdx)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ready])

  const currentChapter = chapters.get(chapterIdx)
  const currentParagraph = currentChapter?.paragraphs[paragraphIdx]
  const readyAudioUrl = currentParagraph?.audioStatus === 'ready' ? currentParagraph.audioUrl : undefined
  const isWaitingForAudio = isPlaying && currentParagraph !== undefined && !readyAudioUrl
  // currentTime/duration state below track the ACTIVE <audio> element
  // directly (el.currentTime/el.duration) - correct for an ordinary
  // paragraph, but not for any member of a scare-quote merge group (see
  // api/types.ts's own Paragraph.audioPointerSeconds): the element's own
  // position/length describe the WHOLE shared clip, not this paragraph's
  // own logical share of it (which can be much shorter, and doesn't start
  // at the clip's own beginning for anything but the group's anchor).
  // Rebasing against the paragraph's own audioPointerSeconds/durationSeconds
  // - already correct for every paragraph, merged or not, since the
  // backend computes them as "this paragraph's own share" either way - is
  // what keeps PlayerBar's chapter-elapsed/remaining math (which sums
  // finished paragraphs' own durationSeconds plus the in-progress one's
  // currentTime/duration fraction) accurate while playing through one.
  const pointerOffsetSeconds = currentParagraph?.audioPointerSeconds ?? 0
  const exposedCurrentTime = Math.max(0, currentTime - pointerOffsetSeconds)
  const exposedDuration =
    currentParagraph?.audioStatus === 'ready' && currentParagraph.durationSeconds
      ? currentParagraph.durationSeconds
      : duration
  const nextReadyAudioUrl = useMemo(
    () => resolveNextClipUrl(chapters, chapterIdx, paragraphIdx),
    [chapters, chapterIdx, paragraphIdx],
  )

  // Keep latest values reachable from event handlers without re-attaching
  // listeners (and their closures) on every render.
  const stateRef = useRef({
    chapterIdx,
    paragraphIdx,
    chapters,
    totalChapters,
    onPosition,
    onNeedChapter,
    playbackRate,
    nextReadyAudioUrl,
  })
  stateRef.current = {
    chapterIdx,
    paragraphIdx,
    chapters,
    totalChapters,
    onPosition,
    onNeedChapter,
    playbackRate,
    nextReadyAudioUrl,
  }

  // Attached once per element, for the element's whole lifetime - each
  // handler ignores events from whichever element isn't currently active,
  // so a swap never needs to detach/reattach listeners.
  useEffect(() => {
    const cleanups = elements.map((el, idx) => {
      const isActive = () => activeIndexRef.current === idx

      const handleTimeUpdate = () => {
        if (!isActive()) return
        setCurrentTime(el.currentTime)
        // Native 'timeupdate' keeps firing even when the RAF loop below is
        // throttled/suspended (a backgrounded tab, a locked phone screen -
        // the normal way an audiobook actually gets listened to), so it
        // doubles as that loop's fallback: without this, paragraphIdx can
        // freeze mid-merge-group while the underlying clip keeps playing
        // right through it, and 'ended' then advances only one paragraph
        // from that stale position - audibly skipping straight to whatever
        // comes after the whole group instead of ever making its members
        // (a scare quote's own short fragment, then the narration after it)
        // active.
        const { chapterIdx: ci, paragraphIdx: pi, chapters: chs, onPosition: report } = stateRef.current
        const advanced = advanceMergeGroup(chs.get(ci), pi, el.currentTime)
        if (advanced !== pi) setParagraphIdx(advanced)
        const now = Date.now()
        if (now - lastReportRef.current > POSITION_REPORT_INTERVAL_MS) {
          lastReportRef.current = now
          report(ci, advanced, el.currentTime)
        }
      }
      const handleDurationChange = () => {
        if (!isActive()) return
        setDuration(Number.isFinite(el.duration) ? el.duration : 0)
      }
      const handleEnded = () => {
        if (!isActive()) return
        const {
          chapterIdx: ci,
          paragraphIdx: pi,
          chapters: chs,
          totalChapters: tc,
          onPosition: report,
          onNeedChapter: needChapter,
        } = stateRef.current

        const chapter = chs.get(ci)
        // Belt-and-suspenders alongside handleTimeUpdate's own fallback
        // above: resolves the merge group's real last member from the
        // clip's own full duration, rather than assuming paragraphIdx (pi)
        // already got walked all the way there - covers the rare case
        // 'timeupdate' didn't get a tick in between the last boundary and
        // the clip actually ending.
        const resolvedPi = advanceMergeGroup(chapter, pi, el.duration || Number.POSITIVE_INFINITY)
        report(ci, resolvedPi, el.duration || 0)

        const advanceWithin = chapter?.paragraphs[resolvedPi + 1] !== undefined
        const nextChapterReady = (chs.get(ci + 1)?.paragraphs.length ?? 0) > 0

        const otherIdx = idx === 0 ? 1 : 0
        const standby = elements[otherIdx]
        const nextUrl = resolveReadyUrl(chs, ci, resolvedPi + 1)
        const standbyReady = nextUrl !== undefined && standby.currentSrc.endsWith(nextUrl) && standby.readyState >= 2

        if (advanceWithin) {
          if (standbyReady) {
            setActiveIndex(otherIdx)
            standby.currentTime = 0
            // Belt-and-suspenders alongside the preload effect's own
            // after-load() reapplication: guarantees the element that's
            // about to become active is at the right rate right as it
            // starts playing, regardless of how it got preloaded.
            standby.playbackRate = stateRef.current.playbackRate
            standby.play().catch(() => {})
          }
          setParagraphIdx(resolvedPi + 1)
          setCurrentTime(0)
          return
        }
        if (ci + 1 < tc) {
          if (nextChapterReady) {
            if (standbyReady) {
              setActiveIndex(otherIdx)
              standby.currentTime = 0
              standby.playbackRate = stateRef.current.playbackRate
              standby.play().catch(() => {})
            }
            setChapterIdx(ci + 1)
            setParagraphIdx(0)
            setCurrentTime(0)
          } else {
            needChapter(ci + 1)
            pendingAdvanceRef.current = true
          }
          return
        }
        setIsPlaying(false)
      }

      el.addEventListener('timeupdate', handleTimeUpdate)
      el.addEventListener('durationchange', handleDurationChange)
      el.addEventListener('ended', handleEnded)
      return () => {
        el.removeEventListener('timeupdate', handleTimeUpdate)
        el.removeEventListener('durationchange', handleDurationChange)
        el.removeEventListener('ended', handleEnded)
      }
    })
    return () => cleanups.forEach((cleanup) => cleanup())
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // Advances the logical "current paragraph" through a scare-quote merge
  // group's own shared clip (see api/types.ts's own
  // Paragraph.audioPointerSeconds) purely by comparing the active element's
  // currentTime against each next paragraph's own start offset within that
  // same clip - never touching the element itself (no seek, no reload, no
  // pause): the underlying recording is one continuous, never-cut file, so
  // there's nothing to actually transition. A merge group's own real end is
  // a genuinely different audioUrl (or nothing at all), handled by
  // handleEnded above, not this.
  //
  // Polls via requestAnimationFrame rather than relying solely on the
  // native 'timeupdate' event (which only fires a few times a second) for
  // the same reason ParagraphText's own word-highlighting does: 'timeupdate'
  // is coarse enough that a very short merge-group member (often just one
  // word, from a scare quote's own short fragment - see
  // store.Paragraph.ScareQuote) can start and end entirely between two
  // ticks, skipping straight past it to whatever comes after without it
  // ever becoming "active" - and since ParagraphText only starts its own
  // per-word highlight polling once a paragraph is active, a skipped one
  // never highlights at all, a real, observed bug this fixes.
  //
  // This is the fine-grained path, not the only one: `requestAnimationFrame`
  // is throttled/suspended in a backgrounded tab or on a locked phone
  // screen - the normal way an audiobook actually gets listened to - which
  // would otherwise freeze paragraphIdx mid-group while the clip keeps
  // playing right through it. handleTimeUpdate's own call into
  // advanceMergeGroup (native 'timeupdate', unaffected by that throttling)
  // and handleEnded's own resolve-from-duration are what keep that case
  // correct; this effect is just the one that makes each boundary land in
  // real time while the tab is actually visible.
  useEffect(() => {
    if (!isPlaying) return
    let frame: number
    const tick = () => {
      const el = elements[activeIndexRef.current]
      const { chapters: chs, chapterIdx: ci, paragraphIdx: pi } = stateRef.current
      const advanced = advanceMergeGroup(chs.get(ci), pi, el.currentTime)
      if (advanced !== pi) setParagraphIdx(advanced)
      frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(frame)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isPlaying, elements])

  // Resumes an advance that was blocked on a not-yet-loaded chapter, once
  // it shows up in the map (e.g. the infinite-scroll fetch caught up).
  useEffect(() => {
    if (!pendingAdvanceRef.current) return
    const nextChapter = chapters.get(chapterIdx + 1)
    if (nextChapter && nextChapter.paragraphs.length > 0) {
      pendingAdvanceRef.current = false
      setChapterIdx(chapterIdx + 1)
      setParagraphIdx(0)
      setCurrentTime(0)
    }
  }, [chapters, chapterIdx])

  // Load (and play, if requested) the active element whenever the resolved
  // audio src changes. If a gapless swap (in handleEnded above) already put
  // the right src on this element, currentSrc already matches and this is
  // a no-op except for the play() call, which is harmless on an already-
  // playing element.
  useEffect(() => {
    const audio = elements[activeIndex]
    if (!readyAudioUrl) {
      audio.pause()
      return
    }

    const needsLoad = !audio.currentSrc.endsWith(readyAudioUrl)
    if (needsLoad) {
      audio.src = readyAudioUrl
      audio.load()
      // load() drops the element's playback rate back to 1 in some
      // browsers, so it has to be re-applied on every paragraph change,
      // not just when the user actually picks a new speed.
      audio.playbackRate = stateRef.current.playbackRate

      // A genuinely new src load needs to seek: either to a previously
      // saved position (once, on this hook's very first load - see
      // appliedInitialSeek), or - if the paragraph being loaded is itself
      // a scare-quote merge group pointer landed on directly (a click, a
      // saved position, or playAt jumping straight into the middle of a
      // group rather than arriving via handleTimeUpdate's own natural
      // advancement) - to that paragraph's own start offset within the
      // shared clip, rather than defaulting to 0 (the anchor's own start,
      // which would replay the group from its very beginning). The saved
      // position always wins when both apply, since it's the more precise
      // "resume exactly here" of the two.
      const isInitialPosition =
        !appliedInitialSeek.current &&
        chapterIdx === initialChapterIdx &&
        paragraphIdx === initialParagraphIdx &&
        initialSeconds > 0
      appliedInitialSeek.current = true
      const seekTarget = isInitialPosition ? initialSeconds : (currentParagraph?.audioPointerSeconds ?? 0)
      if (seekTarget > 0) {
        const seekOnce = () => {
          audio.currentTime = seekTarget
          audio.removeEventListener('loadedmetadata', seekOnce)
        }
        audio.addEventListener('loadedmetadata', seekOnce)
      }
    }

    if (isPlaying) {
      audio.play().catch(() => {
        // Autoplay can be rejected if not triggered by a user gesture; the
        // user's next explicit Play click will retry.
      })
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [readyAudioUrl, isPlaying, activeIndex])

  // Preloads the next paragraph's clip onto the standby element as soon as
  // it's ready, while the active element is still playing the current one.
  // playbackRate is set *after* load() (not before), same reason as the
  // active element's own load effect: load() resets an element's rate back
  // to 1 in some browsers, so setting it first just gets clobbered - the
  // standby element would then get swapped in at 1x by handleEnded's
  // gapless transition below, with nothing left to reapply the real rate.
  useEffect(() => {
    const standby = elements[activeIndex === 0 ? 1 : 0]
    if (!nextReadyAudioUrl) return
    if (standby.currentSrc.endsWith(nextReadyAudioUrl)) return
    standby.src = nextReadyAudioUrl
    standby.load()
    standby.playbackRate = playbackRate
  }, [elements, activeIndex, nextReadyAudioUrl, playbackRate])

  useEffect(() => {
    elements[0].playbackRate = playbackRate
    elements[1].playbackRate = playbackRate
  }, [elements, playbackRate])

  return useMemo(
    () => ({
      chapterIdx,
      paragraphIdx,
      isPlaying,
      isWaitingForAudio,
      currentTime: exposedCurrentTime,
      duration: exposedDuration,
      playbackRate,
      audioElement: elements[activeIndex],
      play: () => setIsPlaying(true),
      pause: () => {
        elements[activeIndex].pause()
        setIsPlaying(false)
      },
      playAt: (ci: number, pi: number, opts?: { autoplay?: boolean }) => {
        pendingAdvanceRef.current = false
        setChapterIdx(ci)
        setParagraphIdx(pi)
        setCurrentTime(0)
        if (opts?.autoplay ?? true) setIsPlaying(true)
      },
      seek: (seconds: number) => {
        const audio = elements[activeIndex]
        audio.currentTime = seconds
        setCurrentTime(seconds)
        if (!isPlaying) {
          setIsPlaying(true)
          audio.play().catch(() => {})
        }
      },
      setPlaybackRate: (rate: number) => {
        storePlaybackRate(rate)
        setPlaybackRateState(rate)
      },
    }),
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [chapterIdx, paragraphIdx, isPlaying, isWaitingForAudio, exposedCurrentTime, exposedDuration, playbackRate, activeIndex],
  )
}
