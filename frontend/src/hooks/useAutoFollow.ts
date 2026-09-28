import { useCallback, useEffect, useRef, useState } from 'react'

// How many viewport-heights away the active paragraph can be before
// following it jumps there instantly instead of smooth-scrolling - see
// scrollToActiveParagraph.
const FAR_SCROLL_VIEWPORTS = 3

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
//
// Paragraphs register through registerParagraphRef (keyed
// "chapterIdx:paragraphIdx"); visibleChapterIdx is useChapterWindowing's
// most-visible chapter; renderedChapters is the deferred chapter map the
// paragraphs actually render from (see the follow effect's retry).
export function useAutoFollow({
  chapterIdx,
  paragraphIdx,
  renderedChapters,
  visibleChapterIdx,
}: {
  chapterIdx: number
  paragraphIdx: number
  renderedChapters: unknown
  visibleChapterIdx: number | null
}) {
  const paragraphRefs = useRef(new Map<string, HTMLElement>())
  const registerParagraphRef = useCallback((key: string, el: HTMLElement | null) => {
    if (el) paragraphRefs.current.set(key, el)
    else paragraphRefs.current.delete(key)
  }, [])

  const [autoFollow, setAutoFollow] = useState(true)
  const hasScrolledOnceRef = useRef(false)
  // Read through a ref so scrollToActiveParagraph's identity doesn't churn
  // every time the observer above reports a new most-visible chapter.
  const visibleChapterIdxRef = useRef(visibleChapterIdx)
  visibleChapterIdxRef.current = visibleChapterIdx
  // Which paragraph ("chapterIdx:paragraphIdx") following last actually
  // scrolled to - lets the follow effect below retry only while the
  // target hasn't rendered yet, instead of re-scrolling on every
  // `chapters` change (a live chapter update lands each time a paragraph's
  // audio finishes generating, and each chapter the sentinels load in is
  // another). Re-issuing scrollIntoView on those restarted any smooth
  // scroll still in flight, which read as stutter.
  const followedKeyRef = useRef<string | null>(null)

  // A requested smooth scroll turns into an instant jump when the target
  // is far from what's on screen - more than one chapter away from the
  // most-visible chapter, or more than FAR_SCROLL_VIEWPORTS screens away
  // within one long chapter. Smooth-scrolling across that much of a long
  // book takes seconds, and passes over chapters that are still sized
  // placeholders (see the windowing above) mounting their full DOM
  // mid-animation, so the scroll stutters and can land off-target as
  // their real heights settle. The target itself is always mounted (the
  // playing chapter never collapses to a placeholder); its neighbors fill
  // in afterwards as a transition (see the IntersectionObserver above).
  // Returns whether the target paragraph was mounted at all (false =
  // retry once it renders).
  const scrollToActiveParagraph = useCallback(
    (behavior: ScrollBehavior) => {
      const key = `${chapterIdx}:${paragraphIdx}`
      const el = paragraphRefs.current.get(key)
      if (!el) return false
      followedKeyRef.current = key
      if (behavior === 'smooth') {
        const visibleIdx = visibleChapterIdxRef.current
        const chapterDistance = visibleIdx === null ? 0 : Math.abs(chapterIdx - visibleIdx)
        const rect = el.getBoundingClientRect()
        const viewportDistance =
          Math.abs(rect.top + rect.height / 2 - window.innerHeight / 2) / window.innerHeight
        if (chapterDistance > 1 || viewportDistance > FAR_SCROLL_VIEWPORTS) behavior = 'instant'
      }
      el.scrollIntoView({ block: 'center', behavior })
      hasScrolledOnceRef.current = true
      return true
    },
    [chapterIdx, paragraphIdx],
  )

  useEffect(() => {
    if (!autoFollow) {
      followedKeyRef.current = null
      return
    }
    if (followedKeyRef.current === `${chapterIdx}:${paragraphIdx}`) return
    scrollToActiveParagraph(hasScrolledOnceRef.current ? 'smooth' : 'auto')
    // Re-runs when renderedChapters changes too (the deferred copy the
    // paragraphs are actually rendered from), so it retries once the resumed
    // chapter's paragraphs actually render (first tick after load, the ref
    // won't exist yet) - but only until that first successful scroll.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [chapterIdx, paragraphIdx, renderedChapters, autoFollow])

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

  const jumpToCurrentParagraph = useCallback(() => {
    setAutoFollow(true)
    scrollToActiveParagraph('smooth')
  }, [scrollToActiveParagraph])

  return { autoFollow, setAutoFollow, registerParagraphRef, jumpToCurrentParagraph }
}
