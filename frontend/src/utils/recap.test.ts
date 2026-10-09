import { describe, expect, it } from 'vitest'
import type { BookRecap } from '../api/types'
import {
  RECAP_NEVER,
  activeRecapWord,
  recapWords,
  shouldHoldForRecap,
  agoLabel,
  recapIsCurrent,
  recapKey,
  shouldOfferRecap,
  shouldRequestRecap,
} from './recap'

const recap = (over: Partial<BookRecap> = {}): BookRecap => ({
  kind: 'position',
  chapterIdx: 4,
  paragraphIdx: 10,
  throughIdx: 50,
  title: 'Book',
  text: 'So far.',
  segments: [],
  readAt: 0,
  updatedAt: 1,
  ...over,
})

const HOUR = 3600
const DAY = 24 * 60 // minutes
const now = 1_000_000_000_000 // ms
const nowS = now / 1000

describe('recapIsCurrent', () => {
  it('covers the place it was written for and a little past it', () => {
    expect(recapIsCurrent(recap(), { chapterIdx: 4, paragraphIdx: 10 })).toBe(true)
    expect(recapIsCurrent(recap(), { chapterIdx: 4, paragraphIdx: 50 })).toBe(true)
  })
  it('is stale behind it, far past it, in another chapter, or failed', () => {
    expect(recapIsCurrent(recap(), { chapterIdx: 4, paragraphIdx: 9 })).toBe(false)
    expect(recapIsCurrent(recap(), { chapterIdx: 4, paragraphIdx: 51 })).toBe(false)
    expect(recapIsCurrent(recap(), { chapterIdx: 5, paragraphIdx: 10 })).toBe(false)
    expect(recapIsCurrent(recap({ text: '' }), { chapterIdx: 4, paragraphIdx: 10 })).toBe(false)
    expect(recapIsCurrent(null, { chapterIdx: 4, paragraphIdx: 10 })).toBe(false)
  })
})

describe('shouldOfferRecap', () => {
  const pos = { chapterIdx: 4, paragraphIdx: 12 }
  it('offers after long enough away', () => {
    expect(shouldOfferRecap(recap(), pos, nowS - 30 * HOUR, now, DAY, null)).toBe(true)
    expect(shouldOfferRecap(recap(), pos, nowS - 2 * HOUR, now, DAY, null)).toBe(false)
  })
  it('offers every time when set to immediately', () => {
    expect(shouldOfferRecap(recap(), pos, nowS - 60, now, 0, null)).toBe(true)
  })
  it('never offers when turned off, turned down, or stale', () => {
    expect(shouldOfferRecap(recap(), pos, nowS - 30 * HOUR, now, RECAP_NEVER, null)).toBe(false)
    expect(shouldOfferRecap(recap(), pos, nowS - 30 * HOUR, now, DAY, recapKey(recap()))).toBe(
      false,
    )
    expect(
      shouldOfferRecap(
        recap(),
        { chapterIdx: 6, paragraphIdx: 0 },
        nowS - 30 * HOUR,
        now,
        24,
        null,
      ),
    ).toBe(false)
  })
  it('offers the previous book to a reader starting a sequel, however recently', () => {
    const series = recap({ kind: 'series', chapterIdx: 0, paragraphIdx: 0, throughIdx: 8 })
    expect(
      shouldOfferRecap(series, { chapterIdx: 0, paragraphIdx: 0 }, undefined, now, DAY, null),
    ).toBe(true)
  })
})

