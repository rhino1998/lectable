import type { BookRecap } from '../api/types'

export interface RecapPos {
  chapterIdx: number
  paragraphIdx: number
}

// How long a reader must have been away from a book before its recap is
// offered unasked, in minutes - one library-wide setting kept by the
// backend (the recapSettings topic): 0 offers it every time the book is
// opened, RECAP_NEVER never.
export const RECAP_NEVER = -1
export const RECAP_AFTER_OPTIONS = [
  { minutes: 0, label: 'immediately' },
  { minutes: 15, label: 'after 15 minutes away' },
  { minutes: 60, label: 'after an hour away' },
  { minutes: 4 * 60, label: 'after 4 hours away' },
  { minutes: 12 * 60, label: 'after 12 hours away' },
  { minutes: 24 * 60, label: 'after a day away' },
  { minutes: 3 * 24 * 60, label: 'after 3 days away' },
  { minutes: 7 * 24 * 60, label: 'after a week away' },
  { minutes: RECAP_NEVER, label: 'never' },
] as const

// What this browser's reader last turned down, per book.
const DISMISSED_KEY = 'lectable.recapDismissed'

// recapKey names the recap a reader turned down - what it is of and the
// place it was written for - so the offer doesn't come back for it.
export function recapKey(recap: BookRecap): string {
  return `${recap.kind}:${recap.chapterIdx}:${recap.paragraphIdx}`
}

function loadDismissed(): Record<string, string> {
  try {
    return JSON.parse(localStorage.getItem(DISMISSED_KEY) ?? '{}') as Record<string, string>
  } catch {
    return {}
  }
}

export function loadDismissedRecap(bookId: string): string | null {
  return loadDismissed()[bookId] ?? null
}

export function saveDismissedRecap(bookId: string, key: string) {
  localStorage.setItem(DISMISSED_KEY, JSON.stringify({ ...loadDismissed(), [bookId]: key }))
}

// recapIsCurrent: the recap was written for where the reader is now - it
// tells nothing past their position and isn't from far behind it.
export function recapIsCurrent(recap: BookRecap | null | undefined, pos: RecapPos): boolean {
  return (
    !!recap &&
    recap.text !== '' &&
    recap.chapterIdx === pos.chapterIdx &&
    recap.paragraphIdx <= pos.paragraphIdx &&
    pos.paragraphIdx <= recap.throughIdx
  )
}

// awayLongEnough: the book was last read (lastReadAt, unix seconds; absent
// for a book never opened) at least afterMinutes before nowMs - any time
// before, for 0.
export function awayLongEnough(
  lastReadAt: number | undefined,
  nowMs: number,
  afterMinutes: number,
): boolean {
  return afterMinutes >= 0 && !!lastReadAt && nowMs / 1000 - lastReadAt >= afterMinutes * 60
}

// shouldOfferRecap decides whether the reader is offered a recap they
// didn't ask for: it is current, they haven't turned it down, and either
// they have been away long enough (lastReadAt as it was when they opened
// the book, before this visit saved anything) or it is the recap of the
// previous book, offered whenever a sequel is started. Pure, for testing.
export function shouldOfferRecap(
  recap: BookRecap | null | undefined,
  pos: RecapPos,
  lastReadAtOnOpen: number | undefined,
  nowMs: number,
  afterMinutes: number,
  dismissed: string | null,
): boolean {
  if (afterMinutes < 0 || !recap || !recapIsCurrent(recap, pos)) return false
  if (recapKey(recap) === dismissed) return false
  return recap.kind === 'series' || awayLongEnough(lastReadAtOnOpen, nowMs, afterMinutes)
}

// shouldRequestRecap: on opening a book, ask the backend for a recap when
// the reader would be offered one and none is ready for where they are -
// back after long enough, or starting a book that has one before it in its
// series. (The backend usually has it ready already; it prepares one
// whenever a book has been left alone for a while.)
export function shouldRequestRecap(
  recap: BookRecap | null | undefined,
  pos: RecapPos,
  book: { lastReadAt?: number; seriesIndex: number },
  nowMs: number,
  afterMinutes: number,
): boolean {
  if (afterMinutes < 0 || recapIsCurrent(recap, pos)) return false
  // Failed here already: left for the reader to ask again.
  if (recap && recap.text === '' && recap.chapterIdx === pos.chapterIdx) return false
  const startingSequel = !book.lastReadAt && book.seriesIndex > 1
  return startingSequel || awayLongEnough(book.lastReadAt, nowMs, afterMinutes)
}

// agoLabel says how long before nowMs a unix-seconds time was, roughly.
export function agoLabel(at: number, nowMs: number): string {
  const hours = Math.max(0, (nowMs / 1000 - at) / 3600)
  if (hours < 1) return 'just now'
  if (hours < 24) return `${Math.round(hours)} hour${Math.round(hours) === 1 ? '' : 's'} ago`
  const days = Math.round(hours / 24)
  if (days < 14) return `${days} day${days === 1 ? '' : 's'} ago`
  if (days < 60) return `${Math.round(days / 7)} weeks ago`
  return `${Math.round(days / 30)} months ago`
}

// One word of a spoken recap: where it is in its segment's text and when it
// is spoken, in seconds into the clip.
export interface RecapWord {
  word: string
  start: number // offset into the segment's text
  end: number
  time: number
}

// recapWords splits a segment's text into its words with the time each is
// spoken: the aligner's own when it has one per word, otherwise spread over
// the segment by position in the text (as ParagraphText does for a
// paragraph).
export function recapWords(segment: {
  text: string
  start: number
  end: number
  words: { start: number }[]
}): RecapWord[] {
  const out: RecapWord[] = []
  for (const m of segment.text.matchAll(/\S+/g)) {
    out.push({ word: m[0], start: m.index, end: m.index + m[0].length, time: 0 })
  }
  const aligned = segment.words.length === out.length
  const span = Math.max(0, segment.end - segment.start)
  out.forEach((w, i) => {
    w.time = aligned
      ? segment.words[i].start
      : segment.start + (segment.text.length ? (w.start / segment.text.length) * span : 0)
  })
  return out
}

// activeRecapWord is the index of the last word spoken by time t among
// times (ascending), -1 before the first.
export function activeRecapWord(times: number[], t: number): number {
  let idx = -1
  for (let i = 0; i < times.length && times[i] <= t; i++) idx = i
  return idx
}
