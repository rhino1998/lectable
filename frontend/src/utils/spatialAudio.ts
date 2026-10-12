import type { ChapterDetail } from '../api/types'

// Speaker positions: each character is heard from a seat to the left or
// right of a centred narrator. Seats are worked out here from the
// chapter's own speakers and applied at playback - clips stay mono, and
// nothing comes from the backend. A per-listener setting
// (useSpatialSettings), changed from PlayerBar's Playback panel.

// "stereo" is plain left/right panning; "hrtf" is binaural, for
// headphones; "hrtf-head" is binaural speakers with narration left
// unplaced, so it is heard inside the head.
export const SPATIAL_MODES = ['off', 'stereo', 'hrtf', 'hrtf-head'] as const
export type SpatialMode = (typeof SPATIAL_MODES)[number]

export interface SpatialSettings {
  mode: SpatialMode
  // How far apart the seats are: a multiplier on MAX_PAN, 0 (everyone in
  // the centre) to MAX_SPATIAL_WIDTH (the outer seats hard left/right).
  width: number
}

// How far the outermost seat sits from centre at width 1, as a
// StereoPannerNode pan (1 is hard left/right). In the hrtf modes the same
// value is a fraction of 90 degrees, so 0.5 is 45 degrees off centre.
const MAX_PAN = 0.5

export const MAX_SPATIAL_WIDTH = 1 / MAX_PAN
export const DEFAULT_SPATIAL_SETTINGS: SpatialSettings = { mode: 'off', width: 1 }

// Seats in the order a chapter's speakers take them: the first two voices
// heard sit far apart on opposite sides, later ones fill in between.
const SEATS = [-1, 1, -0.5, 0.5, -0.75, 0.75, -0.25, 0.25]

const PAN_SMOOTHING_SECONDS = 0.015

// Level of the unplaced (inside the head) path beside the HRTF one.
const UNPLACED_GAIN = 1

function seatKey(speaker: string, speakerIndex: number | undefined): string {
  return speakerIndex ? `${speaker}#${speakerIndex}` : speaker
}

/**
 * Where paragraph idx of chapter is heard from, as a pan in
 * [-MAX_PAN, MAX_PAN] scaled by width, or null for a line with no place
 * in the room: narration, an unattributed line and centerSpeaker (a
 * first-person book's narrator). Anyone else has the seat they took by
 * order of first line in the chapter.
 */
export function paragraphPan(
  chapter: ChapterDetail | undefined,
  idx: number,
  centerSpeaker: string,
  width: number,
): number | null {
  const target = chapter?.paragraphs[idx]
  if (!chapter || !target) return null
  const centred = (speaker: string | undefined) =>
    !speaker || speaker === 'Narrator' || speaker === centerSpeaker
  if (centred(target.speaker)) return null
  const wanted = seatKey(target.speaker ?? '', target.speakerIndex)
  const seen = new Set<string>()
  for (const p of chapter.paragraphs) {
    if (centred(p.speaker)) continue
    const key = seatKey(p.speaker ?? '', p.speakerIndex)
    if (key === wanted) break
    seen.add(key)
  }
  return SEATS[seen.size % SEATS.length] * MAX_PAN * width
}

export interface SpatialStage {
  /** Plays one element's voice from pan (a paragraphPan value) under mode. */
  place: (elementIdx: number, mode: SpatialMode, pan: number | null) => void
  resume: () => void
  close: () => void
}

/**
 * Routes each element through its own panners. From here on the elements
 * are only audible through the stage's AudioContext, whatever the mode
 * (an element can't be taken back out of a context), so it must be
 * created after a user gesture and resumed before playing.
 */
export function createSpatialStage(elements: readonly HTMLAudioElement[]): SpatialStage {
  const ctx = new AudioContext()
  const ramp = (param: AudioParam, value: number) =>
    param.setTargetAtTime(value, ctx.currentTime, PAN_SMOOTHING_SECONDS)
  const placers = elements.map((el) => {
    const source = ctx.createMediaElementSource(el)
    // Three ways out, one audible at a time. Unplaced skips every panner:
    // the same signal in both ears, which is the element as it sounds with
    // positions off and what a voice inside the head is.
    const unplaced = ctx.createGain()
    source.connect(unplaced).connect(ctx.destination)
    const stereoPanner = ctx.createStereoPanner()
    const stereo = ctx.createGain()
    stereo.gain.value = 0
    source.connect(stereoPanner).connect(stereo).connect(ctx.destination)
    const hrtfPanner = ctx.createPanner()
    hrtfPanner.panningModel = 'HRTF'
    hrtfPanner.positionZ.value = -1
    const hrtf = ctx.createGain()
    hrtf.gain.value = 0
    source.connect(hrtfPanner).connect(hrtf).connect(ctx.destination)

    return (mode: SpatialMode, pan: number | null) => {
      const inHead = mode === 'hrtf-head' && pan === null
      const useHrtf = (mode === 'hrtf' || mode === 'hrtf-head') && !inHead
      ramp(unplaced.gain, mode === 'off' ? 1 : inHead ? UNPLACED_GAIN : 0)
      ramp(stereo.gain, mode === 'stereo' ? 1 : 0)
      ramp(hrtf.gain, useHrtf ? 1 : 0)
      if (mode === 'stereo') ramp(stereoPanner.pan, pan ?? 0)
      if (useHrtf) {
        const azimuth = ((pan ?? 0) * Math.PI) / 2
        ramp(hrtfPanner.positionX, Math.sin(azimuth))
        ramp(hrtfPanner.positionZ, -Math.cos(azimuth))
      }
    }
  })
  return {
    place: (elementIdx, mode, pan) => placers[elementIdx]?.(mode, pan),
    resume: () => {
      if (ctx.state === 'suspended') void ctx.resume()
    },
    close: () => void ctx.close(),
  }
}
