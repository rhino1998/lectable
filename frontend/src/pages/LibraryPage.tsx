import { useMemo, useRef, useState } from 'react'
import { Link } from '@tanstack/react-router'
import {
  RiAddLine,
  RiDeleteBinLine,
  RiDownload2Line,
  RiHeadphoneLine,
  RiLinksLine,
  RiLoader4Line,
  RiMagicLine,
  RiRefreshLine,
  RiUploadLine,
  RiUserVoiceLine,
} from 'react-icons/ri'
import {
  useBooks,
  useDeleteBook,
  useGenerateBook,
  useGenerateRemaining,
  useImportBookUrl,
  useJobsSummary,
  usePreprocessBook,
  useSyncBookSource,
  useUploadBook,
} from '../api/queries'
import { useClickOutside } from '../hooks/useClickOutside'
import { ApiError } from '../api/client'
import { formatDurationLong } from '../utils/time'
import { formatEta, narrationRate as measuredNarrationRate } from '../utils/narrationEta'
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
      items.push({
        kind: 'series',
        key: `series:${book.seriesName}`,
        seriesName: book.seriesName,
        books: sorted,
      })
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
  narrationRate,
}: {
  book: BookSummary
  onDelete: () => void
  // Measured narration speed in chars/s (utils/narrationEta), 0 if unknown.
  narrationRate: number
  // Inside a series section the heading already names the series, so the
  // card itself only needs its own index within it.
  showSeriesName: boolean
}) {
  const percent = Math.round(book.progressPercent)
  const generatedPercent = Math.round(book.generatedPercent)
  const remainingSeconds = Math.max(
    0,
    book.estimatedTotalSeconds * (1 - book.progressPercent / 100),
  )

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

  const syncSource = useSyncBookSource()
  const handleSyncSource = () => {
    syncSource.mutate(book.id, {
      onError: (err) =>
        alert(err instanceof ApiError ? err.message : 'Could not check for new chapters'),
    })
  }
  const sourceNote = book.source && sourceStatus(book)

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
            <div
              className="book-cover-processing"
              title="Preprocessing: attributing speakers, characterizing, provisioning voices, tagging directions, resolving pronunciation…"
            >
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
            {book.pendingNarrationChars > 0 &&
              narrationRate > 0 &&
              ` (~${formatEta(book.pendingNarrationChars / narrationRate)} to go)`}
            {' · '}
            {formatDurationLong(remainingSeconds)}
            {!book.estimateCalibrated && ' (est.)'}
          </div>
          {sourceNote && (
            <div
              className={`book-source-status ${book.source?.error ? 'error-text' : 'muted'}`}
              title={book.source?.error}
            >
              {sourceNote}
            </div>
          )}
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
            <button
              className="option-chip"
              title="Generate the whole book from the beginning"
              onClick={handleGenerateAll}
            >
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
      {book.source && (
        <button
          className="icon-button icon-button-source"
          title={
            book.source.syncing
              ? 'Fetching chapters from Royal Road…'
              : 'Check Royal Road for new chapters'
          }
          onClick={handleSyncSource}
          disabled={book.source.syncing || syncSource.isPending}
        >
          <RiRefreshLine className={book.source.syncing ? 'spin' : undefined} />
        </button>
      )}
      <button className="icon-button" title="Delete book" onClick={onDelete}>
        <RiDeleteBinLine />
      </button>
    </div>
  )
}

// The card line for a book imported from a website, when there is
// something to say: chapters still arriving, or a check that stopped
// early. remoteChapters counts the book's own chapters plus the ones the
// last check found and hasn't fetched.
function sourceStatus(book: BookSummary): string | null {
  const source = book.source
  if (!source) return null
  const outstanding = source.remoteChapters - book.chapterCount
  if (source.syncing) {
    return outstanding > 0
      ? `Fetching chapters: ${book.chapterCount} of ${source.remoteChapters}`
      : 'Checking for new chapters…'
  }
  if (outstanding > 0) {
    const n = `${outstanding} chapter${outstanding === 1 ? '' : 's'}`
    return source.error ? `${n} not fetched - check again` : `${n} left to fetch - check again`
  }
  return source.error ? 'Last check for new chapters failed' : null
}

