import { useEffect, useRef } from 'react'
import type { ChapterDetail } from '../api/types'

// Sound-effect overlay playback: a brand-new, independent <audio> (via
// the Audio() constructor, not playback's own audioElement) fired once
// playback actually reaches this paragraph's own chosen trigger word
// (sfxTriggerWord, a 0-based index into words - see ChapterSection's own
// word-picker), not merely once the paragraph starts - deliberately
// never stopped/tracked past that point, so a clip longer than the rest
// of its own paragraph keeps playing to completion even once playback
// has moved on to a later one (see backend/CLAUDE.md's "SFX sound
// effects" section: overlap with later paragraphs is fine, an explicit
// stop on advance is not). Re-checked on every currentTime
// update (usePlayback's own 'timeupdate'-driven state, a few times a
// second - plenty of precision for triggering a sound cue, unlike
// per-frame word highlighting) rather than only on paragraph change, so
// landing mid-paragraph past the trigger word (a seek, or a paragraph
// entered already past it) still fires immediately instead of never.
// Keyed off (chapterIdx, paragraphIdx) so this fires at most once per
// paragraph visit even though the effect re-runs many times while
// playing it, and gated on isPlaying so a paragraph merely landed on
// while paused never triggers one - matching how narration itself only
// plays on Play.
export function useSFXOverlay({
  chapters,
  chapterIdx,
  paragraphIdx,
  currentTime,
  isPlaying,
}: {
  chapters: Map<number, ChapterDetail>
  chapterIdx: number
  paragraphIdx: number
  currentTime: number
  isPlaying: boolean
}) {
  const lastSFXKeyRef = useRef<string | null>(null)
  useEffect(() => {
    if (!isPlaying) return
    const key = `${chapterIdx}:${paragraphIdx}`
    if (lastSFXKeyRef.current === key) return
    const p = chapters.get(chapterIdx)?.paragraphs[paragraphIdx]
    if (p?.sfxStatus !== 'ready' || !p.sfxAudioUrl) return
    const triggerTime = p.words[p.sfxTriggerWord ?? 0]?.start ?? 0
    if (currentTime < triggerTime) return
    lastSFXKeyRef.current = key
    new Audio(p.sfxAudioUrl).play().catch(() => {
      // Autoplay blocked (no user gesture on this tab yet) - nothing to
      // recover from; a later paragraph's own overlay isn't affected.
    })
  }, [isPlaying, chapterIdx, paragraphIdx, currentTime, chapters])
}
