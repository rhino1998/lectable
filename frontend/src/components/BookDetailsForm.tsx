import { useState } from 'react'
import { ApiError } from '../api/client'
import { useRenameBook } from '../api/queries'

// The title and author the library shows a book under (Book settings →
// General). An import names a book as its source does ("He Who Fights with
// Monsters 2: A LitRPG Adventure"), which isn't always what its reader
// calls it. Nothing else depends on either, so renaming touches no audio.
export function BookDetailsForm(props: { bookId: string; title: string; author: string }) {
  const rename = useRenameBook(props.bookId)
  const [title, setTitle] = useState(props.title)
  const [author, setAuthor] = useState(props.author)
  const [error, setError] = useState<string | null>(null)
  // Follows a rename made elsewhere while nothing here is being edited.
  const [seen, setSeen] = useState({ title: props.title, author: props.author })
  if (seen.title !== props.title || seen.author !== props.author) {
    if (title === seen.title) setTitle(props.title)
    if (author === seen.author) setAuthor(props.author)
    setSeen({ title: props.title, author: props.author })
  }
  const changed = title.trim() !== props.title || author.trim() !== props.author

  return (
    <form
      className="book-details-form"
      onSubmit={(e) => {
        e.preventDefault()
        setError(null)
        rename.mutate(
          { title: title.trim(), author: author.trim() },
          {
            onError: (err) => setError(err instanceof ApiError ? err.message : 'Could not rename'),
          },
        )
      }}
    >
      <label>
        Title
        <input type="text" value={title} onChange={(e) => setTitle(e.target.value)} />
      </label>
      <label>
        Author
        <input type="text" value={author} onChange={(e) => setAuthor(e.target.value)} />
      </label>
      <button
        className="primary-button"
        type="submit"
        disabled={!changed || !title.trim() || rename.isPending}
      >
        {rename.isPending ? 'Saving…' : 'Rename'}
      </button>
      {error && <span className="error-text">{error}</span>}
    </form>
  )
}
