import { describe, expect, it } from 'vitest'
import type { ImportPreviewChapter } from '../api/types'
import {
  countBySource,
  defaultReview,
  defaultSelection,
  mergeSelection,
  selectMatching,
  selectRange,
  titlePattern,
} from './importSelection'

const ch = (
  id: string,
  source: ImportPreviewChapter['source'],
  suspect = false,
): ImportPreviewChapter => ({
  id,
  title: `Chapter ${id}`,
  source,
  suspect,
})

describe('import selection', () => {
  const live = [ch('1', 'site'), ch('9', 'site'), ch('10', 'locked')]

  it('starts with everything readable ticked', () => {
    expect([...defaultSelection(live)]).toEqual(['1', '9'])
  })

  it('keeps choices and ticks what is new when the list widens', () => {
    const selected = new Set(['9']) // the reader unticked chapter 1
    const wide = [
      ch('1', 'site'),
      ch('2', 'archive'),
      ch('3', 'archive'),
      ch('9', 'site'),
      ch('10', 'epub'),
    ]
    // 2 and 3 are new; 10 was locked and the epub now has it.
    expect([...mergeSelection(live, selected, wide)]).toEqual(['2', '3', '9', '10'])
  })

  it('leaves a new locked chapter unticked', () => {
    expect([...mergeSelection(live, new Set(['1']), [...live, ch('11', 'locked')])]).toEqual(['1'])
  })

  it('leaves what reads like a notice unticked, at first and when it appears', () => {
    const withNotice = [...live, ch('12', 'site', true)]
    expect([...defaultSelection(withNotice)]).toEqual(['1', '9'])
    expect([...mergeSelection(live, new Set(['1']), withNotice)]).toEqual(['1'])
    expect([...defaultReview([ch('1', 'site'), ch('2', 'site', true)])]).toEqual(['1'])
    expect([...defaultReview([{ id: '3', suspect: false, unavailable: true }])]).toEqual([])
  })

  it('ticks and unticks by a pattern over titles', () => {
    const chapters = [
      { ...ch('1', 'site'), title: 'Chapter 1: Out' },
      { ...ch('2', 'site'), title: 'Poll: what next?' },
      { ...ch('3', 'site'), title: 'chapter 2: Inn' },
    ]
    const chapter = titlePattern('^chapter \\d')!
    expect([...selectMatching(chapters, new Set(['2']), chapter, true)]).toEqual(['2', '1', '3'])
    expect([...selectMatching(chapters, new Set(['1', '2', '3']), chapter, false)]).toEqual(['2'])
    expect(titlePattern('  ')).toBeNull()
    expect(titlePattern('chapter (')).toBeNull()
  })

  it('selects exactly a range, clamped and in either order', () => {
    expect([...selectRange(live, 2, 3)]).toEqual(['9', '10'])
    expect([...selectRange(live, 3, 2)]).toEqual(['9', '10'])
    expect([...selectRange(live, 0, 99)]).toEqual(['1', '9', '10'])
    expect([...selectRange(live, 5, 9)]).toEqual([])
  })

  it('counts by source, optionally only the ticked', () => {
    expect(countBySource(live)).toEqual({ site: 2, archive: 0, epub: 0, missing: 0, locked: 1 })
    expect(countBySource(live, new Set(['1']))).toEqual({
      site: 1,
      archive: 0,
      epub: 0,
      missing: 0,
      locked: 0,
    })
  })
})
