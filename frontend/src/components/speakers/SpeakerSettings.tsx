import { useUpdateVoice, useVoice } from '../../api/queries'
import {
  CLONE_MODELS,
  CHARACTER_VOICE_MODE_DESCRIPTIONS,
  CHARACTER_VOICE_MODE_LABELS,
  CHARACTER_VOICE_MODES,
  DEFAULT_CHARACTER_VOICE_MODE,
  INSTRUCTED_CLONE_MODEL,
  type CharacterVoiceMode,
  type VoiceSettings,
} from '../../api/types'

// The Book settings page's book-wide switches, bound to one book's voice
// settings (PUT .../voice): its clone model (also in the player bar's
// VoicePanel), then BookSettingsFields.
export function SpeakerSettings({ bookId, cloneModel }: { bookId: string; cloneModel: string }) {
  const voiceQuery = useVoice(bookId)
  const updateVoice = useUpdateVoice(bookId)
  const settings = voiceQuery.data

  const changeCloneModel = (next: string) => {
    // Switching clone model deletes the book's generated audio server-side
    // (it isn't part of any voice's cache key) - confirm first.
    if (
      !settings ||
      next === cloneModel ||
      !confirm('Changing the cloning model deletes all generated audio for this book. Continue?')
    ) {
      return
    }
    updateVoice.mutate({ ...settings, cloneModel: next })
  }

  return (
    <>
      <section className="speakers-multivoice">
        <label>
          Cloning model
          <select
            value={cloneModel}
            disabled={!settings || updateVoice.isPending}
            onChange={(e) => changeCloneModel(e.target.value)}
          >
            {!CLONE_MODELS.some((m) => m.id === cloneModel) && (
              <option value={cloneModel}>{cloneModel}</option>
            )}
            {CLONE_MODELS.map((m) => (
              <option key={m.id} value={m.id}>
                {m.label}
              </option>
            ))}
          </select>
        </label>
        <p className="muted">
          The model every voice in this book is cloned through. Changing it deletes the book&apos;s
          generated audio.
        </p>
      </section>
      <BookSettingsFields
        settings={settings}
        pending={updateVoice.isPending}
        cloneModel={cloneModel}
        onChange={(next) => updateVoice.mutate(next)}
      />
    </>
  )
}

// The per-book processing switches - character voice mode, waiting for
// emotion/pronunciation or attribution before generating, requiring the
// book setting, and background music - as fields of a VoiceSettings value:
// one book's (SpeakerSettings), or the defaults new books start with (the
// Voices page).
export function BookSettingsFields({
  settings,
  pending,
  cloneModel,
  onChange,
}: {
  settings: VoiceSettings | undefined
  pending: boolean
  cloneModel: string
  onChange: (next: VoiceSettings) => void
}) {
  // Gates instructCharacterVoices - only actually does anything when the book's
  // clone model is INSTRUCTED_CLONE_MODEL (the one family
  // that honors a clone-time style instruction alongside a reference
  // clip). Backend narration.Resolver already no-ops the setting itself
  // for any other clone model; this is just so the toggle's own copy
  // reflects that instead of silently doing nothing.
  const instructedCloneSupported = cloneModel === INSTRUCTED_CLONE_MODEL

  const changeCharacterVoiceMode = (mode: CharacterVoiceMode) => {
    if (!settings) return
    onChange({ ...settings, characterVoiceMode: mode })
  }

  const toggleSpeechDirection = (checked: boolean) => {
    if (!settings) return
    onChange({ ...settings, speechDirection: checked })
  }

  const toggleStrictOrder = (checked: boolean) => {
    if (!settings) return
    onChange({ ...settings, strictOrder: checked })
  }

  const toggleSettingRequired = (checked: boolean) => {
    if (!settings) return
    onChange({ ...settings, settingRequired: checked })
  }

  const toggleMusicEnabled = (checked: boolean) => {
    if (!settings) return
    onChange({ ...settings, musicEnabled: checked })
  }

  return (
    <section className="speakers-multivoice">
      <label>
        Character voices
        <select
          value={settings?.characterVoiceMode ?? DEFAULT_CHARACTER_VOICE_MODE}
          disabled={!settings || pending}
          onChange={(e) => changeCharacterVoiceMode(e.target.value as CharacterVoiceMode)}
        >
          {CHARACTER_VOICE_MODES.map((mode) => (
            <option key={mode} value={mode}>
              {CHARACTER_VOICE_MODE_LABELS[mode]}
            </option>
          ))}
        </select>
      </label>
      <p className="muted">
        {
          CHARACTER_VOICE_MODE_DESCRIPTIONS[
            settings?.characterVoiceMode ?? DEFAULT_CHARACTER_VOICE_MODE
          ]
        }
      </p>
      {!instructedCloneSupported && (
        <p className="muted">
          The two "style" options above are only available for the "{INSTRUCTED_CLONE_MODEL}" clone
          model - this book uses "{cloneModel}".
        </p>
      )}

      <label className="checkbox-label">
        <input
          type="checkbox"
          checked={settings?.speechDirection ?? false}
          disabled={!settings || pending}
          onChange={(e) => toggleSpeechDirection(e.target.checked)}
        />
        Wait for emotion labeling and pronunciation before generating audio
      </label>
      <p className="muted">
        When on, a chapter's audio waits for emotion labeling and pronunciation resolution to finish
        first, so each line is voiced in its emotion and ambiguous abbreviations are read correctly
        right away, instead of needing a later regenerate. Off by default.
      </p>

      <label className="checkbox-label">
        <input
          type="checkbox"
          checked={settings?.strictOrder ?? false}
          disabled={!settings || pending}
          onChange={(e) => toggleStrictOrder(e.target.checked)}
        />
        Wait for speaker attribution before generating audio
      </label>
      <p className="muted">
        When on (and character voices are in use), a chapter is only narrated once its speakers are
        attributed, so every line is generated once, in its speaker's voice. Attribution runs in
        chapter order, so jumping far ahead in an unprocessed book means waiting for it to catch up.
        Off by default: audio is generated right away and lines are regenerated when attribution
        later gives them to a character.
      </p>

      <label className="checkbox-label">
        <input
          type="checkbox"
          checked={settings?.settingRequired ?? false}
          disabled={!settings || pending}
          onChange={(e) => toggleSettingRequired(e.target.checked)}
        />
        Require the book setting
      </label>
      <p className="muted">
        When on, the passes that read the book's setting (summary, attribution, characterization,
        ambience) fail if the setting can't be derived, instead of running without it. Off by
        default.
      </p>

      <label className="checkbox-label">
        <input
          type="checkbox"
          checked={settings?.musicEnabled ?? false}
          disabled={!settings || pending}
          onChange={(e) => toggleMusicEnabled(e.target.checked)}
        />
        Background music
      </label>
      <p className="muted">
        When on, each chapter's own background music is generated (from the chapter table below, or
        automatically as the reader reaches it) and mixed in under narration. Off by default -
        turning this off doesn't delete anything already scored or generated, it just stops new
        chapters from generating audio.
      </p>
    </section>
  )
}
