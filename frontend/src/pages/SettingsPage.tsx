import { Link } from '@tanstack/react-router'
import {
  useCustomVoicePresets,
  useDefaultVoice,
  useUpdateDefaultVoice,
  useVoicePresets,
  useRecapSettings,
  useSetRecapSettings,
  useSetSourceSettings,
  useSourceSettings,
} from '../api/queries'
import { RECAP_AFTER_OPTIONS } from '../utils/recap'
import { CLONE_MODELS, DEFAULT_CLONE_MODEL } from '../api/types'
import { BookSettingsFields } from '../components/speakers/SpeakerSettings'

// App-wide settings: what a newly added book starts with - the default
// voice (picked with the star on the Voices page), the clone model, and
// the per-book switches each book then has on its own Book settings page. All
// of it is one value (GET|PUT /api/voices/default), so every save sends
// the lot.
const SOURCE_CHECK_OPTIONS = [
  { hours: 0, label: 'never' },
  { hours: 6, label: 'every 6 hours' },
  { hours: 12, label: 'every 12 hours' },
  { hours: 24, label: 'daily' },
  { hours: 48, label: 'every 2 days' },
  { hours: 168, label: 'weekly' },
]

// The interval choices, with the stored one added if it is none of them.
function sourceCheckOptions(current: number | undefined) {
  if (current === undefined || SOURCE_CHECK_OPTIONS.some((o) => o.hours === current)) {
    return SOURCE_CHECK_OPTIONS
  }
  return [...SOURCE_CHECK_OPTIONS, { hours: current, label: `every ${current} hours` }]
}

export function SettingsPage() {
  const defaultVoiceQuery = useDefaultVoice()
  const updateDefaultVoice = useUpdateDefaultVoice()
  const builtins = useVoicePresets().data?.presets ?? []
  const customs = useCustomVoicePresets().data ?? []

  const defaults = defaultVoiceQuery.data
  const cloneModel = defaults?.cloneModel ?? DEFAULT_CLONE_MODEL
  const defaultVoiceName = [...builtins, ...customs].find((p) => p.id === defaults?.presetId)?.name

  const sourceSettings = useSourceSettings()
  const setSourceSettings = useSetSourceSettings()
  const recapSettings = useRecapSettings()
  const setRecapSettings = useSetRecapSettings()
  return (
    <div className="voices-page">
      <div className="library-header">
        <h1>Settings</h1>
      </div>

      <h2>Defaults for new books</h2>
      <p className="muted">
        What a newly added book starts with. Each book has its own voice settings and the same
        switches on its Book settings page; changing these doesn't touch existing books.
      </p>
      {defaultVoiceQuery.isError && <p className="error-text">Could not load the defaults.</p>}
      {updateDefaultVoice.isError && <p className="error-text">Could not save the defaults.</p>}

      <p>
        Voice: {defaultVoiceName ?? (defaults ? 'none picked' : '…')}{' '}
        <Link to="/voices" className="text-button">
          Change on the Voices page
        </Link>
      </p>

      <label>
        Cloning model
        <select
          value={cloneModel}
          onChange={(e) =>
            defaults && updateDefaultVoice.mutate({ ...defaults, cloneModel: e.target.value })
          }
          disabled={!defaults || updateDefaultVoice.isPending}
        >
          {CLONE_MODELS.map((m) => (
            <option key={m.id} value={m.id}>
              {m.label}
            </option>
          ))}
        </select>
      </label>
      <p className="muted">
        Voices don't have a cloning model of their own; previews on the Voices page start on this
        one.
      </p>

      <BookSettingsFields
        settings={defaults}
        pending={updateDefaultVoice.isPending}
        cloneModel={cloneModel}
        onChange={(next) => updateDefaultVoice.mutate(next)}
      />

      <h2>Recap</h2>
      <label>
        Offer the “previously on” recap{' '}
        <select
          value={recapSettings.data?.afterMinutes ?? ''}
          onChange={(e) => setRecapSettings.mutate(Number(e.target.value))}
          disabled={!recapSettings.data || setRecapSettings.isPending}
        >
          {RECAP_AFTER_OPTIONS.map((o) => (
            <option key={o.minutes} value={o.minutes}>
              {o.label}
            </option>
          ))}
        </select>
      </label>
      <p className="muted">
        When you open a book you have been away from this long, the reader offers a spoken recap of
        where you left off. Shared by every device; the reader’s Recap button works either way.
      </p>

      <h2>New chapters</h2>
      <label>
        Check books imported from websites{' '}
        <select
          value={sourceSettings.data?.checkEveryHours ?? ''}
          onChange={(e) => setSourceSettings.mutate(Number(e.target.value))}
          disabled={!sourceSettings.data || setSourceSettings.isPending}
        >
          {sourceCheckOptions(sourceSettings.data?.checkEveryHours).map((o) => (
            <option key={o.hours} value={o.hours}>
              {o.label}
            </option>
          ))}
        </select>
      </label>
      <p className="muted">
        How often each book imported from a website is checked for new entries at its source. A
        check doesn’t import anything by itself: what it finds waits for you to review, and the book
        is marked in the library. “Check all now” is in the library’s Add book menu.
      </p>
    </div>
  )
}
