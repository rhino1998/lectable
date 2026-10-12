import { useState } from 'react'
import { RiDeleteBinLine } from 'react-icons/ri'
import { ApiError } from '../../api/client'
import {
  useAcceptTimeJumpProposal,
  useBook,
  useCreateTimeJump,
  useDeleteTimeJump,
  useSkipTimeJumpProposal,
  useTimeJumps,
} from '../../api/queries'
import type { TimeJump } from '../../api/types'

// The book's time jumps (api.createTimeJump): years that pass in the story
// before a chapter, for the whole cast - where a speaker's own "voice
// changes with age" is one character's. Adding one ages, in the
// background, every character who speaks on both sides of it and whose
// voice would change (a child grown up, an adult grown old); each shows
// its new voice on its own row below, where it can be edited or removed.
// Removing the jump removes the voices it made.
//
// Jumps that fact finding found in the text (the Facts tab's "Find
// facts") are listed first as proposals, each with the sentences it rests
// on: nothing is aged until one is accepted, and its chapter and length
// can be changed first - the pass tends to place a jump a chapter late.
export function TimeJumpsPanel({ bookId }: { bookId: string }) {
  const book = useBook(bookId)
  const all = useTimeJumps(bookId).data ?? []
  const jumps = all.filter((j) => !j.proposed)
  const proposals = all.filter((j) => j.proposed)
  const accept = useAcceptTimeJumpProposal(bookId)
  const skip = useSkipTimeJumpProposal(bookId)
  // What the reader changed of a proposal before accepting it, by id.
  const [edits, setEdits] = useState<Record<string, { chapterIdx?: number; years?: string }>>({})
  const ruling = accept.isPending || skip.isPending
  const chapterOf = (j: TimeJump) => edits[j.id]?.chapterIdx ?? j.chapterIdx
  const yearsOf = (j: TimeJump) => edits[j.id]?.years ?? String(j.years)
  const create = useCreateTimeJump(bookId)
  const remove = useDeleteTimeJump(bookId)
  const [adding, setAdding] = useState(false)
  const [chapterIdx, setChapterIdx] = useState('')
  const [years, setYears] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [asked, setAsked] = useState<number | null>(null)

  const save = () => {
    const idx = Number.parseInt(chapterIdx, 10)
    const n = Number.parseInt(years, 10)
    if (!Number.isFinite(idx)) {
      setError('Choose where the jump falls')
      return
    }
    if (!Number.isFinite(n) || n < 1) {
      setError('Give the number of years that pass')
      return
    }
    setError(null)
    create.mutate(
      { chapterIdx: idx, years: n },
      {
        onSuccess: (res) => {
          setAsked(res.queued)
          setAdding(false)
          setChapterIdx('')
          setYears('')
        },
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Could not add the time jump'),
      },
    )
  }

  return (
    <div className="time-jumps">
      {proposals.length > 0 && (
        <ul className="roster-review-list">
          {proposals.map((j) => (
            <li key={j.id}>
              <div className="roster-review-what">
                <strong>Found in the text:</strong>{' '}
                <label>
                  <input
                    className="speaker-stage-age"
                    type="number"
                    min={1}
                    value={yearsOf(j)}
                    aria-label="Years that pass"
                    onChange={(e) =>
                      setEdits({ ...edits, [j.id]: { ...edits[j.id], years: e.target.value } })
                    }
                  />{' '}
                  years pass
                </label>{' '}
                <label>
                  before{' '}
                  <select
                    value={chapterOf(j)}
                    onChange={(e) =>
                      setEdits({
                        ...edits,
                        [j.id]: { ...edits[j.id], chapterIdx: Number.parseInt(e.target.value, 10) },
                      })
                    }
                  >
                    <option value="0">the whole book (since the one before it)</option>
                    {book.data?.chapters
                      .filter((ch) => ch.idx > 0)
                      .map((ch) => (
                        <option key={ch.idx} value={ch.idx}>
                          {ch.title || `Chapter ${ch.idx + 1}`}
                        </option>
                      ))}
                  </select>
                </label>
              </div>
              {j.evidence && (
                <blockquote className="roster-review-evidence">
                  {j.evidence.split('\n').map((line, i) => (
                    <div key={i}>{line}</div>
                  ))}
                </blockquote>
              )}
              <div className="roster-review-actions">
                <button
                  type="button"
                  className="option-chip"
                  disabled={ruling || !(Number.parseInt(yearsOf(j), 10) >= 1)}
                  title="Add this time jump and age every voice that would change across it"
                  onClick={() => {
                    setError(null)
                    setAsked(null)
                    accept.mutate(
                      {
                        id: j.id,
                        chapterIdx: chapterOf(j),
                        years: Number.parseInt(yearsOf(j), 10),
                      },
                      {
                        onSuccess: (res) => setAsked(res.queued),
                        onError: (err) =>
                          setError(
                            err instanceof ApiError ? err.message : 'Could not add the time jump',
                          ),
                      },
                    )
                  }}
                >
                  Accept
                </button>
                <button
                  type="button"
                  className="option-chip"
                  disabled={ruling}
                  title="No time jump here - don't propose it again"
                  onClick={() => {
                    setError(null)
                    skip.mutate(j.id, {
                      onError: (err) =>
                        setError(err instanceof ApiError ? err.message : 'Could not skip it'),
                    })
                  }}
                >
                  Skip
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
      {jumps.map((j) => (
        <div key={j.id} className="time-jumps-row">
          <span>
            {j.chapterIdx === 0 ? (
              <>
                This book is set <strong>{yearsLabel(j.years)}</strong> after the one before it
              </>
            ) : (
              <>
                <strong>{yearsLabel(j.years)}</strong> pass before{' '}
                <strong>{j.chapterTitle || `chapter ${j.chapterIdx + 1}`}</strong>
              </>
            )}
          </span>
          <span className="muted">
            {j.voices === 0
              ? 'no voices changed'
              : `${j.voices} older ${j.voices === 1 ? 'voice' : 'voices'}`}
          </span>
          <button
            className="icon-action-button"
            disabled={remove.isPending}
            title="Remove this time jump and the older voices it made"
            onClick={() => {
              setError(null)
              setAsked(null)
              remove.mutate(j.id, {
                onError: (err) =>
                  setError(err instanceof ApiError ? err.message : 'Could not remove it'),
              })
            }}
          >
            <RiDeleteBinLine />
          </button>
        </div>
      ))}
      {asked !== null && (
        <p className="muted">
          {asked === 0
            ? 'Nobody speaks on both sides of that jump, so no voice changes.'
            : `Checking ${asked} ${asked === 1 ? 'character' : 'characters'} who speak on both sides of it — most will sound the same; the ones who don’t get an older voice from there on.`}
        </p>
      )}
      {adding ? (
        <div className="time-jumps-add">
          <label>
            <input
              className="speaker-stage-age"
              type="number"
              min={1}
              value={years}
              placeholder="years"
              autoFocus
              onChange={(e) => setYears(e.target.value)}
            />{' '}
            years pass
          </label>
          <label>
            before{' '}
            <select value={chapterIdx} onChange={(e) => setChapterIdx(e.target.value)}>
              <option value="">Choose…</option>
              <option value="0">the whole book (since the one before it)</option>
              {book.data?.chapters
                .filter((ch) => ch.idx > 0)
                .map((ch) => (
                  <option key={ch.idx} value={ch.idx}>
                    {ch.title || `Chapter ${ch.idx + 1}`}
                  </option>
                ))}
            </select>
          </label>
          <button onClick={save} disabled={create.isPending}>
            Add
          </button>
          <button onClick={() => setAdding(false)} disabled={create.isPending}>
            Cancel
          </button>
        </div>
      ) : (
        <div className="speaker-aliases muted">
          <button
            className="link-button"
            onClick={() => setAdding(true)}
            title="Years pass in the story - age every voice that would change, from a chapter on or for the whole book"
          >
            {jumps.length > 0 ? 'Add another time jump' : 'Add a time jump'}
          </button>
        </div>
      )}
      {error && <p className="error-text">{error}</p>}
    </div>
  )
}

function yearsLabel(n: number): string {
  return `${n} ${n === 1 ? 'year' : 'years'}`
}
