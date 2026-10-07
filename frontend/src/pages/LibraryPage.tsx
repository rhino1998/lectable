import { useLayoutEffect, useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import {
  RiAddLine,
  RiArrowGoBackLine,
  RiCheckLine,
  RiCloseCircleLine,
  RiDeleteBinLine,
  RiDownload2Line,
  RiFileUploadLine,
  RiHeadphoneLine,
  RiLinksLine,
  RiLoader4Line,
  RiMagicLine,
  RiRefreshLine,
  RiUploadLine,
  RiSettings3Line,
} from 'react-icons/ri'
import {
  useBackfillBookSource,
  useBooks,
  useCancelBookJobs,
  useDeleteBook,
  useGenerateBook,
  useGenerateRemaining,
  useMarkRead,
  useImportBookUrl,
  useJobsSummary,
  usePreprocessBook,
  usePreprocessSeries,
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

// A word count for display: "950", "85k", "1.2M".
function formatWords(words: number): string {
  if (words >= 1_000_000) return `${(words / 1_000_000).toFixed(1)}M`
  if (words >= 1_000) return `${Math.round(words / 1_000)}k`
  return String(words)
}

// A series section's title-bar totals: how far through the whole series
// the reader is (by estimated length), and its length in time and words.
function seriesTotals(books: BookSummary[]): string {
  const totalSeconds = books.reduce((sum, b) => sum + b.estimatedTotalSeconds, 0)
  const listenedSeconds = books.reduce(
    (sum, b) => sum + b.estimatedTotalSeconds * (b.progressPercent / 100),
    0,
  )
  const words = books.reduce((sum, b) => sum + b.wordCount, 0)
  const percent = totalSeconds > 0 ? Math.round((listenedSeconds / totalSeconds) * 100) : 0
  const listened = listenedSeconds >= 30 ? formatDurationLong(listenedSeconds) : '0m'
  return `${percent}% read · ${listened} of ${formatDurationLong(totalSeconds)} · ${formatWords(words)} words`
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

  // Marking read moves the saved position to the end, so a book partway
  // through loses its place - worth a confirm; a finished or unopened one
  // has nothing to lose.
  const markRead = useMarkRead()
  const handleMarkRead = () => {
    const read = !book.finished
    if (read && percent > 0 && !confirm(`Mark "${book.title}" as read? Your place in it is lost.`))
      return
    markRead.mutate(
      { bookId: book.id, read },
      {
        onError: (err) =>
          alert(err instanceof ApiError ? err.message : 'Could not update the book'),
      },
    )
  }

  const cancelJobs = useCancelBookJobs()
  const handleCancelJobs = () => {
    if (!confirm(`Cancel every queued and running job for "${book.title}"?`)) return
    cancelJobs.mutate(book.id, {
      onError: (err) => alert(err instanceof ApiError ? err.message : 'Could not cancel jobs'),
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
  // A book stopped at a chapter its source won't serve can take that
  // stretch from an epub of it.
  const backfill = useBackfillBookSource()
  const backfillInputRef = useRef<HTMLInputElement>(null)
  const handleBackfill = (file: File | undefined) => {
    if (!file) return
    backfill.mutate(
      { bookId: book.id, file },
      {
        onError: (err) =>
          alert(err instanceof ApiError ? err.message : 'Could not fill chapters from the epub'),
      },
    )
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
            <div
              className="book-cover-processing"
              title="Preprocessing: summarizing, attributing speakers, characterizing, labeling emotions, scoring ambience, resolving pronunciation…"
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
            {book.wordCount > 0 && ` · ${formatWords(book.wordCount)} words`}
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
        to="/books/$bookId/settings"
        params={{ bookId: book.id }}
        className="icon-button icon-button-speakers"
        title="Book settings"
      >
        <RiSettings3Line />
      </Link>
      <button
        className={
          book.preprocessed
            ? 'icon-button icon-button-preprocess icon-button-preprocessed'
            : 'icon-button icon-button-preprocess'
        }
        title={
          book.preprocessed
            ? 'Preprocessed: every chapter has been through every pass. Click to run it again for anything new'
            : "Preprocess: summarize, attribute speakers, characterize, label emotions, score ambience, and resolve pronunciation for the whole book. Voices are made later, when a character's lines are first generated"
        }
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
      <button
        className="icon-button icon-button-read"
        title={book.finished ? 'Mark as unread' : 'Mark as read'}
        onClick={handleMarkRead}
        disabled={markRead.isPending}
      >
        {book.finished ? <RiArrowGoBackLine /> : <RiCheckLine />}
      </button>
      <button
        className="icon-button icon-button-cancel-jobs"
        title="Cancel every queued and running job for this book"
        onClick={handleCancelJobs}
        disabled={cancelJobs.isPending}
      >
        <RiCloseCircleLine />
      </button>
      {book.source && (
        <button
          className="icon-button icon-button-source"
          title={
            book.source.syncing
              ? `Fetching chapters from ${book.source.name}…`
              : `Check ${book.source.name} for new chapters`
          }
          onClick={handleSyncSource}
          disabled={book.source.syncing || syncSource.isPending}
        >
          <RiRefreshLine className={book.source.syncing ? 'spin' : undefined} />
        </button>
      )}
      {book.source?.blocked && (
        <>
          <button
            className="icon-button icon-button-backfill"
            title={`Fill the chapters ${book.source.name} won't serve from an epub of them`}
            onClick={() => backfillInputRef.current?.click()}
            disabled={book.source.syncing || backfill.isPending}
          >
            <RiFileUploadLine />
          </button>
          <input
            ref={backfillInputRef}
            type="file"
            accept=".epub"
            hidden
            onChange={(e) => {
              handleBackfill(e.target.files?.[0])
              e.target.value = ''
            }}
          />
        </>
      )}
      <button className="icon-button icon-button-delete" title="Delete book" onClick={onDelete}>
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
  if (source.error) return 'Last check for new chapters failed'
  // The source lists the next chapter but won't serve it yet; the book
  // never skips past one.
  return source.blocked ? `Next chapter “${source.blocked}” is locked at ${source.name}` : null
}

// A series' one row of books, which scrolls sideways. It opens with the
// first book not yet finished in the middle of the row, so a series
// part-way read shows where the reader is - with what came before and
// what comes next on either side - rather than its first volumes. Placed
// once, when the row first appears: after that the row is the reader's to
// scroll, and a book finishing mid-session doesn't move it under them.
function SeriesRow({ books, children }: { books: BookSummary[]; children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null)
  const firstUnread = books.findIndex((b) => !b.finished)
  const opened = useRef(false)
  useLayoutEffect(() => {
    const row = ref.current
    if (!row || opened.current) return
    opened.current = true
    const card = firstUnread >= 0 ? row.children[firstUnread] : undefined
    if (card) {
      // The browser keeps this within the row's ends, so an early book
      // just leaves the row at its start.
      const at = card.getBoundingClientRect()
      const within = row.getBoundingClientRect()
      row.scrollLeft += at.left - within.left - (within.width - at.width) / 2
    }
  }, [firstUnread])
  return (
    <div className="book-grid" ref={ref}>
      {children}
    </div>
  )
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
  // "Add book" lists the ways in: an epub from disk, or a link to a work
  // on a supported site (which opens the link field under the header).
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

  // Epubs upload one at a time, in the order chosen (several from the file
  // picker, or dropped on the page); one that fails is listed by name and
  // the rest carry on.
  const [uploadProgress, setUploadProgress] = useState<{ done: number; total: number } | null>(null)
  const [uploadFailures, setUploadFailures] = useState<string[]>([])
  const handleFilesChosen = async (files: File[]) => {
    if (files.length === 0 || uploadProgress) return
    setUploadError(null)
    setUploadFailures([])
    setUploadProgress({ done: 0, total: files.length })
    for (const [i, file] of files.entries()) {
      try {
        await uploadBook.mutateAsync(file)
      } catch (err) {
        const message = err instanceof ApiError ? err.message : 'Upload failed'
        if (files.length === 1) setUploadError(message)
        else setUploadFailures((prev) => [...prev, `${file.name}: ${message}`])
      }
      setUploadProgress({ done: i + 1, total: files.length })
    }
    setUploadProgress(null)
  }
  const uploading = uploadProgress !== null

  const preprocessSeries = usePreprocessSeries()
  const runPreprocessSeries = (seriesName: string, bookId: string) => {
    if (!confirm(`Preprocess every book in "${seriesName}", one at a time in order?`)) return
    preprocessSeries.mutate(bookId, {
      onError: (err) =>
        alert(err instanceof ApiError ? err.message : 'Could not start preprocessing the series'),
    })
  }

  const confirmDelete = (book: BookSummary) => {
    if (confirm(`Delete "${book.title}"? This cannot be undone.`)) {
      deleteBook.mutate(book.id)
    }
  }

  // The search box narrows the library to books whose title, author or
  // series contains every word typed.
  const [search, setSearch] = useState('')
  const searchWords = useMemo(() => search.toLowerCase().split(/\s+/).filter(Boolean), [search])
  const searching = searchWords.length > 0
  const items = useMemo(() => {
    const books = booksQuery.data ?? []
    if (searchWords.length === 0) return groupBySeries(books)
    return groupBySeries(
      books.filter((b) => {
        const haystack = `${b.title} ${b.author} ${b.seriesName ?? ''}`.toLowerCase()
        return searchWords.every((w) => haystack.includes(w))
      }),
    )
  }, [booksQuery.data, searchWords])
  // The "Continue reading" row: the ten unfinished books read most
  // recently, latest first. Each also keeps its usual place below. Not
  // shown for a one-book library.
  const continueBooks = useMemo(() => {
    const books = booksQuery.data ?? []
    if (books.length < 2) return []
    return books
      .filter((b) => !b.finished && b.lastReadAt)
      .sort((a, b) => (b.lastReadAt ?? 0) - (a.lastReadAt ?? 0))
      .slice(0, 10)
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
    <div
      className="library"
      onDragOver={(e) => {
        if (e.dataTransfer.types.includes('Files')) e.preventDefault()
      }}
      onDrop={(e) => {
        const epubs = [...e.dataTransfer.files].filter((f) =>
          f.name.toLowerCase().endsWith('.epub'),
        )
        // Always taken, so a stray file isn't opened in place of the app.
        e.preventDefault()
        void handleFilesChosen(epubs)
      }}
    >
      <div className="library-header">
        <h1>Your books</h1>
        <input
          type="search"
          className="library-search"
          placeholder="Search by title, author or series"
          aria-label="Search your books"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <div className="library-add-menu" ref={addMenuRef}>
          <button
            className="primary-button"
            aria-haspopup="menu"
            aria-expanded={addMenuOpen}
            onClick={() => setAddMenuOpen((v) => !v)}
            disabled={uploading || importUrl.isPending}
          >
            <RiAddLine />{' '}
            {uploadProgress
              ? uploadProgress.total > 1
                ? `Uploading ${Math.min(uploadProgress.done + 1, uploadProgress.total)} of ${uploadProgress.total}…`
                : 'Uploading…'
              : importUrl.isPending
                ? 'Importing…'
                : 'Add book'}
          </button>
          {addMenuOpen && (
            <div className="popover-panel library-add-menu-panel" role="menu">
              <button
                role="menuitem"
                title="Upload one or several .epub files (or drop them on this page) - a Lectable export restores the book with its audio"
                onClick={() => {
                  setAddMenuOpen(false)
                  fileInputRef.current?.click()
                }}
              >
                <RiUploadLine /> Upload .epub files
              </button>
              <button
                role="menuitem"
                title="Import by link: Royal Road, The Wandering Inn, a Wayback Machine copy of either, or an archive.org item with an epub"
                onClick={() => {
                  setAddMenuOpen(false)
                  setImportOpen(true)
                }}
              >
                <RiLinksLine /> Import from a link
              </button>
            </div>
          )}
        </div>
        <input
          ref={fileInputRef}
          type="file"
          accept=".epub"
          multiple
          hidden
          onChange={(e) => {
            void handleFilesChosen([...(e.target.files ?? [])])
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
            placeholder="Royal Road, Wandering Inn, web.archive.org or archive.org link"
            aria-label="Link to import"
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
            Royal Road and The Wandering Inn (one volume per book), live or as a Wayback Machine
            link; or an archive.org item with an epub. The first chapter is ready in a few seconds;
            the rest arrive in the background.
          </span>
        </form>
      )}

      {uploadError && <p className="error-text">{uploadError}</p>}
      {uploadFailures.length > 0 && (
        <div className="error-text">
          {uploadFailures.length} {uploadFailures.length === 1 ? 'book' : 'books'} could not be
          added:
          <ul>
            {uploadFailures.map((f) => (
              <li key={f}>{f}</li>
            ))}
          </ul>
        </div>
      )}

      {booksQuery.isLoading && <p>Loading…</p>}
      {booksQuery.isError && <p className="error-text">Could not load your library.</p>}

      {booksQuery.data && booksQuery.data.length === 0 && (
        <p className="muted">
          No books yet. Use “Add book” to upload an .epub or import from a link.
        </p>
      )}

      {booksQuery.data && booksQuery.data.length > 0 && (
        <div className="library-stats muted">
          {stats.finished} finished · {stats.inProgress} in progress ·{' '}
          {formatDurationLong(stats.listenedSeconds)} listened
        </div>
      )}

      {searching && booksQuery.data && booksQuery.data.length > 0 && items.length === 0 && (
        <p className="muted">No books match “{search.trim()}”.</p>
      )}

      {continueBooks.length > 0 && !searching && (
        <section className="library-series-group">
          <h2 className="library-series-heading">Continue reading</h2>
          <div className="book-grid">
            {continueBooks.map((book) => (
              <BookCard
                key={book.id}
                book={book}
                onDelete={() => confirmDelete(book)}
                showSeriesName={true}
                narrationRate={rate}
              />
            ))}
          </div>
        </section>
      )}

      {items.map((item) =>
        item.kind === 'series' ? (
          <section key={item.key} className="library-series-group library-series-box">
            <h2 className="library-series-heading">
              {item.seriesName}
              <span className="library-series-totals muted">{seriesTotals(item.books)}</span>
              <button
                className="icon-action-button"
                title="Preprocess the whole series in order - each book only once the one before it has finished, so later books build on the earlier ones' characters and voices"
                onClick={() => runPreprocessSeries(item.seriesName, item.books[0].id)}
                disabled={preprocessSeries.isPending}
              >
                <RiMagicLine />
              </button>
            </h2>
            <SeriesRow books={item.books}>
              {item.books.map((book) => (
                <BookCard
                  key={book.id}
                  book={book}
                  onDelete={() => confirmDelete(book)}
                  showSeriesName={false}
                  narrationRate={rate}
                />
              ))}
            </SeriesRow>
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
