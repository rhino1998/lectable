import { describe, expect, it } from 'vitest'
import type { Paragraph } from '../api/types'
import { resolveGenerationText } from './resolveGenerationText'
import { cellRanges, tableColumnEnd, tableGenerationMarks } from './table'

function row(text: string, labels: string[] = []): Paragraph {
  const cells = []
  let start = 0
  const parts = text.split(' | ')
  for (let i = 0; i < parts.length; i++) {
    const length = [...parts[i]].length
    cells.push({ start, end: start + length, col: i, colSpan: 1, rowSpan: 1, label: labels[i] })
    start += length + 3
  }
  return { text, table: { table: 0, row: 1, cells } } as unknown as Paragraph
}

function spoken(p: Paragraph): string {
  const marks = tableGenerationMarks(p)
  return resolveGenerationText(p.text, marks.directionMarks, marks.pronunciationMarks)
}

describe('table rows', () => {
  it('reads cells as sentences introduced by their headings', () => {
    expect(spoken(row('Rare | Yes | 72/100', ['Rarity', 'Magical', 'Durability']))).toBe(
      'Rarity: Rare. Magical: Yes. Durability: 72/100',
    )
    expect(spoken(row('Class: None | Renown: Unknown'))).toBe('Class: None. Renown: Unknown')
    expect(spoken(row('Details: | Heals twice. | Rare'))).toBe('Details: Heals twice. Rare')
  })

  it('maps code point offsets onto string indexes', () => {
    const p = row('🔥 Fire | 10 mana')
    const cells = cellRanges(p.text, p.table!)
    expect(cells.map((c) => p.text.slice(c.start, c.end))).toEqual(['🔥 Fire', '10 mana'])
    expect(tableColumnEnd(p.table!)).toBe(2)
  })
})