describe('shouldRequestRecap', () => {
  const pos = { chapterIdx: 4, paragraphIdx: 12 }
  it('asks when back after a while with nothing ready', () => {
    const book = { lastReadAt: nowS - 30 * HOUR, seriesIndex: 0 }
    expect(shouldRequestRecap(null, pos, book, now, DAY)).toBe(true)
    expect(shouldRequestRecap(recap({ chapterIdx: 2 }), pos, book, now, DAY)).toBe(true)
    expect(shouldRequestRecap(recap(), pos, book, now, DAY)).toBe(false)
    expect(shouldRequestRecap(null, pos, { ...book, lastReadAt: nowS - HOUR }, now, DAY)).toBe(
      false,
    )
    expect(shouldRequestRecap(null, pos, book, now, RECAP_NEVER)).toBe(false)
  })
  it('asks for a sequel never opened', () => {
    const start = { chapterIdx: 0, paragraphIdx: 0 }
    expect(shouldRequestRecap(null, start, { seriesIndex: 3 }, now, DAY)).toBe(true)
    expect(shouldRequestRecap(null, start, { seriesIndex: 1 }, now, DAY)).toBe(false)
    expect(shouldRequestRecap(null, start, { seriesIndex: 0 }, now, DAY)).toBe(false)
  })
  it('leaves a recap that failed here to be asked for', () => {
    const book = { lastReadAt: nowS - 30 * HOUR, seriesIndex: 0 }
    expect(shouldRequestRecap(recap({ text: '', error: 'x' }), pos, book, now, DAY)).toBe(false)
  })
})

describe('agoLabel', () => {
  it('rounds to a natural unit', () => {
    expect(agoLabel(nowS - 60, now)).toBe('just now')
    expect(agoLabel(nowS - 5 * HOUR, now)).toBe('5 hours ago')
    expect(agoLabel(nowS - 26 * HOUR, now)).toBe('1 day ago')
    expect(agoLabel(nowS - 72 * HOUR, now)).toBe('3 days ago')
    expect(agoLabel(nowS - 21 * 24 * HOUR, now)).toBe('3 weeks ago')
  })
})

describe('recapWords', () => {
  it("uses the aligner's times when it has one per word", () => {
    const words = recapWords({
      text: 'Previously, in Book.',
      start: 2,
      end: 4,
      words: [{ start: 2.1 }, { start: 2.6 }, { start: 3 }],
    })
    expect(words.map((w) => w.word)).toEqual(['Previously,', 'in', 'Book.'])
    expect(words.map((w) => w.time)).toEqual([2.1, 2.6, 3])
    expect(words[1]).toMatchObject({ start: 12, end: 14 })
  })
  it('spreads the words over the segment otherwise', () => {
    const words = recapWords({ text: 'ab cd', start: 10, end: 15, words: [] })
    expect(words.map((w) => w.time)).toEqual([10, 13])
  })
})

describe('activeRecapWord', () => {
  it('is the last word begun', () => {
    expect(activeRecapWord([1, 2, 3], 0.5)).toBe(-1)
    expect(activeRecapWord([1, 2, 3], 2)).toBe(1)
    expect(activeRecapWord([1, 2, 3], 9)).toBe(2)
  })
})

describe('shouldHoldForRecap', () => {
  const pos = { chapterIdx: 4, paragraphIdx: 12 }
  const away = { lastReadAt: nowS - 30 * HOUR, seriesIndex: 0 }
  it('holds for a recap that is ready or on its way', () => {
    expect(shouldHoldForRecap(recap(), pos, away, now, DAY, null, false)).toBe(true)
    expect(shouldHoldForRecap(null, pos, away, now, DAY, null, true)).toBe(true)
    expect(shouldHoldForRecap(recap({ chapterIdx: 2 }), pos, away, now, DAY, null, true)).toBe(true)
    expect(
      shouldHoldForRecap(
        null,
        { chapterIdx: 0, paragraphIdx: 0 },
        { seriesIndex: 2 },
        now,
        DAY,
        null,
        true,
      ),
    ).toBe(true)
  })
  it('does not hold with nothing on its way, soon after, turned down, failed, or never', () => {
    expect(shouldHoldForRecap(null, pos, away, now, DAY, null, false)).toBe(false)
    const recent = { lastReadAt: nowS - HOUR, seriesIndex: 0 }
    expect(shouldHoldForRecap(null, pos, recent, now, DAY, null, true)).toBe(false)
    expect(shouldHoldForRecap(recap(), pos, away, now, DAY, recapKey(recap()), true)).toBe(false)
    expect(shouldHoldForRecap(recap({ text: '' }), pos, away, now, DAY, null, true)).toBe(false)
    expect(shouldHoldForRecap(recap(), pos, away, now, RECAP_NEVER, null, true)).toBe(false)
  })
})
