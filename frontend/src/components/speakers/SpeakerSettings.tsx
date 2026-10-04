import { useUpdateVoice, useVoice } from '../../api/queries'
import {
  CHARACTER_VOICE_MODE_DESCRIPTIONS,
  CHARACTER_VOICE_MODE_LABELS,
  CHARACTER_VOICE_MODES,
  DEFAULT_CHARACTER_VOICE_MODE,
  INSTRUCTED_CLONE_MODEL,
  type CharacterVoiceMode,
} from '../../api/types'

// The Speakers page's book-wide switches: character voice mode, waiting
// for emotion/pronunciation before generating, and background music - all
// fields of the book's voice settings (PUT .../voice).
export function SpeakerSettings({ bookId, cloneModel }: { bookId: string; cloneModel: string }) {
  const voiceQuery = useVoice(bookId)
  const updateVoice = useUpdateVoice(bookId)

  // Gates instructCharacterVoices - only actually does anything when the book's
  // clone model is INSTRUCTED_CLONE_MODEL (the one family
  // that honors a clone-time style instruction alongside a reference
  // clip). Backend narration.Resolver already no-ops the setting itself
  // for any other clone model; this is just so the toggle's own copy
  // reflects that instead of silently doing nothing.
  const instructedCloneSupported = cloneModel === INSTRUCTED_CLONE_MODEL

  const changeCharacterVoiceMode = (mode: CharacterVoiceMode) => {
    if (!voiceQuery.data) return
    updateVoice.mutate({ ...voiceQuery.data, characterVoiceMode: mode })
  }

  const toggleSpeechDirection = (checked: boolean) => {
    if (!voiceQuery.data) return
    updateVoice.mutate({ ...voiceQuery.data, speechDirection: checked })
  }

  const toggleStrictOrder = (checked: boolean) => {
    if (!voiceQuery.data) return
    updateVoice.mutate({ ...voiceQuery.data, strictOrder: checked })
  }

  const toggleSettingRequired = (checked: boolean) => {
    if (!voiceQuery.data) return
    updateVoice.mutate({ ...voiceQuery.data, settingRequired: checked })
  }

  const toggleMusicEnabled = (checked: boolean) => {
    if (!voiceQuery.data) return
    updateVoice.mutate({ ...voiceQuery.data, musicEnabled: checked })
  }

  return (
    <section className="speakers-multivoice">
      <label>
        Character voices
        <select
          value={voiceQuery.data?.characterVoiceMode ?? DEFAULT_CHARACTER_VOICE_MODE}
          disabled={!voiceQuery.data || updateVoice.isPending}
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
            voiceQuery.data?.characterVoiceMode ?? DEFAULT_CHARACTER_VOICE_MODE
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
          checked={voiceQuery.data?.speechDirection ?? false}
          disabled={!voiceQuery.data || updateVoice.isPending}
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
          checked={voiceQuery.data?.strictOrder ?? false}
          disabled={!voiceQuery.data || updateVoice.isPending}
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
          checked={voiceQuery.data?.settingRequired ?? false}
          disabled={!voiceQuery.data || updateVoice.isPending}
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
          checked={voiceQuery.data?.musicEnabled ?? false}
          disabled={!voiceQuery.data || updateVoice.isPending}
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
