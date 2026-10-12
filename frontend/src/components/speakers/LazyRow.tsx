import { useEffect, useRef, useState, type ReactNode } from 'react'

// One observer for every row: a roster can run to thousands of speakers.
const listeners = new WeakMap<Element, (entry: IntersectionObserverEntry) => void>()
let observer: IntersectionObserver | null = null
function rowObserver(): IntersectionObserver {
  observer ??= new IntersectionObserver(
    (entries) => {
      for (const entry of entries) listeners.get(entry.target)?.(entry)
    },
    // A few screens either side, so scrolling rarely shows an empty slot
    // and a row being edited isn't dropped the moment it leaves the screen.
    { rootMargin: '1500px 0px' },
  )
  return observer
}

// A list item that renders its row only while it is near the screen, and
// holds its place with an empty box of the row's last height otherwise - a
// long roster costs the rows in view, not every speaker's controls and
// voice samples. `pinned` keeps a row with something open (its merge panel,
// voice editor, appearances) rendered wherever the reader scrolls.
export function LazyRow({
  pinned = false,
  initiallyNear = false,
  estimatedHeight,
  children,
}: {
  pinned?: boolean
  // Rendered on the first paint rather than after the observer's first
  // report - the rows at the top of the list.
  initiallyNear?: boolean
  estimatedHeight: number
  children: () => ReactNode
}) {
  const ref = useRef<HTMLLIElement>(null)
  // Where there is no IntersectionObserver (tests), every row renders.
  const [near, setNear] = useState(initiallyNear || typeof IntersectionObserver === 'undefined')
  const [height, setHeight] = useState(estimatedHeight)

  useEffect(() => {
    const el = ref.current
    if (!el || typeof IntersectionObserver === 'undefined') return
    const obs = rowObserver()
    listeners.set(el, (entry) => {
      // Leaving: the box that replaces the row keeps the height it had.
      if (!entry.isIntersecting && el.firstElementChild) setHeight(el.offsetHeight)
      setNear(entry.isIntersecting)
    })
    obs.observe(el)
    return () => {
      obs.unobserve(el)
      listeners.delete(el)
    }
  }, [])

  const shown = near || pinned
  return (
    <li ref={ref} style={shown ? undefined : { height }}>
      {shown ? children() : null}
    </li>
  )
}
