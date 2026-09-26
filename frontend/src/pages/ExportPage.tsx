import { useMemo, useState } from 'react'
import { Link, useParams } from '@tanstack/react-router'
import { RiDeleteBinLine, RiDownload2Line, RiLoader4Line, RiRefreshLine } from 'react-icons/ri'
import { useBook, useBookExports, useCreateExport, useDeleteExport } from '../api/queries'
import { ApiError } from '../api/client'
import { EXPORT_FORMATS, EXPORT_FORMAT_LABELS } from '../api/types'
import type { BookExport, ChapterSummary, CreateExportRequest, ExportFormat } from '../api/types'
import { formatDurationLong } from '../utils/time'

function formatSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`
  const units = ['KB', 'MB', 'GB', 'TB']
  let n = bytes / 1024
  let i = 0
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024
    i++
  }
  return `${n.toFixed(n < 10 ? 1 : 0)} ${units[i]}`
}

// Splits chapters into `parts` consecutive runs of roughly equal size,
// weighted by paragraph count (the closest thing to length the book
// summary carries) - each run becomes its own export.
function splitIntoParts(chapters: ChapterSummary[], parts: number): number[][] {
  const total = chapters.reduce((sum, c) => sum + Math.max(1, c.paragraphCount), 0)
  const out: number[][] = []
  let current: number[] = []
  let acc = 0
  for (const c of chapters) {
    current.push(c.idx)
    acc += Math.max(1, c.paragraphCount)
    const remainingParts = parts - out.length - 1
    if (remainingParts > 0 && acc >= (total * (out.length + 1)) / parts) {
      out.push(current)
      current = []
    }
  }
  if (current.length > 0) out.push(current)
  return out
}

// Highlight granularity. Readers built on a browser engine (Thorium, ...)
// see the audio position only about every 250ms, so a sync unit shorter
// than 250ms times the playback rate can't be tracked and the highlight
// drifts. Word mode groups words into phrases long enough for the chosen
// top speed: 0.3s of audio per 1x (1.5s at 5x); 1x is plain per-word.
type SyncMode = 'paragraph' | 'words'
const SYNC_SPEEDS = [1, 2, 3, 4, 5] as const
const phraseSecondsFor = (speed: number) => (speed <= 1 ? 0 : 0.3 * speed)

function syncLabel(e: BookExport): string | null {
  if (!e.wordLevel) return null
  if (!e.phraseSeconds) return 'Word sync'
  return `Phrase sync ≤${Math.round(e.phraseSeconds / 0.3)}×`
}

function exportStatus(e: BookExport): string {
  if (e.building) return `Building… ${Math.round(e.progress * 100)}%`
  if (e.rendering) return 'Rendering audio…'
  if (e.queued) return 'Queued'
  if (e.error) return `Failed: ${e.error}`
  if (e.stale) return 'Out of date'
  if (e.ready) return 'Ready'
  return ''
}

export function ExportPage() {
  const { bookId } = useParams({ from: '/books/$bookId/export' })
  const bookQuery = useBook(bookId)
  const exportsQuery = useBookExports(bookId)
  const createExport = useCreateExport(bookId)
  const deleteExport = useDeleteExport(bookId)

  const [format, setFormat] = useState<ExportFormat>('epub')
  const [syncMode, setSyncMode] = useState<SyncMode>('words')
  const [syncSpeed, setSyncSpeed] = useState<number>(5)
  const wordLevel = syncMode === 'words'
  const phraseSeconds = wordLevel ? phraseSecondsFor(syncSpeed) : 0
  const [asIs, setAsIs] = useState(false)
  const [includeMusic, setIncludeMusic] = useState(true)
  const [latentTransfer, setLatentTransfer] = useState(false)
  const latents = latentTransfer
  const [wholeBook, setWholeBook] = useState(true)
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [parts, setParts] = useState(2)
  const [error, setError] = useState<string | null>(null)

  const chapters = useMemo(() => bookQuery.data?.chapters ?? [], [bookQuery.data])
  const chosen = useMemo(
    () => (wholeBook ? chapters : chapters.filter((c) => selected.has(c.idx))),
    [wholeBook, chapters, selected],
  )
  const missing = chosen.reduce((sum, c) => sum + Math.max(0, c.paragraphCount - c.readyCount), 0)

  if (bookQuery.isLoading) return <p>Loading…</p>
  if (bookQuery.isError || !bookQuery.data)
    return <p className="error-text">Could not load this book.</p>
  const book = bookQuery.data

  const start = (reqs: CreateExportRequest[]) => {
    setError(null)
    for (const req of reqs) {
      createExport.mutate(req, {
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Could not start the export'),
      })
    }
  }
  const handleBuild = () => {
    const chapters = wholeBook ? [] : [...selected].sort((a, b) => a - b)
    start([
      {
        format,
        wordLevel,
        phraseSeconds,
        asIs: asIs || latents,
        excludeMusic: wholeBook && !includeMusic,
        chapters,
        latents,
      },
    ])
  }
  const handleSplit = () => {
    const runs = splitIntoParts(chapters, Math.min(parts, chapters.length))
    start(
      runs.map((run) => ({
        format,
        wordLevel,
        phraseSeconds,
        asIs: asIs || latents,
        excludeMusic: false,
        chapters: run,
        latents,
      })),
    )
  }
  const handleRebuild = (e: BookExport) => {
    start([
      {
        format: e.format,
        wordLevel: e.wordLevel,
        phraseSeconds: e.phraseSeconds,
        excludeMusic: e.excludeMusic,
        asIs: asIs || e.latents,
        chapters: e.chapters,
        latents: e.latents,
      },
    ])
  }
  const handleDelete = (e: BookExport) => {
    const what =
      e.building || e.rendering || e.queued
        ? 'Cancel this export?'
        : `Delete ${e.fileName ?? 'this export'}?`
    if (!confirm(what)) return
    deleteExport.mutate(e.id, {
      onError: (err) =>
        setError(err instanceof ApiError ? err.message : 'Could not delete the export'),
    })
  }
  const toggle = (idx: number) => {
    setSelected((prev) => {
      const next = new Set(prev)
      if (next.has(idx)) next.delete(idx)
      else next.add(idx)
      return next
    })
  }

  const exports = exportsQuery.data ?? []

  return (
    <div className="export-page">
      <div className="library-header">
        <div>
          <h1>Export</h1>
          <p className="muted">{book.title}</p>
        </div>
        <Link to="/" className="text-button">
          ← Back to library
        </Link>
      </div>

      {error && <p className="error-text">{error}</p>}

      <section>
        <h2>Exports</h2>
        {exportsQuery.isLoading && <p className="muted">Loading…</p>}
        {!exportsQuery.isLoading && exports.length === 0 && (
          <p className="muted">No exports yet.</p>
        )}
        {exports.length > 0 && (
          <table className="jobs-table export-table">
            <thead>
              <tr>
                <th>Contents</th>
                <th>Status</th>
                <th>Length</th>
                <th>Size</th>
                <th>Built</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {exports.map((e) => (
                <tr key={e.id}>
                  <td>
                    {e.chapterLabel || 'Whole book'}
                    <span className="export-badge">{e.format.toUpperCase()}</span>
                    {syncLabel(e) && <span className="export-badge">{syncLabel(e)}</span>}
                    {e.excludeMusic && <span className="export-badge">No music</span>}
                    {e.latents && (
                      <span
                        className="export-badge"
                        title="A latent-only transfer to another Lectable library - not playable as an audiobook"
                      >
                        Latents
                      </span>
                    )}
                    {e.full && (
                      <span
                        className="export-badge"
                        title="Includes Lectable's own data - upload it to another Lectable library to restore this book"
                      >
                        Importable
                      </span>
                    )}
                    {!!e.missingAudio && (
                      <div className="muted export-note">
                        {e.missingAudio} paragraphs without audio
                      </div>
                    )}
                  </td>
                  <td className={e.error ? 'error-text' : e.stale ? 'export-stale' : undefined}>
                    {(e.building || e.rendering || e.queued) && (
                      <RiLoader4Line className="spin export-spinner" />
                    )}
                    {exportStatus(e)}
                    {e.building && (
                      <div className="export-progress">
                        <div
                          className="export-progress-fill"
                          style={{ width: `${Math.round(e.progress * 100)}%` }}
                        />
                      </div>
                    )}
                  </td>
                  <td>{e.ready ? formatDurationLong(e.durationSeconds ?? 0) : ''}</td>
                  <td>{e.ready && e.sizeBytes ? formatSize(e.sizeBytes) : ''}</td>
                  <td>{e.createdAt ? new Date(e.createdAt).toLocaleString() : ''}</td>
                  <td>
                    <div className="export-actions">
                      {e.ready && e.downloadUrl && (
                        <a
                          className="icon-action-button"
                          href={e.downloadUrl}
                          download={e.fileName}
                          title="Download"
                        >
                          <RiDownload2Line />
                        </a>
                      )}
                      <button
                        className="icon-action-button"
                        title={e.stale ? 'Rebuild with the book as it is now' : 'Rebuild'}
                        onClick={() => handleRebuild(e)}
                        disabled={e.building || e.rendering || e.queued}
                      >
                        <RiRefreshLine />
                      </button>
                      <button
                        className="icon-action-button"
                        title={e.building || e.rendering || e.queued ? 'Cancel' : 'Delete'}
                        onClick={() => handleDelete(e)}
                      >
                        <RiDeleteBinLine />
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section>
        <h2>New export</h2>
        <div className="export-form">
          <label>
            Format
            <select value={format} onChange={(ev) => setFormat(ev.target.value as ExportFormat)}>
              {EXPORT_FORMATS.map((f) => (
                <option key={f} value={f}>
                  {EXPORT_FORMAT_LABELS[f]}
                </option>
              ))}
            </select>
          </label>
          <label>
            Highlighting
            <select value={syncMode} onChange={(ev) => setSyncMode(ev.target.value as SyncMode)}>
              <option value="words">Words / phrases</option>
              <option value="paragraph">Paragraphs</option>
            </select>
          </label>
          {wordLevel && (
            <label>
              Keep in sync at playback speeds up to
              <select value={syncSpeed} onChange={(ev) => setSyncSpeed(Number(ev.target.value))}>
                {SYNC_SPEEDS.map((s) => (
                  <option key={s} value={s}>
                    {s === 1
                      ? '1× (every word)'
                      : `${s}× (phrases of ${phraseSecondsFor(s).toFixed(1)}s+)`}
                  </option>
                ))}
              </select>
            </label>
          )}
          <label className="checkbox-label">
            <input type="checkbox" checked={asIs} onChange={(ev) => setAsIs(ev.target.checked)} />
            Export as-is - don't generate missing audio first (those paragraphs go in text-only)
          </label>
          <label className="checkbox-label">
            <input
              type="checkbox"
              checked={latentTransfer}
              onChange={(ev) => setLatentTransfer(ev.target.checked)}
            />
            Latent-only transfer - ships the models' latents instead of audio where they exist (much
            smaller); only for moving the book to another Lectable library, which decodes each clip
            when it's first played. Always as-is: an in-progress book moves with what it has.
            Selected chapters import as their own book (&ldquo;Title (chapters&nbsp;…)&rdquo;)
          </label>
          <label className="checkbox-label">
            <input
              type="radio"
              name="scope"
              checked={wholeBook}
              onChange={() => setWholeBook(true)}
            />
            Whole book - includes Lectable's own data, so it can be uploaded to another Lectable
            library
          </label>
          {wholeBook && (
            <label className="checkbox-label export-indent">
              <input
                type="checkbox"
                checked={includeMusic}
                onChange={(ev) => setIncludeMusic(ev.target.checked)}
              />
              Include background music - usually most of the file (without it, an importing library
              keeps the music scoring and renders the music again)
            </label>
          )}
          <label className="checkbox-label">
            <input
              type="radio"
              name="scope"
              checked={!wholeBook}
              onChange={() => setWholeBook(false)}
            />
            Selected chapters
          </label>
        </div>

        {!wholeBook && (
          <div className="export-chapters">
            <div className="export-chapter-actions">
              <button
                className="option-chip"
                onClick={() => setSelected(new Set(chapters.map((c) => c.idx)))}
              >
                All
              </button>
              <button className="option-chip" onClick={() => setSelected(new Set())}>
                None
              </button>
              <span className="muted">{selected.size} selected</span>
            </div>
            <div className="export-chapter-list">
              {chapters.map((c) => (
                <label key={c.idx} className="checkbox-label export-chapter">
                  <input
                    type="checkbox"
                    checked={selected.has(c.idx)}
                    onChange={() => toggle(c.idx)}
                  />
                  <span className="export-chapter-idx">{c.idx + 1}</span>
                  <span className="export-chapter-title">{c.title || `Chapter ${c.idx + 1}`}</span>
                  <span className={c.readyCount < c.paragraphCount ? 'export-stale' : 'muted'}>
                    {c.readyCount}/{c.paragraphCount}
                  </span>
                </label>
              ))}
            </div>
          </div>
        )}

        {missing > 0 && (
          <p className="export-stale">
            {missing} paragraph{missing === 1 ? '' : 's'} in{' '}
            {wholeBook ? 'the book' : 'the selection'} have no audio yet -{' '}
            {asIs || latents
              ? "they'll be in the text but not read aloud."
              : 'the export generates them first and waits for them (it shows on the Jobs page).'}
          </p>
        )}

        <div className="export-buttons">
          <button
            className="primary-button"
            onClick={handleBuild}
            disabled={chosen.length === 0 || createExport.isPending}
          >
            <RiDownload2Line /> Build export
          </button>
          <span className="muted">or split the whole book into</span>
          <input
            type="number"
            className="export-parts"
            min={2}
            max={Math.max(2, chapters.length)}
            value={parts}
            onChange={(ev) => setParts(Math.max(2, Number(ev.target.value) || 2))}
          />
          <button
            className="option-chip"
            onClick={handleSplit}
            disabled={chapters.length < 2 || createExport.isPending}
          >
            parts
          </button>
        </div>
        <p className="muted">
          Each export runs as a task on the Jobs page. Building the same contents again replaces
          that export. Exports stay on the server until you delete them, so downloading again is
          instant; one marked out of date no longer matches the book.
        </p>
      </section>
    </div>
  )
}
