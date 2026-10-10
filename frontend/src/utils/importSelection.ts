import type { ImportChapterSource, ImportPreviewChapter } from '../api/types'
import { foldTitle } from './titleRule'

// Which chapters of an import preview are ticked: pure helpers for the
// chapter picker (components/ImportPreviewPanel.tsx).

// Whether a chapter starts out ticked: it can actually be read (a locked
// chapter stops an import at it) and doesn't read like an author's notice.
function tickedByDefault(c: ImportPreviewChapter): boolean {
  return c.source !== 'locked' && !c.suspect
}

// What is ticked when a preview first appears.
export function defaultSelection(chapters: ImportPreviewChapter[]): Set<string> {
  return new Set(chapters.filter(tickedByDefault).map((c) => c.id))
}

// Which entries waiting for review start out as "import": all but the
// ones that read like a notice, and chapters that couldn't be read
// (unavailable - importing those only tries again).
export function defaultReview(
  entries: { id: string; suspect: boolean; unavailable?: boolean }[],
): Set<string> {
  return new Set(entries.filter((e) => !e.suspect && !e.unavailable).map((e) => e.id))
}

// Splits the entries waiting at a book's sources into the ones to decide
// on and the ones the source lists but won't serve this reader (patrons
// only, early access): those show what is there, and aren't a decision -
// leaving one unticked must not skip it for good.
export function splitReview<E extends { locked?: boolean }>(
  entries: E[] | null | undefined,
): { open: E[]; locked: E[] } {
  const all = entries ?? []
  return { open: all.filter((e) => !e.locked), locked: all.filter((e) => e.locked) }
}

// Carries a selection over to a widened preview (the archive searched, an
// epub attached): what was ticked stays ticked, what was unticked stays
// unticked, and chapters that are new - or have just become readable -
// are ticked.
export function mergeSelection(
  before: ImportPreviewChapter[],
  selected: Set<string>,
  after: ImportPreviewChapter[],
): Set<string> {
  const was = new Map(before.map((c) => [c.id, c.source]))
  const next = new Set<string>()
  for (const c of after) {
    const old = was.get(c.id)
    if (old === undefined) {
      if (tickedByDefault(c)) next.add(c.id)
    } else if (selected.has(c.id) || (old === 'locked' && tickedByDefault(c))) {
      next.add(c.id)
    }
  }
  return next
}

// Exactly the chapters at positions from..to (1-based, inclusive, either
// order), clamped to the list.
export function selectRange(chapters: { id: string }[], from: number, to: number): Set<string> {
  const lo = Math.max(1, Math.min(from, to))
  const hi = Math.min(chapters.length, Math.max(from, to))
  return new Set(chapters.slice(lo - 1, hi).map((c) => c.id))
}

// The pattern a title filter is typed as: a regular expression, matched
// anywhere in a title and ignoring case. null for nothing typed or a
// pattern that doesn't parse.
export function titlePattern(text: string): RegExp | null {
  if (!text.trim()) return null
  try {
    return new RegExp(text, 'i')
  } catch {
    return null
  }
}

// The selection with every chapter whose title matches ticked (tick) or
// unticked; the others stay as they were.
export function selectMatching(
  chapters: { id: string; title: string }[],
  selected: Set<string>,
  pattern: RegExp,
  tick: boolean,
): Set<string> {
  const next = new Set(selected)
  for (const c of chapters) {
    if (!pattern.test(foldTitle(c.title))) continue
    if (tick) next.add(c.id)
    else next.delete(c.id)
  }
  return next
}

// How many chapters come from each place, among all of them or only the
// ticked ones.
export function countBySource(
  chapters: ImportPreviewChapter[],
  selected?: Set<string>,
): Record<ImportChapterSource, number> {
  const counts: Record<ImportChapterSource, number> = {
    site: 0,
    archive: 0,
    epub: 0,
    missing: 0,
    locked: 0,
  }
  for (const c of chapters) {
    if (!selected || selected.has(c.id)) counts[c.source]++
  }
  return counts
}
