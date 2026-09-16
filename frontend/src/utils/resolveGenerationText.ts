import type { DirectionMark, PronunciationMark } from '../api/types'

// Client-side mirror of backend store.Paragraph.ResolveGenerationText
// (internal/deliverytags.Merge + internal/pronounce.Apply composed in one
// offset-ordered pass) - the exact string the backend hands the voice
// clone for this paragraph, reconstructed here purely from what the
// paragraph DTO already carries (directionMarks/pronunciationMarks are
// sent pre-resolved for whichever clone model currently narrates this
// paragraph - see Paragraph.directionMarks's own doc comment in
// api/types.ts) - no separate endpoint needed. Used by the annotations
// view's "show the text sent to the voice clone" toggle (ChapterSection's
// ParagraphGroup), so a reader/QA can verify exactly what's being spoken,
// tags and pronunciation substitutions included, not just the carets/
// strikeouts overlaid on the original text ParagraphText renders for
// on-screen reading.
//
// Ordering at a shared offset matches ParagraphText's own renderWithMarks:
// any direction-mark tag literals emit first, then a pronunciation
// substitution (if one starts there) replaces its own span of the
// original text - directionMarks arrives already in the right relative
// order for marks stacked at the same offset (see that field's own doc
// comment), so this only needs to get insertions-before-substitution
// right, not resort within a single offset itself.
export function resolveGenerationText(
  text: string,
  directionMarks: DirectionMark[] | undefined,
  pronunciationMarks: PronunciationMark[] | undefined,
): string {
  const marksByOffset = new Map<number, string[]>()
  for (const m of directionMarks ?? []) {
    const list = marksByOffset.get(m.offset)
    if (list) list.push(m.tag)
    else marksByOffset.set(m.offset, [m.tag])
  }
  const subByOffset = new Map<number, PronunciationMark>()
  for (const p of pronunciationMarks ?? []) subByOffset.set(p.offset, p)

  if (marksByOffset.size === 0 && subByOffset.size === 0) return text

  const offsets = [...new Set([...marksByOffset.keys(), ...subByOffset.keys(), text.length])].sort((a, b) => a - b)

  let result = ''
  let cursor = 0
  for (const offset of offsets) {
    if (offset > cursor) {
      result += text.slice(cursor, offset)
      cursor = offset
    }
    const tags = marksByOffset.get(offset)
    if (tags) result += tags.join('')
    const sub = subByOffset.get(offset)
    if (sub) {
      result += sub.replacement
      cursor = offset + sub.length
    }
  }
  if (cursor < text.length) result += text.slice(cursor)
  return result
}
