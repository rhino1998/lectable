import { useState } from 'react'
import { Link, useSearch } from '@tanstack/react-router'
import { ApiError } from '../api/client'
import {
  useCustomVoicePresets,
  useDefaultVoice,
  useUpdateDefaultVoice,
  useVoicePresets,
  useRecapSettings,
  useSetRecapSettings,
  useSetPatreonSession,
  useSetSourceSettings,
  useSourceSettings,
} from '../api/queries'
import { RECAP_AFTER_OPTIONS } from '../utils/recap'
import { CLONE_MODELS, DEFAULT_CLONE_MODEL } from '../api/types'
import { BookSettingsFields } from '../components/speakers/SpeakerSettings'
import { GlobalFactsPanel } from '../components/settings/GlobalFactsPanel'
import { GlobalLexiconPanel } from '../components/settings/GlobalLexiconPanel'
import { SETTINGS_TABS, type SettingsTab } from '../utils/settingsTabs'

const TAB_LABELS: Record<SettingsTab, string> = {
  general: 'General',
  sources: 'Sources',
  facts: 'Facts',
  pronunciations: 'Pronunciations',
}

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

// App-wide settings, in tabs: General (what a newly added book starts
// with, and the recap offer), Sources (checking websites for new
// chapters, Patreon), Facts and Pronunciations (the ones that apply to
// every book).
export function SettingsPage() {
  const { tab = 'general' } = useSearch({ from: '/settings' })
  return (
    <div className="voices-page">
      <div className="library-header">
        <h1>Settings</h1>
      </div>

      <nav className="page-tabs">
        {SETTINGS_TABS.map((t) => (
          <Link
            key={t}
            to="/settings"
            search={{ tab: t }}
            replace
            className={t === tab ? 'page-tab page-tab-active' : 'page-tab'}
          >
            {TAB_LABELS[t]}
          </Link>
        ))}
      </nav>

      {tab === 'general' && <GeneralSettings />}
      {tab === 'sources' && <SourceSettingsTab />}
      {tab === 'facts' && <GlobalFactsPanel />}
      {tab === 'pronunciations' && <GlobalLexiconPanel />}
    </div>
  )
}

// What a newly added book starts with - the default voice (picked with the
// star on the Voices page), the clone model, and the per-book switches
// each book then has on its own Book settings page. All of it is one value
// (GET|PUT /api/voices/default), so every save sends the lot. Then the
// recap offer.
function GeneralSettings() {
  const defaultVoiceQuery = useDefaultVoice()
  const updateDefaultVoice = useUpdateDefaultVoice()
  const builtins = useVoicePresets().data?.presets ?? []
  const customs = useCustomVoicePresets().data ?? []

  const defaults = defaultVoiceQuery.data
  const cloneModel = defaults?.cloneModel ?? DEFAULT_CLONE_MODEL
  const defaultVoiceName = [...builtins, ...customs].find((p) => p.id === defaults?.presetId)?.name

  const recapSettings = useRecapSettings()
  const setRecapSettings = useSetRecapSettings()
  return (
    <>
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
    </>
  )
}

function SourceSettingsTab() {
  const sourceSettings = useSourceSettings()
  const setSourceSettings = useSetSourceSettings()
  const setPatreonSession = useSetPatreonSession()
  const [patreonSession, setPatreonSessionText] = useState('')
  const [patreonError, setPatreonError] = useState<string | null>(null)
  const signInToPatreon = (sessionId: string) => {
    setPatreonError(null)
    setPatreonSession.mutate(sessionId, {
      onSuccess: () => setPatreonSessionText(''),
      onError: (err) =>
        setPatreonError(err instanceof ApiError ? err.message : 'Could not reach the server'),
    })
  }
  return (
    <>
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

      <h2>Patreon</h2>
      {sourceSettings.data?.patreonAccount ? (
        <p>
          Signed in as <strong>{sourceSettings.data.patreonAccount}</strong>.{' '}
          <button
            className="option-chip"
            onClick={() => signInToPatreon('')}
            disabled={setPatreonSession.isPending}
          >
            Sign out
          </button>
        </p>
      ) : (
        <form
          className="settings-patreon"
          onSubmit={(e) => {
            e.preventDefault()
            signInToPatreon(patreonSession)
          }}
        >
          <input
            type="password"
            autoComplete="off"
            placeholder="session_id cookie from patreon.com"
            value={patreonSession}
            onChange={(e) => setPatreonSessionText(e.target.value)}
          />
          <button
            className="primary-button"
            disabled={!patreonSession.trim() || setPatreonSession.isPending}
          >
            {setPatreonSession.isPending ? 'Checking…' : 'Sign in'}
          </button>
        </form>
      )}
      {patreonError && <p className="error-text">{patreonError}</p>}
      <p className="muted">
        A book can track a creator’s Patreon for chapters posted there. Anyone can see what a
        creator has posted, so new chapters are listed either way; reading the patrons-only ones
        takes your membership. Patreon has no sign-in for apps like this one, so the library borrows
        your browser’s: sign in to patreon.com, open the browser’s developer tools (Application or
        Storage, then Cookies), and paste the value of the <code>session_id</code> cookie here. It
        is kept on this server only and sent only to patreon.com. If Patreon signs that browser out,
        chapters show as locked again and a new cookie is needed.
      </p>
    </>
  )
}
