import { useCallback, useLayoutEffect, useRef } from 'react'

// Wraps `fn` in a stable function identity (never changes across renders)
// that always calls whatever the latest `fn` actually is - the same
// "latest ref" pattern usePlayback's own stateRef uses, applied here so
// callbacks handed down to memoized children (ChapterSection/
// ParagraphGroup) don't force them to re-render just because the
// underlying handler closed over some value that changed (e.g. a mutation
// object TanStack Query re-creates each render).
export function useStableCallback<A extends unknown[], R>(
  fn: (...args: A) => R,
): (...args: A) => R {
  const fnRef = useRef(fn)
  // Updated after render (before paint, so before any event can fire)
  // rather than during it - render must stay free of side effects.
  useLayoutEffect(() => {
    fnRef.current = fn
  })
  return useCallback((...args: A) => fnRef.current(...args), [])
}
