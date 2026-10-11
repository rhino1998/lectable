// Front and back matter (ChapterSummary.matter): chapters that aren't story
// - a title page, the copyright page, "About the Author". Playback steps
// over them and reading progress leaves them out; a reader can still open
// one and have it read.

interface MatterChapter {
  idx: number
  matter: boolean
  paragraphCount: number
}

// The chapter playback goes on to after chapterIdx: the next one that isn't
// matter. One past the last chapter when there is none - the book is over.
export function nextStoryChapter(chapters: MatterChapter[], chapterIdx: number): number {
  for (const c of chapters) {
    if (c.idx > chapterIdx && !c.matter) return c.idx
  }
  // Before the book's chapter list has loaded there is nothing to skip.
  return chapters.length === 0 ? chapterIdx + 1 : chapters.length
}

// How far through the book a position is, and whether that is its end.
// Mirrors the backend's toBookSummary exactly (matter counts for nothing;
// the book ends with its last story chapter), so the live figure in the
// player and the saved one in the library agree.
export function readingProgress(
  chapters: MatterChapter[],
  chapterIdx: number,
  paragraphIdx: number,
): { percent: number; finished: boolean } {
  let lastStory = -1
  chapters.forEach((c, i) => {
    if (!c.matter) lastStory = i
  })
  let total = 0
  let completed = 0
  let finished = false
  chapters.forEach((c, i) => {
    if (c.matter) {
      if (lastStory >= 0 && i > lastStory && c.idx <= chapterIdx) finished = true
      return
    }
    total += c.paragraphCount
    if (c.idx < chapterIdx) {
      completed += c.paragraphCount
    } else if (c.idx === chapterIdx) {
      completed += Math.min(paragraphIdx, c.paragraphCount)
      if (i === lastStory && paragraphIdx >= c.paragraphCount - 1) finished = true
    }
  })
  const percent = total > 0 ? Math.min(100, (completed / total) * 100) : 0
  return { percent, finished }
}
