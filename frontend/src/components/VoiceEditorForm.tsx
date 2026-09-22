import type { CSSProperties } from 'react'
import {
  CLONE_MODELS,
  CLONE_MODEL_LABELS,
  DESIGN_MODELS,
  DESIGN_MODEL_GUIDANCE_DEFAULTS,
  DESIGN_MODEL_LABELS,
  DEFAULT_DESIGN_MODEL,
  type CloneModel,
  type DesignModel,
} from '../api/types'

// Kept identical to tts-service's own DEFAULT_REF_TEXT (voices.py) - the
// same phonetically-balanced pangram-style line it clones its built-in
// presets from, so a custom voice's default reference clip samples English
// phonemes just as broadly instead of whatever a short throwaway line
// happens to contain.
export const DEFAULT_REF_TEXT =
  'The beige hue on the waters of the loch impressed all, including the French queen, ' +
  'before she heard that symphony again, just as young Arthur wanted.'

// Matches tts-service's WSOLA range (wsola.py's MIN/MAX_PLAYBACK_RATE) -
// speed_multiplier is applied to the voice's reference clip, not live
// playback, so it shares the same pitch-preserving time-stretch limits.
export const SPEED_MIN = 0.5
export const SPEED_MAX = 4
export const SPEED_PRESETS = [0.5, 0.75, 1, 1.25, 1.5, 1.75, 2, 2.5, 3, 3.5, 4]

// Kept identical to the backend's httpapi.DefaultCloneModel - what a
// fresh voice starts with before the user picks otherwise.
export const DEFAULT_CLONE_MODEL: CloneModel = 'audiocpp-higgs-4b'

// A positive 31-bit seed, same shape as the backend's own store.RandomSeed
// (doesn't need to match bit-for-bit, just "some positive int"). A brand
// new voice pins one of these up front rather than leaving it for the
// server to pick at save time, specifically so a design preview and the
// eventual create-voice call agree on the same seed - otherwise they'd be
// unrelated random samples of the same instruct, and saving could never
// reuse what testing just rendered (see backend voicerefs.DesignConfigHash).
export function randomSeed(): number {
  return Math.floor(Math.random() * 0x7fffffff)
}

// Where a speed value sits along the slider, as a 0-100% position - shared
// by the fill gradient (how far the accent-colored portion extends) and
// the tick/label overlay (where each preset's mark and number line up).
function speedPercent(value: number): number {
  return ((value - SPEED_MIN) / (SPEED_MAX - SPEED_MIN)) * 100
}

