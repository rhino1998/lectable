import { useMemo, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { RiDeleteBinLine, RiDownload2Line, RiHeadphoneLine, RiLoader4Line, RiMagicLine, RiUploadLine, RiUserVoiceLine } from 'react-icons/ri'
import { useBooks, useDeleteBook, useGenerateBook, useGenerateRemaining, usePreprocessBook, useUploadBook } from '../api/queries'
import { useClickOutside } from '../hooks/useClickOutside'
import { ApiError } from '../api/client'
import { formatDurationLong } from '../utils/time'
import type { BookSummary } from '../api/types'

// One library grid entry: either a run of consecutive standalone books (no
// series, or a series with only one book present - not worth its own
// section for just one cover) rendered in the normal grid, or a titled
// section for a series with 2+ books present, sorted by seriesIndex.
type LibraryItem =
  | { kind: 'standalone'; key: string; books: BookSummary[] }
  | { kind: 'series'; key: string; seriesName: string; books: BookSummary[] }

// Groups books by series while preserving the API's own ordering (newest
// added first) at the group level: a series section sits wherever its
// first-encountered book would have sat standalone, and consecutive
// standalone books between series sections stay merged into one grid
// rather than one grid per book.
function groupBySeries(books: BookSummary[]): LibraryItem[] {
  const items: LibraryItem[] = []
  const seenSeries = new Set<string>()

  for (const book of books) {
    const seriesMates = book.seriesName ? books.filter((b) => b.seriesName === book.seriesName) : []
    if (book.seriesName && seriesMates.length > 1) {
      if (seenSeries.has(book.seriesName)) continue
      seenSeries.add(book.seriesName)
      const sorted = [...seriesMates].sort((a, b) => (a.seriesIndex ?? 0) - (b.seriesIndex ?? 0))
      items.push({ kind: 'series', key: `series:${book.seriesName}`, seriesName: book.seriesName, books: sorted })
      continue
    }
    const last = items[items.length - 1]
    if (last?.kind === 'standalone') last.books.push(book)
    else items.push({ kind: 'standalone', key: `standalone:${book.id}`, books: [book] })
  }

  return items
}

function BookCard({
  book,
  onDelete,
  showSeriesName,
}: {
  book: BookSummary
  onDelete: () => void
  // Inside a series section the heading already names the series, so the
  // card itself only needs its own index within it.
  showSeriesName: boolean
}) {
  const percent = Math.round(book.progressPercent)
  const generatedPercent = Math.round(book.generatedPercent)
  const remainingSeconds = Math.max(0, book.estimatedTotalSeconds * (1 - book.progressPercent / 100))

  const preprocessBook = usePreprocessBook()
  const handlePreprocess = () => {
    preprocessBook.mutate(book.id, {
      onError: (err) =>
        alert(err instanceof ApiError ? err.message : 'Could not start preprocessing'),
    })
  }

  const generateBook = useGenerateBook()
  const generateRemaining = useGenerateRemaining()
  const [generateMenuOpen, setGenerateMenuOpen] = useState(false)
  const generateMenuRef = useRef<HTMLDivElement>(null)
  useClickOutside([generateMenuRef], () => setGenerateMenuOpen(false), generateMenuOpen)
  const handleGenerateAll = () => {
    setGenerateMenuOpen(false)
    generateBook.mutate(book.id, {
      onError: (err) => alert(err instanceof ApiError ? err.message : 'Could not start generation'),
    })
  }
  const handleGenerateRemaining = () => {
    setGenerateMenuOpen(false)
    generateRemaining.mutate(book.id, {
      onError: (err) => alert(err instanceof ApiError ? err.message : 'Could not start generation'),
    })
  }

  return (
    <div className="book-card">
      <Link to="/books/$bookId" params={{ bookId: book.id }} className="book-card-link">
        <div className="book-cover">
          {book.coverUrl ? (
            <img src={book.coverUrl} alt="" />
          ) : (
            <div className="book-cover-placeholder">{book.title.slice(0, 1).toUpperCase()}</div>
          )}
          {book.finished && <span className="book-finished-badge">Finished</span>}
          {book.preprocessing && (
            <div className="book-cover-processing" title="Preprocessing: attributing speakers, characterizing, provisioning voices, tagging directions, resolving pronunciation…">
              <RiLoader4Line className="spin" />
            </div>
          )}
        </div>
        <div className="book-meta">
          <div className="book-title">{book.title}</div>
          <div className="book-author">{book.author || 'Unknown author'}</div>
          {book.seriesName && (showSeriesName || !!book.seriesIndex) && (
            <div className="book-series muted">
              {showSeriesName && book.seriesName}
              {!!book.seriesIndex && `${showSeriesName ? ' ' : ''}#${book.seriesIndex}`}
            </div>
          )}
          <div className="book-progress-track">
            <div className="book-progress-generated" style={{ width: `${generatedPercent}%` }} />
            <div className="book-progress-fill" style={{ width: `${percent}%` }} />
          </div>
          <div className="book-progress-label muted">
            {book.finished ? 'Finished' : percent > 0 ? `${percent}% read` : 'Not started'}
            {' · '}
            {generatedPercent}% generated
            {' · '}
            {formatDurationLong(remainingSeconds)}
            {!book.estimateCalibrated && ' (est.)'}
          </div>
        </div>
      </Link>
      <Link
        to="/books/$bookId/speakers"
        params={{ bookId: book.id }}
        className="icon-button icon-button-speakers"
        title="Speakers"
      >
        <RiUserVoiceLine />
      </Link>
      <button
        className="icon-button icon-button-preprocess"
        title="Preprocess: attribute speakers, characterize, provision voices, tag directions, and resolve pronunciation for the whole book"
        onClick={handlePreprocess}
        disabled={book.preprocessing || preprocessBook.isPending}
      >
        <RiMagicLine />
      </button>
      <div className="book-generate-menu" ref={generateMenuRef}>
        <button
          className="icon-button"
          title="Generate audio in the background"
          onClick={() => setGenerateMenuOpen((v) => !v)}
          disabled={generateBook.isPending || generateRemaining.isPending}
        >
          <RiHeadphoneLine />
        </button>
        {generateMenuOpen && (
          <div className="popover-panel book-generate-menu-panel">
            <button className="option-chip" title="Generate the whole book from the beginning" onClick={handleGenerateAll}>
              All
            </button>
            <button
              className="option-chip"
              title="Generate from your current reading position to the end"
              onClick={handleGenerateRemaining}
            >
              Remaining
            </button>
          </div>
        )}
      </div>
      <Link
        to="/books/$bookId/export"
        params={{ bookId: book.id }}
        className="icon-button icon-button-export"
        title="Export"
      >
        <RiDownload2Line />
      </Link>
      <button className="icon-button" title="Delete book" onClick={onDelete}>
        <RiDeleteBinLine />
      </button>
    </div>
  )
}

export function LibraryPage() {
  const booksQuery = useBooks()
  const uploadBook = useUploadBook()
  const deleteBook = useDeleteBook()
  const fileInputRef = useRef<HTMLInputElement>(null)
  const [uploadError, setUploadError] = useState<string | null>(null)

  const handleFileChosen = (file: File | undefined) => {
    if (!file) return
    setUploadError(null)
    uploadBook.mutate(file, {
      onError: (err) => setUploadError(err instanceof ApiError ? err.message : 'Upload failed'),
    })
  }

  const confirmDelete = (book: BookSummary) => {
    if (confirm(`Delete "${book.title}"? This cannot be undone.`)) {
      deleteBook.mutate(book.id)
    }
  }

  const items = useMemo(() => groupBySeries(booksQuery.data ?? []), [booksQuery.data])

  // Approximated, not tracked directly: no event logs actual listening
  // time, so "hours listened" is each book's self-calibrating estimated
  // total length scaled by how far into it the saved position is -
  // exactly the same math BookCard already uses for one book's remaining
  // time, just summed the other way (progress instead of 1 - progress).
  const stats = useMemo(() => {
    const books = booksQuery.data ?? []
    const finished = books.filter((b) => b.finished).length
    const inProgress = books.filter((b) => !b.finished && b.progressPercent > 0).length
    const listenedSeconds = books.reduce((sum, b) => sum + b.estimatedTotalSeconds * (b.progressPercent / 100), 0)
    return { finished, inProgress, listenedSeconds }
  }, [booksQuery.data])

  return (
    <div className="library">
      <div className="library-header">
        <h1>Your books</h1>
        <button
          className="primary-button"
          title="Upload an .epub - or a Lectable export, which restores the book with its audio"
          onClick={() => fileInputRef.current?.click()}
          disabled={uploadBook.isPending}
        >
          <RiUploadLine /> {uploadBook.isPending ? 'Uploading…' : 'Upload .epub'}
        </button>
        <input
          ref={fileInputRef}
          type="file"
          accept=".epub"
          hidden
          onChange={(e) => handleFileChosen(e.target.files?.[0])}
        />
      </div>

      {uploadError && <p className="error-text">{uploadError}</p>}

      {booksQuery.isLoading && <p>Loading…</p>}
      {booksQuery.isError && <p className="error-text">Could not load your library.</p>}

      {booksQuery.data && booksQuery.data.length === 0 && (
        <p className="muted">No books yet. Upload an .epub to get started.</p>
      )}

      {booksQuery.data && booksQuery.data.length > 0 && (
        <div className="library-stats muted">
          {stats.finished} finished · {stats.inProgress} in progress ·{' '}
          {formatDurationLong(stats.listenedSeconds)} listened
        </div>
      )}

      {items.map((item) =>
        item.kind === 'series' ? (
          <section key={item.key} className="library-series-group">
            <h2 className="library-series-heading">{item.seriesName}</h2>
            <div className="book-grid">
              {item.books.map((book) => (
                <BookCard key={book.id} book={book} onDelete={() => confirmDelete(book)} showSeriesName={false} />
              ))}
            </div>
          </section>
        ) : (
          <div key={item.key} className="book-grid">
            {item.books.map((book) => (
              <BookCard key={book.id} book={book} onDelete={() => confirmDelete(book)} showSeriesName={true} />
            ))}
          </div>
        ),
      )}
    </div>
  )
}
