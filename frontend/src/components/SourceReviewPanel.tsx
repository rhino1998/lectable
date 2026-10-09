import { useState } from 'react'
import { ApiError } from '../api/client'
import { useReviewBookSource } from '../api/queries'
import type { BookSummary } from '../api/types'
import { defaultReview } from '../utils/importSelection'

// Entries a check for new chapters found in one book's source, waiting for
// the reader to say import or skip (BookSummary.source.review). A site's
// chapter list isn't all story - authors post announcements and release
// notices in it - so nothing new is taken unasked, and the book takes
// nothing past the first entry waiting here until it is decided.
export function SourceReviewPanel({ book }: { book: BookSummary }) {
  const entries = book.source?.review ?? []
  const review = useReviewBookSource()
  // Ids set to "import"; the rest are skipped. Entries that read like a
  // notice start as skip.
  const [importing, setImporting] = useState(() => defaultReview(entries))
  // Entries that arrive while the panel is open take their default.
  const [known, setKnown] = useState(() => new Set(entries.map((e) => e.id)))
  const fresh = entries.filter((e) => !known.has(e.id))
  if (fresh.length > 0) {
    setKnown(new Set(entries.map((e) => e.id)))
    setImporting(new Set([...importing, ...defaultReview(fresh)]))
  }
  if (entries.length === 0) return null

  const toggle = (id: string) => {
    const next = new Set(importing)
    if (!next.delete(id)) next.add(id)
    setImporting(next)
  }
  const apply = () =>
    review.mutate(
      {
        bookId: book.id,
        importIds: entries.filter((e) => importing.has(e.id)).map((e) => e.id),
        skipIds: entries.filter((e) => !importing.has(e.id)).map((e) => e.id),
      },
      { onError: (err) => alert(err instanceof ApiError ? err.message : 'Could not save that') },
    )
  const taking = entries.filter((e) => importing.has(e.id)).length

  return (
    <div className="source-review">
      <div>
        <strong>{book.title}</strong>
        <span className="muted">
          {' '}
          - {entries.length} new {entries.length === 1 ? 'entry' : 'entries'} at {book.source?.name}
          . Tick what is story; the rest is left out for good.
        </span>
      </div>
      <ul className="import-preview-list">
        {entries.map((e) => (
          <li key={e.id}>
            <label>
              <input type="checkbox" checked={importing.has(e.id)} onChange={() => toggle(e.id)} />
              <span className="import-preview-title">{e.title}</span>
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
      </ul>
      <div className="import-preview-foot">
        <button
          type="button"
          className="primary-button"
          onClick={apply}
          disabled={review.isPending || !!book.source?.syncing}
        >
          {review.isPending ? 'Saving…' : `Import ${taking}, skip ${entries.length - taking}`}
        </button>
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
      </div>
    </div>
  )
}
