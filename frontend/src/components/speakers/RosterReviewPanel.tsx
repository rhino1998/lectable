import { useState } from 'react'
import { ApiError } from '../../api/client'
import { useDecideRosterProposal, useRosterReview } from '../../api/queries'
import type { RosterProposal } from '../../api/types'

const SOURCE_TITLES: Record<RosterProposal['source'], string> = {
  rule: 'Found by a rule, without reading the text',
  llm: 'The review read the passages below and judged it',
  agent: 'Filed from outside the app',
}

function lines(n: number): string {
  return `${n} ${n === 1 ? 'line' : 'lines'}`
}

// What accepting a proposal does, in a sentence.
function describe(p: RosterProposal): string {
  switch (p.kind) {
    case 'merge':
      return `Merge ${p.subject} (${lines(p.subjectLines)}) into ${p.target} (${lines(p.targetLines)})`
    case 'remove_alias':
      return `${p.subject} is not also called ${p.detail}`
    case 'split_alias':
      return `${p.detail} is someone else, not ${p.subject} (${lines(p.subjectLines)}) - split them off`
    case 'mark_role':
      return `${p.subject} is a kind of person, not one character`
    case 'invalidate':
      return `${p.subject} (${lines(p.subjectLines)}) is nobody's name`
  }
}

// Fixes a roster review proposed for this book's speakers, each waiting
// for a say: duplicate names to merge, aliases that are someone else's
// name, descriptions to mark as roles. Nothing changes until accepted, and
// a merge can't be undone - the merged name's voice is deleted and its
// lines can no longer be told from the other's.
export function RosterReviewPanel({ bookId }: { bookId: string }) {
  const proposals = useRosterReview(bookId).data?.proposals ?? []
  const decide = useDecideRosterProposal(bookId)
  const [error, setError] = useState<string | null>(null)
  if (proposals.length === 0) return null

  const act = (p: RosterProposal, accept: boolean, noAlias = false, split?: boolean) => {
    setError(null)
    decide.mutate(
      { proposalId: p.id, accept, noAlias, split },
      { onError: (err) => setError(err instanceof ApiError ? err.message : 'Could not save that') },
    )
  }

  return (
    <div className="source-review roster-review">
      <div>
        <strong>Roster review</strong>
        <span className="muted">
          {' '}
          - {proposals.length} suggested {proposals.length === 1 ? 'fix' : 'fixes'}. Nothing changes
          until you accept it; a merge can&rsquo;t be undone.
        </span>
      </div>
      {error && <p className="error-text">{error}</p>}
      <ul className="roster-review-list">
        {proposals.map((p) => (
          <li key={p.id}>
            <div className="roster-review-what">
              <span>{describe(p)}</span>
              <span className="import-preview-source" title={SOURCE_TITLES[p.source]}>
                {p.source}
              </span>
            </div>
            {p.reason && <div className="muted">{p.reason}</div>}
            {p.evidence.map((e, i) => (
              <blockquote key={i} className="roster-review-evidence">
                {e}
              </blockquote>
            ))}
            <div className="roster-review-actions">
              <button
                type="button"
                className="option-chip"
                onClick={() => act(p, true)}
                disabled={decide.isPending}
              >
                Accept
              </button>
              {p.kind === 'remove_alias' && (
                <button
                  type="button"
                  className="option-chip"
                  onClick={() => act(p, true, false, true)}
                  disabled={decide.isPending}
                  title={`Also make "${p.detail}" a character of their own, and look over ${p.subject}'s lines in the chapters that mention the name for the ones that are theirs`}
                >
                  Accept and split off
                </button>
              )}
              {p.kind === 'split_alias' && (
                <button
                  type="button"
                  className="option-chip"
                  onClick={() => act(p, true, false, false)}
                  disabled={decide.isPending}
                  title={`Take "${p.detail}" off ${p.subject}'s names without making a character or moving any lines`}
                >
                  Only remove the name
                </button>
              )}
              {p.kind === 'merge' && (
                <button
                  type="button"
                  className="option-chip"
                  onClick={() => act(p, true, true)}
                  disabled={decide.isPending}
                  title={`Merge, but don't keep "${p.subject}" as another name for ${p.target} - for a name that was never theirs`}
                >
                  Accept without alias
                </button>
              )}
              <button
                type="button"
                className="option-chip"
                onClick={() => act(p, false)}
                disabled={decide.isPending}
              >
                Skip
              </button>
            </div>
          </li>
        ))}
      </ul>
    </div>
  )
}
