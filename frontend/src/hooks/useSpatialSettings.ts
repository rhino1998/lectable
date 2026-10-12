import { useCallback, useState } from 'react'
import {
  DEFAULT_SPATIAL_SETTINGS,
  MAX_SPATIAL_WIDTH,
  SPATIAL_MODES,
  type SpatialMode,
  type SpatialSettings,
} from '../utils/spatialAudio'

// Speaker positions (utils/spatialAudio) - a per-listener preference kept
// in localStorage, like usePlayback's playback rate: whether they suit
// depends on what this device plays through.
const SPATIAL_MODE_KEY = 'lectable:spatial-mode'
const SPATIAL_WIDTH_KEY = 'lectable:spatial-width'

function clampWidth(width: number): number {
  return Math.min(Math.max(width, 0), MAX_SPATIAL_WIDTH)
}

function loadSpatialSettings(): SpatialSettings {
  try {
    const mode = window.localStorage.getItem(SPATIAL_MODE_KEY)
    const rawWidth = window.localStorage.getItem(SPATIAL_WIDTH_KEY)
    const width = rawWidth === null ? NaN : Number(rawWidth)
    return {
      mode: SPATIAL_MODES.includes(mode as SpatialMode)
        ? (mode as SpatialMode)
        : DEFAULT_SPATIAL_SETTINGS.mode,
      width: Number.isFinite(width) ? clampWidth(width) : DEFAULT_SPATIAL_SETTINGS.width,
    }
  } catch {
    return DEFAULT_SPATIAL_SETTINGS
  }
}

export function useSpatialSettings(): [SpatialSettings, (next: SpatialSettings) => void] {
  const [settings, setSettingsState] = useState(loadSpatialSettings)
  const setSettings = useCallback((next: SpatialSettings) => {
    const clamped = { mode: next.mode, width: clampWidth(next.width) }
    setSettingsState(clamped)
    try {
      window.localStorage.setItem(SPATIAL_MODE_KEY, clamped.mode)
      window.localStorage.setItem(SPATIAL_WIDTH_KEY, String(clamped.width))
    } catch {
      // Private browsing / blocked storage - the setting just won't persist.
    }
  }, [])
  return [settings, setSettings]
}
