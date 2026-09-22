import { useEffect, useState } from 'react'
import {
  RiCloseLine,
  RiDeleteBinLine,
  RiEditLine,
  RiGitForkLine,
  RiPlayLine,
  RiRefreshLine,
  RiStarFill,
  RiStarLine,
} from 'react-icons/ri'
import {
  useCreateCustomVoicePreset,
  useCustomVoicePresets,
  useDefaultVoice,
  useDeleteCustomVoicePreset,
  useRegenerateCustomVoicePreset,
  useRegeneratePreset,
  useTestCustomVoicePreset,
  useTestPreset,
  useTestVoiceDesign,
  useUpdateCustomVoicePreset,
  useUpdateDefaultVoice,
  useVoicePresets,
} from '../api/queries'
import { ApiError } from '../api/client'
import {
  CLONE_MODELS,
  CLONE_MODEL_LABELS,
  DEFAULT_CHARACTER_VOICE_MODE,
  DEFAULT_CLONE_MODEL,
  DEFAULT_DESIGN_MODEL,
  DESIGN_MODEL_LABELS,
  type CustomVoicePreset,
  type DesignModel,
} from '../api/types'
import { DEFAULT_REF_TEXT, VoiceEditorForm, randomSeed } from '../components/VoiceEditorForm'
import { withCacheBust } from '../utils/cacheBust'

// "Regenerate" control for a single list row - built-in and custom preset
// rows both use this, only which mutation onRegenerate calls differs. A
// non-empty refError in the response (the render itself failed, not the
// request) surfaces the same way a thrown request error does: neither
// backend list endpoint reports a stale refError on an ordinary refetch
// (see httpapi.handleListCustomVoicePresets/handleVoicePresets, which
// never check the on-disk clip just to list presets), so this is the only
// place that error is ever visible - own local state, not the list query's
// cached data. onRegenerated (only called on genuine success - no thrown
// error, no refError) lets the caller cache-bust the now-stale <audio>
// element showing this preset's clip - see VoicesPage's own audioVersion,
// necessary because the clip's URL is a stable per-preset path that never
// itself changes, so React has no reason to tell the browser to re-fetch
// it just because the file on disk did.
function RegenerateButton({
  onRegenerate,
  onRegenerated,
}: {
  onRegenerate: () => Promise<{ refError?: string }>
  onRegenerated?: () => void
}) {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const run = async () => {
    setPending(true)
    setError(null)
    try {
      const result = await onRegenerate()
      setError(result.refError ?? null)
      if (!result.refError) onRegenerated?.()
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not regenerate this voice')
    } finally {
      setPending(false)
    }
  }

  return (
    <>
      <button
        className="icon-action-button"
        onClick={run}
        disabled={pending}
        title={pending ? 'Regenerating…' : "Regenerate — re-render this voice's reference clip from its saved recipe"}
      >
        <RiRefreshLine />
      </button>
      {error && (
        <span className="error-text" title={error}>
          ⚠
        </span>
      )}
    </>
  )
}

// Compact "test with arbitrary text" control for a single list row - a
// lighter-weight sibling of the full test box in the edit form below,
// usable right from the list without opening anything first. Shared
// between built-in and custom preset rows; only which mutation it calls
// differs.
function InlineVoiceTest({ onTest }: { onTest: (text: string) => Promise<Blob> }) {
  const [open, setOpen] = useState(false)
  const [text, setText] = useState(DEFAULT_REF_TEXT)
  const [audioUrl, setAudioUrl] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [pending, setPending] = useState(false)

  useEffect(() => {
    return () => {
      if (audioUrl) URL.revokeObjectURL(audioUrl)
    }
  }, [audioUrl])

  if (!open) {
    return (
      <button className="icon-action-button" onClick={() => setOpen(true)} title="Test with new text">
        <RiPlayLine />
      </button>
    )
  }

  const run = async () => {
    setError(null)
    setPending(true)
    try {
      setAudioUrl(await onTest(text).then(URL.createObjectURL))
    } catch (err) {
      setError(err instanceof ApiError ? err.message : 'Could not synthesize test text')
    } finally {
      setPending(false)
    }
  }

  return (
    <span className="inline-voice-test">
      <input
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder="Type text to test..."
        onKeyDown={(e) => e.key === 'Enter' && text.trim() && !pending && run()}
      />
      <button
        className="icon-action-button"
        onClick={run}
        disabled={!text.trim() || pending}
        title={pending ? 'Synthesizing…' : 'Play'}
      >
        <RiPlayLine />
      </button>
      {error && <span className="error-text">{error}</span>}
      {audioUrl && <audio controls autoPlay preload="none" src={audioUrl} />}
      <button className="icon-action-button" onClick={() => setOpen(false)} title="Close">
        <RiCloseLine />
      </button>
    </span>
  )
}

