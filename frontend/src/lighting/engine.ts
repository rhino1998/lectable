// LightingEngine turns the ambience pass's lighting cues into Hue commands
// for one room, animating each cue's effect locally (the backend only
// stores cues - see backend store.Lighting). Mirrored in android's
// playback/lighting/LightingEngine.kt - keep the two in step.
//
// Hue's local API takes roughly 10 light commands a second across all
// lights, but only about 1 group command a second, so:
//   - a new cue (or brightness cap) goes out as one group command, faded
//     over the cue's transition - quickly into a "cut" region, slowly
//     into a "continuation";
//   - an effect then animates one light per tick, round-robin, each command
//     fading over the time until that light's next update, so every light
//     moves smoothly and the whole room stays within the budget;
//   - lightning flashes are a pair of group commands (flash, then back),
//     rare enough to fit the group budget;
//   - flash (a siren or alarm) flips each light once per half cycle, one
//     light per tick - see FLASH_PERIOD_MS.
// Effects run only while narration plays; paused, the room holds the cue.

import type { LightEffect, Lighting } from '../api/types'
import { hueBrightness, mixRgb, parseHex, rgbToXy, type Rgb } from './color'
import type { HueBridge, HueLight, HueState } from './hue'

const TICK_MS = 125
const GROUP_MIN_INTERVAL_MS = 1000
// Transition into a new cue, in deciseconds.
const CUT_TRANSITION_DS = 25
const CONTINUATION_TRANSITION_DS = 100
const PULSE_PERIOD_MS = 4500
const SHIMMER_PERIOD_MS = 11000
const LIGHTNING_MIN_GAP_MS = 7000
const LIGHTNING_MAX_GAP_MS = 22000
// Flicker is a gentle candle-like wander, not a strobe: each light's level
// random-walks within the top FLICKER_DEPTH of the cue's brightness, easing
// FLICKER_EASE of the way toward a fresh random target per update and fading
// over the whole time until its next one. (It was 45% with 100-300ms jumps -
// far too intense on real bulbs - then 20% with a 0.5 ease, which kept most
// lights within ~85-95% and was invisible; this lands roughly 65-95%.)
// Color moves only a little: at most FLICKER_COLOR_SHIFT toward a slightly
// deeper tint of the base itself (flickerTint), never toward the cue's accent, which can be a wildly
// different color. Hue keeps brightness (bri) separate from color (xy), so
// that drift doesn't change how bright the room is.
const FLICKER_DEPTH = 0.4
const FLICKER_COLOR_SHIFT = 0.4
const FLICKER_EASE = 0.8
const FLICKER_MIN_DS = 3
// Flash is a siren or alarm beacon: every light flips once per half cycle,
// between the cue's color and its accent (neighbouring lights opposite, like
// a police lightbar) or, with no accent, between full and FLASH_DIM of the
// cue's brightness, all together. A 2s cycle is about as fast as the bridge
// goes: each flip is a command per light, so a room of up to
// FLASH_MAX_LIGHTS fits the one-light-per-tick budget (the flips ripple
// across the room, 125ms apart); a bigger room flips as one group command
// per half cycle instead, which is the whole group budget. Real strobing
// (several flashes a second) isn't possible over this API.
const FLASH_PERIOD_MS = 2000
const FLASH_DIM = 0.15
const FLASH_TRANSITION_DS = 2
const FLASH_MAX_LIGHTS = 8
const MAX_IN_FLIGHT = 2

export type Transition = 'cut' | 'continuation'

export interface EngineCallbacks {
  onError?: (err: unknown) => void
  onOk?: () => void
}

interface Snapshot {
  id: string
  state: HueState
}

export class LightingEngine {
  private readonly bridge: HueBridge
  private readonly groupId: string
  private readonly lights: HueLight[]
  private readonly callbacks: EngineCallbacks
  private cap: number
  private cue: Lighting | null = null
  private transition: Transition = 'cut'
  private playing = false
  private baseDirty = false
  private lastGroupSend = 0
  // Effects wait for the fade into a new cue to finish.
  private effectsFrom = 0
  private nextLight = 0
  private nextFlash = 0
  private inFlight = 0
  private timer: ReturnType<typeof setInterval> | null = null
  private snapshot: Snapshot[] = []
  // Each light's flicker position, 0 (dimmest) to 1 - see FLICKER_DEPTH.
  private flame: number[] = []
  // The half cycle each light (and, for a big room, the group) last flipped
  // to - see FLASH_PERIOD_MS.
  private flashBeat: number[] = []
  private groupBeat = -1

