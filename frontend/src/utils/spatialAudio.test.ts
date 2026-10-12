import { describe, expect, it } from 'vitest'
import type { ChapterDetail } from '../api/types'
import { paragraphPan } from './spatialAudio'

function chapter(...speakers: (string | [string, number])[]): ChapterDetail {
  return {
    paragraphs: speakers.map((s, idx) =>
      typeof s === 'string' ? { idx, speaker: s } : { idx, speaker: s[0], speakerIndex: s[1] },
    ),
  } as unknown as ChapterDetail
}

describe('paragraphPan', () => {
  const ch = chapter('', 'Narrator', 'Jonah', 'Castle', 'Mona', 'Castle', 'Frank')

  it('gives narration and the first-person narrator no seat', () => {
    expect(paragraphPan(ch, 0, 'Jonah', 1)).toBeNull()
    expect(paragraphPan(ch, 1, 'Jonah', 1)).toBeNull()
    expect(paragraphPan(ch, 2, 'Jonah', 1)).toBeNull()
    expect(paragraphPan(ch, 99, 'Jonah', 1)).toBeNull()
    expect(paragraphPan(undefined, 0, 'Jonah', 1)).toBeNull()
  })
  it('seats speakers by first line, the first two on opposite sides', () => {
    expect(paragraphPan(ch, 3, 'Jonah', 1)).toBe(-0.5)
    expect(paragraphPan(ch, 4, 'Jonah', 1)).toBe(0.5)
    expect(paragraphPan(ch, 5, 'Jonah', 1)).toBe(-0.5)
    expect(paragraphPan(ch, 6, 'Jonah', 1)).toBe(-0.25)
  })
  it('seats a third-person book’s own lead like anyone else', () => {
    expect(paragraphPan(ch, 2, '', 1)).toBe(-0.5)
    expect(paragraphPan(ch, 3, '', 1)).toBe(0.5)
  })
  it('scales by width', () => {
    expect(paragraphPan(ch, 3, 'Jonah', 0.5)).toBe(-0.25)
    expect(paragraphPan(ch, 4, 'Jonah', 2)).toBe(1)
    expect(paragraphPan(ch, 4, 'Jonah', 0)).toBe(0)
  })
  it('tells numbered members of a role apart', () => {
    const guards = chapter(['Guard', 1], ['Guard', 2], ['Guard', 1])
    expect(paragraphPan(guards, 0, '', 1)).toBe(-0.5)
    expect(paragraphPan(guards, 1, '', 1)).toBe(0.5)
    expect(paragraphPan(guards, 2, '', 1)).toBe(-0.5)
  })
})
