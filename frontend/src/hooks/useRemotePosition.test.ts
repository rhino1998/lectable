import { describe, expect, it } from 'vitest'
import { remotePosition } from './useRemotePosition'

const pos = (chapterIdx: number, paragraphIdx: number) => ({ chapterIdx, paragraphIdx })

describe('remotePosition', () => {
  it('ignores the reader’s own position and its own (possibly late) saves', () => {
    expect(remotePosition(undefined, pos(0, 0), [], null)).toBeNull()
    expect(remotePosition(pos(2, 5), pos(2, 5), [], null)).toBeNull()
    expect(remotePosition(pos(2, 4), pos(2, 6), ['2:4', '2:5'], null)).toBeNull()
  })
  it('reports a position moved elsewhere until dismissed', () => {
    expect(remotePosition(pos(9, 1), pos(2, 6), ['2:4'], null)).toEqual(pos(9, 1))
    expect(remotePosition(pos(9, 1), pos(2, 6), [], '9:1')).toBeNull()
    expect(remotePosition(pos(9, 2), pos(2, 6), [], '9:1')).toEqual(pos(9, 2))
  })
})
