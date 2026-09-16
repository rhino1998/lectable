import { DEFAULT_FONT_SIZE, MAX_FONT_SIZE, MIN_FONT_SIZE, READER_FONT_FAMILIES } from '../hooks/useReaderTypography'
import type { ReaderFontFamily } from '../hooks/useReaderTypography'

// The web analogue of android's own reading-font-size/family controls in
// SettingsScreen.kt - reachable from PlayerBar's "Text size" button
// instead of a dedicated settings page (this app has none - see
// routes.tsx), the same way VoicePanel is reachable from the adjacent
// "Voice" button. See useReaderTypography for persistence and
// ReaderPage's own consumption of the resulting values (as CSS custom
// properties on .scroll-reader).
export function ReaderTypographyPanel({
  fontSize,
  fontFamily,
  onFontSizeChange,
  onFontFamilyChange,
}: {
  fontSize: number
  fontFamily: ReaderFontFamily
  onFontSizeChange: (size: number) => void
  onFontFamilyChange: (family: ReaderFontFamily) => void
}) {
  return (
    <div className="reader-typography-panel">
      <div className="reader-typography-row-label">
        Text size ({fontSize}px)
        {fontSize !== DEFAULT_FONT_SIZE && (
          <button className="text-button" onClick={() => onFontSizeChange(DEFAULT_FONT_SIZE)}>
            Reset
          </button>
        )}
      </div>
      <div className="speed-slider">
        <input
          type="range"
          min={MIN_FONT_SIZE}
          max={MAX_FONT_SIZE}
          step={1}
          value={fontSize}
          onChange={(e) => onFontSizeChange(Number(e.target.value))}
        />
      </div>
      <div className="reader-typography-row-label">Font</div>
      <div className="reader-typography-family-row">
        {READER_FONT_FAMILIES.map((f) => (
          <button
            key={f.value}
            className={
              'reader-typography-family-button' + (fontFamily === f.value ? ' reader-typography-family-button-active' : '')
            }
            style={{ fontFamily: f.stack }}
            onClick={() => onFontFamilyChange(f.value)}
          >
            {f.label}
          </button>
        ))}
      </div>
    </div>
  )
}