type FormTarget = null | '__new__' | string

export function VoicesPage() {
  const builtinsQuery = useVoicePresets()
  const presetsQuery = useCustomVoicePresets()
  const defaultVoiceQuery = useDefaultVoice()
  const createPreset = useCreateCustomVoicePreset()
  const updatePreset = useUpdateCustomVoicePreset()
  const deletePreset = useDeleteCustomVoicePreset()
  const regeneratePreset = useRegenerateCustomVoicePreset()
  const regenerateBuiltinPreset = useRegeneratePreset()
  const updateDefaultVoice = useUpdateDefaultVoice()
  const testPreset = useTestCustomVoicePreset()
  const testBuiltinPreset = useTestPreset()
  const testDesign = useTestVoiceDesign()

  const builtins = builtinsQuery.data?.presets ?? []
  const presets = presetsQuery.data ?? []
  const defaultPresetId = defaultVoiceQuery.data?.presetId

  // Bumped per preset id on a successful Regenerate so its <audio> element
  // re-fetches instead of showing/playing the pre-regenerate clip - see
  // withCacheBust's own doc comment.
  const [audioVersion, setAudioVersion] = useState<Record<string, number>>({})
  const bumpAudioVersion = (id: string) => setAudioVersion((v) => ({ ...v, [id]: (v[id] ?? 0) + 1 }))

  const updateDefaults = (patch: { presetId: string; instruct: string } | { cloneModel: string }) => {
    const current = defaultVoiceQuery.data
    updateDefaultVoice.mutate({
      presetId: current?.presetId ?? '',
      instruct: current?.instruct ?? '',
      language: current?.language ?? 'Auto',
      cloneModel: current?.cloneModel ?? DEFAULT_CLONE_MODEL,
      ...patch,
      // characterVoiceMode/speechDirection/musicEnabled are per-book
      // settings (see SpeakersPage) - meaningless for "what a freshly
      // uploaded book starts with", so none of them are read by
      // handleUpdateDefaultVoice; sent at their defaults just to satisfy
      // the shared VoiceSettings shape.
      characterVoiceMode: DEFAULT_CHARACTER_VOICE_MODE,
      speechDirection: false,
      musicEnabled: false,
    })
  }
  const selectDefault = (p: { id: string; instruct: string }) => updateDefaults({ presetId: p.id, instruct: p.instruct })

  const [target, setTarget] = useState<FormTarget>(null)
  const [name, setName] = useState('')
  const [instruct, setInstruct] = useState('')
  const [refText, setRefText] = useState('')
  const [speedMultiplier, setSpeedMultiplier] = useState(1)
  // Preview-only - which clone model "Play test" runs a saved voice
  // through. Not part of the voice (it has none); starts at the default
  // clone model new books get.
  const [previewCloneModel, setPreviewCloneModel] = useState<string | null>(null)
  const effectivePreviewCloneModel = previewCloneModel ?? defaultVoiceQuery.data?.cloneModel ?? DEFAULT_CLONE_MODEL
  // Which VoiceDesign engine renders the reference clip - always reset to
  // DEFAULT_DESIGN_MODEL for a brand-new voice (openNew) or one derived
  // from an existing preset (openDerive), regardless of what that source
  // preset's own designModel is - see voices.DefaultDesignModel's own doc
  // comment for why a new voice never inherits this. Only openEdit (an
  // existing custom preset) reads it back from the preset being edited.
  const [designModel, setDesignModel] = useState<string>(DEFAULT_DESIGN_MODEL)
  // Only set when deriving from an existing voice (see openDerive) - a
  // fresh "New voice" leaves this undefined so the backend picks a random
  // seed, same as before.
  const [seed, setSeed] = useState<number | undefined>(undefined)
  const [error, setError] = useState<string | null>(null)
  const [testText, setTestText] = useState(DEFAULT_REF_TEXT)
  const [testAudioUrl, setTestAudioUrl] = useState<string | null>(null)
  const [testError, setTestError] = useState<string | null>(null)

  // Revokes the previous object URL whenever it's replaced, and on unmount.
  useEffect(() => {
    return () => {
      if (testAudioUrl) URL.revokeObjectURL(testAudioUrl)
    }
  }, [testAudioUrl])

  const editing = target && target !== '__new__' ? presets.find((p) => p.id === target) : undefined
  const saving = createPreset.isPending || updatePreset.isPending

  const resetTest = () => {
    setTestText(DEFAULT_REF_TEXT)
    setTestAudioUrl(null)
    setTestError(null)
  }

  const openNew = () => {
    setName('')
    setInstruct('')
    setRefText(DEFAULT_REF_TEXT)
    setSpeedMultiplier(1)
    // Pinned now (not left for the server to pick at save time) so a
    // design preview and the eventual Create voice call are guaranteed to
    // agree on one - see randomSeed's own comment.
    setSeed(randomSeed())
    setDesignModel(DEFAULT_DESIGN_MODEL)
    setError(null)
    resetTest()
    setTarget('__new__')
  }

  const openEdit = (p: CustomVoicePreset) => {
    setName(p.name)
    setInstruct(p.instruct)
    setRefText(p.refText)
    setSpeedMultiplier(p.speedMultiplier)
    setSeed(p.seed)
    setDesignModel(p.designModel || DEFAULT_DESIGN_MODEL)
    setError(null)
    resetTest()
    setTarget(p.id)
  }

  // Starts a new voice pre-filled from an existing preset's recipe (built-in
  // or custom), keeping the same seed - instruct/refText/seed together
  // are what a rendered reference clip is deterministic on, so
  // leaving them unchanged is what lets the backend recognize a speed-only
  // derivation and reuse the source's clip instead of paying for a full
  // re-render. Changing the instruct text (a common reason to derive) still
  // gives a genuinely different voice even with the seed held fixed.
  //
  // designModel is deliberately NOT part of source/this derivation - a
  // brand-new voice always starts on DEFAULT_DESIGN_MODEL regardless of
  // which engine the source preset itself renders through (see voices.
  // DefaultDesignModel's own doc comment: "regardless of parentage").
  const openDerive = (source: {
    name: string
    instruct: string
    refText: string
    speedMultiplier: number
    seed: number
  }) => {
    setName(`${source.name} (derived)`)
    setInstruct(source.instruct)
    setRefText(source.refText)
    setSpeedMultiplier(source.speedMultiplier)
    setSeed(source.seed)
    setDesignModel(DEFAULT_DESIGN_MODEL)
    setError(null)
    resetTest()
    setTarget('__new__')
  }

  // Design-preview guidance_scale override - see VoiceEditorForm's
  // test.guidance. "" = the design engine's own default.
  const [designGuidanceScale, setDesignGuidanceScale] = useState('')
  // Sampling-temperature override for either test path - see
  // VoiceEditorForm's test.temperature. "" = the model's own default.
  const [testTemperature, setTestTemperature] = useState('')
  const temperatureOverride = testTemperature.trim() ? Number(testTemperature) : undefined

  const runTest = () => {
    setTestError(null)
    if (editing) {
      testPreset.mutate(
        {
          id: editing.id,
          text: testText,
          cloneModel: effectivePreviewCloneModel,
          temperature: temperatureOverride,
        },
        {
          onSuccess: (blob) => setTestAudioUrl(URL.createObjectURL(blob)),
          onError: (err) => setTestError(err instanceof ApiError ? err.message : 'Could not synthesize test text'),
        },
      )
      return
    }
    // No saved preset yet - preview via VoiceDesign directly, rendering
    // refText specifically (not a separate freeform line) so that saving
    // right after, unchanged, reuses this exact clip - see backend
    // voicerefs.DesignConfigHash.
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
        onError: (err) => setTestError(err instanceof ApiError ? err.message : 'Could not preview this voice'),
      },
    )
  }

  // Groups auto-created speaker voices (see SpeakersPage's
  // CharacterVoiceEditor) by the series - or book, if that character isn't
  // part of one - their roster belongs to (backend's groupLabel, derived
  // from store.SeriesScope), so a book/series worth of character voices
  // reads as one cluster instead of being lost in one flat list alongside a
  // reader's own general-purpose voices. Those ungrouped voices (no
  // groupLabel - never assigned to a character) render first, unchanged.
  const groupedPresets = new Map<string, CustomVoicePreset[]>()
  const ungroupedPresets: CustomVoicePreset[] = []
  for (const p of presets) {
    if (p.groupLabel) {
      const list = groupedPresets.get(p.groupLabel)
      if (list) list.push(p)
      else groupedPresets.set(p.groupLabel, [p])
    } else {
      ungroupedPresets.push(p)
    }
  }
  const groupNames = [...groupedPresets.keys()].sort((a, b) => a.localeCompare(b))

  const renderPreset = (p: CustomVoicePreset) => (
    <li key={p.id}>
      <span>{p.name}</span>
      <span className="muted voice-model-badge">
        {DESIGN_MODEL_LABELS[(p.designModel || DEFAULT_DESIGN_MODEL) as DesignModel] ?? p.designModel}
      </span>
      {p.refError && (
        <span className="error-text" title={p.refError}>
          ⚠
        </span>
      )}
      <audio controls preload="none" src={p.audioUrl} />
      {defaultPresetId === p.id ? (
        <span className="icon-action-button" title="Default voice">
          <RiStarFill />
        </span>
      ) : (
        <button
          className="icon-action-button"
          onClick={() => selectDefault(p)}
          disabled={updateDefaultVoice.isPending}
          title="Set as default"
        >
          <RiStarLine />
        </button>
      )}
      <button className="icon-action-button" onClick={() => openEdit(p)} title="Edit">
        <RiEditLine />
      </button>
      <RegenerateButton onRegenerate={() => regeneratePreset.mutateAsync(p.id)} />
      <button
        className="icon-action-button"
        onClick={() =>
          openDerive({
            name: p.name,
            instruct: p.instruct,
            refText: p.refText,
            speedMultiplier: p.speedMultiplier,
            seed: p.seed,
          })
        }
        title="Derive a new voice"
      >
        <RiGitForkLine />
      </button>
      <button
        className="icon-action-button"
        onClick={() => confirm(`Delete voice "${p.name}"?`) && deletePreset.mutate(p.id)}
        title="Delete"
      >
        <RiDeleteBinLine />
      </button>
      <InlineVoiceTest onTest={(text) => testPreset.mutateAsync({ id: p.id, text })} />
    </li>
  )

  const save = () => {
    setError(null)
    const input = { name, instruct, refText, speedMultiplier, seed, designModel }
    const onError = (err: unknown) => setError(err instanceof ApiError ? err.message : 'Could not save voice')
    if (editing) {
      updatePreset.mutate({ id: editing.id, input }, { onSuccess: () => setTarget(null), onError })
    } else {
      createPreset.mutate(input, { onSuccess: () => setTarget(null), onError })
    }
  }

  return (
    <div className="voices-page">
      <div className="library-header">
        <h1>Your voices</h1>
        {target === null && (
          <button className="primary-button" onClick={openNew}>
            + New voice
          </button>
        )}
      </div>

      {target === null && (
        <>
          <label>
            Default cloning model for new books
            <select
              value={defaultVoiceQuery.data?.cloneModel ?? DEFAULT_CLONE_MODEL}
              onChange={(e) => updateDefaults({ cloneModel: e.target.value })}
              disabled={!defaultVoiceQuery.data || updateDefaultVoice.isPending}
            >
              {CLONE_MODELS.map((m) => (
                <option key={m} value={m}>
                  {CLONE_MODEL_LABELS[m]}
                </option>
              ))}
            </select>
          </label>
          <p className="muted">
            Each book picks its own cloning model in its voice settings. This only sets what a newly added
            book starts with. Voices don't have a cloning model of their own, and previews here use this
            default.
          </p>

          <h2>Built-in</h2>
          {builtinsQuery.isLoading && <p>Loading…</p>}
          {builtinsQuery.isError && <p className="error-text">Could not load built-in voices.</p>}
          <ul className="custom-voice-list custom-voice-list-page">
            {builtins.map((p) => (
              <li key={p.id}>
                <span>{p.name}</span>
                <span className="muted voice-model-badge">
                  {DESIGN_MODEL_LABELS[(p.designModel || DEFAULT_DESIGN_MODEL) as DesignModel] ?? p.designModel}
                </span>
                <audio controls preload="none" src={withCacheBust(p.audioUrl, audioVersion[p.id])} />
                {defaultPresetId === p.id ? (
                  <span className="icon-action-button" title="Default voice">
                    <RiStarFill />
                  </span>
                ) : (
                  <button
                    className="icon-action-button"
                    onClick={() => selectDefault(p)}
                    disabled={updateDefaultVoice.isPending}
                    title="Set as default"
                  >
                    <RiStarLine />
                  </button>
                )}
                <button
                  className="icon-action-button"
                  onClick={() =>
                    openDerive({
                      name: p.name,
                      instruct: p.instruct,
                      refText: p.ref_text,
                      speedMultiplier: p.speed_multiplier,
                      seed: p.seed,
                    })
                  }
                  title="Derive a new voice"
                >
                  <RiGitForkLine />
                </button>
                <RegenerateButton
                  onRegenerate={() => regenerateBuiltinPreset.mutateAsync(p.id)}
                  onRegenerated={() => bumpAudioVersion(p.id)}
                />
                <InlineVoiceTest onTest={(text) => testBuiltinPreset.mutateAsync({ id: p.id, text })} />
              </li>
            ))}
          </ul>

          <h2>Your voices</h2>
        </>
      )}

      {presetsQuery.isLoading && <p>Loading…</p>}
      {presetsQuery.isError && <p className="error-text">Could not load your voices.</p>}
      {presets.length === 0 && target === null && (
        <p className="muted">No custom voices yet. Create one to use across any book.</p>
      )}

      {target === null ? (
        <>
          {ungroupedPresets.length > 0 && (
            <ul className="custom-voice-list custom-voice-list-page">{ungroupedPresets.map(renderPreset)}</ul>
          )}
          {groupNames.map((name) => (
            <div key={name} className="custom-voice-group">
              <h3>{name}</h3>
              <ul className="custom-voice-list custom-voice-list-page">
                {groupedPresets.get(name)!.map(renderPreset)}
              </ul>
            </div>
          ))}
        </>
      ) : (
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
          currentAudioUrl={editing?.audioUrl}
          error={error}
          saving={saving}
          saveLabel={editing ? 'Save changes' : 'Create voice'}
          saveDisabled={!name || !instruct || !refText}
          onSave={save}
          onCancel={() => setTarget(null)}
          test={{
            text: testText,
            onTextChange: setTestText,
            audioUrl: testAudioUrl,
            error: testError,
            pending: editing ? testPreset.isPending : testDesign.isPending,
            disabled: editing ? !testText.trim() : !instruct.trim() || !refText.trim(),
            buttonLabel: editing ? 'Play test' : 'Preview',
            hint: editing
              ? 'Uses the saved voice through the preview cloning model above - save any other changes first to hear them reflected here.'
              : 'Preview renders the reference line above via VoiceDesign directly - no preset saved yet. Creating the voice right after, unchanged, reuses this exact clip instead of rendering again.',
            onRun: runTest,
            guidance: editing ? undefined : { value: designGuidanceScale, onChange: setDesignGuidanceScale },
            temperature: { value: testTemperature, onChange: setTestTemperature },
            cloneModel: editing ? { value: effectivePreviewCloneModel, onChange: setPreviewCloneModel } : undefined,
          }}
        />
      )}
    </div>
  )
}
