import { useEffect, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { RiDeleteBinLine, RiUserCommunityLine } from 'react-icons/ri'
import {
  useCustomVoicePresets,
  useDeleteBookAudio,
  useUpdateVoice,
  useVoice,
  useVoiceLanguages,
  useVoicePresets,
} from '../api/queries'
import { ApiError } from '../api/client'
import { CLONE_MODELS, DEFAULT_CHARACTER_VOICE_MODE, DEFAULT_CLONE_MODEL } from '../api/types'

// Voice *selection* for one book - a dropdown of built-in and custom voices,
// a language, and the clone model every voice in the book narrates through,
// nothing else. Creating/editing custom voices lives on its own page (see
// VoicesPage), not here. Also hosts the link to
// the Speakers page (character roster/attribution) - moved in here from
// the reader header since it's a voice-adjacent, occasional-use screen,
// not something that needs to be one click away from the reading view.
export function VoicePanel({ bookId }: { bookId: string }) {
  const voiceQuery = useVoice(bookId)
  const presetsQuery = useVoicePresets()
  const customPresetsQuery = useCustomVoicePresets()
  const languagesQuery = useVoiceLanguages()
  const updateVoice = useUpdateVoice(bookId)
  const deleteBookAudio = useDeleteBookAudio(bookId)

  const [presetId, setPresetId] = useState('')
  const [language, setLanguage] = useState('Auto')
  const [cloneModel, setCloneModel] = useState<string>(DEFAULT_CLONE_MODEL)
  const [dirty, setDirty] = useState(false)
  const [deleteAudioError, setDeleteAudioError] = useState<string | null>(null)

  // Seed local editable state once, from the book's saved voice.
  useEffect(() => {
    if (voiceQuery.data && !dirty) {
      setPresetId(voiceQuery.data.presetId)
      setLanguage(voiceQuery.data.language || 'Auto')
      setCloneModel(voiceQuery.data.cloneModel || DEFAULT_CLONE_MODEL)
    }
  }, [voiceQuery.data, dirty])

  const builtins = presetsQuery.data?.presets ?? []
  const customs = customPresetsQuery.data ?? []
  const languages = languagesQuery.data?.languages ?? ['auto']

  const save = () => {
    // Switching clone model deletes the book's generated audio server-side
    // (it isn't part of any voice's cache key) - confirm first.
    if (
      voiceQuery.data &&
      cloneModel !== voiceQuery.data.cloneModel &&
      !confirm('Changing the cloning model deletes all generated audio for this book. Continue?')
    ) {
      return
    }
    const preset = builtins.find((p) => p.id === presetId) ?? customs.find((p) => p.id === presetId)
    updateVoice.mutate(
      {
        presetId,
        instruct: preset?.instruct ?? '',
        language,
        cloneModel,
        characterVoiceMode: voiceQuery.data?.characterVoiceMode ?? DEFAULT_CHARACTER_VOICE_MODE,
        speechDirection: voiceQuery.data?.speechDirection ?? false,
        musicEnabled: voiceQuery.data?.musicEnabled ?? false,
      },
      { onSuccess: () => setDirty(false) },
    )
  }

  const runDeleteAudio = () => {
    if (
      !confirm(
        'Delete all generated audio for this book? This cannot be undone - every paragraph will need to be regenerated.',
      )
    ) {
      return
    }
    setDeleteAudioError(null)
    deleteBookAudio.mutate(undefined, {
      onError: (err) =>
        setDeleteAudioError(
          err instanceof ApiError ? err.message : 'Could not delete generated audio',
        ),
    })
  }

  return (
    <div className="voice-panel-body">
      <label>
        Voice
        <select
          value={presetId}
          onChange={(e) => {
            setDirty(true)
            setPresetId(e.target.value)
          }}
        >
          {builtins.length > 0 && (
            <optgroup label="Built-in">
              {builtins.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </optgroup>
          )}
          {customs.length > 0 && (
            <optgroup label="My voices">
              {customs.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </optgroup>
          )}
        </select>
      </label>

      <label>
        Language
        <select
          value={language.toLowerCase()}
          onChange={(e) => {
            setDirty(true)
            setLanguage(e.target.value)
          }}
        >
          {languages.map((l) => (
            <option key={l} value={l}>
              {l}
            </option>
          ))}
        </select>
      </label>

      <label>
        Cloning model
        <select
          value={cloneModel}
          onChange={(e) => {
            setDirty(true)
            setCloneModel(e.target.value)
          }}
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

      <div className="voice-panel-actions">
        <button
          className="primary-button"
          onClick={save}
          disabled={!dirty || updateVoice.isPending}
        >
          {updateVoice.isPending ? 'Saving…' : 'Save voice'}
        </button>
        <button
          className="icon-action-button"
          onClick={runDeleteAudio}
          disabled={deleteBookAudio.isPending}
          title="Delete all generated audio for this book"
        >
          <RiDeleteBinLine />
        </button>
        <Link
          to="/books/$bookId/speakers"
          params={{ bookId }}
          className="icon-action-button"
          title="Speakers — character roster and attribution"
        >
          <RiUserCommunityLine />
        </Link>
      </div>
      {deleteAudioError && <p className="error-text">{deleteAudioError}</p>}

      <Link to="/voices" className="text-button">
        Manage custom voices →
      </Link>
    </div>
  )
}
