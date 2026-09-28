import type { JobsSnapshot } from '../api/types'

export interface BookEta {
  bookId: string
  bookTitle: string
  chars: number
  seconds: number
}

export interface NarrationEta {
  charsPerSecond: number
  totalSeconds: number
  books: BookEta[] // most remaining first
}

// Time left for the narration already queued or in flight: every
// voice_clone/voice_design task's paragraph text (QueueTask.chars), at the
// backend's recently measured rate (JobsSnapshot.narrationCharsPerSecond,
// every generation slot together). Only covers what's enqueued - reading
// ahead or a bulk "Generate" adds more as it goes - and null while the
// backend has no rate to offer or nothing narration-shaped is queued.
export function narrationEta(snapshot: JobsSnapshot | undefined): NarrationEta | null {
  const rate = snapshot?.narrationCharsPerSecond ?? 0
  if (!snapshot || rate <= 0) return null
  const byBook = new Map<string, BookEta>()
  for (const t of [...snapshot.inFlight, ...snapshot.queued]) {
    if (!t.chars) continue
    const b = byBook.get(t.bookId) ?? {
      bookId: t.bookId,
      bookTitle: t.bookTitle,
      chars: 0,
      seconds: 0,
    }
    b.chars += t.chars
    byBook.set(t.bookId, b)
  }
  if (byBook.size === 0) return null
  const books = [...byBook.values()]
  for (const b of books) b.seconds = b.chars / rate
  books.sort((a, b) => b.chars - a.chars)
  const totalChars = books.reduce((n, b) => n + b.chars, 0)
  return { charsPerSecond: rate, totalSeconds: totalChars / rate, books }
}

// formatEta is formatDurationLong with a floor: anything under a minute
// reads "<1m" rather than "0m".
export function formatEta(seconds: number): string {
  if (seconds < 60) return '<1m'
  const totalMinutes = Math.round(seconds / 60)
  const h = Math.floor(totalMinutes / 60)
  const m = totalMinutes % 60
  if (h === 0) return `${m}m`
  return m === 0 ? `${h}h` : `${h}h ${m}m`
}
