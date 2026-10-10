import { useRef, useState } from 'react'
import { ApiError } from '../api/client'
import { useBackfillBookSource, useListEpubChapters, useMatchEpubChapter } from '../api/queries'
import type { BookSource, ChapterSummary, EpubChapter } from '../api/types'

// Matching an epub's chapters to the book's by hand (Book settings →
// Chapters), for when titles and text can't settle it: a chapter that was
// filled from the wrong epub chapter is replaced in place
// (api.matchEpubChapter - same title and position, so nothing else in the
// book moves), and for a book imported from a website the first chapter it
// is missing can be pointed at the epub chapter it is, the rest following
// on from there (api.backfillBookSource's `first`).
//
// After a replacement both pickers step on by one, so a run of chapters
// that are all off by the same amount is a few clicks each.

// The "book chapter" picker's value for the first chapter the book lacks.
const NEXT_MISSING = -1

function label(c: EpubChapter): string {
  return `${c.index + 1}. ${c.title || '(untitled)'}`
}

export function EpubMatchPanel(props: {
  bookId: string
  chapters: ChapterSummary[]
  source?: BookSource | null
}) {
  const { bookId, chapters, source } = props
  const list = useListEpubChapters(bookId)
  const match = useMatchEpubChapter(bookId)
  const backfill = useBackfillBookSource()
  const inputRef = useRef<HTMLInputElement>(null)
  const [file, setFile] = useState<File | null>(null)
  const [epub, setEpub] = useState<EpubChapter[]>([])
  const [bookIdx, setBookIdx] = useState(0)
  const [epubIdx, setEpubIdx] = useState(0)
  const [note, setNote] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  const fail = (fallback: string) => (err: unknown) =>
    setError(err instanceof ApiError ? err.message : fallback)

  // The epub chapter that opens the way book chapter idx does, if any.
  const sameText = (all: EpubChapter[], idx: number) =>
    all.find((c) => c.bookChapter === idx)?.index

  const open = (picked: File | undefined) => {
    if (!picked) return
    setError(null)
    setNote(null)
    list.mutate(picked, {
      onSuccess: (resp) => {
        setFile(picked)
        setEpub(resp.chapters)
        setEpubIdx(sameText(resp.chapters, bookIdx) ?? 0)
      },
      onError: fail('Could not read the epub'),
    })
  }

  const pickBook = (idx: number) => {
    setBookIdx(idx)
    const same = sameText(epub, idx)
    if (same !== undefined) setEpubIdx(same)
  }

  const chosen = epub[epubIdx]
  const current = chapters.find((c) => c.idx === bookIdx)
  const busy = list.isPending || match.isPending || backfill.isPending

  const apply = () => {
    if (!file || !chosen) return
    setError(null)
    setNote(null)
    if (bookIdx === NEXT_MISSING) {
      backfill.mutate(
        { bookId, file, first: chosen.index },
        {
          onSuccess: (resp) =>
            setNote(
              `Filling ${resp.queued} missing chapter${resp.queued === 1 ? '' : 's'} from “${chosen.title}” on.`,
            ),
          onError: fail('Could not fill from the epub'),
        },
      )
      return
    }
    match.mutate(
      { chapterIdx: bookIdx, file, epubChapter: chosen.index },
      {
        onSuccess: (resp) => {
          setNote(
            `Chapter ${bookIdx + 1} now reads as “${chosen.title}” (${resp.paragraphs} paragraphs).`,
          )
          if (chapters.some((c) => c.idx === bookIdx + 1)) setBookIdx(bookIdx + 1)
          if (epubIdx + 1 < epub.length) setEpubIdx(epubIdx + 1)
        },
        onError: fail('Could not replace the chapter'),
      },
    )
  }

  return (
    <section className="epub-match">
      <h2>Match epub chapters</h2>
      <p className="muted">
        Replace a chapter with one from an epub, chosen by hand - for a chapter that was filled from
        the wrong one. The chapter keeps its title and its place; its audio and speakers start over.
      </p>
      <div className="import-preview-tools">
        <button
          type="button"
          className="option-chip"
          onClick={() => inputRef.current?.click()}
          disabled={busy}
        >
          {list.isPending
            ? 'Reading the epub…'
            : file
              ? `${file.name} - change…`
              : 'Choose an epub…'}
        </button>
        <input
          ref={inputRef}
          type="file"
          accept=".epub"
          hidden
          onChange={(e) => {
            open(e.target.files?.[0])
            e.target.value = ''
          }}
        />
      </div>
      {file && epub.length > 0 && (
        <>
          <div className="epub-match-pair">
            <label>
              Book chapter
              <select value={bookIdx} onChange={(e) => pickBook(Number(e.target.value))}>
                {source && (
                  <option value={NEXT_MISSING}>
                    First missing chapter{source.blocked ? ` (${source.blocked})` : ''}
                  </option>
                )}
                {chapters.map((c) => (
                  <option key={c.idx} value={c.idx}>
                    {c.idx + 1}. {c.title}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Epub chapter
              <select value={epubIdx} onChange={(e) => setEpubIdx(Number(e.target.value))}>
                {epub.map((c) => (
                  <option key={c.index} value={c.index}>
                    {label(c)}
                    {c.bookChapter != null
                      ? ` - same text as book chapter ${c.bookChapter + 1}`
                      : ''}
                  </option>
                ))}
              </select>
            </label>
          </div>
          {chosen && (
            <p className="epub-match-opening">
              <span className="muted">Opens: </span>
              {chosen.opening || '(no text)'}
            </p>
          )}
          <div className="import-preview-tools">
            <button
              type="button"
              className="primary-button"
              onClick={apply}
              disabled={busy || !chosen || (bookIdx !== NEXT_MISSING && !current)}
            >
              {match.isPending || backfill.isPending
                ? 'Working…'
                : bookIdx === NEXT_MISSING
                  ? 'Fill missing chapters from here'
                  : `Replace chapter ${bookIdx + 1}`}
            </button>
          </div>
        </>
      )}
      {note && <p className="muted">{note}</p>}
      {error && <p className="error-text">{error}</p>}
    </section>
  )
}
