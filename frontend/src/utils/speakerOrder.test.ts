import { describe, expect, it } from 'vitest'
import type { ChapterDetail, Speaker } from '../api/types'
import { chapterCharacterNames, pickerSections, sortedRoster } from './speakerOrder'

const sp = (name: string, id = name.toLowerCase()) => ({ id, name }) as Speaker

describe('sortedRoster', () => {
  it('puts the Narrator row first, then everyone alphabetically', () => {
    const got = sortedRoster([sp('zed'), sp('Narrator', ''), sp('alice'), sp('Bob')])
    expect(got.map((s) => s.name)).toEqual(['Narrator', 'alice', 'Bob', 'zed'])
  })
})

describe('pickerSections', () => {
  it('lists chapter names first, then the whole roster, alphabetically', () => {
    const roster = ['Zed', 'Narrator', 'Unknown', 'amy', 'Bob', 'Cal', 'Bob']
    expect(pickerSections(roster, new Set(['Zed', 'Bob', 'Ghost']))).toEqual({
      local: ['Bob', 'Zed'],
      all: ['amy', 'Bob', 'Cal', 'Zed'],
    })
    expect(pickerSections(roster, new Set(['Zed', 'Bob']), 'Bob')).toEqual({
      local: ['Zed'],
      all: ['amy', 'Cal', 'Zed'],
    })
  })
})

describe('chapterCharacterNames', () => {
  it('collects speakers and described characters, not the narrator', () => {
    const chapter = {
      paragraphs: [
        { speaker: 'Nyx' },
        { speaker: 'Narrator' },
        { speaker: '', describesCharacters: ['Aeris'] },
      ],
    } as ChapterDetail
    expect([...chapterCharacterNames(chapter)].sort()).toEqual(['Aeris', 'Nyx'])
    expect(chapterCharacterNames(undefined).size).toBe(0)
  })
})
