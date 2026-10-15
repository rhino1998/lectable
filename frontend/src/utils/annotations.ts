import { ANNOTATION_LABELS } from '../api/types'
import type { AnnotationKind, Paragraph } from '../api/types'
import { formatDirectionTag } from './directionTags'
import { emotionLabel } from './emotions'

// Which narration role a paragraph segment plays - dialogue
// (isQuote) takes priority regardless of whether it's been attributed to a
// named speaker yet (an unattributed quote is still dialogue, not
// narration - see Paragraph.isQuote's own doc comment) UNLESS it's been
// flagged a scare quote: despite being structurally an IsQuote span, it
// isn't actually spoken aloud (see backend store.Paragraph.ScareQuote), so
// it colors and narrates as plain narrator text - the dashed
// .annotation-scare-quote underline (ChapterSection) is what still marks
// it as a flagged quote, layered on top of that narrator color rather than
// the speaker one. Everything else is narration. Depends only on structural/attribution
// fields set at import/attribution time, never on audioStatus - so a
// segment gets colored the same whether or not its audio has been
// generated yet.
export function annotationKind(p: Paragraph): AnnotationKind {
  if (p.isQuote && !p.scareQuote) return 'speaker'
  return 'narrator'
}

// Hover text for one segment's annotation highlight - names the actual
// character where one's known, rather than just the generic role label
// ANNOTATION_LABELS alone would give: "Speaker" tells you it's dialogue,
// "Speaker: Jake" tells you whose. The line's emotion and any direction
// tags (pauses) are appended regardless of kind - they're an orthogonal
// "how it's delivered" fact, not a narration-role classification.
export function annotationTitle(p: Paragraph): string {
  const kind = annotationKind(p)
  let base: string
  if (kind === 'speaker') {
    // p.speaker is "" until attribution runs (or "Narrator" for a quote
    // attributed to the book's own first-person narrator) - still dialogue
    // either way, just not yet (or never) tied to a named character.
    base = p.speaker || 'Not yet attributed'
  } else {
    base = ANNOTATION_LABELS.narrator
  }
  if (p.scareQuote) {
    base += ' · Scare quote'
  }
  if (p.emotion) {
    base += ` · ${emotionLabel(p.emotion)}`
  }
  if (p.delivery) {
    // A directed line: its style and tags, readably.
    const parts = [
      p.delivery.style && `(${p.delivery.style})`,
      ...(p.delivery.marks ?? []).map((m) => `[${m.tag}]`),
      ...(p.delivery.spans ?? []).map((s) => `<${s.tag}>`),
    ].filter(Boolean)
    return parts.length > 0 ? `${base} · ${parts.join(' ')}` : base
  }
  const tags = p.directionMarks ?? []
  return tags.length > 0
    ? `${base} · ${tags.map((m) => formatDirectionTag(m.tag)).join(', ')}`
    : base
}

// Whether a segment counts as one of `selected`'s own lines, for the
// speaker-list "select to highlight" feature - "Narrator" is a sentinel
// matching both "" (never attributed) and the literal "Narrator" value,
// same normalization store.Paragraph.Speaker's own doc comment describes,
// since both narrate in the book's own voice and a reader picking
// "Narrator" from the list means "the book's own voice", not literally
// the string.
export function matchesSelectedSpeaker(p: Paragraph, selected: string): boolean {
  if (selected === 'Narrator') return !p.speaker || p.speaker === 'Narrator'
  return p.speaker === selected
}
