import { useEffect } from 'react'
import type { RefObject } from 'react'

// Calls onIntersect whenever the observed element is on-screen. Used to
// grow the loaded chapter range as the reader scrolls, instead of
// requiring an explicit chapter picker.
//
// `deps` should include whatever changes the surrounding content's height
// (e.g. the loaded range itself). Re-observing on each change forces a
// fresh intersection check even if the sentinel never actually left the
// viewport between loads (short chapters on a tall screen would otherwise
// only get one expansion, since IntersectionObserver only reports
// *changes* in intersection, not a continuous "still visible" signal).
export function useInView(
  ref: RefObject<Element | null>,
  onIntersect: () => void,
  rootMargin = '600px',
  deps: unknown[] = [],
) {
  useEffect(() => {
    const el = ref.current
    if (!el) return

    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (entry.isIntersecting) onIntersect()
        }
      },
      { rootMargin },
    )
    observer.observe(el)
    return () => observer.disconnect()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ref.current, rootMargin, ...deps])
}
