import { describe, expect, it } from 'vitest'
import { continueReading } from './continueReading'

const book = (
  id: string,
  opts: { finished?: boolean; lastReadAt?: number; series?: string; index?: number } = {},
) => ({
  id,
  finished: !!opts.finished,
  lastReadAt: opts.lastReadAt,
  seriesName: opts.series,
  seriesIndex: opts.index,
})

const ids = (books: { id: string }[]) => books.map((b) => b.id).join(',')

describe('continueReading', () => {
  it('lists unfinished books being read, latest first', () => {
    const books = [
      book('old', { lastReadAt: 10 }),
      book('new', { lastReadAt: 30 }),
      book('done', { lastReadAt: 40, finished: true }),
      book('unopened'),
    ]
    expect(ids(continueReading(books, 10))).toBe('new,old')
    expect(ids(continueReading(books, 1))).toBe('new')
    expect(continueReading([book('only', { lastReadAt: 5 })], 10)).toEqual([])
  })

  it('adds the next book of a series once the one before it is finished', () => {
    const books = [
      // Listed out of order: the series' own numbering decides.
      book('s3', { series: 'Saga', index: 3 }),
      book('s1', { series: 'Saga', index: 1, finished: true, lastReadAt: 20 }),
      book('s2', { series: 'Saga', index: 2, finished: true, lastReadAt: 50 }),
      book('s4', { series: 'Saga', index: 4 }),
      book('reading', { lastReadAt: 40 }),
      // Its first book isn't finished, so its second isn't next.
      book('t1', { series: 'Trilogy', index: 1, lastReadAt: 5 }),
      book('t2', { series: 'Trilogy', index: 2 }),
    ]
    // s3 counts from when s2 was finished; s4 waits for s3.
    expect(ids(continueReading(books, 10))).toBe('s3,reading,t1')
  })

  it('keeps a next book that was already opened where its own reading puts it', () => {
    const books = [
      book('a1', { series: 'A', index: 1, finished: true, lastReadAt: 10 }),
      book('a2', { series: 'A', index: 2, lastReadAt: 90 }),
      book('other', { lastReadAt: 50 }),
      // Marked read without ever being opened: its next book still shows, last.
      book('b1', { series: 'B', index: 1, finished: true }),
      book('b2', { series: 'B', index: 2 }),
    ]
    expect(ids(continueReading(books, 10))).toBe('a2,other,b2')
  })
})
