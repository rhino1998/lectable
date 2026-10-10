import { useState } from 'react'
import { RiDeleteBinLine, RiRefreshLine } from 'react-icons/ri'
import { ApiError } from '../api/client'
import { useAddBookSource, useRemoveBookSource, useSyncBookSource } from '../api/queries'
import type { BookSummary, TrackedSource } from '../api/types'
import { SourceReviewPanel } from './SourceReviewPanel'
import { SourceTitleRuleEditor } from './SourceTitleRuleEditor'

// Where a book's chapters come from (BookSummary.source.tracked): the
// site it was imported from and any added here - a serial's advance
// chapters on Patreon beside its public ones on Royal Road. Every source
// is checked for new entries together; a chapter posted in more than one
// is taken once. Adding a source imports nothing by itself: what the book
// doesn't have waits for review, below and in the library.
export function BookSourcesPanel({ book }: { book: Pick<BookSummary, 'id' | 'title' | 'source'> }) {
  const add = useAddBookSource(book.id)
  const remove = useRemoveBookSource(book.id)
  const sync = useSyncBookSource()
  const [url, setUrl] = useState('')
  const [error, setError] = useState<string | null>(null)
  const tracked = book.source?.tracked ?? []
  const syncing = !!book.source?.syncing

  const fail = (fallback: string) => (err: unknown) =>
    setError(err instanceof ApiError ? err.message : fallback)
  const submit = (e: React.FormEvent) => {
    e.preventDefault()
    setError(null)
    add.mutate(url.trim(), {
      onSuccess: () => setUrl(''),
      onError: fail('Could not add that source'),
    })
  }
  const handleRemove = (s: TrackedSource) => {
    if (
      !confirm(
        `Stop tracking ${s.name} for "${book.title}"? The chapters it has given the book stay.`,
      )
    )
      return
    setError(null)
    remove.mutate(s.id, { onError: fail('Could not remove that source') })
  }

  return (
    <section className="book-sources">
      <div className="library-header">
        <h2>Sources</h2>
        {tracked.length > 0 && (
          <button
            className="option-chip"
            onClick={() =>
              sync.mutate(book.id, { onError: fail('Could not check for new chapters') })
            }
            disabled={syncing || sync.isPending}
          >
            <RiRefreshLine className={syncing ? 'spin' : undefined} />{' '}
            {syncing ? 'Checking…' : 'Check for new chapters'}
          </button>
        )}
      </div>
      {tracked.length === 0 ? (
        <p className="muted">
          This book came from a file. Add a link to where it is posted to check there for new
          chapters.
        </p>
      ) : (
        <ul className="book-sources-list">
          {tracked.map((s) => (
            <li key={s.id}>
              <div className="book-sources-main">
                <a href={s.url} target="_blank" rel="noreferrer">
                  {s.name}
                </a>
                <span className={s.error ? 'error-text' : 'muted'} title={s.error || undefined}>
                  {sourceLine(s)}
                </span>
              </div>
              <button
                className="icon-button"
                title={`Stop tracking ${s.name}`}
                onClick={() => handleRemove(s)}
                disabled={syncing || remove.isPending}
              >
                <RiDeleteBinLine />
              </button>
              <SourceTitleRuleEditor bookId={book.id} source={s} syncing={syncing} />
            </li>
          ))}
        </ul>
      )}
      <form className="book-sources-add" onSubmit={submit}>
        <input
          type="url"
          placeholder="Link to this book somewhere else - Royal Road, Patreon, The Wandering Inn…"
          value={url}
          onChange={(e) => setUrl(e.target.value)}
        />
        <button className="primary-button" disabled={!url.trim() || add.isPending || syncing}>
          {add.isPending ? 'Looking…' : 'Add source'}
        </button>
      </form>
      {error && <p className="error-text">{error}</p>}
      <SourceReviewPanel book={book} titleRules={false} />
    </section>
  )
}

// One source's state in a line: how its last check went and what waits.
function sourceLine(s: TrackedSource): string {
  if (s.error) return 'Last check failed'
  const parts: string[] = []
  if (s.waiting > 0) parts.push(`${s.waiting} to review`)
  if (s.locked > 0) parts.push(`${s.locked} locked`)
  if (s.blocked) parts.push(`stopped at “${s.blocked}” (locked)`)
  if (parts.length > 0) return parts.join(', ')
  return s.checkedAt
    ? `checked ${new Date(s.checkedAt * 1000).toLocaleDateString()}`
    : 'not checked yet'
}
