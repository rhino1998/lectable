import { useState } from 'react'
import { RiDeleteBinLine } from 'react-icons/ri'
import {
  useBook,
  useDeleteCharacterStage,
  useSetCharacterStage,
  useUpdateCharacterStage,
} from '../../api/queries'
import { ApiError } from '../../api/client'
import type { SpeakerStage, SpeakerVoice } from '../../api/types'
import { SpeakerVoiceLine } from './SpeakerVoices'

// The link under a character's voices that gives them an older (or
// younger) voice from a chosen chapter on - and through the rest of the
// series - from a casting the backend rewrites from their own. The age
// then shows as a voice of its own on the card (StageVoiceLine). The
// speakers list is live, so a save shows up on its own.
export function AddStage({
  bookId,
  characterId,
  hasStages,
  hasCasting,
}: {
  bookId: string
  characterId: string
  hasStages: boolean
  // Whether the character has been characterized - the casting a new
  // stage's is rewritten from.
  hasCasting: boolean
}) {
  const [adding, setAdding] = useState(false)
  if (adding) {
    return (
      <AddStageForm bookId={bookId} characterId={characterId} onDone={() => setAdding(false)} />
    )
  }
  return (
    <div className="speaker-aliases muted">
      <button
        className="link-button"
        onClick={() => setAdding(true)}
        disabled={!hasCasting}
        title={
          hasCasting
            ? 'Give this character an older (or younger) voice from a chapter on - for a story that follows them over years'
            : 'Characterize this character first - a later voice is written from their casting'
        }
      >
        {hasStages ? 'Add another age' : 'Voice changes with age'}
      </button>
    </div>
  )
}

// The voice a character has from one of their ages on (SpeakerVoice.stage):
// its age and where it starts, its samples, Edit and remove - with its
// casting beneath.
export function StageVoiceLine({
  bookId,
  characterId,
  speakerName,
  voice,
  stage,
}: {
  bookId: string
  characterId: string
  speakerName: string
  voice: SpeakerVoice
  stage: SpeakerStage
}) {
  const otherBook = stage.bookId !== bookId
  const update = useUpdateCharacterStage(bookId)
  const remove = useDeleteCharacterStage(bookId)
  const [draft, setDraft] = useState<{ age: string; instruct: string } | null>(null)
  const [error, setError] = useState<string | null>(null)
  const busy = update.isPending || remove.isPending

  const save = (instruct: string | undefined) => {
    if (draft === null) return
    const age = Number.parseInt(draft.age, 10)
    if (!Number.isFinite(age) || age < 1) {
      setError('Give an age in years')
      return
    }
    setError(null)
    update.mutate(
      { characterId, stageId: stage.id, age, instruct },
      {
        onSuccess: () => setDraft(null),
        onError: (err) => setError(err instanceof ApiError ? err.message : 'Could not save'),
      },
    )
  }

  const where =
    (stage.chapterTitle || `chapter ${stage.chapterIdx + 1}`) +
    (otherBook && stage.bookTitle ? ` (${stage.bookTitle})` : '')
  return (
    <>
      <SpeakerVoiceLine
        bookId={bookId}
        speakerName={speakerName}
        voice={voice}
        label={
          <span>
            About {stage.age} <span className="muted">from {where}</span>
          </span>
        }
      >
        {draft === null && (
          <>
            <button
              className="link-button"
              onClick={() => setDraft({ age: String(stage.age), instruct: stage.instruct })}
            >
              Edit
            </button>
            <button
              className="speaker-emotion-button"
              disabled={busy}
              aria-label="Remove this age"
              title="Remove - these lines go back to the voice before this age"
              onClick={() => {
                setError(null)
                remove.mutate(
                  { characterId, stageId: stage.id },
                  {
                    onError: (err) =>
                      setError(err instanceof ApiError ? err.message : 'Could not remove'),
                  },
                )
              }}
            >
              <RiDeleteBinLine />
            </button>
          </>
        )}
        {error && <span className="error-text">{error}</span>}
      </SpeakerVoiceLine>
      {draft === null && <p className="muted speaker-row-summary">{stage.instruct}</p>}
      {draft !== null && (
        <div className="speaker-stage-edit">
          <label>
            Age{' '}
            <input
              className="speaker-stage-age"
              type="number"
              min={1}
              value={draft.age}
              onChange={(e) => setDraft({ ...draft, age: e.target.value })}
            />
          </label>
          <textarea
            rows={4}
            value={draft.instruct}
            aria-label="Voice casting at this age"
            onChange={(e) => setDraft({ ...draft, instruct: e.target.value })}
          />
          <div className="speaker-stage-buttons">
            <button onClick={() => save(draft.instruct.trim() || undefined)} disabled={busy}>
              Save
            </button>
            <button
              onClick={() => save(undefined)}
              disabled={busy}
              title="Write the casting again from the character's own, for this age"
            >
              {update.isPending ? 'Writing…' : 'Rewrite for this age'}
            </button>
            <button onClick={() => setDraft(null)} disabled={busy}>
              Cancel
            </button>
          </div>
        </div>
      )}
    </>
  )
}

function AddStageForm({
  bookId,
  characterId,
  onDone,
}: {
  bookId: string
  characterId: string
  onDone: () => void
}) {
  const book = useBook(bookId)
  const add = useSetCharacterStage(bookId)
  const [chapterIdx, setChapterIdx] = useState('')
  const [age, setAge] = useState('')
  const [error, setError] = useState<string | null>(null)

  const save = () => {
    const idx = Number.parseInt(chapterIdx, 10)
    const years = Number.parseInt(age, 10)
    if (!Number.isFinite(idx)) {
      setError('Choose the chapter the older voice starts at')
      return
    }
    if (!Number.isFinite(years) || years < 1) {
      setError('Give an age in years')
      return
    }
    setError(null)
    add.mutate(
      { characterId, chapterIdx: idx, age: years },
      {
        onSuccess: onDone,
        onError: (err) => setError(err instanceof ApiError ? err.message : 'Could not add'),
      },
    )
  }

  return (
    <div className="speaker-stage speaker-stage-add">
      <label>
        From{' '}
        <select value={chapterIdx} onChange={(e) => setChapterIdx(e.target.value)}>
          <option value="">Choose a chapter…</option>
          {book.data?.chapters.map((ch) => (
            <option key={ch.idx} value={ch.idx}>
              {ch.title || `Chapter ${ch.idx + 1}`}
            </option>
          ))}
        </select>
      </label>
      <label>
        they are about{' '}
        <input
          className="speaker-stage-age"
          type="number"
          min={1}
          value={age}
          placeholder="age"
          onChange={(e) => setAge(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter') save()
          }}
        />
      </label>
      <button onClick={save} disabled={add.isPending}>
        {add.isPending ? 'Writing their voice…' : 'Add'}
      </button>
      <button onClick={onDone} disabled={add.isPending}>
        Cancel
      </button>
      {error && <p className="error-text">{error}</p>}
      <p className="muted speaker-stage-note">
        Their lines from that chapter on are generated again in the new voice.
      </p>
    </div>
  )
}
