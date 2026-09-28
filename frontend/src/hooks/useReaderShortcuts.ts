import { useEffect } from 'react'

// Play/pause/skip/bookmark keyboard shortcuts - ignored while a form
// field has focus (typing " "/arrow keys/"b" in the search box, a
// voice-editor textarea, etc. must behave normally, not hijack playback)
// or while a modifier is held (so e.g. Cmd+ArrowLeft browser-back still
// works).
export function useReaderShortcuts({
  isPlaying,
  play,
  pause,
  chapterIdx,
  paragraphIdx,
  skipParagraph,
  toggleBookmark,
  selectedSpeaker,
  jumpToNextAppearance,
}: {
  isPlaying: boolean
  play: () => void
  pause: () => void
  chapterIdx: number
  paragraphIdx: number
  skipParagraph: (direction: 1 | -1) => void
  toggleBookmark: (chapterIdx: number, paragraphIdx: number) => void
  selectedSpeaker: string | null
  jumpToNextAppearance: (speakerName: string) => void
}) {
  useEffect(() => {
    const isTypingTarget = (el: EventTarget | null) => {
      if (!(el instanceof HTMLElement)) return false
      return (
        el.tagName === 'INPUT' ||
        el.tagName === 'TEXTAREA' ||
        el.tagName === 'SELECT' ||
        el.isContentEditable
      )
    }
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.metaKey || e.ctrlKey || e.altKey || isTypingTarget(e.target)) return
      if (e.key === ' ' || e.key === 'Spacebar') {
        e.preventDefault()
        if (isPlaying) pause()
        else play()
      } else if (e.key === 'ArrowRight') {
        e.preventDefault()
        skipParagraph(1)
      } else if (e.key === 'ArrowLeft') {
        e.preventDefault()
        skipParagraph(-1)
      } else if (e.key === 'b' || e.key === 'B') {
        e.preventDefault()
        toggleBookmark(chapterIdx, paragraphIdx)
      } else if ((e.key === 'n' || e.key === 'N') && selectedSpeaker) {
        e.preventDefault()
        jumpToNextAppearance(selectedSpeaker)
      }
    }
    window.addEventListener('keydown', onKeyDown)
    return () => window.removeEventListener('keydown', onKeyDown)
  }, [
    isPlaying,
    play,
    pause,
    chapterIdx,
    paragraphIdx,
    skipParagraph,
    toggleBookmark,
    selectedSpeaker,
    jumpToNextAppearance,
  ])
}
