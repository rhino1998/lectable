import type { BookSummary } from '../api/types'

type Book = Pick<BookSummary, 'id' | 'finished' | 'lastReadAt' | 'seriesName' | 'seriesIndex'>

// The library's "Continue reading" row: unfinished books that are being
// read, and the next book of a series - an unfinished one whose
// predecessor in the series is finished - whether or not it has been
// opened. Most recent first: a book counts from when it was last read, a
// next book also from when the one before it was (so it turns up as that
// one is finished). Not shown for a one-book library.
export function continueReading<B extends Book>(books: B[], limit: number): B[] {
  if (books.length < 2) return []
  const when = new Map<string, number>()
  for (const b of books) {
    if (!b.finished && b.lastReadAt) when.set(b.id, b.lastReadAt)
  }
  const series = new Map<string, B[]>()
  for (const b of books) {
    if (!b.seriesName) continue
    series.set(b.seriesName, [...(series.get(b.seriesName) ?? []), b])
  }
  for (const inSeries of series.values()) {
    const ordered = [...inSeries].sort((a, b) => (a.seriesIndex ?? 0) - (b.seriesIndex ?? 0))
    for (let i = 1; i < ordered.length; i++) {
      const before = ordered[i - 1]
      const next = ordered[i]
      if (!before.finished || next.finished) continue
      when.set(next.id, Math.max(when.get(next.id) ?? 0, before.lastReadAt ?? 0))
    }
  }
  return books
    .filter((b) => when.has(b.id))
    .sort((a, b) => (when.get(b.id) ?? 0) - (when.get(a.id) ?? 0))
    .slice(0, limit)
}
