import { useState } from 'react'
import { ApiError } from '../../api/client'
import { useKindAccents, useSetKindAccent } from '../../api/queries'
import type { KindAccents as Kind } from '../../api/types'

function names(list: { name: string }[], limit = 6): string {
  const shown = list.slice(0, limit).map((c) => c.name)
  return list.length > limit
    ? `${shown.join(', ')} and ${list.length - limit} more`
    : shown.join(', ')
}

// How the characters of each kind in this series are cast, by accent - so
// a people that is one culture can be made to sound like one. Kinds cast
// with more than one accent are listed; choosing an accent records it in
// the series' fact for the kind and recasts only the characters cast
// otherwise. The text never names an accent for a people, so which one is
// the reader's call - as is whether the odd ones out are mistakes or
// people from somewhere else.
export function KindAccents({ bookId }: { bookId: string }) {
  const query = useKindAccents(bookId)
  const set = useSetKindAccent(bookId)
  const [showUniform, setShowUniform] = useState(false)
  // An accent typed in for a kind, instead of one of those in use.
  const [other, setOther] = useState<Record<string, string>>({})
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const kinds = query.data ?? []
  const mixed = kinds.filter((k) => k.accents.length > 1)
  const uniform = kinds.filter((k) => k.accents.length === 1)
  if (kinds.length === 0) return null

  const choose = (k: Kind, accent: string) => {
    const others = k.accents
      .filter((g) => g.accent.toLowerCase() !== accent.toLowerCase())
      .flatMap((g) => g.characters)
    const recast =
      others.length === 0
        ? 'Nobody is cast otherwise, so nobody is recast.'
        : `${others.length} ${others.length === 1 ? 'character is' : 'characters are'} cast otherwise and will be recast, replacing their voice and the audio of their lines: ${names(others, 12)}.`
    if (!confirm(`Record that every ${k.key} speaks with a ${accent} accent? ${recast}`)) return
    setError(null)
    setNotice(null)
    set.mutate(
      { key: k.key, accent },
      {
        onSuccess: (saved) =>
          setNotice(
            `Recorded for "${k.key}". ${saved.recast} ${saved.recast === 1 ? 'character is' : 'characters are'} being recast.`,
          ),
        onError: (err) => setError(err instanceof ApiError ? err.message : 'Could not save that'),
      },
    )
  }

  const row = (k: Kind) => (
    <li key={k.key}>
      <div className="roster-review-what">
        <strong>{k.key}</strong>
        <span className="muted">{k.members} cast</span>
      </div>
      {k.fact && <div className="muted">{k.fact}</div>}
      {k.accents.map((g, i) => (
        <div key={g.accent} className="kind-accent-group">
          <button
            type="button"
            className="option-chip"
            onClick={() => choose(k, g.accent)}
            disabled={set.isPending}
            title={`Make ${g.accent} the accent of every ${k.key}`}
          >
            {g.accent} · {g.characters.length}
          </button>
          {/* The majority needs no naming; the odd ones out do. */}
          {(i > 0 || k.accents.length === 1) && (
            <span className="muted">{names(g.characters)}</span>
          )}
        </div>
      ))}
      <form
        className="kind-accent-group"
        onSubmit={(e) => {
          e.preventDefault()
          const accent = (other[k.key] ?? '').trim()
          if (accent) choose(k, accent)
        }}
      >
        <input
          placeholder="Another accent (e.g. Welsh)"
          value={other[k.key] ?? ''}
          onChange={(e) => setOther({ ...other, [k.key]: e.target.value })}
          aria-label={`Another accent for ${k.key}`}
        />
        <button
          type="submit"
          className="text-button"
          disabled={set.isPending || !(other[k.key] ?? '').trim()}
        >
          Use for all
        </button>
      </form>
    </li>
  )

  return (
    <section className="lexicon-panel">
      <h2>Accents by kind</h2>
      <p className="muted">
        How the characters of each kind are cast today. Pick an accent to make it that kind&rsquo;s:
        it is written into the kind&rsquo;s fact for this series, and the characters cast with
        another accent are recast. Someone from elsewhere may be the odd one out for a reason - give
        them a fact of their own first.
      </p>
      {error && <p className="error-text">{error}</p>}
      {notice && <p className="muted">{notice}</p>}
      {mixed.length === 0 && <p className="muted">Every kind is cast with one accent.</p>}
      {mixed.length > 0 && <ul className="roster-review-list">{mixed.map(row)}</ul>}
      {uniform.length > 0 && (
        <>
          <button
            type="button"
            className="text-button"
            onClick={() => setShowUniform(!showUniform)}
          >
            {showUniform ? 'Hide' : 'Show'} the {uniform.length} already cast with one accent
          </button>
          {showUniform && <ul className="roster-review-list">{uniform.map(row)}</ul>}
        </>
      )}
    </section>
  )
}
