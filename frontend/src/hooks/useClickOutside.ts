import { useEffect, useRef } from 'react'
import type { RefObject } from 'react'

// Calls onOutside for a pointerdown landing outside every given ref's
// element, while active - closes an open popover on an outside click.
// Takes an array of refs (not just one) since a toggle button and its
// panel are sometimes separate, non-nested DOM subtrees (the player bar's
// Voice popover), and clicking the toggle button itself must not count as
// "outside" - it already has its own open/close handler.
//
// pointerdown (not click) is used so this fires and closes the popover
// before the button's own onClick would otherwise immediately reopen it
// in the same gesture. refs/onOutside are read from a ref each call
// rather than listed as effect dependencies, so passing a fresh array or
// closure literal each render (the normal case for a caller) doesn't
// re-attach the listener on every render - only when `active` flips, same
// stateRef pattern usePlayback.ts uses for its own DOM listeners.
export function useClickOutside(
  refs: readonly RefObject<HTMLElement | null>[],
  onOutside: () => void,
  active: boolean,
) {
  const latest = useRef({ refs, onOutside })
  useEffect(() => {
    latest.current = { refs, onOutside }
  })

  useEffect(() => {
    if (!active) return
    const handlePointerDown = (e: PointerEvent) => {
      const target = e.target as Node
      const { refs: currentRefs, onOutside: currentOnOutside } = latest.current
      const inside = currentRefs.some((ref) => ref.current?.contains(target))
      if (!inside) currentOnOutside()
    }
    document.addEventListener('pointerdown', handlePointerDown)
    return () => document.removeEventListener('pointerdown', handlePointerDown)
  }, [active])
}
