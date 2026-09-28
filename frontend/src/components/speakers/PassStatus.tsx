import { RiCheckLine, RiCloseLine } from 'react-icons/ri'
import type { IconType } from 'react-icons'

// PassStatus is one chapter-table cell for a yes/no pipeline pass: a
// check once done, an X if not, a spinner while its task is running. The
// full wording lives in the tooltip/aria-label to keep the table narrow.
// A chapter-table bulk-action icon, column-aligned with the per-chapter
// icon of the same pass. With no onClick it renders an invisible spacer
// instead, for a per-chapter action that has no bulk variant.
export function BulkActionButton({
  icon: Icon,
  onClick,
  disabled = false,
  busy = false,
  title,
}: {
  icon: IconType
  onClick?: () => void
  disabled?: boolean
  busy?: boolean
  title?: string
}) {
  if (!onClick) {
    return (
      <span className="icon-action-button icon-action-spacer" aria-hidden>
        <Icon />
      </span>
    )
  }
  return (
    <button
      className="icon-action-button"
      disabled={disabled || busy}
      onClick={onClick}
      title={title}
    >
      <Icon className={busy ? 'spin' : undefined} />
    </button>
  )
}

// PassStatus is a chapter pass's done/not-done mark. No running state of
// its own - the action button beside it spins while the pass runs.
export function PassStatus({ done, label }: { done: boolean; label: string }) {
  const text = `${label}: ${done ? 'done' : 'not done'}`
  return (
    <span
      className={'chapter-attribute-status' + (done ? ' chapter-attribute-status-done' : '')}
      title={text}
      aria-label={text}
    >
      {done ? <RiCheckLine /> : <RiCloseLine />}
    </span>
  )
}

// CountStatus is PassStatus for a ready/total count (narration paragraphs,
// music regions): a check once everything is ready, otherwise the count
// itself - still short, and more useful than a bare X. errors, if any, is
// flagged in the error color.
export function CountStatus({
  ready,
  total,
  errors = 0,
  label,
  unit = 'paragraphs',
}: {
  ready: number
  total: number
  errors?: number
  label: string
  unit?: string
}) {
  const done = total > 0 && ready >= total
  const text =
    `${label}: ${ready}/${total} ${unit} ready` + (errors > 0 ? `, ${errors} failed` : '')
  return (
    <span
      className={
        'chapter-attribute-status' +
        (done ? ' chapter-attribute-status-done' : '') +
        (errors > 0 ? ' chapter-attribute-status-error' : '')
      }
      title={text}
      aria-label={text}
    >
      {done ? <RiCheckLine /> : `${ready}/${total}`}
    </span>
  )
}
