import { describe, expect, it } from 'vitest'
import { hueBrightness, mixRgb, parseHex, rgbToXy } from './color'

describe('parseHex', () => {
  it('reads #rrggbb and rrggbb into 0-1 channels', () => {
    expect(parseHex('#ff8000')).toEqual({ r: 1, g: 128 / 255, b: 0 })
    expect(parseHex(' FF8000 ')).toEqual({ r: 1, g: 128 / 255, b: 0 })
  })
  it('rejects anything else', () => {
    for (const bad of [undefined, '', '#fff', '#ff80001', 'zzzzzz', 'rgb(1,2,3)']) {
      expect(parseHex(bad)).toBeNull()
    }
  })
})

describe('mixRgb', () => {
  const a = { r: 0, g: 0.5, b: 1 }
  const b = { r: 1, g: 0.5, b: 0 }
  it('interpolates linearly', () => {
    expect(mixRgb(a, b, 0)).toEqual(a)
    expect(mixRgb(a, b, 1)).toEqual(b)
    expect(mixRgb(a, b, 0.5)).toEqual({ r: 0.5, g: 0.5, b: 0.5 })
  })
})

describe('rgbToXy', () => {
  it('maps black to the D65 white point', () => {
    expect(rgbToXy({ r: 0, g: 0, b: 0 })).toEqual([0.3127, 0.329])
  })
  it('maps white near D65', () => {
    const [x, y] = rgbToXy({ r: 1, g: 1, b: 1 })
    expect(x).toBeCloseTo(0.3227, 3)
    expect(y).toBeCloseTo(0.329, 3)
  })
  it('puts primaries in the expected corners of the gamut', () => {
    const [rx, ry] = rgbToXy({ r: 1, g: 0, b: 0 })
    const [gx, gy] = rgbToXy({ r: 0, g: 1, b: 0 })
    const [bx, by] = rgbToXy({ r: 0, g: 0, b: 1 })
    expect(rx).toBeGreaterThan(0.6)
    expect(gy).toBeGreaterThan(0.6)
    expect(bx).toBeLessThan(0.2)
    expect(by).toBeLessThan(0.1)
    expect(ry).toBeLessThan(0.35)
    expect(gx).toBeLessThan(0.25)
  })
  it('rounds to 4 decimals', () => {
    for (const v of rgbToXy({ r: 0.3, g: 0.6, b: 0.9 })) {
      expect(Math.round(v * 10000) / 10000).toBe(v)
    }
  })
})

describe('hueBrightness', () => {
  it('maps 1-100 x cap onto 1-254', () => {
    expect(hueBrightness(100, 1)).toBe(254)
    expect(hueBrightness(0, 1)).toBe(1)
    expect(hueBrightness(50, 1)).toBe(128)
    expect(hueBrightness(100, 0.5)).toBe(128)
  })
  it('clamps out-of-range input', () => {
    expect(hueBrightness(500, 1)).toBe(254)
    expect(hueBrightness(-20, 1)).toBe(1)
    expect(hueBrightness(100, 2)).toBe(254)
  })
})
