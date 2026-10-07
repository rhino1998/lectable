import { useEffect, useState } from 'react'
import { RiPauseFill, RiPlayFill } from 'react-icons/ri'

// A standalone, isolated preview of one music region's generated clip
// (the reader's region markers and the Book settings page's AmbiencePreview) - its
// own plain <audio> (not tied to usePlayback's narration element or
// useBackgroundMusic's own live-mixing Web Audio graph at all), the same
// "quick audition, nothing else on the page needs to know about it" shape
// the SFX panel's own <audio controls> preview already has, just a single
// icon toggle instead of the browser's own full control bar (this sits
// inside a small pill, not a whole panel). Playing a preview does not
// pause/duck narration or the live background-music mix - both keep
// running underneath, exactly like clicking the SFX panel's preview does.
export function MusicRegionPreviewButton({ audioUrl }: { audioUrl: string }) {
  const [playing, setPlaying] = useState(false)
  // Created once, on first render - like the ref it replaces, the clip
  // stays the one this button first mounted with.
  const [audio] = useState(() => {
    const a = new Audio(audioUrl)
    a.addEventListener('ended', () => setPlaying(false))
    a.addEventListener('pause', () => setPlaying(false))
    return a
  })

  // Stops playback when the button unmounts mid-preview - a reader marker
  // whose chapter scrolled out of the render window (ChapterSection's
  // renderFull/placeholderHeight), or a collapsed Speakers-page ambience
  // preview (AmbiencePreview) - otherwise the clip would keep playing
  // invisibly with no way left on screen to stop it.
  useEffect(() => {
    return () => {
      audio.pause()
    }
  }, [audio])

  return (
    <button
      className="chapter-music-boundary-preview"
      onClick={(e) => {
        e.stopPropagation()
        if (playing) {
          audio.pause()
        } else {
          void audio.play()
          setPlaying(true)
        }
      }}
      title={playing ? 'Pause preview' : 'Preview this region’s generated music'}
    >
      {playing ? <RiPauseFill /> : <RiPlayFill />}
    </button>
  )
}
