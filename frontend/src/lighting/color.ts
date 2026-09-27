// Color math for driving Philips Hue lights from the ambience pass's
// lighting cues (backend store.Lighting: "#rrggbb" colors plus a 1-100
// brightness). Mirrored in android's playback/lighting/HueColor.kt - keep
// the two in step.

export interface Rgb {
  r: number
  g: number
  b: number
}

// parseHex reads "#rrggbb" (or "rrggbb") into 0-1 channels; anything else
// is null.
export function parseHex(hex: string | undefined): Rgb | null {
  if (!hex) return null
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim())
  if (!m) return null
  const n = parseInt(m[1], 16)
  return { r: ((n >> 16) & 0xff) / 255, g: ((n >> 8) & 0xff) / 255, b: (n & 0xff) / 255 }
}

// mixRgb linearly interpolates a toward b by t (0-1) - good enough for the
// short drifts the effects make.
export function mixRgb(a: Rgb, b: Rgb, t: number): Rgb {
  return { r: a.r + (b.r - a.r) * t, g: a.g + (b.g - a.g) * t, b: a.b + (b.b - a.b) * t }
}

// rgbToXy converts sRGB to CIE xy the way Philips' own developer docs do:
// undo sRGB gamma, then the Wide RGB D65 matrix. Out-of-gamut points are
// left for the bridge, which clamps to each light's own gamut. Black
// (no chromaticity) maps to the D65 white point.
export function rgbToXy(c: Rgb): [number, number] {
  const lin = (v: number) => (v > 0.04045 ? Math.pow((v + 0.055) / 1.055, 2.4) : v / 12.92)
  const r = lin(c.r)
  const g = lin(c.g)
  const b = lin(c.b)
  const X = r * 0.664511 + g * 0.154324 + b * 0.162028
  const Y = r * 0.283881 + g * 0.668433 + b * 0.047685
  const Z = r * 0.000088 + g * 0.07231 + b * 0.986039
  const sum = X + Y + Z
  if (sum === 0) return [0.3127, 0.329]
  return [round4(X / sum), round4(Y / sum)]
}

function round4(v: number): number {
  return Math.round(v * 10000) / 10000
}

// hueBrightness maps a cue's 1-100 brightness, scaled by the listener's
// own cap (0-1), onto Hue's 1-254 "bri" range.
export function hueBrightness(brightness: number, cap: number): number {
  const frac = Math.max(0, Math.min(1, (brightness / 100) * cap))
  return Math.max(1, Math.min(254, Math.round(1 + frac * 253)))
}
