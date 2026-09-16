import { useCallback, useState } from 'react'

// The web analogue of android's ReadingSettingsRepository - reader
// paragraph-text size/family, a per-listener preference (like
// usePlayback's own playback rate) rather than anything server-side, so
// this stays in localStorage the same way loadStoredPlaybackRate/
// storePlaybackRate does. Bounds/default match android's own
// MIN_FONT_SIZE_SP/MAX_FONT_SIZE_SP/DEFAULT_FONT_SIZE_SP exactly, so a
// listener who's dialed in a comfortable size on one client has a
// consistent expectation of what "16" or "24" looks like on the other.
export const MIN_FONT_SIZE = 12
export const MAX_FONT_SIZE = 32
export const DEFAULT_FONT_SIZE = 16

export type ReaderFontFamily = 'default' | 'serif' | 'sans' | 'mono'

// Web has no bundled font files to ship (same reasoning android's own
// ReaderFontFamily comment gives for sticking to built-in families) - each
// non-default option is a generic CSS font stack rather than a specific
// webfont, so nothing needs downloading and every option renders
// immediately regardless of network conditions.
export const READER_FONT_FAMILIES: { value: ReaderFontFamily; label: string; stack: string }[] = [
  { value: 'default', label: 'Default', stack: 'inherit' },
  { value: 'serif', label: 'Serif', stack: 'Georgia, "Times New Roman", Times, serif' },
  { value: 'sans', label: 'Sans', stack: '-apple-system, "Segoe UI", Roboto, Helvetica, Arial, sans-serif' },
  { value: 'mono', label: 'Mono', stack: 'ui-monospace, "SFMono-Regular", Menlo, Consolas, "Liberation Mono", monospace' },
]

const FONT_SIZE_KEY = 'lectable:reader-font-size'
const FONT_FAMILY_KEY = 'lectable:reader-font-family'

function clampFontSize(size: number): number {
  return Math.min(MAX_FONT_SIZE, Math.max(MIN_FONT_SIZE, size))
}

function loadStoredFontSize(): number {
  try {
    const raw = window.localStorage.getItem(FONT_SIZE_KEY)
    const parsed = raw ? Number(raw) : NaN
    return Number.isFinite(parsed) ? clampFontSize(parsed) : DEFAULT_FONT_SIZE
  } catch {
    return DEFAULT_FONT_SIZE
  }
}

function loadStoredFontFamily(): ReaderFontFamily {
  try {
    const raw = window.localStorage.getItem(FONT_FAMILY_KEY)
    return READER_FONT_FAMILIES.some((f) => f.value === raw) ? (raw as ReaderFontFamily) : 'default'
  } catch {
    return 'default'
  }
}

export interface ReaderTypography {
  fontSize: number
  fontFamily: ReaderFontFamily
  setFontSize: (size: number) => void
  setFontFamily: (family: ReaderFontFamily) => void
}

export function useReaderTypography(): ReaderTypography {
  const [fontSize, setFontSizeState] = useState(loadStoredFontSize)
  const [fontFamily, setFontFamilyState] = useState<ReaderFontFamily>(loadStoredFontFamily)

  const setFontSize = useCallback((size: number) => {
    const clamped = clampFontSize(size)
    setFontSizeState(clamped)
    try {
      window.localStorage.setItem(FONT_SIZE_KEY, String(clamped))
    } catch {
      // Private browsing / blocked storage - the size just won't persist.
    }
  }, [])

  const setFontFamily = useCallback((family: ReaderFontFamily) => {
    setFontFamilyState(family)
    try {
      window.localStorage.setItem(FONT_FAMILY_KEY, family)
    } catch {
      // Same as above.
    }
  }, [])

  return { fontSize, fontFamily, setFontSize, setFontFamily }
}
