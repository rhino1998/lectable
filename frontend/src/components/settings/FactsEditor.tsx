import { useState, type ReactNode } from 'react'
import { RiAddLine, RiDeleteBinLine } from 'react-icons/ri'
import { ApiError } from '../../api/client'
import type { UpsertWorldFactRequest, WorldFact } from '../../api/types'

type MutateCallbacks = { onSuccess?: () => void; onError?: (err: unknown) => void }

// The form and table both facts pages share (GlobalFactsPanel on the
// Settings page, BookFactsPanel on a book's): `facts` are the ones this
// page edits; `inherited`, on a book's page, are the global ones under
// them, read-only, each with a button that starts a fact of the page's own
// under the same word.
export function FactsEditor({
  intro,
  facts,
  inherited = [],
  loaded,
  loadError,
  saving,
  removing,
  onSave,
  onDelete,
}: {
  intro: ReactNode
  facts: WorldFact[]
  inherited?: WorldFact[]
  loaded: boolean
  loadError: boolean
  saving: boolean
  removing: boolean
  onSave: (body: UpsertWorldFactRequest, cb: MutateCallbacks) => void
  onDelete: (factId: string, cb: MutateCallbacks) => void
}) {
  // The fact being edited, undefined while adding a new one.
  const [editingId, setEditingId] = useState<string>()
  const [key, setKey] = useState('')
  const [aliases, setAliases] = useState('')
  const [fact, setFact] = useState('')
  const [error, setError] = useState<string | null>(null)

  const load = (f?: WorldFact, id?: string) => {
    setEditingId(id)
    setKey(f?.key ?? '')
    setAliases(f?.aliases.join(', ') ?? '')
    setFact(f?.fact ?? '')
  }
  const canSave = key.trim() !== '' && fact.trim() !== '' && !saving
  const save = () => {
    setError(null)
    onSave(
      {
        id: editingId,
        key,
        aliases: aliases
          .split(',')
          .map((a) => a.trim())
          .filter(Boolean),
        fact,
      },
      {
        onSuccess: () => load(),
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Could not save this fact'),
      },
    )
  }
  const runDelete = (f: WorldFact) => {
    if (!confirm(`Remove the fact about "${f.key}"?`)) return
    setError(null)
    onDelete(f.id, {
      onSuccess: () => f.id === editingId && load(),
      onError: (err) =>
        setError(err instanceof ApiError ? err.message : 'Could not remove this fact'),
    })
  }
  const words = (f: WorldFact) => (
    <>
      {f.key}
      {f.aliases.length > 0 && (
        <div className="muted lexicon-matched-words">{f.aliases.join(', ')}</div>
      )}
    </>
  )

  return (
    <section className="lexicon-panel">
      <h2>Facts</h2>
      <p className="muted">{intro}</p>
      {error && <p className="error-text">{error}</p>}
      <form
        className="lexicon-form"
        onSubmit={(e) => {
          e.preventDefault()
          if (canSave) save()
        }}
      >
        <input
          placeholder="Word (e.g. dwarf)"
          value={key}
          onChange={(e) => setKey(e.target.value)}
          aria-label="Word"
        />
        <input
          placeholder="Also (e.g. dwarven, dwarves)"
          value={aliases}
          onChange={(e) => setAliases(e.target.value)}
          aria-label="Other words that bring it up, comma-separated"
          title="Other words that bring this fact up, comma-separated"
        />
        <input
          className="fact-form-fact"
          placeholder="Fact (e.g. Scottish accent)"
          value={fact}
          onChange={(e) => setFact(e.target.value)}
          aria-label="Fact"
        />
        <button
          className="icon-action-button"
          type="submit"
          disabled={!canSave}
          title={editingId ? 'Save changes' : 'Add fact'}
        >
          <RiAddLine className={saving ? 'spin' : undefined} />
        </button>
        {editingId && (
          <button className="text-button" type="button" onClick={() => load()}>
            Cancel edit
          </button>
        )}
      </form>
      {loadError && <p className="error-text">Could not load the facts.</p>}
      {loaded && facts.length === 0 && <p className="muted">No facts yet.</p>}
      {facts.length > 0 && (
        <table className="lexicon-table">
          <thead>
            <tr>
              <th>Word</th>
              <th>Fact</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {facts.map((f) => (
              <tr key={f.id}>
                <td>{words(f)}</td>
                <td>{f.fact}</td>
                <td>
                  <button
                    className="icon-action-button"
                    onClick={() => load(f, f.id)}
                    title="Edit - loads this fact into the form above"
                  >
                    ✎
                  </button>
                  <button
                    className="icon-action-button"
                    onClick={() => runDelete(f)}
                    disabled={removing}
                    title="Remove fact"
                  >
                    <RiDeleteBinLine />
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {inherited.length > 0 && (
        <>
          <h3>From every book</h3>
          <table className="lexicon-table">
            <tbody>
              {inherited.map((f) => (
                <tr key={f.id} className={f.superseded ? 'fact-superseded' : undefined}>
                  <td>{words(f)}</td>
                  <td>
                    {f.fact}
                    {f.superseded && <div className="muted">Replaced here by a fact above.</div>}
                  </td>
                  <td>
                    {!f.superseded && (
                      <button
                        className="text-button"
                        onClick={() => load(f)}
                        title="Start a fact of this series' own under the same word, which replaces this one here"
                      >
                        Override
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </section>
  )
}
