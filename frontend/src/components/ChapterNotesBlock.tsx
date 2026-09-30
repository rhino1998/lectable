import { useState } from 'react'
import { RiAddLine, RiCloseLine, RiEditLine, RiRefreshLine } from 'react-icons/ri'
import { ApiError } from '../api/client'
import type { ChapterNotes } from '../api/types'

const PRONOUN_OPTIONS = ['', 'he/him', 'she/her', 'they/them', 'it/its']

// Draft rows carry a local key so a scene's present list survives a cast
// member being renamed mid-edit.
interface CastDraft {
  key: number
  name: string
  role: boolean
  pronouns: string
  refs: string // "; "-separated while editing
}

interface SceneDraft {
  key: number
  startIdx: number
  location: string
  time: string
  present: number[] // CastDraft keys
}

interface Draft {
  summary: string
  cast: CastDraft[]
  scenes: SceneDraft[]
}

let nextKey = 1

function toDraft(notes: ChapterNotes): Draft {
  const cast = notes.cast.map((m) => ({
    key: nextKey++,
    name: m.name,
    role: m.role,
    pronouns: m.pronouns,
    refs: m.refs.join('; '),
  }))
  const keyByName = new Map(cast.map((c) => [c.name.toLowerCase(), c.key]))
  return {
    summary: notes.summary,
    cast,
    scenes: notes.scenes.map((sc) => ({
      key: nextKey++,
      startIdx: sc.startIdx,
      location: sc.location,
      time: sc.time,
      present: sc.present
        .map((n) => keyByName.get(n.toLowerCase()))
        .filter((k): k is number => k !== undefined),
    })),
  }
}

function fromDraft(d: Draft): ChapterNotes {
  const nameByKey = new Map(d.cast.map((c) => [c.key, c.name.trim()]))
  return {
    summary: d.summary,
    cast: d.cast.map((c) => ({
      name: c.name,
      role: c.role,
      pronouns: c.pronouns,
      refs: c.refs
        .split(';')
        .map((r) => r.trim())
        .filter(Boolean),
    })),
    scenes: d.scenes.map((sc) => ({
      startIdx: sc.startIdx,
      location: sc.location,
      time: sc.time,
      present: sc.present.map((k) => nameByKey.get(k) ?? '').filter(Boolean),
    })),
    edited: true,
  }
}

// ChapterNotesBlock is annotations view's chapter summary under the
// heading (backend summary pass - see ChapterPassTable's Summary column):
// what happens, and who's in the chapter with what the text calls them.
// Editable in place: attribution reads the cast (who's present in each
// scene, what the text calls them), so fixing a wrong or missing cast
// member is often worth more than fixing lines one by one. Saved notes
// are marked edited and kept by later summary runs; "Regenerate" replaces
// them.
export function ChapterNotesBlock({
  chapterIdx,
  notes,
  onSave,
  onRegenerate,
  onAttribute,
}: {
  chapterIdx: number
  notes: ChapterNotes
  onSave: (chapterIdx: number, notes: ChapterNotes) => Promise<void>
  onRegenerate: (chapterIdx: number) => void
  onAttribute: (chapterIdx: number) => void
}) {
  const [draft, setDraft] = useState<Draft | null>(null)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')
  const [saved, setSaved] = useState(false)

  if (draft) {
    return (
      <NotesEditor
        draft={draft}
        onChange={setDraft}
        saving={saving}
        error={error}
        onCancel={() => {
          setDraft(null)
          setError('')
        }}
        onSave={() => {
          setSaving(true)
          setError('')
          onSave(chapterIdx, fromDraft(draft))
            .then(() => {
              setDraft(null)
              setSaved(true)
            })
            .catch((err) =>
              setError(err instanceof ApiError ? err.message : 'Could not save the notes'),
            )
            .finally(() => setSaving(false))
        }}
      />
    )
  }

  return (
    <div className="chapter-summary-annotation">
      <div className="chapter-notes-actions">
        {notes.edited && <span className="muted">edited</span>}
        <button
          className="icon-action-button"
          onClick={() => {
            setSaved(false)
            setDraft(toDraft(notes))
          }}
          title="Edit this chapter's notes — fix the cast, who is in each scene, and what the text calls them"
        >
          <RiEditLine />
        </button>
        {notes.edited && (
          <button
            className="icon-action-button"
            onClick={() => onRegenerate(chapterIdx)}
            title="Regenerate these notes from the text, replacing your edits"
          >
            <RiRefreshLine />
          </button>
        )}
      </div>
      {notes.summary && <p>{notes.summary}</p>}
      {notes.cast.length > 0 && (
        <p className="chapter-summary-cast">
          <span className="muted">Cast: </span>
          {notes.cast.map((m, i) => (
            <span key={m.name}>
              {i > 0 && ', '}
              {m.name}
              {m.role && <span className="muted"> (role)</span>}
              {m.refs.length > 0 && <span className="muted"> — {m.refs.join('; ')}</span>}
            </span>
          ))}
        </p>
      )}
      {saved && (
        <p className="chapter-notes-saved">
          Saved. Attribution reads the new cast the next time it runs.{' '}
          <button
            className="text-button"
            onClick={() => {
              setSaved(false)
              onAttribute(chapterIdx)
            }}
          >
            Re-run attribution now
          </button>
        </p>
      )}
    </div>
  )
}

