import type { ImportChapterSource, ImportPreviewChapter } from '../api/types'

// Which chapters of an import preview are ticked: pure helpers for the
// chapter picker (components/ImportPreviewPanel.tsx).

// What is ticked when a preview first appears: every chapter that can
// actually be read. A locked chapter stops an import at it, so it starts
// unticked.
export function defaultSelection(chapters: ImportPreviewChapter[]): Set<string> {
  return new Set(chapters.filter((c) => c.source !== 'locked').map((c) => c.id))
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
      if (c.source !== 'locked') next.add(c.id)
    } else if (selected.has(c.id) || (old === 'locked' && c.source !== 'locked')) {
      next.add(c.id)
    }
  }
  return next
}

// Exactly the chapters at positions from..to (1-based, inclusive, either
// order), clamped to the list.
export function selectRange(
  chapters: ImportPreviewChapter[],
  from: number,
  to: number,
): Set<string> {
  const lo = Math.max(1, Math.min(from, to))
  const hi = Math.min(chapters.length, Math.max(from, to))
  return new Set(chapters.slice(lo - 1, hi).map((c) => c.id))
}

// How many chapters come from each place, among all of them or only the
// ticked ones.
export function countBySource(
  chapters: ImportPreviewChapter[],
  selected?: Set<string>,
): Record<ImportChapterSource, number> {
  const counts: Record<ImportChapterSource, number> = { site: 0, archive: 0, epub: 0, locked: 0 }
  for (const c of chapters) {
    if (!selected || selected.has(c.id)) counts[c.source]++
  }
  return counts
}
