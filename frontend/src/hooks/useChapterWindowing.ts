import { startTransition, useCallback, useEffect, useRef, useState } from 'react'

// The reader's chapter-level scroll bookkeeping over its mounted range
// [rangeStart, rangeEnd]: which chapter is most visible, and each
// chapter's last-measured height for windowing. Chapter sections register
// through registerChapterSectionRef; both observers re-attach whenever the
// range changes.
//
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
export function useChapterWindowing(rangeStart: number | undefined, rangeEnd: number | undefined) {
  const chapterSectionRefs = useRef(new Map<number, HTMLElement>())
  const registerChapterSectionRef = useCallback((idx: number, el: HTMLElement | null) => {
    if (el) chapterSectionRefs.current.set(idx, el)
    else chapterSectionRefs.current.delete(idx)
  }, [])
  const chapterVisibilityRef = useRef(new Map<number, number>())
  const [visibleChapterIdx, setVisibleChapterIdx] = useState<number | null>(null)
  useEffect(() => {
    // A fresh observer reports every observed section on its first
    // callback, so drop ratios left over from the previous range - after
    // a jump resets the range, a chapter no longer mounted would otherwise
    // keep its last ratio forever and could win "most visible".
    chapterVisibilityRef.current.clear()
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
        // A transition: this recenters the render window (focusChapterIdx
        // below), which mounts whole chapters - a couple hundred
        // paragraphs each - as they come into range. Rendered urgently
        // that blocked the main thread for hundreds of ms mid-scroll (and
        // right after every far jump); as a transition React time-slices
        // it between frames instead.
        if (best !== null) {
          const next = best
          startTransition(() => setVisibleChapterIdx(next))
        }
      },
      { threshold: [0, 0.1, 0.25, 0.5, 0.75, 1] },
    )
    for (const el of chapterSectionRefs.current.values()) observer.observe(el)
    return () => observer.disconnect()
  }, [rangeStart, rangeEnd])

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
  }, [rangeStart, rangeEnd, registerHeight])

  return { registerChapterSectionRef, visibleChapterIdx, chapterHeights }
}
