import { useRef, useState } from 'react'
import type { ImportChapterSource, ImportPreviewChapter, ImportPreviewResponse } from '../api/types'
import { countBySource, selectRange } from '../utils/importSelection'

// The chapter picker for an import by link: the work's chapter list as the
// backend previewed it (httpapi.handlePreviewImportURL), each chapter with
// where its text would come from, to tick or untick before importing.
//
// The list can be widened from here - the Internet Archive searched for
// chapters the site has delisted, an epub attached to stand in for
// chapters the site won't serve - which the page does by asking for the
// preview again; `busy` names which of those is running.

const SOURCE_LABELS: Record<ImportChapterSource, string> = {
  site: '',
  archive: 'archive',
  epub: 'epub',
  locked: 'locked',
}

const SOURCE_TITLES: Record<ImportChapterSource, string> = {
  site: '',
  archive: 'No longer listed by the site - read from the Internet Archive’s copy',
  epub: 'Taken from the attached epub',
  locked: 'Listed but not readable (early access) - an import stops at it',
}

// The hover text for a chapter's row. A chapter with replacedBy is one the
// site has put something else in the place of (a notice, a summary): the
// archive's copy of the chapter is listed just ahead of what replaced it.
function chapterNote(c: ImportPreviewChapter): string {
  if (c.replacedBy) {
    return `The site now has “${c.replacedBy}” in this chapter’s place (the next entry). This is the chapter as the Internet Archive saved it before.`
  }
  return SOURCE_TITLES[c.source]
}

export type ImportPreviewBusy = 'archive' | 'epub' | 'import' | null

export function ImportPreviewPanel(props: {
  preview: ImportPreviewResponse
  selected: Set<string>
  onSelect: (selected: Set<string>) => void
  busy: ImportPreviewBusy
  onSearchArchive: () => void
  onAttachEpub: (file: File) => void
  onImport: () => void
  onClose: () => void
}) {
  const { preview, selected, onSelect, busy } = props
  const chapters = preview.chapters
  const epubInputRef = useRef<HTMLInputElement>(null)
  const [from, setFrom] = useState('1')
  const [to, setTo] = useState(String(chapters.length))

  const all = countBySource(chapters)
  const chosen = countBySource(chapters, selected)
  const chosenTotal = selected.size
  const parts = [
    `${all.site} from ${preview.site}`,
    all.archive > 0 && `${all.archive} from the Internet Archive`,
    all.epub > 0 && `${all.epub} from the epub`,
    all.locked > 0 && `${all.locked} locked`,
  ].filter(Boolean)

  const toggle = (id: string) => {
    const next = new Set(selected)
    if (!next.delete(id)) next.add(id)
    onSelect(next)
  }

  return (
    <div className="import-preview">
      <div className="import-preview-head">
        <div>
          <strong>{preview.title}</strong>
          {preview.author && <span className="muted"> by {preview.author}</span>}
          <div className="muted import-preview-summary">
            {chapters.length} chapters: {parts.join(', ')}
          </div>
        </div>
        <button type="button" className="text-button" onClick={props.onClose}>
          Close
        </button>
      </div>

      {preview.existingBookId && (
        <p className="error-text">
          This is already in the library - check it for new chapters instead.
        </p>
      )}

      <div className="import-preview-tools">
        {preview.canSearchArchive && (
          <button
            type="button"
            className="option-chip"
            title="Chapters the site has taken down (a stubbed fiction) may still be in the Internet Archive. This can take a minute."
            onClick={props.onSearchArchive}
            disabled={busy !== null}
          >
            {busy === 'archive'
              ? 'Searching the Internet Archive… (up to a minute)'
              : 'Find delisted chapters in the Internet Archive'}
          </button>
        )}
        {preview.archiveSearched && all.archive === 0 && (
          <span className="muted">The Internet Archive has no chapters the site doesn’t list.</span>
        )}
        <button
          type="button"
          className="option-chip"
          title="An epub of the published part: its chapters stand in for ones the site won’t serve, matched by title"
          onClick={() => epubInputRef.current?.click()}
          disabled={busy !== null}
        >
          {busy === 'epub'
            ? 'Reading the epub…'
            : preview.epubChapters > 0
              ? `Epub attached (${preview.epubChapters} chapters) - replace…`
              : 'Attach an epub…'}
        </button>
        <input
          ref={epubInputRef}
          type="file"
          accept=".epub"
          hidden
          onChange={(e) => {
            const file = e.target.files?.[0]
            if (file) props.onAttachEpub(file)
            e.target.value = ''
          }}
        />
      </div>

      <div className="import-preview-tools">
        <button
          type="button"
          className="option-chip"
          onClick={() => onSelect(new Set(chapters.map((c) => c.id)))}
        >
          All
        </button>
        <button type="button" className="option-chip" onClick={() => onSelect(new Set())}>
          None
        </button>
        <label className="import-preview-range">
          From
          <input
            type="number"
            min={1}
            max={chapters.length}
            value={from}
            onChange={(e) => setFrom(e.target.value)}
          />
        </label>
        <label className="import-preview-range">
          to
          <input
            type="number"
            min={1}
            max={chapters.length}
            value={to}
            onChange={(e) => setTo(e.target.value)}
          />
        </label>
        <button
          type="button"
          className="option-chip"
          title="Tick exactly the chapters at these positions in the list"
          onClick={() =>
            onSelect(selectRange(chapters, Number(from) || 1, Number(to) || chapters.length))
          }
        >
          Select range
        </button>
      </div>

      <ol className="import-preview-list">
        {chapters.map((c, i) => (
          <li key={c.id}>
            <label title={chapterNote(c) || undefined}>
              <input type="checkbox" checked={selected.has(c.id)} onChange={() => toggle(c.id)} />
              <span className="import-preview-number muted">{i + 1}</span>
              <span className="import-preview-title">{c.title}</span>
              {c.replacedBy && <span className="import-preview-source">replaced on the site</span>}
              {c.suspect && (
                <span
                  className="import-preview-source"
                  title="The title reads like an author’s notice rather than a chapter, so it starts unticked"
                >
                  notice?
                </span>
              )}
              {c.source !== 'site' && (
                <span className={`import-preview-source import-preview-source-${c.source}`}>
                  {SOURCE_LABELS[c.source]}
                </span>
              )}
            </label>
          </li>
        ))}
      </ol>

      <div className="import-preview-foot">
        <button
          type="button"
          className="primary-button"
          onClick={props.onImport}
          disabled={chosenTotal === 0 || busy !== null || !!preview.existingBookId}
        >
          {busy === 'import'
            ? 'Importing…'
            : `Import ${chosenTotal} chapter${chosenTotal === 1 ? '' : 's'}`}
        </button>
        {chosen.locked > 0 && (
          <span className="muted">
            {chosen.locked} ticked {chosen.locked === 1 ? 'chapter is' : 'chapters are'} locked: the
            import will stop at the first.
          </span>
        )}
        {chosen.archive > 0 && (
          <span className="muted">
            Chapters from the Internet Archive arrive slowly - about one a minute.
          </span>
        )}
      </div>
    </div>
  )
}
