import { useCallback, useState } from 'react'

export interface ParagraphPos {
  chapterIdx: number
  paragraphIdx: number
}

const posKey = (p: ParagraphPos) => `${p.chapterIdx}:${p.paragraphIdx}`

// How many of this reader's own recent saves count as echoes. Saves are
// throttled and the book topic can deliver an older one after a newer one
// has gone out, so the last one alone isn't enough.
const OWN_SAVES_KEPT = 20

// remotePosition decides whether the book's saved position (the live book
// topic's pos*) was moved by another device: it differs from where this
// reader is, isn't an echo of one of this reader's own recent saves, and
// hasn't been dismissed. Pure, for testing.
export function remotePosition(
  saved: ParagraphPos | undefined,
  current: ParagraphPos,
  ownSaves: readonly string[],
  dismissed: string | null,
): ParagraphPos | null {
  if (!saved) return null
  const k = posKey(saved)
  if (k === posKey(current) || ownSaves.includes(k) || k === dismissed) return null
  return saved
}

// useRemotePosition backs the reader's "moved on another device" notice:
// the Android app or another tab saving to the same book while this reader
// sat open - without it, resuming here would silently play from the stale
// spot and overwrite the other device's progress. It only holds the
// reader's own recent saves (noteOwnSave, called with every position it
// saves) and what's been dismissed; the reader combines them with its live
// position via remotePosition, only while paused - whoever is actively
// listening here owns the position.
export function useRemotePosition() {
  const [ownSaves, setOwnSaves] = useState<string[]>([])
  const [dismissed, setDismissed] = useState<string | null>(null)

  const noteOwnSave = useCallback((p: ParagraphPos) => {
    const k = posKey(p)
    setOwnSaves((prev) => (prev.includes(k) ? prev : [...prev.slice(1 - OWN_SAVES_KEPT), k]))
  }, [])
  const dismiss = useCallback((p: ParagraphPos) => setDismissed(posKey(p)), [])

  return { ownSaves, dismissed, noteOwnSave, dismiss }
}
