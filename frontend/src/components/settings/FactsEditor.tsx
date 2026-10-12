import { useState, type ReactNode } from 'react'
import { RiAddLine, RiDeleteBinLine } from 'react-icons/ri'
import { ApiError } from '../../api/client'
import {
  VOICE_EFFECTS,
  VOICE_EFFECT_LABELS,
  WORD_CLASSES,
  WORD_CLASS_LABELS,
  type UpsertWorldFactRequest,
  type VoiceEffect,
  type WordClass,
  type WorldFact,
} from '../../api/types'

type MutateCallbacks = { onSuccess?: () => void; onError?: (err: unknown) => void }

// The form and table both facts pages share (GlobalFactsPanel on the
// Settings page, BookFactsPanel on a book's): `facts` are the ones this
// page edits; `inherited`, on a book's page, are the global ones under
// them, read-only, each with a button that starts a fact of the page's own
// under the same word. A fact can also say what sort of word it is filed
// under - a title, role, race, nationality or place of the world - which
// the roster then never reads as someone's name; a fact with a class
// needs no text, and can have both (a race with its lifespan and accent).
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
  const [wordClass, setWordClass] = useState<WordClass | ''>('')
  const [effect, setEffect] = useState<VoiceEffect | ''>('')
  const [error, setError] = useState<string | null>(null)

  const load = (f?: WorldFact, id?: string) => {
    setEditingId(id)
    setKey(f?.key ?? '')
    setAliases(f?.aliases.join(', ') ?? '')
    setFact(f?.fact ?? '')
    setWordClass(f?.class ?? '')
    setEffect(f?.effect ?? '')
  }
  const canSave =
    key.trim() !== '' && (wordClass !== '' || effect !== '' || fact.trim() !== '') && !saving
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
        class: wordClass || undefined,
        effect: effect || undefined,
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

  const factText = (f: WorldFact) => (
    <>
      {f.class && (
        <span
          className="muted"
          title={
            f.usedBy?.length
              ? `${f.usedBy.length} names use "${f.key}":\n${f.usedBy.join('\n')}`
              : undefined
          }
        >
          {WORD_CLASS_LABELS[f.class]}
          {f.fact && '. '}
        </span>
      )}
      {f.fact}
      {f.effect && <span className="muted"> Voice effect: {VOICE_EFFECT_LABELS[f.effect]}.</span>}
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
        <label className="fact-form-class">
          Kind of word{' '}
          <select
            value={wordClass}
            onChange={(e) => setWordClass(e.target.value as WordClass | '')}
            aria-label="What sort of word this is"
            title="What sort of word this is, if it is one the roster must never read as someone's name: a title (Prognugator, Ser), a role (Baker), a race (Drake), a nationality (Pallassian) or a place (Pallass). The word can be several words (Wall Lord). A character named only by it is not merged into someone whose name has it, and a name made only of roles, races and nationalities is treated as a role. Needs no fact text, but can have it."
          >
            <option value="">Not a kind of word</option>
            {WORD_CLASSES.map((c) => (
              <option key={c} value={c}>
                {WORD_CLASS_LABELS[c]}
              </option>
            ))}
          </select>
        </label>
        <label className="fact-form-class">
          Voice effect{' '}
          <select
            value={effect}
            onChange={(e) => setEffect(e.target.value as VoiceEffect | '')}
            aria-label="Voice effect for characters of this kind"
            title="An effect put on every line of a character whose race is this word, after the line is generated: what a voice model won't produce from a casting. Changing it re-narrates their lines."
          >
            <option value="">No voice effect</option>
            {VOICE_EFFECTS.map((e) => (
              <option key={e} value={e}>
                {VOICE_EFFECT_LABELS[e]}
              </option>
            ))}
          </select>
        </label>
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
                <td>{factText(f)}</td>
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
                    {factText(f)}
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
