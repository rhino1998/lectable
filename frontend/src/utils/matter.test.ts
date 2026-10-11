import { describe, expect, it } from 'vitest'
import { nextStoryChapter, readingProgress } from './matter'

const book = [
  { idx: 0, matter: true, paragraphCount: 1 },
  { idx: 1, matter: true, paragraphCount: 40 },
  { idx: 2, matter: false, paragraphCount: 10 },
  { idx: 3, matter: true, paragraphCount: 5 },
  { idx: 4, matter: false, paragraphCount: 10 },
  { idx: 5, matter: true, paragraphCount: 3 },
]

describe('nextStoryChapter', () => {
  it('steps over matter', () => {
    expect(nextStoryChapter(book, 0)).toBe(2)
    expect(nextStoryChapter(book, 2)).toBe(4)
  })
  it('is past the end after the last story chapter', () => {
    expect(nextStoryChapter(book, 4)).toBe(6)
    expect(nextStoryChapter(book, 5)).toBe(6)
  })
  it('is the next chapter before the list has loaded', () => {
    expect(nextStoryChapter([], 3)).toBe(4)
  })
})

describe('readingProgress', () => {
  it('counts story chapters only', () => {
    expect(readingProgress(book, 2, 0)).toEqual({ percent: 0, finished: false })
    expect(readingProgress(book, 4, 0)).toEqual({ percent: 50, finished: false })
    // In matter between chapters: what is behind it is read.
    expect(readingProgress(book, 3, 2)).toEqual({ percent: 50, finished: false })
  })
  it('ends with the last story chapter', () => {
    expect(readingProgress(book, 4, 9).finished).toBe(true)
    expect(readingProgress(book, 5, 0)).toEqual({ percent: 100, finished: true })
  })
  it('is the old sum for a book with no matter', () => {
    const plain = book.map((c) => ({ ...c, matter: false }))
    expect(readingProgress(plain, 1, 20).percent).toBeCloseTo((21 / 69) * 100)
    expect(readingProgress(plain, 5, 2).finished).toBe(true)
  })
})
