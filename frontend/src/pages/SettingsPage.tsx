import { Link } from '@tanstack/react-router'
import {
  useCustomVoicePresets,
  useDefaultVoice,
  useUpdateDefaultVoice,
  useVoicePresets,
} from '../api/queries'
import { CLONE_MODELS, DEFAULT_CLONE_MODEL } from '../api/types'
import { BookSettingsFields } from '../components/speakers/SpeakerSettings'

// App-wide settings: what a newly added book starts with - the default
// voice (picked with the star on the Voices page), the clone model, and
// the per-book switches each book then has on its own Speakers page. All
// of it is one value (GET|PUT /api/voices/default), so every save sends
// the lot.
export function SettingsPage() {
  const defaultVoiceQuery = useDefaultVoice()
  const updateDefaultVoice = useUpdateDefaultVoice()
  const builtins = useVoicePresets().data?.presets ?? []
  const customs = useCustomVoicePresets().data ?? []

  const defaults = defaultVoiceQuery.data
  const cloneModel = defaults?.cloneModel ?? DEFAULT_CLONE_MODEL
  const defaultVoiceName = [...builtins, ...customs].find((p) => p.id === defaults?.presetId)?.name

  return (
    <div className="voices-page">
      <div className="library-header">
        <h1>Settings</h1>
      </div>

      <h2>Defaults for new books</h2>
      <p className="muted">
        What a newly added book starts with. Each book has its own voice settings and the same
        switches on its Speakers page; changing these doesn't touch existing books.
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
    </div>
  )
}
