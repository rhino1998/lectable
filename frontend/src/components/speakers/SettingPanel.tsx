import { useState } from 'react'
import { RiRefreshLine, RiSave3Line } from 'react-icons/ri'
import { ApiError } from '../../api/client'
import {
  useBookSetting,
  useBookSettingDeriving,
  useDeriveBookSetting,
  useSetBookSetting,
} from '../../api/queries'
import type { BookSetting } from '../../api/types'

const EMPTY: BookSetting = {
  era: '',
  technology: '',
  place: '',
  accent: '',
  genre: '',
  narrator: '',
}

const FIELDS: { key: keyof BookSetting; label: string; placeholder: string; title: string }[] = [
  {
    key: 'technology',
    label: 'Technology',
    placeholder: 'e.g. pre-industrial: lanterns, candles, carriages',
    title:
      "What actually works in the story's present - which light sources and sounds the ambience and lighting may use",
  },
  {
    key: 'era',
    label: 'Era',
    placeholder: 'e.g. medieval-style secondary world',
    title: 'When the story takes place',
  },
  {
    key: 'place',
    label: 'Place',
    placeholder: 'e.g. Crete and the Mediterranean',
    title: 'The region or world',
  },
  {
    key: 'accent',
    label: 'Accent',
    placeholder: 'e.g. General American',
    title:
      "The main characters' accent - voice characterization's default when a character's own lines don't say",
  },
  { key: 'genre', label: 'Genre', placeholder: 'e.g. epic fantasy', title: 'The genre' },
  {
    key: 'narrator',
    label: 'Narrator',
    placeholder: 'first-person books only, e.g. Steve',
    title:
      'Who the "I" is in a first-person book - attribution gives them the lines tagged "I said". Empty for third-person books.',
  },
]

// The Speakers page's book setting profile (backend httpapi/setting.go):
// the world the story takes place in, written by one LLM call the first
// time the ambience pass or a voice characterization needs it, and shown
// to their prompts. Per book, not per series. Editable; an edit only
// affects passes run afterwards.
export function SettingPanel({ bookId }: { bookId: string }) {
  const settingQuery = useBookSetting(bookId)
  const save = useSetBookSetting(bookId)
  const derive = useDeriveBookSetting(bookId)
  // The pass runs as a job; its result arrives on the bookSetting topic.
  const deriving = useBookSettingDeriving(bookId) || derive.isPending
  const stored = settingQuery.data?.setting ?? null
  const source = settingQuery.data?.source
  // The reader's unsaved edit, null while not editing - the form shows the
  // stored value (live) until then.
  const [edit, setEdit] = useState<BookSetting | null>(null)
  const dirty = edit !== null
  const draft = edit ?? stored ?? EMPTY
  const [error, setError] = useState<string | null>(null)

  const onError = (fallback: string) => (err: unknown) =>
    setError(err instanceof ApiError ? err.message : fallback)

  const runSave = () => {
    setError(null)
    save.mutate(draft, { onSuccess: () => setEdit(null), onError: onError('Could not save') })
  }

  const runDerive = () => {
    if (
      (source === 'manual' || dirty) &&
      !confirm('Re-read the setting from the book? This replaces your edits.')
    )
      return
    setError(null)
    derive.mutate(undefined, {
      onSuccess: () => setEdit(null),
      onError: onError('Could not read the setting'),
    })
  }

  return (
    <section className="setting-panel">
      <div className="library-header">
        <h2>
          Setting
          {source && (
            <span className="muted"> ({source === 'manual' ? 'edited' : 'from the book'})</span>
          )}
        </h2>
        <div className="speakers-roster-actions">
          <button
            className="icon-action-button"
            onClick={runDerive}
            disabled={deriving}
            title="Read the setting from the book now (one LLM call over excerpts and technology words from across it)"
          >
            <RiRefreshLine className={deriving ? 'spin' : undefined} />
          </button>
        </div>
      </div>
      <p className="muted">
        The world this book takes place in, shown to the ambience, lighting and voice
        characterization passes so their sounds, light sources and accents fit it. Read from the
        book the first time one of them runs. Changes apply to passes run afterwards - re-run
        ambience to hear them.
      </p>
      {error && <p className="error-text">{error}</p>}
      {settingQuery.isError && <p className="error-text">Could not load the setting.</p>}
      {!stored && !dirty && !settingQuery.isLoading && <p className="muted">Not read yet.</p>}
      <form
        className="setting-form"
        onSubmit={(e) => {
          e.preventDefault()
          if (dirty) runSave()
        }}
      >
        {FIELDS.map((f) => (
          <label key={f.key} title={f.title}>
            <span>{f.label}</span>
            <input
              value={draft[f.key]}
              placeholder={f.placeholder}
              onChange={(e) => setEdit({ ...draft, [f.key]: e.target.value })}
            />
          </label>
        ))}
        {dirty && (
          <div className="setting-form-actions">
            <button type="button" onClick={() => setEdit(null)}>
              Cancel
            </button>
            <button type="submit" disabled={save.isPending}>
              <RiSave3Line className={save.isPending ? 'spin' : undefined} /> Save
            </button>
          </div>
        )}
      </form>
    </section>
  )
}
