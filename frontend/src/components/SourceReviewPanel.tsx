import { useState } from 'react'
import { ApiError } from '../api/client'
import { useReviewBookSource } from '../api/queries'
import type { BookSummary } from '../api/types'
import { defaultReview, selectRange, splitReview } from '../utils/importSelection'
import { SourceTitleRuleEditor } from './SourceTitleRuleEditor'
import { TitleFilterRow } from './TitleFilterRow'

// Entries a check for new chapters found at one book's sources, waiting
// for the reader to say import or skip (BookSummary.source.review). A
// site's chapter list isn't all story - authors post announcements and
// release notices in it - so nothing new is taken unasked, and the book
// takes nothing past the first entry waiting here until it is decided.
//
// Entries a source lists but won't serve this reader (a Patreon the
// library isn't a member of, early access) are shown apart: they say what
// is there, can't be imported yet, and wait until they open unless
// skipped.
export function SourceReviewPanel({
  book,
  titleRules = true,
}: {
  book: Pick<BookSummary, 'id' | 'title' | 'source'>
  // Whether each source's title rule can be edited here (not where the
  // panel sits under the book's list of sources, which has them).
  titleRules?: boolean
}) {
  const { open: entries, locked } = splitReview(book.source?.review)
  const several = (book.source?.tracked.length ?? 0) > 1
  const review = useReviewBookSource()
  // Ids set to "import"; the rest are skipped. Entries that read like a
  // notice start as skip.
  const [importing, setImporting] = useState(() => defaultReview(entries))
  // Entries that arrive while the panel is open take their default.
  const [known, setKnown] = useState(() => new Set(entries.map((e) => e.id)))
  // Where "Rest from" starts: a position in the list.
  const [from, setFrom] = useState('1')
  const fromIdx = Math.min(Math.max(Number(from) || 1, 1), Math.max(entries.length, 1))
  const fresh = entries.filter((e) => !known.has(e.id))
  if (fresh.length > 0) {
    setKnown(new Set(entries.map((e) => e.id)))
    setImporting(new Set([...importing, ...defaultReview(fresh)]))
  }
  if (entries.length === 0 && locked.length === 0) return null

  const toggle = (id: string) => {
    const next = new Set(importing)
    if (!next.delete(id)) next.add(id)
    setImporting(next)
  }
  const onError = (err: unknown) =>
    alert(err instanceof ApiError ? err.message : 'Could not save that')
  const apply = () =>
    review.mutate(
      {
        bookId: book.id,
        importIds: entries.filter((e) => importing.has(e.id)).map((e) => e.id),
        skipIds: entries.filter((e) => !importing.has(e.id)).map((e) => e.id),
      },
      { onError },
    )
  // Locked entries are decided together: agreed to (taken as soon as they
  // open - the check stops at the first until then) or skipped for good.
  const decideLocked = (take: boolean) => {
    const ids = locked.map((e) => e.id)
    review.mutate(
      { bookId: book.id, importIds: take ? ids : [], skipIds: take ? [] : ids },
      { onError },
    )
  }
  const taking = entries.filter((e) => importing.has(e.id)).length
  const lockedAt = [...new Set(locked.map((e) => e.source))].join(' and ')
  const busy = review.isPending || !!book.source?.syncing

  return (
    <div className="source-review">
      {entries.length > 0 && (
        <>
          <div>
            <strong>{book.title}</strong>
            <span className="muted">
              {' '}
              - {entries.length} new {entries.length === 1 ? 'entry' : 'entries'} at{' '}
              {book.source?.name}. Tick what is story; the rest is left out for good.
            </span>
          </div>
          <div className="import-preview-tools">
            <button
              type="button"
              className="option-chip"
              onClick={() => setImporting(new Set(entries.map((e) => e.id)))}
            >
              All
            </button>
            <button type="button" className="option-chip" onClick={() => setImporting(new Set())}>
              None
            </button>
            <label className="import-preview-range">
              From
              <input
                type="number"
                min={1}
                max={entries.length}
                value={from}
                onChange={(e) => setFrom(e.target.value)}
              />
            </label>
            <button
              type="button"
              className="option-chip"
              title="Tick exactly the entries from the “From” position to the end of the list"
              onClick={() => setImporting(selectRange(entries, fromIdx, entries.length))}
            >
              Rest from {fromIdx}
            </button>
          </div>
          <TitleFilterRow items={entries} selected={importing} onSelect={setImporting} />
          {titleRules &&
            (book.source?.tracked ?? [])
              .filter((t) => t.titleRule || entries.some((e) => e.source === t.name))
              .map((t) => (
                <SourceTitleRuleEditor
                  key={t.id}
                  bookId={book.id}
                  source={t}
                  syncing={!!book.source?.syncing}
                />
              ))}
          <ol className="import-preview-list">
            {entries.map((e, i) => (
              <li key={e.id}>
                <label>
                  <input
                    type="checkbox"
                    checked={importing.has(e.id)}
                    onChange={() => toggle(e.id)}
                  />
                  <span className="import-preview-number muted">{i + 1}</span>
                  <span className="import-preview-title">{e.title}</span>
                  {several && <span className="import-preview-source">{e.source}</span>}
                  {e.suspect && (
                    <span
                      className="import-preview-source"
                      title="The title reads like an author’s notice rather than a chapter"
                    >
                      notice?
                    </span>
                  )}
                  {e.unavailable && (
                    <span
                      className="import-preview-source"
                      title="The Internet Archive has no usable copy of this chapter (what it saved is the site’s “removed” page). Leave it unticked to go on without it, or tick it to try again."
                    >
                      not in the archive
                    </span>
                  )}
                </label>
              </li>
            ))}
          </ol>
          <div className="import-preview-foot">
            <button type="button" className="primary-button" onClick={apply} disabled={busy}>
              {review.isPending ? 'Saving…' : `Import ${taking}, skip ${entries.length - taking}`}
            </button>
          </div>
        </>
      )}
      {locked.length > 0 && (
        <details className="source-review-locked" open={entries.length === 0}>
          <summary>
            {entries.length === 0 && <strong>{book.title} - </strong>}
            {locked.length} more at {lockedAt}, locked
          </summary>
          <p className="muted">
            {lockedAt} lists {locked.length === 1 ? 'this' : 'these'} but won’t serve{' '}
            {locked.length === 1 ? 'it' : 'them'} to this library - patrons only, or early access.
            {locked.some((e) => e.source === 'Patreon') &&
              ' Sign the library in to Patreon on the Settings page to read what your membership covers.'}
          </p>
          <ul className="import-preview-list">
            {locked.map((e) => (
              <li key={e.id}>
                <span className="import-preview-title import-preview-source-locked">{e.title}</span>
              </li>
            ))}
          </ul>
          <div className="import-preview-foot">
            <button
              type="button"
              className="option-chip"
              title="Agree to them now: each is imported as soon as it opens"
              onClick={() => decideLocked(true)}
              disabled={busy}
            >
              Import when they open
            </button>
            <button
              type="button"
              className="option-chip"
              title="Leave them out of this book for good"
              onClick={() => decideLocked(false)}
              disabled={busy}
            >
              Skip them
            </button>
          </div>
        </details>
      )}
    </div>
  )
}
