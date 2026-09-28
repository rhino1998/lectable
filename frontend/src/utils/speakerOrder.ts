import type { ChapterDetail, Speaker } from '../api/types'

const byName = (a: string, b: string) => a.localeCompare(b, undefined, { sensitivity: 'base' })

// The roster in display order: the Narrator row first, then everyone else
// alphabetically (case-insensitive) rather than the backend's appearance
// order, so a name is easy to find. Mirrors android's sortedRoster.
export function sortedRoster(speakers: Speaker[]): Speaker[] {
  const isNarrator = (s: Speaker) => !s.id && s.name === 'Narrator'
  return [...speakers].sort((a, b) =>
    isNarrator(a) !== isNarrator(b) ? (isNarrator(a) ? -1 : 1) : byName(a.name, b.name),
  )
}

// Everyone who speaks or is described anywhere in chapter - the reassign
// menu's "This chapter" section, since a misattributed line or description
// usually belongs to someone already in the scene. Narrator/blank excluded.
// Mirrors android's chapterCharacterNames.
export function chapterCharacterNames(chapter: ChapterDetail | undefined): Set<string> {
  const out = new Set<string>()
  for (const p of chapter?.paragraphs ?? []) {
    if (p.speaker && p.speaker !== 'Narrator') out.add(p.speaker)
    for (const name of p.describesCharacters ?? []) out.add(name)
  }
  return out
}

// A reassign picker's two sections: the roster names in chapterNames
// (local), then the whole roster (all) - both alphabetical, Narrator/blank
// and exclude left out. Mirrors android's pickerSections.
export function pickerSections(
  roster: string[],
  chapterNames: Set<string>,
  exclude?: string,
): { local: string[]; all: string[] } {
  const all = [...new Set(roster)]
    .filter((n) => n !== '' && n !== 'Narrator' && n !== exclude)
    .sort(byName)
  return { local: all.filter((n) => chapterNames.has(n)), all }
}
