import { useEffect, useState } from 'react'
import { useTestSFX } from '../api/queries'
import { ApiError } from '../api/client'
import { SFX_ENGINES, SFX_ENGINE_LABELS, type SFXEngine } from '../api/types'

// Standalone sound-effect/music test page (see backend/CLAUDE.md's "SFX
// sound effects" section) - a faster iteration loop for a prompt/knob/
// engine combination than the reader's own per-paragraph SFX panel
// (ChapterSection.tsx, Stable Audio SFX only), since nothing here is
// attached to a book/chapter/paragraph or persisted; every generated clip
// only ever lives as a local blob URL in this page's own state, same
// shape as VoicesPage's "test design" flow (useTestVoiceDesign/
// testVoiceDesign). One page, one engine picker (SFX_ENGINES) - not one
// page per engine - matching the backend's own single
// POST /api/sfx/generate endpoint.
export function SFXPage() {
  const testSFX = useTestSFX()

  const [engine, setEngine] = useState<SFXEngine>('stable_audio_music')
  const [prompt, setPrompt] = useState('')
  const [lyrics, setLyrics] = useState('')
  const [negativePrompt, setNegativePrompt] = useState('')
  // Text, not number, for the optional numeric knobs below - an empty
  // string is exactly "unset, defer to this engine's own default" (see
  // GenerateSFXRequest), which a bare `0` can't distinguish from "the reader
  // typed zero".
  const [durationSeconds, setDurationSeconds] = useState('')
  const [numInferenceSteps, setNumInferenceSteps] = useState('')
  const [guidanceScale, setGuidanceScale] = useState('')
  const [seed, setSeed] = useState('')

  const [audioUrl, setAudioUrl] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    return () => {
      if (audioUrl) URL.revokeObjectURL(audioUrl)
    }
  }, [audioUrl])

  const run = () => {
    if (!prompt.trim()) return
    setError(null)
    testSFX.mutate(
      {
        engine,
        prompt: prompt.trim(),
        lyrics: engine === 'ace_step' ? lyrics.trim() || undefined : undefined,
        negativePrompt: negativePrompt.trim() || undefined,
        durationSeconds: durationSeconds ? Number(durationSeconds) : undefined,
        numInferenceSteps: numInferenceSteps ? Number(numInferenceSteps) : undefined,
        guidanceScale: guidanceScale ? Number(guidanceScale) : undefined,
        seed: seed ? Number(seed) : undefined,
      },
      {
        onSuccess: (blob) => setAudioUrl(URL.createObjectURL(blob)),
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Could not generate audio'),
      },
    )
  }

  return (
    <div className="voices-page">
      <div className="library-header">
        <h1>SFX</h1>
      </div>
      <p className="muted">
        Generate a standalone sound effect or music clip to test a prompt before attaching one to a
        paragraph in the reader. Nothing here is saved.
      </p>
      <div className="custom-voice-form">
        <label>
          Engine
          <select value={engine} onChange={(e) => setEngine(e.target.value as SFXEngine)}>
            {SFX_ENGINES.map((e) => (
              <option key={e} value={e}>
                {SFX_ENGINE_LABELS[e]}
              </option>
            ))}
          </select>
        </label>
        <label>
          Prompt
          <textarea
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            rows={2}
            placeholder={
              engine === 'ace_step'
                ? 'cinematic synth pop with clear vocals'
                : engine === 'stable_audio_sfx'
                  ? 'footsteps on gravel, close perspective, crisp natural stone texture'
                  : 'uplifting house music with bright synths and festival drums'
            }
          />
        </label>
        {engine === 'ace_step' && (
          <label>
            Lyrics (optional)
            <textarea value={lyrics} onChange={(e) => setLyrics(e.target.value)} rows={2} />
          </label>
        )}
        <label>
          Negative prompt (optional)
          <input
            value={negativePrompt}
            onChange={(e) => setNegativePrompt(e.target.value)}
            placeholder=""
          />
        </label>
        <label>
          Duration (seconds)
          <input
            type="number"
            min={0}
            step={0.5}
            value={durationSeconds}
            onChange={(e) => setDurationSeconds(e.target.value)}
          />
        </label>
        <label>
          Inference steps
          <input
            type="number"
            min={1}
            max={200}
            step={1}
            value={numInferenceSteps}
            onChange={(e) => setNumInferenceSteps(e.target.value)}
          />
        </label>
        <label>
          Guidance scale
          <input
            type="number"
            min={0}
            max={20}
            step={0.5}
            value={guidanceScale}
            onChange={(e) => setGuidanceScale(e.target.value)}
          />
        </label>
        <label>
          Seed
          <input
            type="number"
            min={0}
            step={1}
            value={seed}
            onChange={(e) => setSeed(e.target.value)}
          />
        </label>
        {error && <p className="error-text">{error}</p>}
        <div className="voice-panel-actions">
          <button
            className="primary-button"
            onClick={run}
            disabled={!prompt.trim() || testSFX.isPending}
          >
            {testSFX.isPending ? 'Generating…' : 'Generate'}
          </button>
          {audioUrl && <audio controls autoPlay src={audioUrl} />}
        </div>
      </div>
    </div>
  )
}
