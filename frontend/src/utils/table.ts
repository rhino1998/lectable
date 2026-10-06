import type { DirectionMark, Paragraph, PronunciationMark, Table } from '../api/types'

// One table cell's span of its paragraph's text, as string indexes.
export interface CellRange {
  start: number
  end: number
  col: number
  colSpan: number
  header: boolean
  label: string
}

// The backend counts a cell's offsets in code points (see TableCell);
// strings index by UTF-16 unit, which only differs past the first
// astral character (an emoji in a stat block).
function codePointIndexes(text: string): (offset: number) => number {
  if (!/[\uD800-\uDFFF]/.test(text)) return (offset) => offset
  const starts: number[] = []
  let i = 0
  for (const ch of text) {
    starts.push(i)
    i += ch.length
  }
  starts.push(i)
  return (offset) => starts[Math.min(offset, starts.length - 1)]
}

export function cellRanges(text: string, table: Table): CellRange[] {
  const at = codePointIndexes(text)
  return table.cells.map((c) => ({
    start: at(c.start),
    end: at(c.end),
    col: c.col,
    colSpan: Math.max(c.colSpan, 1),
    header: !!c.header,
    label: c.label ?? '',
  }))
}

// How many grid columns a table paragraph's cells reach.
export function tableColumnEnd(table: Table): number {
  return table.cells.reduce((end, c) => Math.max(end, c.col + Math.max(c.colSpan, 1)), 0)
}

// How narration reads a table row, as the marks resolveGenerationText
// applies - the mirror of backend store.TablePos.spoken: each separator
// between cells becomes a sentence break, and a cell under a heading row
// is introduced by its column's heading.
export function tableGenerationMarks(p: Paragraph): {
  directionMarks: DirectionMark[]
  pronunciationMarks: PronunciationMark[]
} {
  const directionMarks = [...(p.directionMarks ?? [])]
  const pronunciationMarks = [...(p.pronunciationMarks ?? [])]
  if (!p.table) return { directionMarks, pronunciationMarks }
  let prevEnd = 0
  cellRanges(p.text, p.table).forEach((c, i) => {
    if (i > 0) {
      const original = p.text.slice(prevEnd, c.start)
      const endsSentence = '.!?:;,…'.includes(p.text.charAt(prevEnd - 1))
      pronunciationMarks.push({
        offset: prevEnd,
        length: original.length,
        original,
        replacement: endsSentence ? ' ' : '. ',
      })
    }
    if (c.label && c.end > c.start) directionMarks.push({ offset: c.start, tag: `${c.label}: ` })
    prevEnd = c.end
  })
  return { directionMarks, pronunciationMarks }
}
