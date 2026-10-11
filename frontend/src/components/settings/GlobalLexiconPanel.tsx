import { useState } from 'react'
import { RiAddLine, RiDeleteBinLine } from 'react-icons/ri'
import { ApiError } from '../../api/client'
import {
  useDeleteGlobalLexiconEntry,
  useGlobalLexicon,
  useUpsertGlobalLexiconEntry,
} from '../../api/queries'
import type { GlobalLexiconEntry } from '../../api/types'

// The Settings page's pronunciations: the "say X as Y" rules that apply to
// every book - the ones a book's own Pronunciations tab saves with "All
// books" (LexiconPanel), listed and edited here without going through a
// book. There is no listen button or match count: both are one book's.
export function GlobalLexiconPanel() {
  const lexiconQuery = useGlobalLexicon()
  const upsert = useUpsertGlobalLexiconEntry()
  const remove = useDeleteGlobalLexiconEntry()
  const [term, setTerm] = useState('')
  const [replacement, setReplacement] = useState('')
  const [caseSensitive, setCaseSensitive] = useState(false)
  // Also cover words starting with the term ("gaol" -> "gaoler").
  const [prefix, setPrefix] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const entries = lexiconQuery.data ?? []

  const load = (e?: GlobalLexiconEntry) => {
    setTerm(e?.term ?? '')
    setReplacement(e?.replacement ?? '')
    setCaseSensitive(e?.caseSensitive ?? false)
    setPrefix(e?.prefix ?? false)
  }
  const canSave = term.trim() !== '' && replacement.trim() !== '' && !upsert.isPending
  const save = () => {
    setError(null)
    upsert.mutate(
      { term, replacement, caseSensitive, prefix, global: true },
      {
        onSuccess: () => load(),
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Could not save this rule'),
      },
    )
  }
  const runDelete = (e: GlobalLexiconEntry) => {
    if (!confirm(`Remove "${e.term}"? Audio for every line it matches will regenerate.`)) return
    setError(null)
    remove.mutate(e.id, {
      onError: (err) =>
        setError(err instanceof ApiError ? err.message : 'Could not remove this rule'),
    })
  }

  return (
    <section className="lexicon-panel">
      <h2>Pronunciations</h2>
      <p className="muted">
        Say a word the way you spell it here, in every book - whole words, or with "Derived words"
        any word starting with it. A series' own rule for the same word wins in its books. Saving or
        removing a rule regenerates the audio of every line it matches, across the library; to hear
        a rule first, or see what it matches, write it on a book's Pronunciations tab.
      </p>
      {error && <p className="error-text">{error}</p>}
      <form
        className="lexicon-form"
        onSubmit={(e) => {
          e.preventDefault()
          if (canSave) save()
        }}
      >
        <input
          placeholder="Word (e.g. Nyx)"
          value={term}
          onChange={(e) => setTerm(e.target.value)}
          aria-label="Word"
        />
        <span className="muted">→</span>
        <input
          placeholder="Say it as (e.g. Nicks)"
          value={replacement}
          onChange={(e) => setReplacement(e.target.value)}
          aria-label="Pronunciation"
        />
        <label className="checkbox-label">
          <input
            type="checkbox"
            checked={caseSensitive}
            onChange={(e) => setCaseSensitive(e.target.checked)}
          />
          Match case
        </label>
        <label
          className="checkbox-label"
          title="Also respell words that start with it - gaol also covers gaoler, gaols, gaolbird"
        >
          <input type="checkbox" checked={prefix} onChange={(e) => setPrefix(e.target.checked)} />
          Derived words
        </label>
        <button className="icon-action-button" type="submit" disabled={!canSave} title="Save rule">
          <RiAddLine className={upsert.isPending ? 'spin' : undefined} />
        </button>
      </form>
      {lexiconQuery.isError && <p className="error-text">Could not load the rules.</p>}
      {lexiconQuery.data && entries.length === 0 && <p className="muted">No rules yet.</p>}
      {entries.length > 0 && (
        <table className="lexicon-table">
          <thead>
            <tr>
              <th>Word</th>
              <th>Said as</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {entries.map((e) => (
              <tr key={e.id}>
                <td>
                  {e.term}
                  {e.prefix && <span className="muted">…</span>}
                  {e.caseSensitive && <span className="muted"> (case)</span>}
                </td>
                <td>{e.replacement}</td>
                <td>
                  <button
                    className="icon-action-button"
                    onClick={() => load(e)}
                    title="Edit - loads this rule into the form above"
                  >
                    ✎
                  </button>
                  <button
                    className="icon-action-button"
                    onClick={() => runDelete(e)}
                    disabled={remove.isPending}
                    title="Remove rule"
                  >
                    <RiDeleteBinLine />
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}
