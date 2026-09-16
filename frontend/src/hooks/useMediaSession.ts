import { useEffect, useRef } from 'react'

// android's analogue is playback/PlaybackService.kt's MediaSessionService -
// this is the web equivalent: the browser's own Media Session API
// (https://developer.mozilla.org/en-US/docs/Web/API/Media_Session_API),
// which surfaces play/pause/skip on the OS lock screen, hardware/headset
// media keys, and the browser tab's own media indicator. With no
// integration at all (the state before this hook), backgrounding the tab
// loses every system-level control - the only way to pause is switching
// back to the tab itself.
interface UseMediaSessionArgs {
  // "Track" metadata - one chapter is the closest analogue of a track
  // (android's own MediaMetadata construction does the same: chapter as
  // title, book as album/artist), since that's the granularity a listener
  // actually skips by (nexttrack/previoustrack - see onNextTrack/
  // onPreviousTrack below), not the individual paragraph.
  title: string
  artist: string
  album: string
  artworkUrl?: string
  isPlaying: boolean
  onPlay: () => void
  onPause: () => void
  // Bound to the browser's nexttrack/previoustrack actions (headset/lock-
  // screen skip buttons) - ReaderPage passes its own paragraph-level
  // skipParagraph(1)/skipParagraph(-1), the same granularity the
  // ArrowRight/ArrowLeft keyboard shortcuts already use.
  onNextTrack: () => void
  onPreviousTrack: () => void
  currentTime: number
  duration: number
  playbackRate: number
}

export function useMediaSession({
  title,
  artist,
  album,
  artworkUrl,
  isPlaying,
  onPlay,
  onPause,
  onNextTrack,
  onPreviousTrack,
  currentTime,
  duration,
  playbackRate,
}: UseMediaSessionArgs) {
  const supported = typeof navigator !== 'undefined' && 'mediaSession' in navigator

  // Re-set only when the "track" itself actually changes (title/artist/
  // album/artwork), not on every playback tick - matches how often a real
  // track's metadata would change.
  useEffect(() => {
    if (!supported) return
    navigator.mediaSession.metadata = new MediaMetadata({
      title,
      artist,
      album,
      artwork: artworkUrl ? [{ src: artworkUrl, sizes: '512x512', type: 'image/jpeg' }] : [],
    })
  }, [supported, title, artist, album, artworkUrl])

  // Action handlers registered once, not re-bound on every render - each
  // reads through a ref so it always calls whatever the latest callback
  // is without needing to be in this effect's own deps (the same "latest
  // ref" pattern ReaderPage's useStableCallback formalizes).
  const onPlayRef = useRef(onPlay)
  onPlayRef.current = onPlay
  const onPauseRef = useRef(onPause)
  onPauseRef.current = onPause
  const onNextRef = useRef(onNextTrack)
  onNextRef.current = onNextTrack
  const onPrevRef = useRef(onPreviousTrack)
  onPrevRef.current = onPreviousTrack

  useEffect(() => {
    if (!supported) return
    navigator.mediaSession.setActionHandler('play', () => onPlayRef.current())
    navigator.mediaSession.setActionHandler('pause', () => onPauseRef.current())
    navigator.mediaSession.setActionHandler('nexttrack', () => onNextRef.current())
    navigator.mediaSession.setActionHandler('previoustrack', () => onPrevRef.current())
    return () => {
      navigator.mediaSession.setActionHandler('play', null)
      navigator.mediaSession.setActionHandler('pause', null)
      navigator.mediaSession.setActionHandler('nexttrack', null)
      navigator.mediaSession.setActionHandler('previoustrack', null)
    }
  }, [supported])

  useEffect(() => {
    if (!supported) return
    navigator.mediaSession.playbackState = isPlaying ? 'playing' : 'paused'
  }, [supported, isPlaying])

  // setPositionState lets the OS draw a real scrubber on the lock-screen/
  // notification instead of a static play/pause pair - optional per spec
  // (Safari lacks it), so this stays a no-op there rather than throwing.
  useEffect(() => {
    if (!supported || typeof navigator.mediaSession.setPositionState !== 'function') return
    if (!Number.isFinite(duration) || duration <= 0) return
    try {
      navigator.mediaSession.setPositionState({
        duration,
        playbackRate,
        position: Math.min(Math.max(currentTime, 0), duration),
      })
    } catch {
      // Thrown transiently when currentTime/duration briefly disagree
      // (e.g. mid-paragraph-swap, before `duration` has caught up to the
      // new clip) - harmless to skip a frame's position update.
    }
  }, [supported, currentTime, duration, playbackRate])
}
