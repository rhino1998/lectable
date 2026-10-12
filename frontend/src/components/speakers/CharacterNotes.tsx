import { useState } from 'react'
import { ApiError } from '../../api/client'
import { useCharacterNotes, useRuleCharacterNotes } from '../../api/queries'
import type { CharacterNote } from '../../api/types'

// Where in the series a note's quote is.
function where(n: CharacterNote): string {
  if (n.evidenceChapterIdx == null) return ''
  const chapter = n.evidenceChapterTitle || `Chapter ${n.evidenceChapterIdx + 1}`
  return n.evidenceBookTitle ? `${n.evidenceBookTitle}, ${chapter}` : chapter
}

function label(n: CharacterNote): string {
  return n.kind === 'age' ? `${n.value} years old` : n.value
}

// What the text states of single characters in this series - the kind of
// being someone is, how old they are at some chapter - each with the
// sentence it rests on. "Find facts" proposes them; only an accepted note
// is shown to the casting of that character's voice, beside everything
// else casting reads, and an accepted kind makes them one of that kind
// for the facts and accents above. Proposed kinds are grouped by kind, so
// every goblin found can be accepted at once.
export function CharacterNotes({ bookId }: { bookId: string }) {
  const notes = useCharacterNotes(bookId).data ?? []
  const rule = useRuleCharacterNotes(bookId)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  const run = (ids: string[], accept: boolean) => {
    setError(null)
    setNotice(null)
    rule.mutate(
      { ids, accept },
      {
        onSuccess: (res) => {
          if (res.recast > 0)
            setNotice(
              `${res.recast} ${res.recast === 1 ? 'character is' : 'characters are'} being recast: a fact about their kind now applies to them.`,
            )
        },
        onError: (err) => setError(err instanceof ApiError ? err.message : 'Could not save that'),
      },
    )
  }

  const proposed = notes.filter((n) => n.proposed)
  const accepted = notes.filter((n) => !n.proposed && !n.skipped)
  const skipped = notes.filter((n) => n.skipped)
  // Proposed notes in groups: one per kind, then the ages together.
  const groups: { title: string; notes: CharacterNote[] }[] = []
  for (const n of proposed) {
    const title = n.kind === 'age' ? 'Ages' : n.value
    const last = groups[groups.length - 1]
    if (last && last.title === title) last.notes.push(n)
    else groups.push({ title, notes: [n] })
  }
  if (notes.length === 0) return null

  return (
    <section className="lexicon-panel">
      <h2>Character notes</h2>
      <p className="muted">
        Single things the text says about a character, found by &ldquo;Find facts&rdquo;: what kind
        of being they are and how old they are at some point. Read the quote - a figure of speech or
        someone else&rsquo;s age can slip through. An accepted note is shown to the casting of that
        character&rsquo;s voice, and an accepted kind brings the facts about that kind with it,
        which recasts them if a fact now applies that did not before.
      </p>
      {error && <p className="error-text">{error}</p>}
      {notice && <p className="muted">{notice}</p>}
      {groups.map((g) => (
        <div key={g.title} className="kind-accent-group">
          <div className="roster-review-what">
            <strong>{g.title}</strong>{' '}
            <span className="muted">
              {g.notes.length} {g.notes.length === 1 ? 'proposal' : 'proposals'}
            </span>{' '}
            {g.notes.length > 1 && (
              <button
                type="button"
                className="option-chip"
                disabled={rule.isPending}
                onClick={() =>
                  run(
                    g.notes.map((n) => n.id),
                    true,
                  )
                }
              >
                Accept all {g.notes.length}
              </button>
            )}
          </div>
          <ul className="roster-review-list">
            {g.notes.map((n) => (
              <li key={n.id}>
                <div className="roster-review-what">
                  <strong>{n.characterName}</strong>: {label(n)}
                </div>
                {n.evidence && (
                  <blockquote className="roster-review-evidence">
                    <div>{n.evidence}</div>
                    {where(n) && <div className="muted">{where(n)}</div>}
                  </blockquote>
                )}
                <div className="roster-review-actions">
                  <button
                    type="button"
                    className="option-chip"
                    disabled={rule.isPending}
                    onClick={() => run([n.id], true)}
                  >
                    Accept
                  </button>
                  <button
                    type="button"
                    className="option-chip"
                    disabled={rule.isPending}
                    onClick={() => run([n.id], false)}
                  >
                    Skip
                  </button>
                </div>
              </li>
            ))}
          </ul>
        </div>
      ))}
      {accepted.length > 0 && (
        <div className="kind-accent-group">
          <div className="roster-review-what">
            <strong>Accepted</strong>
          </div>
          <ul className="character-notes-accepted">
            {accepted.map((n) => (
              <li key={n.id} title={n.evidence}>
                <strong>{n.characterName}</strong>: {label(n)}
                {n.kind === 'age' && where(n) && <span className="muted"> ({where(n)})</span>}{' '}
                <button
                  type="button"
                  className="link-button"
                  disabled={rule.isPending}
                  title="Remove this note - it won't be proposed again"
                  onClick={() => run([n.id], false)}
                >
                  remove
                </button>
              </li>
            ))}
          </ul>
        </div>
      )}
      {skipped.length > 0 && (
        <details className="kind-accent-group">
          <summary>Skipped ({skipped.length})</summary>
          <p className="muted">
            Notes you turned down or removed.{' '}
            <button
              type="button"
              className="text-button"
              disabled={rule.isPending}
              onClick={() =>
                rule.mutate({ ids: skipped.map((n) => n.id), accept: false, review: true })
              }
            >
              Review all {skipped.length} again
            </button>
          </p>
          <ul className="character-notes-accepted">
            {skipped.map((n) => (
              <li key={n.id} title={n.evidence}>
                <strong>{n.characterName}</strong>: {label(n)}{' '}
                <button
                  type="button"
                  className="link-button"
                  disabled={rule.isPending}
                  title="Put this note back among the proposals"
                  onClick={() => rule.mutate({ ids: [n.id], accept: false, review: true })}
                >
                  review again
                </button>
              </li>
            ))}
          </ul>
        </details>
      )}
    </section>
  )
}