  constructor(
    bridge: HueBridge,
    groupId: string,
    lights: HueLight[],
    cap: number,
    callbacks: EngineCallbacks = {},
  ) {
    this.bridge = bridge
    this.groupId = groupId
    this.lights = lights
    this.cap = cap
    this.callbacks = callbacks
  }

  // start remembers the room's current state (restored by stop) and begins
  // ticking.
  start() {
    this.snapshot = this.lights.map((l) => ({ id: l.id, state: restorableState(l.state) }))
    this.timer = setInterval(() => this.tick(), TICK_MS)
  }

  // stop halts the engine and, if restore, puts every light back the way
  // start found it.
  async stop(restore: boolean) {
    if (this.timer) clearInterval(this.timer)
    this.timer = null
    if (!restore) return
    for (const s of this.snapshot) {
      await this.bridge.setLight(s.id, { ...s.state, transitiontime: 10 }).catch(() => {})
      await sleep(TICK_MS)
    }
  }

  setCue(cue: Lighting | null, transition: Transition) {
    if (!cue || sameCue(cue, this.cue)) return
    this.cue = cue
    this.transition = transition
    this.baseDirty = true
    this.scheduleFlash(Date.now())
  }

  setCap(cap: number) {
    if (cap === this.cap) return
    this.cap = cap
    this.baseDirty = true
  }

  setPlaying(playing: boolean) {
    if (playing === this.playing) return
    this.playing = playing
    // Settle every light back onto the plain cue when pausing mid-effect.
    if (!playing) this.baseDirty = true
  }

  private tick() {
    const cue = this.cue
    if (!cue) return
    const now = Date.now()
    if (this.baseDirty) {
      if (now - this.lastGroupSend < GROUP_MIN_INTERVAL_MS) return
      const ds = this.transition === 'cut' ? CUT_TRANSITION_DS : CONTINUATION_TRANSITION_DS
      this.baseDirty = false
      this.effectsFrom = now + ds * 100
      this.sendGroup(this.baseState(cue, ds), now)
      return
    }
    if (!this.playing || now < this.effectsFrom) return
    switch (cue.effect) {
      case 'lightning':
        if (now >= this.nextFlash && now - this.lastGroupSend >= GROUP_MIN_INTERVAL_MS) {
          this.flash(cue, now)
        }
        return
      case 'flicker':
      case 'pulse':
      case 'shimmer':
        this.animateNextLight(cue, cue.effect, now)
        return
      case 'flash':
        this.flashNext(cue, now)
        return
    }
  }

  private baseState(cue: Lighting, transitionDs: number): HueState {
    const rgb = parseHex(cue.color) ?? { r: 1, g: 0.85, b: 0.7 }
    return {
      on: true,
      bri: hueBrightness(cue.brightness, this.cap),
      xy: rgbToXy(rgb),
      transitiontime: transitionDs,
    }
  }

  private animateNextLight(cue: Lighting, effect: LightEffect, now: number) {
    if (this.lights.length === 0 || this.inFlight >= MAX_IN_FLIGHT) return
    const i = this.nextLight % this.lights.length
    this.nextLight = i + 1
    const light = this.lights[i]
    // How long until this light comes round again - its fade length.
    const intervalMs = TICK_MS * this.lights.length
    const base = parseHex(cue.color) ?? { r: 1, g: 0.85, b: 0.7 }
    const accent = parseHex(cue.accent)
    let rgb = base
    let level = 1
    let ds = Math.max(1, Math.round(intervalMs / 100))
    switch (effect) {
      case 'flicker': {
        // Each light wanders independently - a room of flames, not one strobe.
        const prev = this.flame[i] ?? 1
        const f = prev + (Math.random() - prev) * FLICKER_EASE
        this.flame[i] = f
        level = 1 - FLICKER_DEPTH * (1 - f)
        rgb = mixRgb(base, flickerTint(base), (1 - f) * FLICKER_COLOR_SHIFT)
        ds = Math.max(FLICKER_MIN_DS, ds)
        break
      }
      case 'pulse': {
        // One shared phase so the room breathes together.
        const phase = (now / PULSE_PERIOD_MS) * 2 * Math.PI
        level = 0.55 + 0.45 * (0.5 + 0.5 * Math.sin(phase))
        break
      }
      case 'shimmer': {
        // Per-light phase offsets so the colors drift across the room.
        const phase = (now / SHIMMER_PERIOD_MS) * 2 * Math.PI + i * 1.7
        rgb = accent ? mixRgb(base, accent, 0.5 + 0.5 * Math.sin(phase)) : base
        level = 0.85 + 0.15 * Math.sin(phase * 1.3)
        break
      }
    }
    const state: HueState = {
      on: true,
      bri: hueBrightness(cue.brightness * level, this.cap),
      transitiontime: ds,
    }
    if (light.color) state.xy = rgbToXy(rgb)
    this.send(this.bridge.setLight(light.id, state))
  }