function NotesEditor({
  draft,
  onChange,
  saving,
  error,
  onCancel,
  onSave,
}: {
  draft: Draft
  onChange: (d: Draft) => void
  saving: boolean
  error: string
  onCancel: () => void
  onSave: () => void
}) {
  const setCast = (key: number, patch: Partial<CastDraft>) =>
    onChange({ ...draft, cast: draft.cast.map((c) => (c.key === key ? { ...c, ...patch } : c)) })
  const setScene = (key: number, patch: Partial<SceneDraft>) =>
    onChange({
      ...draft,
      scenes: draft.scenes.map((sc) => (sc.key === key ? { ...sc, ...patch } : sc)),
    })

  return (
    <div className="chapter-summary-annotation chapter-notes-editor">
      <label className="chapter-notes-field">
        <span className="muted">Summary</span>
        <textarea
          rows={3}
          value={draft.summary}
          onChange={(e) => onChange({ ...draft, summary: e.target.value })}
        />
      </label>

      <div className="chapter-notes-section">
        <span className="muted">Cast — what the text calls each one, separated by “;”</span>
        {draft.cast.map((c) => (
          <div key={c.key} className="chapter-notes-row">
            <input
              className="chapter-notes-name"
              placeholder="Name"
              value={c.name}
              onChange={(e) => setCast(c.key, { name: e.target.value })}
            />
            <label
              className="chapter-notes-check"
              title="A generic role (Guard, Waiter) rather than one person"
            >
              <input
                type="checkbox"
                checked={c.role}
                onChange={(e) => setCast(c.key, { role: e.target.checked })}
              />
              role
            </label>
            <select
              value={c.pronouns}
              onChange={(e) => setCast(c.key, { pronouns: e.target.value })}
              title="Pronouns"
            >
              {PRONOUN_OPTIONS.map((p) => (
                <option key={p} value={p}>
                  {p || '—'}
                </option>
              ))}
            </select>
            <input
              className="chapter-notes-refs"
              placeholder="the old sailor; the captain"
              value={c.refs}
              onChange={(e) => setCast(c.key, { refs: e.target.value })}
            />
            <button
              className="icon-action-button"
              title="Remove from the cast"
              onClick={() =>
                onChange({
                  ...draft,
                  cast: draft.cast.filter((x) => x.key !== c.key),
                  scenes: draft.scenes.map((sc) => ({
                    ...sc,
                    present: sc.present.filter((k) => k !== c.key),
                  })),
                })
              }
            >
              <RiCloseLine />
            </button>
          </div>
        ))}
        <button
          className="text-button"
          onClick={() =>
            onChange({
              ...draft,
              cast: [
                ...draft.cast,
                { key: nextKey++, name: '', role: false, pronouns: '', refs: '' },
              ],
            })
          }
        >
          <RiAddLine /> Add cast member
        </button>
      </div>

      <div className="chapter-notes-section">
        <span className="muted">Scenes — first paragraph, place, time, and who is there</span>
        {draft.scenes.map((sc) => (
          <div key={sc.key} className="chapter-notes-scene">
            <div className="chapter-notes-row">
              <input
                type="number"
                min={0}
                className="chapter-notes-start"
                title="First paragraph of the scene"
                value={sc.startIdx}
                onChange={(e) => setScene(sc.key, { startIdx: Number(e.target.value) })}
              />
              <input
                className="chapter-notes-location"
                placeholder="Place"
                value={sc.location}
                onChange={(e) => setScene(sc.key, { location: e.target.value })}
              />
              <input
                className="chapter-notes-time"
                placeholder="Time"
                value={sc.time}
                onChange={(e) => setScene(sc.key, { time: e.target.value })}
              />
              <button
                className="icon-action-button"
                title="Remove this scene"
                onClick={() =>
                  onChange({ ...draft, scenes: draft.scenes.filter((x) => x.key !== sc.key) })
                }
              >
                <RiCloseLine />
              </button>
            </div>
            <div className="chapter-notes-present">
              {draft.cast
                .filter((c) => c.name.trim())
                .map((c) => {
                  const on = sc.present.includes(c.key)
                  return (
                    <button
                      key={c.key}
                      className={'chapter-notes-chip' + (on ? ' chapter-notes-chip-on' : '')}
                      aria-pressed={on}
                      onClick={() =>
                        setScene(sc.key, {
                          present: on
                            ? sc.present.filter((k) => k !== c.key)
                            : [...sc.present, c.key],
                        })
                      }
                    >
                      {c.name}
                    </button>
                  )
                })}
            </div>
          </div>
        ))}
        <button
          className="text-button"
          onClick={() =>
            onChange({
              ...draft,
              scenes: [
                ...draft.scenes,
                {
                  key: nextKey++,
                  startIdx: draft.scenes.length
                    ? draft.scenes[draft.scenes.length - 1].startIdx + 1
                    : 0,
                  location: '',
                  time: '',
                  present: [],
                },
              ],
            })
          }
        >
          <RiAddLine /> Add scene
        </button>
      </div>

      {error && <p className="error-text">{error}</p>}
      <div className="chapter-notes-row">
        <button className="primary-button" disabled={saving} onClick={onSave}>
          {saving ? 'Saving…' : 'Save notes'}
        </button>
        <button className="text-button" disabled={saving} onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  )
}
