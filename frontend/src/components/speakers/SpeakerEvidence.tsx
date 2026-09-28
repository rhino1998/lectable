import { useState } from 'react'
import { RiRefreshLine } from 'react-icons/ri'
import {
  useRegenerateParagraph,
  useSetParagraphDescription,
  useSetParagraphSpeaker,
} from '../../api/queries'
import { ApiError } from '../../api/client'
import type { SpeakerAppearance } from '../../api/types'

// One line from "View appearances", with its own "regenerate this line"
// and "reassign this line" actions - a character's appearances can span
// every book in their series (see backend Store.ParagraphsForSpeaker), so
// this owns its own useRegenerateParagraph(appearance.bookId)/
// useSetParagraphSpeaker(appearance.bookId) rather than sharing ones bound
// to whichever book the Speakers page itself is open on. reassignTargets
// is "Narrator" plus every other character's name (the row's own
// mergeTargets, reused - same set, since reassigning one line and merging
// a whole character both mean "someone other than this character").
export function AppearanceRow({
  appearance,
  reassignTargets,
}: {
  appearance: SpeakerAppearance
  reassignTargets: string[]
}) {
  const regenerate = useRegenerateParagraph(appearance.bookId)
  const reassign = useSetParagraphSpeaker(appearance.bookId)
  const [error, setError] = useState<string | null>(null)
  const [queued, setQueued] = useState(false)
  const [reassigned, setReassigned] = useState(false)
  const [reassignError, setReassignError] = useState<string | null>(null)

  const run = () => {
    setError(null)
    regenerate.mutate(
      { chapterIdx: appearance.chapterIdx, paragraphIdx: appearance.paragraphIdx },
      {
        onSuccess: () => setQueued(true),
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Could not generate this line'),
      },
    )
  }

  const runReassign = (targetName: string) => {
    setReassignError(null)
    reassign.mutate(
      {
        chapterIdx: appearance.chapterIdx,
        paragraphIdx: appearance.paragraphIdx,
        speaker: targetName,
      },
      {
        onSuccess: () => setReassigned(true),
        onError: (err) =>
          setReassignError(err instanceof ApiError ? err.message : 'Could not reassign this line'),
      },
    )
  }

  // Reassigned lines disappear from this list once the appearances topic
  // pushes the change (they're no longer this character's) - reassigned
  // stays true in the meantime so a still-visible stale row shows as done
  // rather than silently reappearing enabled, in the gap between the
  // mutation resolving and that update landing.
  return (
    <div className="speaker-appearance-item">
      <div className="speaker-appearance-item-main">
        <span className="book-search-result-chapter">
          {appearance.bookTitle} · {appearance.chapterTitle}
        </span>
        <span className="book-search-result-snippet">{appearance.text}</span>
      </div>
      {appearance.audioUrl && <audio controls preload="none" src={appearance.audioUrl} />}
      <button
        className="icon-action-button"
        onClick={run}
        disabled={regenerate.isPending}
        title={
          regenerate.isPending
            ? 'Generating…'
            : appearance.audioUrl
              ? 'Regenerate this line'
              : 'Generate this line for this speaker'
        }
      >
        <RiRefreshLine className={regenerate.isPending ? 'spin' : undefined} />
      </button>
      <select
        value=""
        disabled={reassign.isPending || reassigned}
        onChange={(e) => {
          if (e.target.value) runReassign(e.target.value)
        }}
        title="Reassign this line to a different speaker"
      >
        <option value="" disabled>
          {reassigned ? 'Reassigned' : 'Reassign to…'}
        </option>
        {reassignTargets.map((name) => (
          <option key={name} value={name}>
            {name}
          </option>
        ))}
      </select>
      {!appearance.audioUrl && queued && !error && <span className="muted">Queued</span>}
      {error && (
        <span className="error-text" title={error}>
          ⚠
        </span>
      )}
      {reassignError && (
        <span className="error-text" title={reassignError}>
          ⚠
        </span>
      )}
    </div>
  )
}

// One line from "View descriptions". Unlike AppearanceRow, this paragraph
// doesn't belong to characterName as its *speaker* (it's narration *about*
// them, almost always still attributed to Narrator as its speaker - see
// backend Store.ParagraphsDescribing's own doc comment) - so "reassign"
// here means moving this paragraph off characterName's own describes-list
// and onto a different character's instead (setParagraphDescription), not
// changing who speaks it. The audio preview, when present, plays whatever
// narrates that paragraph today (the book's own voice, per the same
// resolution).
export function DescriptionRow({
  description,
  characterName,
  reassignTargets,
}: {
  description: SpeakerAppearance
  characterName: string
  reassignTargets: string[]
}) {
  const reassign = useSetParagraphDescription(description.bookId)
  const [reassigned, setReassigned] = useState(false)
  const [reassignError, setReassignError] = useState<string | null>(null)

  const runReassign = (targetName: string) => {
    setReassignError(null)
    reassign.mutate(
      {
        chapterIdx: description.chapterIdx,
        paragraphIdx: description.paragraphIdx,
        from: characterName,
        to: targetName,
      },
      {
        onSuccess: () => setReassigned(true),
        onError: (err) =>
          setReassignError(
            err instanceof ApiError ? err.message : 'Could not reassign this description',
          ),
      },
    )
  }

  // Same "stays visible as done rather than silently reappearing enabled"
  // reasoning as AppearanceRow's own reassigned flag - this row disappears
  // from the list once the descriptions topic pushes the change.
  return (
    <div className="speaker-appearance-item">
      <div className="speaker-appearance-item-main">
        <span className="book-search-result-chapter">
          {description.bookTitle} · {description.chapterTitle}
        </span>
        <span className="book-search-result-snippet">{description.text}</span>
      </div>
      {description.audioUrl && <audio controls preload="none" src={description.audioUrl} />}
      <select
        value=""
        disabled={reassign.isPending || reassigned}
        onChange={(e) => {
          if (e.target.value) runReassign(e.target.value)
        }}
        title="Reassign this description to a different character"
      >
        <option value="" disabled>
          {reassigned ? 'Reassigned' : 'Reassign to…'}
        </option>
        {reassignTargets.map((name) => (
          <option key={name} value={name}>
            {name}
          </option>
        ))}
      </select>
      {reassignError && (
        <span className="error-text" title={reassignError}>
          ⚠
        </span>
      )}
    </div>
  )
}
