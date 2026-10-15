import { useEffect, useRef, useState, type ReactNode } from 'react'
import { RiPauseFill, RiPlayFill, RiRefreshLine } from 'react-icons/ri'
import { useRegenerateVariant } from '../../api/queries'
import { ApiError } from '../../api/client'
import { withCacheBust } from '../../utils/cacheBust'
import type { SpeakerVoice } from '../../api/types'

// The neutral sample's key among a voice's playing samples and regenerated
// clips - no emotion has an empty id, and the backend takes '' to mean the
// neutral clip too.
const NEUTRAL = ''

// One of the voices a speaker's lines are spoken in (Speaker.voices), as a
// line of sample chips: first Neutral - the reference clip its lines clone
// from - then one per emotion those lines use. Each chip has a play button
// once its clip exists, its line count, and a regenerate button
// (api.regenerateVariant: an emotion's re-renders the variant and resets
// those lines' audio; the neutral one renders the voice afresh, where it
// can be). A clip that doesn't exist yet renders by itself the first time
// one of its lines generates. A speaker card stacks one line per voice -
// their own, each numbered member of a role, each age - told apart by
// label; children follow the chips (an age's actions). A voice with no
// clip of its own and no emotional lines - the Narrator's, an unvoiced
// character's - renders nothing.
export function SpeakerVoiceLine({
  bookId,
  speakerName,
  voice,
  label,
  children,
}: {
  bookId: string
  speakerName: string
  voice: SpeakerVoice
  label?: ReactNode
  children?: ReactNode
}) {
  const regenerate = useRegenerateVariant(bookId)
  const [error, setError] = useState<string | null>(null)
  // Bumped per sample on regenerate, so the replaced clip's URL isn't
  // served from the browser cache.
  const [versions, setVersions] = useState<Record<string, number>>({})
  const [playing, setPlaying] = useState<string | null>(null)
  const audioRef = useRef<HTMLAudioElement | null>(null)
  useEffect(() => () => audioRef.current?.pause(), [])

  // One sample plays at a time; key is the emotion id, or NEUTRAL.
  const togglePlay = (key: string, url: string | undefined) => {
    audioRef.current?.pause()
    if (playing === key || !url) {
      setPlaying(null)
      return
    }
    const audio = new Audio(withCacheBust(url, versions[key]))
    audio.onended = () => setPlaying(null)
    audioRef.current = audio
    setPlaying(key)
    audio.play().catch(() => setPlaying(null))
  }

  const runRegenerate = (emotion: string) => {
    setError(null)
    regenerate.mutate(
      { speaker: speakerName, emotion, speakerIndex: voice.number, stageId: voice.stage?.id },
      {
        onSuccess: () => setVersions((v) => ({ ...v, [emotion]: Date.now() })),
        onError: (err) =>
          setError(err instanceof ApiError ? err.message : 'Could not regenerate this clip'),
      },
    )
  }

  const emotions = voice.emotions ?? []
  // A voice of the speaker's own, as opposed to the book's voice they fall
  // back on: it has a neutral clip, or will.
  const hasNeutral = Boolean(
    voice.refAudioUrl || voice.canRegenerate || voice.number || voice.stage,
  )
  if (!label && !hasNeutral && emotions.length === 0) return null
  return (
    <div className="speaker-emotions">
      {label && <span className="speaker-emotions-label">{label}</span>}
      {hasNeutral && (
        <span
          className={
            'speaker-emotion-chip speaker-emotion-' + (voice.refAudioUrl ? 'ready' : 'pending')
          }
          title={
            voice.refAudioUrl
              ? `Neutral: ${voice.neutralCount} line(s) with no emotion - the clip this voice's lines are cloned from`
              : `Neutral: ${voice.neutralCount} line(s) with no emotion - the clip renders the first time one of these lines generates`
          }
        >
          {voice.refAudioUrl && (
            <button
              className="speaker-emotion-button"
              onClick={() => togglePlay(NEUTRAL, voice.refAudioUrl)}
              aria-label={playing === NEUTRAL ? 'Stop neutral sample' : 'Play neutral sample'}
            >
              {playing === NEUTRAL ? <RiPauseFill /> : <RiPlayFill />}
            </button>
          )}
          Neutral <span className="muted">{voice.neutralCount}</span>
          {voice.canRegenerate && (
            <button
              className="speaker-emotion-button"
              onClick={() => runRegenerate(NEUTRAL)}
              disabled={regenerate.isPending}
              aria-label="Regenerate this voice"
              title="Render this voice afresh - a new take on the same casting (and re-voice its lines)"
            >
              <RiRefreshLine />
            </button>
          )}
        </span>
      )}
      {emotions.map((e) => (
        <span
          key={e.emotion}
          className={'speaker-emotion-chip speaker-emotion-' + e.status}
          title={
            e.status === 'ready'
              ? `${e.label}: ${e.count} line(s), variant ready`
              : e.status === 'failed'
                ? `${e.label}: ${e.count} line(s) - the variant couldn't be rendered, so these lines use the base voice`
                : e.status === 'directed'
                  ? `${e.label}: ${e.count} line(s) - this book's clone model reads each line's direction from its text, so no variant is used`
                  : `${e.label}: ${e.count} line(s) - the variant renders the first time one of these lines generates`
          }
        >
          {e.status === 'ready' && (
            <button
              className="speaker-emotion-button"
              onClick={() => togglePlay(e.emotion, e.audioUrl)}
              aria-label={
                playing === e.emotion ? `Stop ${e.label} sample` : `Play ${e.label} sample`
              }
            >
              {playing === e.emotion ? <RiPauseFill /> : <RiPlayFill />}
            </button>
          )}
          {e.label} <span className="muted">{e.count}</span>
          {e.status !== 'directed' && (
            <button
              className="speaker-emotion-button"
              onClick={() => runRegenerate(e.emotion)}
              disabled={regenerate.isPending}
              aria-label={`Regenerate ${e.label} variant`}
              title={`Regenerate the ${e.label} variant (and re-voice these lines)`}
            >
              <RiRefreshLine />
            </button>
          )}
        </span>
      ))}
      {children}
      {error && <span className="error-text">{error}</span>}
    </div>
  )
}
