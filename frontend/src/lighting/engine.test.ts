import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Lighting } from '../api/types'
import { LightingEngine } from './engine'
import type { HueBridge, HueLight, HueState } from './hue'

function fakeBridge() {
  const calls: { kind: 'group' | 'light'; id: string; state: HueState; at: number }[] = []
  const bridge = {
    setGroup: vi.fn(async (id: string, state: HueState) => {
      calls.push({ kind: 'group', id, state, at: Date.now() })
    }),
    setLight: vi.fn(async (id: string, state: HueState) => {
      calls.push({ kind: 'light', id, state, at: Date.now() })
    }),
  }
  return { bridge: bridge as unknown as HueBridge, calls }
}

const lights: HueLight[] = [
  {
    id: '1',
    name: 'a',
    color: true,
    state: { on: true, bri: 10, xy: [0.4, 0.4], colormode: 'xy' },
  },
  { id: '2', name: 'b', color: false, state: { on: false, bri: 200, ct: 300, colormode: 'ct' } },
]

const cue = (over: Partial<Lighting> = {}): Lighting => ({
  color: '#ff8000',
  brightness: 100,
  effect: 'steady',
  ...over,
})

describe('LightingEngine', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.setSystemTime(10_000)
  })
  afterEach(() => vi.useRealTimers())

  it('sends a new cue as one group command with a cut or slow fade', async () => {
    const { bridge, calls } = fakeBridge()
    const engine = new LightingEngine(bridge, 'g', lights, 1)
    engine.start()
    engine.setCue(cue(), 'cut')
    await vi.advanceTimersByTimeAsync(125)
    expect(calls).toHaveLength(1)
    expect(calls[0]).toMatchObject({ kind: 'group', id: 'g' })
    expect(calls[0].state).toMatchObject({ on: true, bri: 254, transitiontime: 25 })

    // The same cue again is a no-op; a new one waits out the group budget.
    engine.setCue(cue(), 'cut')
    engine.setCue(cue({ color: '#0000ff' }), 'continuation')
    await vi.advanceTimersByTimeAsync(500)
    expect(calls).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(600)
    expect(calls).toHaveLength(2)
    expect(calls[1].state.transitiontime).toBe(100)
    expect(calls[1].at - calls[0].at).toBeGreaterThanOrEqual(1000)
    await engine.stop(false)
  })

  it('animates effects per light only while playing, colour only on colour bulbs', async () => {
    const { bridge, calls } = fakeBridge()
    const engine = new LightingEngine(bridge, 'g', lights, 1)
    engine.start()
    engine.setCue(cue({ effect: 'pulse' }), 'cut')
    await vi.advanceTimersByTimeAsync(3000)
    expect(calls.filter((c) => c.kind === 'light')).toHaveLength(0)

    engine.setPlaying(true)
    await vi.advanceTimersByTimeAsync(1000)
    const perLight = calls.filter((c) => c.kind === 'light')
    expect(perLight.length).toBeGreaterThan(4)
    // Round-robin across both lights, within Hue's ~10 cmds/s budget.
    expect(new Set(perLight.map((c) => c.id))).toEqual(new Set(['1', '2']))
    expect(perLight.length).toBeLessThanOrEqual(8)
    for (const c of perLight) expect('xy' in c.state).toBe(c.id === '1')
    await engine.stop(false)
  })

  it('flashes each light on a 2s cycle, neighbours on opposite colours', async () => {
    const { bridge, calls } = fakeBridge()
    const pair: HueLight[] = [lights[0], { ...lights[0], id: '3' }, lights[1]]
    const engine = new LightingEngine(bridge, 'g', pair, 1)
    engine.start()
    engine.setCue(cue({ effect: 'flash', color: '#ff0000', accent: '#0000ff' }), 'cut')
    engine.setPlaying(true)
    await vi.advanceTimersByTimeAsync(3000)
    calls.length = 0
    await vi.advanceTimersByTimeAsync(6000)
    expect(calls.every((c) => c.kind === 'light')).toBe(true)
    for (const id of ['1', '3', '2']) {
      const mine = calls.filter((c) => c.id === id)
      expect(mine.length).toBeGreaterThanOrEqual(5)
      expect(mine.length).toBeLessThanOrEqual(7)
      for (let i = 1; i < mine.length; i++) {
        expect(mine[i].at - mine[i - 1].at).toBe(1000)
        // Colour bulbs swap colour at full level; a white one blinks.
        if (id === '2') expect(mine[i].state.bri).not.toBe(mine[i - 1].state.bri)
        else {
          expect(mine[i].state.xy).not.toEqual(mine[i - 1].state.xy)
          expect(mine[i].state.bri).toBe(254)
        }
      }
    }
    // Neighbouring colour bulbs are never on the same colour.
    const a = calls.filter((c) => c.id === '1')
    const b = calls.filter((c) => c.id === '3')
    const partner = b.find((c) => c.at > a[1].at && c.at - a[1].at < 500)
    expect(partner).toBeDefined()
    expect(partner?.state.xy).not.toEqual(a[1].state.xy)
    await engine.stop(false)
  })

  it('flashes a big room as one group command per half cycle', async () => {
    const { bridge, calls } = fakeBridge()
    const many = Array.from({ length: 9 }, (_, i) => ({ ...lights[0], id: String(i + 1) }))
    const engine = new LightingEngine(bridge, 'g', many, 1)
    engine.start()
    engine.setCue(cue({ effect: 'flash' }), 'cut')
    engine.setPlaying(true)
    await vi.advanceTimersByTimeAsync(3000)
    calls.length = 0
    await vi.advanceTimersByTimeAsync(4000)
    expect(calls.every((c) => c.kind === 'group')).toBe(true)
    expect(calls.length).toBeGreaterThanOrEqual(3)
    for (let i = 1; i < calls.length; i++) {
      expect(calls[i].at - calls[i - 1].at).toBeGreaterThanOrEqual(1000)
      expect(calls[i].state.bri).not.toBe(calls[i - 1].state.bri)
    }
    await engine.stop(false)
  })

  it("restores the snapshot it took at start, in the light's own colour mode", async () => {
    const { bridge, calls } = fakeBridge()
    const engine = new LightingEngine(bridge, 'g', lights, 1)
    engine.start()
    const stopped = engine.stop(true)
    await vi.runAllTimersAsync()
    await stopped
    expect(calls.map((c) => [c.id, c.state])).toEqual([
      ['1', { on: true, bri: 10, xy: [0.4, 0.4], transitiontime: 10 }],
      ['2', { on: false, bri: 200, ct: 300, transitiontime: 10 }],
    ])
  })

  it('scales brightness by the cap', async () => {
    const { bridge, calls } = fakeBridge()
    const engine = new LightingEngine(bridge, 'g', lights, 0.5)
    engine.start()
    engine.setCue(cue(), 'cut')
    await vi.advanceTimersByTimeAsync(125)
    expect(calls[0].state.bri).toBe(128)
    await engine.stop(false)
  })
})
