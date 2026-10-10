import { useState } from 'react'
import { ApiError } from '../api/client'
import { useSetSourceTitleRule } from '../api/queries'
import type { TrackedSource } from '../api/types'
import { NO_TITLE_RULE, compileTitleRule } from '../utils/titleRule'
import { TitleRuleFields } from './TitleRuleFields'

// One tracked source's title rule, editable: saving it checks the book
// again, so what waits for review is listed (and named) by the new rule.
// Chapters the book already has keep their names.
export function SourceTitleRuleEditor(props: {
  bookId: string
  source: TrackedSource
  syncing: boolean
}) {
  const { bookId, source, syncing } = props
  const saved = source.titleRule ?? NO_TITLE_RULE
  const save = useSetSourceTitleRule(bookId)
  const [draft, setDraft] = useState(saved)
  const [error, setError] = useState<string | null>(null)
  // The saved rule changing under the form (another device) shows, unless
  // this one is being edited.
  const [seen, setSeen] = useState(saved)
  const same = (a: typeof saved, b: typeof saved) =>
    a.pattern === b.pattern && a.output === b.output && a.only === b.only
  if (!same(seen, saved)) {
    if (same(draft, seen)) setDraft(saved)
    setSeen(saved)
  }
  const dirty = !same(draft, saved)
  const invalid = compileTitleRule(draft) === 'invalid'

  return (
    <details className="title-rule-editor" open={!!source.titleRule || undefined}>
      <summary>
        Chapter names at {source.name}
        {source.titleRule ? ` - /${source.titleRule.pattern}/` : ''}
      </summary>
      <TitleRuleFields value={draft} onChange={setDraft} disabled={save.isPending} />
      <div className="import-preview-foot">
        <button
          type="button"
          className="option-chip"
          title="Save the rule and check this book's sources again by it"
          disabled={!dirty || invalid || syncing || save.isPending}
          onClick={() => {
            setError(null)
            save.mutate(
              { sourceId: source.id, rule: draft },
              {
                onError: (err) =>
                  setError(err instanceof ApiError ? err.message : 'Could not save the rule'),
              },
            )
          }}
        >
          {save.isPending ? 'Saving…' : 'Save and check again'}
        </button>
        {error && <span className="error-text">{error}</span>}
      </div>
    </details>
  )
}