  // flashNext flips the next light that hasn't yet taken the current half
  // cycle's state, or the whole group in a room too big to flip per light.
  private flashNext(cue: Lighting, now: number) {
    const beat = Math.floor(now / (FLASH_PERIOD_MS / 2))
    const n = this.lights.length
    if (n > FLASH_MAX_LIGHTS) {
      if (beat === this.groupBeat || now - this.lastGroupSend < GROUP_MIN_INTERVAL_MS) return
      this.groupBeat = beat
      this.sendGroup(this.flashState(cue, beat, true), now)
      return
    }
    if (this.inFlight >= MAX_IN_FLIGHT) return
    for (let k = 0; k < n; k++) {
      const i = (this.nextLight + k) % n
      if (this.flashBeat[i] === beat) continue
      this.flashBeat[i] = beat
      this.nextLight = i + 1
      const light = this.lights[i]
      // With an accent, neighbours sit on opposite colors.
      const lightBeat = cue.accent && light.color ? beat + i : beat
      this.send(this.bridge.setLight(light.id, this.flashState(cue, lightBeat, light.color)))
      return
    }
  }

  // flashState is one side of the flash: the cue itself on even beats, and
  // on odd ones its accent - or, with no accent (or a bulb with no color),
  // the cue dimmed.
  private flashState(cue: Lighting, beat: number, color: boolean): HueState {
    const base = parseHex(cue.color) ?? { r: 1, g: 0.85, b: 0.7 }
    const accent = color ? parseHex(cue.accent) : null
    const odd = beat % 2 !== 0
    const state: HueState = {
      on: true,
      bri: hueBrightness(cue.brightness * (odd && !accent ? FLASH_DIM : 1), this.cap),
      transitiontime: FLASH_TRANSITION_DS,
    }
    if (color) state.xy = rgbToXy(odd && accent ? accent : base)
    return state
  }

  private flash(cue: Lighting, now: number) {
    const accent = parseHex(cue.accent) ?? { r: 0.86, g: 0.9, b: 1 }
    const back = this.baseState(cue, 4)
    const strike = () =>
      this.bridge.setGroup(this.groupId, {
        on: true,
        bri: hueBrightness(100, this.cap),
        xy: rgbToXy(accent),
        transitiontime: 0,
      })
    this.lastGroupSend = now
    this.scheduleFlash(now)
    const double = Math.random() < 0.35
    this.send(
      (async () => {
        await strike()
        await sleep(140)
        await this.bridge.setGroup(this.groupId, back)
        if (double) {
          await sleep(320)
          await strike()
          await sleep(110)
          await this.bridge.setGroup(this.groupId, back)
        }
      })(),
    )
  }

  private scheduleFlash(now: number) {
    this.nextFlash =
      now + LIGHTNING_MIN_GAP_MS + Math.random() * (LIGHTNING_MAX_GAP_MS - LIGHTNING_MIN_GAP_MS)
  }

  private sendGroup(state: HueState, now: number) {
    this.lastGroupSend = now
    this.send(this.bridge.setGroup(this.groupId, state))
  }

  private send(p: Promise<void>) {
    this.inFlight++
    p.then(
      () => this.callbacks.onOk?.(),
      (err: unknown) => this.callbacks.onError?.(err),
    ).finally(() => {
      this.inFlight--
    })
  }
}

// flickerTint is base nudged a little redder (less green, less blue) - where
// a flame's color dips as it gutters. Only its chromaticity reaches the
// lights (xy); brightness comes from bri.
function flickerTint(base: Rgb): Rgb {
  return { r: base.r, g: base.g * 0.85, b: base.b * 0.7 }
}

// restorableState keeps only what a later PUT can put back: on/bri, plus
// whichever color mode the light was actually in.
function restorableState(s: HueState): HueState {
  const out: HueState = { on: s.on, bri: s.bri }
  if (s.colormode === 'ct' && s.ct !== undefined) out.ct = s.ct
  else if (s.xy) out.xy = s.xy
  return out
}

function sameCue(a: Lighting, b: Lighting | null): boolean {
  return (
    !!b &&
    a.color === b.color &&
    (a.accent ?? '') === (b.accent ?? '') &&
    a.brightness === b.brightness &&
    a.effect === b.effect
  )
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms))
}
