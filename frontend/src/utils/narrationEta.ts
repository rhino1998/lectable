import type { JobsSnapshot, JobsSummary } from '../api/types'

export interface BookEta {
  bookId: string
  bookTitle: string
  chars: number
  seconds: number
}

export interface NarrationEta {
  charsPerSecond: number
  // Seconds of audio rendered per wall-clock second
  // (JobsSnapshot.narrationRealtimeFactor), 0 if unknown.
  realtimeFactor: number
  totalSeconds: number
  books: BookEta[] // most remaining first
}

// Time left for the narration already queued or in flight: every
// voice_clone/voice_design task's paragraph text, summed per book by the
// backend (JobsSnapshot.narrationLeft - the snapshot's own task list is
// only the head of the queue), at the
// backend's recently measured rate (JobsSnapshot.narrationCharsPerSecond,
// every generation slot together). Only covers what's enqueued - reading
// ahead or a bulk "Generate" adds more as it goes - and null while the
// backend has no rate to offer or nothing narration-shaped is queued.
export function narrationEta(snapshot: JobsSnapshot | undefined): NarrationEta | null {
  const rate = snapshot?.narrationCharsPerSecond ?? 0
  if (!snapshot || rate <= 0) return null
  const books = (snapshot.narrationLeft ?? [])
    .filter((b) => b.chars > 0)
    .map((b) => ({ ...b, seconds: b.chars / rate }))
    .sort((a, b) => b.chars - a.chars)
  if (books.length === 0) return null
  const totalChars = books.reduce((n, b) => n + b.chars, 0)
  return {
    charsPerSecond: rate,
    realtimeFactor: snapshot.narrationRealtimeFactor ?? 0,
    totalSeconds: totalChars / rate,
    books,
  }
}

// narrationRate is the speed to estimate not-yet-queued work at: the live
// rate while narration is running, else the last run's (the backend keeps
// it after the run ends); 0 when nothing has been measured since startup.
export function narrationRate(snapshot: JobsSnapshot | JobsSummary | undefined): number {
  return snapshot?.narrationCharsPerSecond || snapshot?.lastNarrationCharsPerSecond || 0
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
