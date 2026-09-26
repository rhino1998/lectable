import { useCallback } from 'react'
import { ApiError } from '../api/client'
import { useReimportChapter } from '../api/queries'

// Re-import: re-parse one chapter from the book's stored source epub
// (api.reimportChapter), shared by the reader's chapter menu and the
// Speakers page's chapter rows. Two recoverable failures - the backend has
// no stored epub for an older book (ask for the file, which it then keeps),
// or the epub's chapter title differs (confirm, then force). Any other
// failure goes to onError. reimportingIdx is the chapter currently being
// re-imported, or null.
export function useReimportChapterAction(
  bookId: string,
  onError: (message: string | null) => void,
) {
  const reimportChapter = useReimportChapter(bookId)

  const run = useCallback(
    (idx: number, title: string) => {
      if (
        !confirm(
          `Re-import "${title}" from the book's epub? Its speakers, emotions, pronunciation fixes, sound effects and generated audio are all reset, and the chapter needs preprocessing again. Other chapters are untouched.`,
        )
      ) {
        return
      }
      onError(null)
      const attempt = (file?: File, force?: boolean) =>
        reimportChapter.mutate(
          { chapterIdx: idx, file, force },
          {
            onError: (err) => {
              if (err instanceof ApiError && err.code === 'no_source_epub') {
                const input = document.createElement('input')
                input.type = 'file'
                input.accept = '.epub,application/epub+zip'
                input.onchange = () => {
                  const picked = input.files?.[0]
                  if (picked) attempt(picked, force)
                }
                input.click()
                return
              }
              if (err instanceof ApiError && err.code === 'title_mismatch') {
                if (confirm(`${err.message}. Re-import anyway?`)) attempt(file, true)
                return
              }
              onError(err instanceof ApiError ? err.message : 'Could not re-import this chapter')
            },
          },
        )
      attempt()
    },
    [reimportChapter, onError],
  )

  const reimportingIdx = reimportChapter.isPending
    ? (reimportChapter.variables?.chapterIdx ?? null)
    : null
  return { run, reimportingIdx }
}
