import { useEffect } from 'react'
import { useChapterMusic } from '../api/queries'
import { lightingController, useLightingState } from '../lighting/controller'

interface UseLightingArgs {
  bookId: string
  chapterIdx: number
  paragraphIdx: number
  isPlaying: boolean
}

// useLighting drives the listener's Hue lights from the ambience pass's
// lighting cues as narration moves through a chapter's music regions -
// useBackgroundMusic's sibling, reading the same chapterMusic topic (each
// region carries its own `lighting`). Needs no generated audio at all, so
// it works whether or not background music is on. The actual bridge
// traffic and effect animation live in lighting/controller.ts and
// lighting/engine.ts; this only picks the region under the current
// paragraph and holds the controller while the reader is mounted.
export function useLighting({ bookId, chapterIdx, paragraphIdx, isPlaying }: UseLightingArgs) {
  const { settings, previewing } = useLightingState()
  // Also while a Lights panel is open, for its preview swatch.
  const tracking = settings.enabled || previewing
  const { data } = useChapterMusic(bookId, chapterIdx, tracking)

  useEffect(() => {
    if (!settings.enabled) return
    lightingController.acquire()
    return () => lightingController.release()
  }, [settings.enabled])

  const region = data?.regions.find((r) => paragraphIdx >= r.startIdx && paragraphIdx <= r.endIdx)
  const cue = region?.lighting ?? null
  const transition = region?.transition ?? 'cut'
  // Keyed on the cue's values, not the region object - a live patch to the
  // region (its music finishing) must not re-send the same light.
  const cueKey = cue ? `${cue.color}|${cue.accent ?? ''}|${cue.brightness}|${cue.effect}` : ''
  useEffect(() => {
    if (tracking && cue) lightingController.setCue(cue, transition)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tracking, cueKey])

  useEffect(() => {
    lightingController.setPlaying(isPlaying)
  }, [isPlaying])
}
