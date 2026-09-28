import { useEffect, useState } from 'react'
import {
  useCreateCustomVoicePreset,
  useSetCharacterVoice,
  useTestCloneInstruct,
  useTestCustomVoicePreset,
  useTestVoiceDesign,
  useUpdateCustomVoicePreset,
} from '../../api/queries'
import { ApiError } from '../../api/client'
import { DEFAULT_REF_TEXT, VoiceEditorForm, randomSeed } from '../../components/VoiceEditorForm'
import { DEFAULT_DESIGN_MODEL, INSTRUCTED_CLONE_MODEL } from '../../api/types'
import type { CustomVoicePreset, Speaker, VoicePreset } from '../../api/types'

// Full voice editor for one character - the same recipe fields the Voices
// page uses (VoiceEditorForm), pre-filled from whichever voice this
// character currently resolves to (their own assigned preset if they have
// one, otherwise their characterization summary, if any) and, on save,
// either updating that preset in place (already a custom one of theirs) or
// creating a fresh custom preset and assigning it to this character in one
// step (currently unassigned, or currently resolving to a built-in, which
// can't be edited in place).
export function CharacterVoiceEditor({
  bookId,
  speaker,
  builtins,
  customs,
  narratorPresetId,
  bookCloneModel,
  onDone,
}: {
  bookId: string
  speaker: Speaker
  builtins: VoicePreset[]
  customs: CustomVoicePreset[]
  narratorPresetId: string | undefined
  bookCloneModel: string
  onDone: () => void
}) {
  const createPreset = useCreateCustomVoicePreset()
  const updatePreset = useUpdateCustomVoicePreset()
  const setCharacterVoice = useSetCharacterVoice(bookId)
  const testPreset = useTestCustomVoicePreset()
  const testDesign = useTestVoiceDesign()
  const testCloneInstruct = useTestCloneInstruct()

  // The preset this character's current voice comes from, if any - a
  // custom one of theirs (editable in place) or a built-in (only usable
  // as a starting point for a new custom one, same as the Voices page's
  // own "Derive a new voice").
  const customSource = customs.find((p) => p.id === speaker.voicePresetId)
  const builtinSource = !customSource
    ? builtins.find((p) => p.id === speaker.voicePresetId)
    : undefined
  const isEditingCustom = !!customSource

  const [name, setName] = useState(customSource?.name ?? builtinSource?.name ?? speaker.name)
  const [instruct, setInstruct] = useState(
    customSource?.instruct ?? builtinSource?.instruct ?? speaker.summary ?? '',
  )
  const [refText, setRefText] = useState(
    customSource?.refText ?? builtinSource?.ref_text ?? DEFAULT_REF_TEXT,
  )
  const [speedMultiplier, setSpeedMultiplier] = useState(
    customSource?.speedMultiplier ?? builtinSource?.speed_multiplier ?? 1,
  )
  // Preview-only - which clone model "Play test" runs the saved voice
  // through. Not part of the voice (it has none); starts at this book's
  // own clone model, what the voice will actually narrate through here.
  const [previewCloneModel, setPreviewCloneModel] = useState(bookCloneModel)
  // Only ever inherited from an already-editable custom preset, never from
  // a builtinSource - same "a new voice never inherits another preset's
  // design model" reasoning as VoicesPage's own openDerive.
  const [designModel, setDesignModel] = useState(customSource?.designModel || DEFAULT_DESIGN_MODEL)
  // Pinned once (not left for the server to pick at save time) so a
  // preview and the eventual create-voice call agree on the same seed -
  // see backend voicerefs.DesignConfigHash. Irrelevant once isEditingCustom
  // (its own already-saved seed is reused untouched).
  const [seed] = useState(customSource?.seed ?? builtinSource?.seed ?? randomSeed())
  const [error, setError] = useState<string | null>(null)
  const [testText, setTestText] = useState(refText)
  const [testAudioUrl, setTestAudioUrl] = useState<string | null>(null)
  const [testError, setTestError] = useState<string | null>(null)

  // "Test with another voice as a base" - previews instructed voice
  // cloning (BreezeTTS 2's own ability to clone a reference clip *and*
  // apply a style instruction in one call) directly, without saving
  // anything: clones baseVoiceId's own already-rendered reference clip
  // while applying the instruct text above as a clone-time instruction -
  // the same thing store.Book.InstructCharacterVoices does automatically
  // for an unassigned character, previewable here per-character before
  // turning that book-wide setting on. Defaults to the book's own
  // narrator voice, the base a reader will pick most often. Kept as its
  // own audio/error state, independent of the "Test with text" box above,
  // so running one doesn't clobber the other's result.
  const [baseVoiceId, setBaseVoiceId] = useState(narratorPresetId ?? builtins[0]?.id ?? '')
  // "" defers to the backend's own configured default
  // (LECTABLE_AUDIOCPP_BREEZE_CLONE_GUIDANCE_SCALE) - only overrides it for
  // this one test call when a reader types a value, so experimenting here
  // never requires a server restart/env var change.
  const [guidanceScale, setGuidanceScale] = useState('')
  const [baseTestAudioUrl, setBaseTestAudioUrl] = useState<string | null>(null)
  const [baseTestError, setBaseTestError] = useState<string | null>(null)

  useEffect(() => {
    return () => {
      if (testAudioUrl) URL.revokeObjectURL(testAudioUrl)
    }
  }, [testAudioUrl])

  useEffect(() => {
    return () => {
      if (baseTestAudioUrl) URL.revokeObjectURL(baseTestAudioUrl)
    }
  }, [baseTestAudioUrl])

  const saving = createPreset.isPending || updatePreset.isPending || setCharacterVoice.isPending

  // Design-preview guidance_scale override - see VoiceEditorForm's
  // test.guidance. "" = the design engine's own default.
  const [designGuidanceScale, setDesignGuidanceScale] = useState('')
  // Sampling-temperature override for either test path - see
  // VoiceEditorForm's test.temperature. "" = the model's own default.
  const [testTemperature, setTestTemperature] = useState('')
  const temperatureOverride = testTemperature.trim() ? Number(testTemperature) : undefined

  const runTest = () => {
    setTestError(null)
    if (isEditingCustom) {
      testPreset.mutate(
        {
          id: customSource.id,
          text: testText,
          cloneModel: previewCloneModel,
          temperature: temperatureOverride,
        },
        {
          onSuccess: (blob) => setTestAudioUrl(URL.createObjectURL(blob)),
          onError: (err) =>
            setTestError(err instanceof ApiError ? err.message : 'Could not synthesize test text'),
        },
      )
      return
    }
    testDesign.mutate(
      {
        instruct,
        text: refText,
        seed,
        designModel,
        guidanceScale: designGuidanceScale.trim() ? Number(designGuidanceScale) : undefined,
        temperature: temperatureOverride,
      },
      {
        onSuccess: (blob) => setTestAudioUrl(URL.createObjectURL(blob)),
        onError: (err) =>
          setTestError(err instanceof ApiError ? err.message : 'Could not preview this voice'),
      },
    )
  }

  const runBaseTest = () => {
    if (!baseVoiceId) return
    setBaseTestError(null)
    testCloneInstruct.mutate(
      {
        baseId: baseVoiceId,
        instruct,
        text: testText,
        cloneModel: INSTRUCTED_CLONE_MODEL,
        guidanceScale: guidanceScale.trim() || undefined,
      },
      {
        onSuccess: (blob) => setBaseTestAudioUrl(URL.createObjectURL(blob)),
        onError: (err) =>
          setBaseTestError(
            err instanceof ApiError ? err.message : 'Could not synthesize this voice',
          ),
      },
    )
  }

  const save = () => {
    setError(null)
    const input = { name, instruct, refText, speedMultiplier, seed, designModel }
    const onError = (err: unknown) =>
      setError(err instanceof ApiError ? err.message : 'Could not save voice')
    if (isEditingCustom) {
      updatePreset.mutate({ id: customSource.id, input }, { onSuccess: onDone, onError })
      return
    }
    createPreset.mutate(input, {
      onSuccess: (created) =>
        setCharacterVoice.mutate(
          { characterId: speaker.id, voicePresetId: created.id },
          { onSuccess: onDone, onError },
        ),
      onError,
    })
  }

  return (
    <div className="speaker-voice-editor">
      <VoiceEditorForm
        name={name}
        onNameChange={setName}
        instruct={instruct}
        onInstructChange={setInstruct}
        refText={refText}
        onRefTextChange={setRefText}
        speedMultiplier={speedMultiplier}
        onSpeedChange={setSpeedMultiplier}
        designModel={designModel}
        onDesignModelChange={setDesignModel}
        currentAudioUrl={isEditingCustom ? speaker.refAudioUrl : undefined}
        error={error}
        saving={saving}
        saveLabel={isEditingCustom ? 'Save changes' : 'Create and assign voice'}
        saveDisabled={!name || !instruct || !refText}
        onSave={save}
        onCancel={onDone}
        test={{
          text: testText,
          onTextChange: setTestText,
          audioUrl: testAudioUrl,
          error: testError,
          pending: isEditingCustom ? testPreset.isPending : testDesign.isPending,
          disabled: isEditingCustom ? !testText.trim() : !instruct.trim() || !refText.trim(),
          buttonLabel: isEditingCustom ? 'Play test' : 'Preview',
          hint: isEditingCustom
            ? 'Uses the saved voice through the preview cloning model above - save any other changes first to hear them reflected here.'
            : 'Preview renders the reference line above via VoiceDesign directly - no preset saved yet. Saving right after, unchanged, reuses this exact clip instead of rendering again.',
          onRun: runTest,
          guidance: isEditingCustom
            ? undefined
            : { value: designGuidanceScale, onChange: setDesignGuidanceScale },
          temperature: { value: testTemperature, onChange: setTestTemperature },
          cloneModel: isEditingCustom
            ? { value: previewCloneModel, onChange: setPreviewCloneModel }
            : undefined,
        }}
      />

      <div className="voice-test-box">
        <label>
          Test with another voice as a base
          <select value={baseVoiceId} onChange={(e) => setBaseVoiceId(e.target.value)}>
            {builtins.map((p) => (
              <option key={p.id} value={p.id}>
                {p.id === narratorPresetId ? `${p.name} (this book's narrator)` : p.name}
              </option>
            ))}
            {customs.map((p) => (
              <option key={p.id} value={p.id}>
                {p.id === narratorPresetId ? `${p.name} (this book's narrator)` : p.name}
              </option>
            ))}
          </select>
        </label>
        <p className="muted">
          Clones the selected voice's own reference clip while applying the voice instruction above
          as a clone-time style direction, instead of designing a brand-new voice from scratch -
          previews exactly what "Style unassigned characters with the narrator's own voice"
          (Speakers page settings) does automatically. Only the "{INSTRUCTED_CLONE_MODEL}" clone
          model actually honors the instruction this way, so this always tests through that model
          regardless of this book's own cloning model.
        </p>
        <label>
          Guidance scale
          <input
            type="number"
            min={0}
            step={0.5}
            placeholder="server default"
            value={guidanceScale}
            onChange={(e) => setGuidanceScale(e.target.value)}
          />
        </label>
        <p className="muted">
          How strongly the model follows the instruction (and the reference clip/text) above -
          higher values push harder toward all of that conditioning at once, at some cost to
          generation speed. Blank uses the backend's own configured default. Overrides it for this
          test only, so you can compare values without a server restart.
        </p>
        {baseTestError && <p className="error-text">{baseTestError}</p>}
        <div className="voice-panel-actions">
          <button
            className="primary-button"
            onClick={runBaseTest}
            disabled={
              !baseVoiceId || !instruct.trim() || !testText.trim() || testCloneInstruct.isPending
            }
          >
            {testCloneInstruct.isPending ? 'Synthesizing…' : 'Test with base voice'}
          </button>
          {baseTestAudioUrl && <audio controls autoPlay src={baseTestAudioUrl} />}
        </div>
      </div>
    </div>
  )
}
