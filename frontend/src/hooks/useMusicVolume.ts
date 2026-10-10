import { useCallback, useState } from 'react'

// Background music's level under narration - a per-listener preference
// kept in localStorage (like usePlayback's playback rate), the web
// analogue of android's PlaybackSettingsRepository.musicVolume with the
// same default. 1 is as loud as narration itself.
export const DEFAULT_MUSIC_VOLUME = 0.22

const MUSIC_VOLUME_KEY = 'lectable:music-volume'

function clampVolume(volume: number): number {
  return Math.min(Math.max(volume, 0), 1)
}

function loadMusicVolume(): number {
  try {
    const raw = window.localStorage.getItem(MUSIC_VOLUME_KEY)
    const parsed = raw === null ? NaN : Number(raw)
    return Number.isFinite(parsed) ? clampVolume(parsed) : DEFAULT_MUSIC_VOLUME
  } catch {
    return DEFAULT_MUSIC_VOLUME
  }
}

export function useMusicVolume(): [number, (volume: number) => void] {
  const [volume, setVolumeState] = useState(loadMusicVolume)
  const setVolume = useCallback((next: number) => {
    const clamped = clampVolume(next)
    setVolumeState(clamped)
    try {
      window.localStorage.setItem(MUSIC_VOLUME_KEY, String(clamped))
    } catch {
      // Private browsing / blocked storage - the volume just won't persist.
    }
  }, [])
  return [volume, setVolume]
}
