// The web reader's one lighting controller: the listener's Hue settings
// (kept in localStorage - they're this browser's own pairing with this
// network's bridge, not library state, so they never go to the backend)
// and the LightingEngine's lifecycle. A module singleton so the reader's
// useLighting hook (which feeds it cues) and PlayerBar's LightingPanel
// (which edits settings and shows status) share it; React reads it through
// useLightingState (useSyncExternalStore).

import { useSyncExternalStore } from 'react'
import type { Lighting } from '../api/types'
import { LightingEngine, type Transition } from './engine'
import { describeError, HueBridge, type HueGroup } from './hue'

export interface LightingSettings {
  enabled: boolean
  bridgeIp: string
  username: string
  groupId: string
  // 0.1-1: the brightest any cue may drive the room.
  brightnessCap: number
  // Put the lights back the way they were when the reader stops lighting.
  restoreOnStop: boolean
}

export type LightingStatus = 'off' | 'unconfigured' | 'connecting' | 'active' | 'error'

export interface LightingState {
  settings: LightingSettings
  status: LightingStatus
  message: string | null
  // The cue under the current paragraph, tracked while lighting is on or a
  // Lights panel is previewing it.
  cue: Lighting | null
  previewing: boolean
}

const STORAGE_KEY = 'lectable.lighting'

const DEFAULT_SETTINGS: LightingSettings = {
  enabled: false,
  bridgeIp: '',
  username: '',
  groupId: '',
  brightnessCap: 0.8,
  restoreOnStop: true,
}

function loadSettings(): LightingSettings {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (raw) return { ...DEFAULT_SETTINGS, ...(JSON.parse(raw) as Partial<LightingSettings>) }
  } catch {
    // Private window / blocked storage - lighting just starts unconfigured.
  }
  return DEFAULT_SETTINGS
}

function saveSettings(s: LightingSettings) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(s))
  } catch {
    // See loadSettings.
  }
}

export function isConfigured(s: LightingSettings): boolean {
  return s.bridgeIp !== '' && s.username !== '' && s.groupId !== ''
}

class LightingController {
  private state: LightingState
  private listeners = new Set<() => void>()
  private engine: LightingEngine | null = null
  // How many readers want lights driven right now (normally 0 or 1).
  private holders = 0
  private playing = false
  private transition: Transition = 'cut'
  // Bumped on every (re)start so a slow start that's been superseded
  // doesn't install its engine.
  private generation = 0

  constructor() {
    const settings = loadSettings()
    this.state = { settings, status: 'off', message: null, cue: null, previewing: false }
  }

  subscribe = (fn: () => void) => {
    this.listeners.add(fn)
    return () => this.listeners.delete(fn)
  }

  getState = () => this.state

  private set(patch: Partial<LightingState>) {
    this.state = { ...this.state, ...patch }
    this.listeners.forEach((fn) => fn())
  }

  bridge(): HueBridge | null {
    const s = this.state.settings
    return s.bridgeIp && s.username ? new HueBridge(s.bridgeIp, s.username) : null
  }

  updateSettings(patch: Partial<LightingSettings>) {
    const prev = this.state.settings
    const settings = { ...prev, ...patch }
    saveSettings(settings)
    this.set({ settings })
    if (
      settings.enabled !== prev.enabled ||
      settings.bridgeIp !== prev.bridgeIp ||
      settings.username !== prev.username ||
      settings.groupId !== prev.groupId
    ) {
      void this.restart()
    } else if (settings.brightnessCap !== prev.brightnessCap) {
      this.engine?.setCap(settings.brightnessCap)
    }
  }

  // acquire/release bracket a reader that wants the room lit; the engine
  // runs while at least one holds it and lighting is enabled.
  acquire() {
    this.holders++
    if (this.holders === 1) void this.restart()
  }

  release() {
    this.holders = Math.max(0, this.holders - 1)
    if (this.holders === 0) void this.restart()
  }

  // setPreviewing marks a Lights panel open (or closed): the reader then
  // tracks cues for its preview swatch even while lighting is off.
  private previewers = 0
  setPreviewing(on: boolean) {
    this.previewers = Math.max(0, this.previewers + (on ? 1 : -1))
    this.set({ previewing: this.previewers > 0 })
  }

  setCue(cue: Lighting | null, transition: Transition) {
    if (!cue) return // an unlit region holds whatever is showing
    this.transition = transition
    this.set({ cue })
    this.engine?.setCue(cue, transition)
  }

  setPlaying(playing: boolean) {
    this.playing = playing
    this.engine?.setPlaying(playing)
  }

  // groups lists the paired bridge's rooms and zones, for the room picker.
  async groups(): Promise<HueGroup[]> {
    const b = this.bridge()
    return b ? b.groups() : []
  }

  private async restart() {
    const gen = ++this.generation
    const old = this.engine
    this.engine = null
    if (old) await old.stop(this.state.settings.restoreOnStop)
    const s = this.state.settings
    if (!s.enabled || this.holders === 0) {
      this.set({ status: 'off', message: null })
      return
    }
    if (!isConfigured(s)) {
      this.set({ status: 'unconfigured', message: null })
      return
    }
    this.set({ status: 'connecting', message: null })
    try {
      const bridge = new HueBridge(s.bridgeIp, s.username)
      const [groups, lights] = await Promise.all([bridge.groups(), bridge.lights()])
      if (gen !== this.generation) return
      const group = groups.find((g) => g.id === s.groupId)
      if (!group) throw new Error('the selected room no longer exists on the bridge')
      const members = lights.filter((l) => group.lights.includes(l.id))
      const engine = new LightingEngine(bridge, group.id, members, s.brightnessCap, {
        onError: (err) => {
          void describeError(err, s.bridgeIp).then((message) => {
            if (gen === this.generation) this.set({ status: 'error', message })
          })
        },
        onOk: () => {
          if (this.state.status === 'error') this.set({ status: 'active', message: null })
        },
      })
      engine.start()
      engine.setPlaying(this.playing)
      if (this.state.cue) engine.setCue(this.state.cue, this.transition)
      this.engine = engine
      this.set({ status: 'active', message: null })
    } catch (err) {
      const message = await describeError(err, s.bridgeIp)
      if (gen !== this.generation) return
      this.set({ status: 'error', message })
    }
  }
}

export const lightingController = new LightingController()

export function useLightingState(): LightingState {
  return useSyncExternalStore(lightingController.subscribe, lightingController.getState)
}