// The recipe fields + test box shared by VoicesPage's create/edit form and
// SpeakersPage's per-character "Customize voice" editor - the "full voice
// editor" both pages present is this same component, just wired to
// different save/test business logic (create vs. update-in-place vs.
// create-then-assign-to-a-character).
export function VoiceEditorForm({
  name,
  onNameChange,
  instruct,
  onInstructChange,
  refText,
  onRefTextChange,
  speedMultiplier,
  onSpeedChange,
  cloneModel,
  onCloneModelChange,
  designModel,
  onDesignModelChange,
  currentAudioUrl,
  error,
  saving,
  saveLabel,
  saveDisabled,
  onSave,
  onCancel,
  test,
}: {
  name: string
  onNameChange: (v: string) => void
  instruct: string
  onInstructChange: (v: string) => void
  refText: string
  onRefTextChange: (v: string) => void
  speedMultiplier: number
  onSpeedChange: (v: number) => void
  cloneModel: string
  onCloneModelChange: (v: string) => void
  // Which VoiceDesign engine renders this voice's reference clip - a
  // separate concern from cloneModel above, which is what clones
  // per-paragraph audio from that already-rendered clip.
  designModel: string
  onDesignModelChange: (v: string) => void
  // Shown as "Current reference clip" when set - only a preset being
  // edited in place (not a fresh/derived one) has one yet.
  currentAudioUrl?: string
  error?: string | null
  saving: boolean
  saveLabel: string
  saveDisabled?: boolean
  onSave: () => void
  onCancel: () => void
  test: {
    text: string
    onTextChange: (v: string) => void
    audioUrl: string | null
    error: string | null
    pending: boolean
    disabled: boolean
    buttonLabel: string
    hint: string
    onRun: () => void
    // Set only while the test runs through VoiceDesign (a voice not yet
    // saved) - a one-off guidance_scale override for that preview, "" for
    // the design engine's own default. Hidden for an engine with no
    // guidance option at all.
    guidance?: {
      value: string
      onChange: (v: string) => void
    }
  }
}) {
  const guidanceDefault = DESIGN_MODEL_GUIDANCE_DEFAULTS[(designModel || DEFAULT_DESIGN_MODEL) as DesignModel]

  return (
    <div className="custom-voice-form">
      <label>
        Name
        <input value={name} onChange={(e) => onNameChange(e.target.value)} placeholder="e.g. Grandpa Joe" />
      </label>
      {currentAudioUrl && (
        <div className="voice-ref-preview">
          <span className="muted">Current reference clip:</span>
          <audio controls preload="none" src={currentAudioUrl} />
        </div>
      )}
      <label>
        Voice instruction
        <textarea
          value={instruct}
          onChange={(e) => onInstructChange(e.target.value)}
          rows={2}
          placeholder="Describe the voice: pace, tone, accent..."
        />
      </label>
      <label>
        Reference line
        <textarea value={refText} onChange={(e) => onRefTextChange(e.target.value)} rows={2} />
      </label>
      <label>
        Speed ({speedMultiplier.toFixed(2)}×)
        <div className="speed-slider">
          <input
            type="range"
            min={SPEED_MIN}
            max={SPEED_MAX}
            step={0.05}
            list="speed-presets"
            value={speedMultiplier}
            onChange={(e) => onSpeedChange(Number(e.target.value))}
            style={{ '--fill': `${speedPercent(speedMultiplier)}%` } as CSSProperties}
          />
          {/* list="speed-presets" gives the thumb a slight magnetic pull
              toward these values, on top of the tick marks/labels drawn
              below, which handle the visible number-line look. */}
          <datalist id="speed-presets">
            {SPEED_PRESETS.map((s) => (
              <option key={s} value={s} />
            ))}
          </datalist>
          <div className="speed-slider-ticks">
            {SPEED_PRESETS.map((s) => (
              <span key={s} className="speed-slider-tick" style={{ left: `${speedPercent(s)}%` }} />
            ))}
          </div>
          <div className="speed-slider-labels">
            {SPEED_PRESETS.map((s, i) => (
              <span
                key={s}
                className={
                  'speed-slider-label' +
                  (i === 0 ? ' speed-slider-label-start' : '') +
                  (i === SPEED_PRESETS.length - 1 ? ' speed-slider-label-end' : '')
                }
                style={{ left: `${speedPercent(s)}%` }}
              >
                {s}×
              </span>
            ))}
          </div>
        </div>
      </label>
      <label>
        Cloning model
        <select value={cloneModel} onChange={(e) => onCloneModelChange(e.target.value)}>
          {CLONE_MODELS.map((b) => (
            <option key={b} value={b}>
              {CLONE_MODEL_LABELS[b]}
            </option>
          ))}
        </select>
      </label>
      <label>
        Design model
        <select value={designModel || DEFAULT_DESIGN_MODEL} onChange={(e) => onDesignModelChange(e.target.value)}>
          {DESIGN_MODELS.map((b) => (
            <option key={b} value={b}>
              {DESIGN_MODEL_LABELS[b]}
            </option>
          ))}
        </select>
      </label>
      {error && <p className="error-text">{error}</p>}
      <div className="voice-panel-actions">
        <button className="primary-button" onClick={onSave} disabled={saveDisabled || saving}>
          {saving ? 'Saving…' : saveLabel}
        </button>
        <button className="text-button" onClick={onCancel}>
          Cancel
        </button>
      </div>

      <div className="voice-test-box">
        <label>
          Test with text
          <textarea
            value={test.text}
            onChange={(e) => test.onTextChange(e.target.value)}
            rows={2}
            placeholder="Type any sentence to hear this voice say it..."
          />
        </label>
        {test.guidance && guidanceDefault !== undefined && (
          <>
            <label>
              Guidance scale
              <input
                type="number"
                min={0}
                step={0.1}
                placeholder={`default (${guidanceDefault})`}
                value={test.guidance.value}
                onChange={(e) => test.guidance?.onChange(e.target.value)}
              />
            </label>
            <p className="muted">
              How strongly the design model follows the voice instruction - higher values push harder toward
              the description, lower values sound more natural but drift from it. Applies to this preview
              only: a preview with a custom value is never reused on save, which renders the reference clip
              at the model's default.
            </p>
          </>
        )}
        <p className="muted">{test.hint}</p>
        {test.error && <p className="error-text">{test.error}</p>}
        <div className="voice-panel-actions">
          <button className="primary-button" onClick={test.onRun} disabled={test.disabled || test.pending}>
            {test.pending ? 'Synthesizing…' : test.buttonLabel}
          </button>
          {test.audioUrl && <audio controls autoPlay src={test.audioUrl} />}
        </div>
      </div>
    </div>
  )
}
