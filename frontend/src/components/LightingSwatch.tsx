import type { Lighting } from '../api/types'

// LightingSwatch previews a lighting cue (backend store.Lighting): its color,
// with the accent as a second half, dimmed by its brightness (times `cap`,
// the listener's brightness cap, where the preview should match the room),
// and animated per effect in CSS (static under prefers-reduced-motion).
// Shared by the reader's region markers (ChapterSection) and the Lights
// panel.
export function LightingSwatch({
  lighting,
  cap = 1,
  large = false,
}: {
  lighting: Lighting
  cap?: number
  large?: boolean
}) {
  const accent = lighting.accent || lighting.color
  return (
    <span
      className={
        `lighting-cue-swatch lighting-cue-${lighting.effect}` +
        (large ? ' lighting-cue-swatch-large' : '')
      }
      style={
        {
          '--cue-color': lighting.color,
          '--cue-accent': accent,
          '--cue-level': 0.35 + 0.65 * ((lighting.brightness / 100) * cap),
        } as React.CSSProperties
      }
    />
  )
}
