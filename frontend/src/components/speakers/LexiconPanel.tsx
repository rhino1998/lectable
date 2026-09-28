import { useState } from 'react'
import { RiAddLine, RiDeleteBinLine } from 'react-icons/ri'
import { ApiError } from '../../api/client'
import { useDeleteLexiconEntry, useLexicon, useUpsertLexiconEntry } from '../../api/queries'
import type { LexiconEntry } from '../../api/types'

// The Speakers page's pronunciation lexicon: listener-authored "say X as Y"
// rules for names and coinages the TTS keeps getting wrong. Shared across
// the book's whole series (like the roster), matched as whole words, and
// applied at generation time on top of - and overriding - the LLM
// pronunciation pass (backend pronounce.LexiconSubstitutions). Saving or
// removing a rule clears audio for every paragraph it matches, so those
// re-render with the new pronunciation; the reader's annotations view marks
// each match like any other pronunciation fix.
export function LexiconPanel({ bookId, seriesName }: { bookId: string; seriesName?: string }) {
  const lexiconQuery = useLexicon(bookId)
  const upsert = useUpsertLexiconEntry(bookId)
  const remove = useDeleteLexiconEntry(bookId)
  const [term, setTerm] = useState('')
  const [replacement, setReplacement] = useState('')
  const [caseSensitive, setCaseSensitive] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const entries = lexiconQuery.data ?? []

  const save = (body: { term: string; replacement: string; caseSensitive: boolean }) => {
    setError(null)
    upsert.mutate(body, {
      onSuccess: () => {
        setTerm('')
        setReplacement('')
        setCaseSensitive(false)
      },
      onError: (err) =>
        setError(err instanceof ApiError ? err.message : 'Could not save this rule'),
    })
  }

  const runDelete = (e: LexiconEntry) => {
    if (
      !confirm(
        `Remove "${e.term}"? Audio for its ${e.matches} matching paragraph(s) will regenerate.`,
      )
    )
      return
    setError(null)
    remove.mutate(e.id, {
      onError: (err) =>
        setError(err instanceof ApiError ? err.message : 'Could not remove this rule'),
    })
  }

  const canSave = term.trim() !== '' && replacement.trim() !== '' && !upsert.isPending

  return (
    <section className="lexicon-panel">
      <div className="library-header">
        <h2>Pronunciations</h2>
      </div>
      <p className="muted">
        Say a word the way you spell it here - whole words only, overriding the automatic
        pronunciation pass. Shared by every book in{' '}
        {seriesName ? `the "${seriesName}" series` : 'this book'}; saving a rule regenerates the
        audio of every line it matches.
      </p>
      {error && <p className="error-text">{error}</p>}
      <form
        className="lexicon-form"
        onSubmit={(e) => {
          e.preventDefault()
          if (canSave) save({ term, replacement, caseSensitive })
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
        <button className="icon-action-button" type="submit" disabled={!canSave} title="Save rule">
          <RiAddLine className={upsert.isPending ? 'spin' : undefined} />
        </button>
      </form>
      {lexiconQuery.isError && <p className="error-text">Could not load the lexicon.</p>}
      {entries.length > 0 && (
        <table className="lexicon-table">
          <thead>
            <tr>
              <th>Word</th>
              <th>Said as</th>
              <th title="Paragraphs in this book the rule applies to">Matches</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {entries.map((e) => (
              <tr key={e.id}>
                <td>
                  {e.term}
                  {e.caseSensitive && <span className="muted"> (case)</span>}
                </td>
                <td>{e.replacement}</td>
                <td className={e.matches === 0 ? 'muted' : undefined}>{e.matches}</td>
                <td>
                  <button
                    className="icon-action-button"
                    onClick={() => {
                      setTerm(e.term)
                      setReplacement(e.replacement)
                      setCaseSensitive(e.caseSensitive)
                    }}
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
