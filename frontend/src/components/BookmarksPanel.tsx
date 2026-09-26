import { useRef, useState } from 'react'
import { RiBookmarkLine, RiCheckLine, RiDeleteBinLine, RiEditLine } from 'react-icons/ri'
import { useBookmarks, useDeleteBookmark, useUpdateBookmarkNote } from '../api/queries'
import { useClickOutside } from '../hooks/useClickOutside'
import type { Bookmark } from '../api/types'

// Toggleable list of a book's bookmarked paragraphs - bookmarking itself
// happens inline per-paragraph in ReaderPage (the small bookmark icon next
// to each paragraph); this panel is where the resulting list is reviewed,
// jumped to, annotated, or removed.
export function BookmarksPanel({
  bookId,
  onJump,
  align = 'below',
}: {
  bookId: string
  onJump: (chapterIdx: number, paragraphIdx: number) => void
  align?: 'below' | 'above'
}) {
  const [open, setOpen] = useState(false)
  const containerRef = useRef<HTMLDivElement>(null)
  const bookmarksQuery = useBookmarks(bookId)
  const deleteBookmark = useDeleteBookmark(bookId)
  const updateNote = useUpdateBookmarkNote(bookId)
  const [editingId, setEditingId] = useState<string | null>(null)
  const [noteDraft, setNoteDraft] = useState('')

  const bookmarks = bookmarksQuery.data ?? []

  useClickOutside([containerRef], () => setOpen(false), open)

  const jump = (b: Bookmark) => {
    onJump(b.chapterIdx, b.paragraphIdx)
    setOpen(false)
  }

  const startEdit = (b: Bookmark) => {
    setEditingId(b.id)
    setNoteDraft(b.note)
  }

  const saveNote = (id: string) => {
    updateNote.mutate({ id, note: noteDraft })
    setEditingId(null)
  }

  return (
    <div className="bookmarks-panel" ref={containerRef}>
      <button
        className={'text-button-icon' + (open ? ' text-button-icon-active' : '')}
        onClick={() => setOpen((v) => !v)}
      >
        <RiBookmarkLine /> Bookmarks{bookmarks.length > 0 ? ` (${bookmarks.length})` : ''}
      </button>

      {open && (
        <div className={'popover-panel' + (align === 'above' ? ' popover-panel-above' : '')}>
          {bookmarks.length === 0 && (
            <p className="muted">
              No bookmarks yet — click the bookmark icon next to a paragraph to add one.
            </p>
          )}
          <div className="book-search-results">
            {bookmarks.map((b) => (
              <div key={b.id} className="bookmark-item">
                <button className="book-search-result" onClick={() => jump(b)}>
                  <span className="book-search-result-chapter">{b.chapterTitle}</span>
                  <span className="book-search-result-snippet">{b.text}</span>
                </button>

                {editingId === b.id ? (
                  <div className="bookmark-note-edit">
                    <input
                      value={noteDraft}
                      onChange={(e) => setNoteDraft(e.target.value)}
                      placeholder="Add a note…"
                      autoFocus
                      onKeyDown={(e) => e.key === 'Enter' && saveNote(b.id)}
                    />
                    <button
                      className="book-search-close"
                      onClick={() => saveNote(b.id)}
                      title="Save note"
                    >
                      <RiCheckLine />
                    </button>
                  </div>
                ) : (
                  <div className="bookmark-actions">
                    {b.note && <span className="bookmark-note">{b.note}</span>}
                    <button
                      className="book-search-close"
                      onClick={() => startEdit(b)}
                      title="Edit note"
                    >
                      <RiEditLine />
                    </button>
                    <button
                      className="book-search-close"
                      onClick={() => deleteBookmark.mutate(b.id)}
                      title="Remove bookmark"
                    >
                      <RiDeleteBinLine />
                    </button>
                  </div>
                )}
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}
