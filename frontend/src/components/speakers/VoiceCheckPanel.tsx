import { useEffect, useRef, useState } from 'react'
import { RiPauseFill, RiPlayFill } from 'react-icons/ri'
import { ApiError } from '../../api/client'
import {
  useDismissVoiceCheck,
  useRegenerateVariant,
  useVoiceCheck,
  useVoiceRendering,
} from '../../api/queries'
import type { VoiceCheck, VoiceTraits } from '../../api/types'

const TRAIT_LABELS: Record<VoiceCheck['mismatches'][number], string> = {
  gender: 'Gender',
  age: 'Age',
  pitch: 'Pitch',
}

// One trait as "cast X, heard Y".
function disagreement(
  trait: VoiceCheck['mismatches'][number],
  cast: VoiceTraits,
  heard: VoiceTraits,
) {
  return `${TRAIT_LABELS[trait]}: cast ${cast[trait] ?? '?'}, heard ${heard[trait] ?? '?'}`
}

// Voices a voice check heard differently from their casting, each waiting
// for a say: render the voice afresh (another take of the same casting,
// which re-voices that character's lines) or keep it. The check is a rough
// judge - it tells a man from a woman, a child from an adult and a high
// voice from a deep one, not much finer - so every finding has its clip to
// listen to first. A voice rendered afresh leaves the list until the next
// check hears the new clip.
export function VoiceCheckPanel({ bookId }: { bookId: string }) {
  const check = useVoiceCheck(bookId).data
  const regenerate = useRegenerateVariant(bookId)
  const dismiss = useDismissVoiceCheck(bookId)
  const [error, setError] = useState<string | null>(null)
  // Voices asked to render afresh from here: the render is a queued job, so
  // the finding stays - marked - until the new clip replaces the one heard.
  const rendering = useVoiceRendering(bookId)
  const [asked, setAsked] = useState<Set<string>>(new Set())
  const [playing, setPlaying] = useState<string | null>(null)
  const audioRef = useRef<HTMLAudioElement | null>(null)
  useEffect(() => () => audioRef.current?.pause(), [])

  const findings = check?.findings ?? []
  if (findings.length === 0) return null

  const togglePlay = (f: VoiceCheck) => {
    audioRef.current?.pause()
    if (playing === f.presetId || !f.refAudioUrl) {
      setPlaying(null)
      return
    }
    const audio = new Audio(f.refAudioUrl)
    audio.onended = () => setPlaying(null)
    audioRef.current = audio
    setPlaying(f.presetId)
    audio.play().catch(() => setPlaying(null))
  }

  const onError = (err: unknown) =>
    setError(err instanceof ApiError ? err.message : 'Could not save that')

  const redo = (f: VoiceCheck) => {
    setError(null)
    regenerate.mutate(
      { speaker: f.speaker, emotion: '', stageId: f.stageId },
      { onError, onSuccess: () => setAsked((s) => new Set(s).add(f.presetId)) },
    )
  }

  const keep = (f: VoiceCheck) => {
    setError(null)
    dismiss.mutate(f.presetId, { onError })
  }

  const busy = regenerate.isPending || dismiss.isPending
  return (
    <div className="source-review roster-review">
      <div>
        <strong>Voice check</strong>
        <span className="muted">
          {' '}
          - {findings.length} of {check?.checked ?? 0} voices heard{' '}
          {findings.length === 1 ? "doesn't" : "don't"} sound like their casting. It is a rough
          judge: listen first. Rendering a voice afresh re-voices that character&rsquo;s lines.
        </span>
      </div>
      {error && <p className="error-text">{error}</p>}
      <ul className="roster-review-list">
        {findings.map((f) => (
          <li key={f.presetId}>
            <div className="roster-review-what">
              <span>
                {f.refAudioUrl && (
                  <button
                    type="button"
                    className="icon-action-button"
                    onClick={() => togglePlay(f)}
                    title="Play this voice"
                  >
                    {playing === f.presetId ? <RiPauseFill /> : <RiPlayFill />}
                  </button>
                )}{' '}
                {f.speaker}
                {f.stageAge ? ` at ${f.stageAge}` : ''} -{' '}
                {f.mismatches.map((m) => disagreement(m, f.cast, f.heard)).join('; ')}
              </span>
            </div>
            <div className="muted">{f.instruct}</div>
            {f.heard.accent && (
              <div className="muted">
                Accent heard: {f.heard.accent}
                {f.cast.accent ? ` (cast ${f.cast.accent}; not compared)` : ''}
              </div>
            )}
            <div className="roster-review-actions">
              <button
                type="button"
                className="option-chip"
                onClick={() => redo(f)}
                disabled={busy || asked.has(f.presetId) || rendering.has(f.speaker)}
              >
                {asked.has(f.presetId) || rendering.has(f.speaker)
                  ? 'Rendering a new take…'
                  : 'Render afresh'}
              </button>
              <button type="button" className="option-chip" onClick={() => keep(f)} disabled={busy}>
                Keep
              </button>
            </div>
          </li>
        ))}
      </ul>
    </div>
  )
}
