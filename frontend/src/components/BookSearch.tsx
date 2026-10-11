import { useEffect, useRef, useState } from 'react'
import { RiCloseLine, RiSearchLine } from 'react-icons/ri'
import { useSearchBook } from '../api/queries'
import { useClickOutside } from '../hooks/useClickOutside'
import type { SearchResult } from '../api/types'

const SEARCH_DEBOUNCE_MS = 300
// Matches the backend's searchResultLimit - purely to decide whether to
// show the "showing first N" note, not enforced client-side.
const RESULT_LIMIT = 200
const SNIPPET_RADIUS = 60

// Renders `text` as a snippet centered on the first match of `query`
// (case-insensitive), so a long paragraph's match isn't buried off-screen,
// with the match itself wrapped in <mark>.
function snippet(text: string, query: string) {
  const idx = text.toLowerCase().indexOf(query.toLowerCase())
  if (idx === -1) return text

  const start = Math.max(0, idx - SNIPPET_RADIUS)
  const end = Math.min(text.length, idx + query.length + SNIPPET_RADIUS)
  return (
    <>
      {start > 0 && '…'}
      {text.slice(start, idx)}
      <mark>{text.slice(idx, idx + query.length)}</mark>
      {text.slice(idx + query.length, end)}
      {end < text.length && '…'}
    </>
  )
}

// A toggleable search-this-book panel: type to find paragraphs containing
// a phrase, click a result to jump playback there (same "click a
// paragraph" jump ReaderPage already does for the paragraph list itself).
// "By meaning" finds the paragraphs closest to what was typed instead -
// a description of a passage rather than its words - best first, in a
// book whose chapters have been embedded (a preprocessing phase).
export function BookSearch({
  bookId,
  onJump,
  align = 'below',
}: {
  bookId: string
  onJump: (chapterIdx: number, paragraphIdx: number) => void
  // Which side of the toggle button the results panel opens toward - the
  // reader header has room below it, but the player bar sits at the
  // bottom of the viewport so its instance needs to open upward instead.
  align?: 'below' | 'above'
}) {
  const [open, setOpen] = useState(false)
  const [input, setInput] = useState('')
  const [query, setQuery] = useState('')
  const [byMeaning, setByMeaning] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)
  const containerRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const t = setTimeout(() => setQuery(input), SEARCH_DEBOUNCE_MS)
    return () => clearTimeout(t)
  }, [input])

  useEffect(() => {
    if (open) inputRef.current?.focus()
  }, [open])

  const searchQuery = useSearchBook(bookId, query, byMeaning)
  const results = searchQuery.data ?? []
  const showResults = query.trim().length > 1

  const close = () => {
    setOpen(false)
    setInput('')
    setQuery('')
  }

  const jump = (r: SearchResult) => {
    onJump(r.chapterIdx, r.paragraphIdx)
    close()
  }

  useClickOutside([containerRef], close, open)

  return (
    <div className="book-search" ref={containerRef}>
      <button
        className={'text-button-icon' + (open ? ' text-button-icon-active' : '')}
        onClick={() => setOpen((v) => !v)}
      >
        <RiSearchLine /> Search
      </button>

      {open && (
        <div className={'popover-panel' + (align === 'above' ? ' popover-panel-above' : '')}>
          <div className="book-search-input-row">
            <input
              ref={inputRef}
              type="text"
              placeholder={byMeaning ? 'Describe what you are looking for…' : 'Search this book…'}
              value={input}
              onChange={(e) => setInput(e.target.value)}
              onKeyDown={(e) => e.key === 'Escape' && close()}
            />
            <button
              className={'text-button-icon' + (byMeaning ? ' text-button-icon-active' : '')}
              onClick={() => setByMeaning((v) => !v)}
              title="Find passages by what they say rather than their exact words"
            >
              By meaning
            </button>
            <button className="book-search-close" onClick={close} title="Close search">
              <RiCloseLine />
            </button>
          </div>

          {showResults && (
            <div className="book-search-results">
              {searchQuery.isLoading && <p className="muted">Searching…</p>}
              {searchQuery.isError && <p className="error-text">Search failed.</p>}
              {!searchQuery.isLoading && !searchQuery.isError && results.length === 0 && (
                <p className="muted">
                  {byMeaning
                    ? 'Nothing found. This book may not be preprocessed yet.'
                    : 'No matches.'}
                </p>
              )}
              {results.map((r) => (
                <button
                  key={`${r.chapterIdx}:${r.paragraphIdx}`}
                  className="book-search-result"
                  onClick={() => jump(r)}
                >
                  <span className="book-search-result-chapter">{r.chapterTitle}</span>
                  <span className="book-search-result-snippet">{snippet(r.text, query)}</span>
                </button>
              ))}
              {!byMeaning && results.length >= RESULT_LIMIT && (
                <p className="muted book-search-truncated">Showing first {RESULT_LIMIT} matches.</p>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  )
}