export function LibraryPage() {
  const booksQuery = useBooks()
  // Only the rate is read - for each card's "to go" estimate.
  const rate = measuredNarrationRate(useJobsSummary().data)
  const uploadBook = useUploadBook()
  const deleteBook = useDeleteBook()
  const fileInputRef = useRef<HTMLInputElement>(null)
  const [uploadError, setUploadError] = useState<string | null>(null)

  const importUrl = useImportBookUrl()
  const [importOpen, setImportOpen] = useState(false)
  const [importLink, setImportLink] = useState('')
  // "Add book" lists the ways in: an epub from disk, or a Royal Road link
  // (which opens the link field under the header).
  const [addMenuOpen, setAddMenuOpen] = useState(false)
  const addMenuRef = useRef<HTMLDivElement>(null)
  useClickOutside([addMenuRef], () => setAddMenuOpen(false), addMenuOpen)
  const handleImport = () => {
    const link = importLink.trim()
    if (!link || importUrl.isPending) return
    setUploadError(null)
    importUrl.mutate(link, {
      onSuccess: () => {
        setImportLink('')
        setImportOpen(false)
      },
      onError: (err) => setUploadError(err instanceof ApiError ? err.message : 'Import failed'),
    })
  }

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
  // The "Continue reading" entry: the unfinished book read most recently.
  // It also keeps its usual place below. Not shown for a one-book library.
  const continueBook = useMemo(() => {
    const books = booksQuery.data ?? []
    if (books.length < 2) return null
    return books
      .filter((b) => !b.finished && b.lastReadAt)
      .reduce<BookSummary | null>(
        (best, b) => (!best || (b.lastReadAt ?? 0) > (best.lastReadAt ?? 0) ? b : best),
        null,
      )
  }, [booksQuery.data])

  // Approximated, not tracked directly: no event logs actual listening
  // time, so "hours listened" is each book's self-calibrating estimated
  // total length scaled by how far into it the saved position is -
  // exactly the same math BookCard already uses for one book's remaining
  // time, just summed the other way (progress instead of 1 - progress).
  const stats = useMemo(() => {
    const books = booksQuery.data ?? []
    const finished = books.filter((b) => b.finished).length
    const inProgress = books.filter((b) => !b.finished && b.progressPercent > 0).length
    const listenedSeconds = books.reduce(
      (sum, b) => sum + b.estimatedTotalSeconds * (b.progressPercent / 100),
      0,
    )
    return { finished, inProgress, listenedSeconds }
  }, [booksQuery.data])

  return (
    <div className="library">
      <div className="library-header">
        <h1>Your books</h1>
        <div className="library-add-menu" ref={addMenuRef}>
          <button
            className="primary-button"
            aria-haspopup="menu"
            aria-expanded={addMenuOpen}
            onClick={() => setAddMenuOpen((v) => !v)}
            disabled={uploadBook.isPending || importUrl.isPending}
          >
            <RiAddLine />{' '}
            {uploadBook.isPending ? 'Uploading…' : importUrl.isPending ? 'Importing…' : 'Add book'}
          </button>
          {addMenuOpen && (
            <div className="popover-panel library-add-menu-panel" role="menu">
              <button
                role="menuitem"
                title="Upload an .epub - or a Lectable export, which restores the book with its audio"
                onClick={() => {
                  setAddMenuOpen(false)
                  fileInputRef.current?.click()
                }}
              >
                <RiUploadLine /> Upload .epub
              </button>
              <button
                role="menuitem"
                title="Import a Royal Road fiction by its link"
                onClick={() => {
                  setAddMenuOpen(false)
                  setImportOpen(true)
                }}
              >
                <RiLinksLine /> Import from Royal Road
              </button>
            </div>
          )}
        </div>
        <input
          ref={fileInputRef}
          type="file"
          accept=".epub"
          hidden
          onChange={(e) => {
            handleFileChosen(e.target.files?.[0])
            // Cleared so choosing the same file again (a retry) still fires onChange.
            e.target.value = ''
          }}
        />
      </div>

      {importOpen && (
        <form
          className="library-import"
          onSubmit={(e) => {
            e.preventDefault()
            handleImport()
          }}
        >
          <input
            type="url"
            autoFocus
            placeholder="https://www.royalroad.com/fiction/…"
            aria-label="Royal Road fiction link"
            value={importLink}
            onChange={(e) => setImportLink(e.target.value)}
            disabled={importUrl.isPending}
          />
          <button
            className="primary-button"
            type="submit"
            disabled={!importLink.trim() || importUrl.isPending}
          >
            {importUrl.isPending ? 'Importing…' : 'Import'}
          </button>
          <button type="button" className="text-button" onClick={() => setImportOpen(false)}>
            Cancel
          </button>
          <span className="muted">
            The first chapter is ready in a few seconds; the rest arrive in the background.
          </span>
        </form>
      )}

      {uploadError && <p className="error-text">{uploadError}</p>}

      {booksQuery.isLoading && <p>Loading…</p>}
      {booksQuery.isError && <p className="error-text">Could not load your library.</p>}

      {booksQuery.data && booksQuery.data.length === 0 && (
        <p className="muted">
          No books yet. Use “Add book” to upload an .epub or import a Royal Road fiction.
        </p>
      )}

      {booksQuery.data && booksQuery.data.length > 0 && (
        <div className="library-stats muted">
          {stats.finished} finished · {stats.inProgress} in progress ·{' '}
          {formatDurationLong(stats.listenedSeconds)} listened
        </div>
      )}

      {continueBook && (
        <section className="library-series-group">
          <h2 className="library-series-heading">Continue reading</h2>
          <div className="book-grid">
            <BookCard
              book={continueBook}
              onDelete={() => confirmDelete(continueBook)}
              showSeriesName={true}
              narrationRate={rate}
            />
          </div>
        </section>
      )}

      {items.map((item) =>
        item.kind === 'series' ? (
          <section key={item.key} className="library-series-group">
            <h2 className="library-series-heading">{item.seriesName}</h2>
            <div className="book-grid">
              {item.books.map((book) => (
                <BookCard
                  key={book.id}
                  book={book}
                  onDelete={() => confirmDelete(book)}
                  showSeriesName={false}
                  narrationRate={rate}
                />
              ))}
            </div>
          </section>
        ) : (
          <div key={item.key} className="book-grid">
            {item.books.map((book) => (
              <BookCard
                key={book.id}
                book={book}
                onDelete={() => confirmDelete(book)}
                showSeriesName={true}
                narrationRate={rate}
              />
            ))}
          </div>
        ),
      )}
    </div>
  )
}
