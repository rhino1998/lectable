import { useEffect, useMemo, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import type { RecapSegment } from '../api/types'
import {
  useBookRecap,
  usePrepareBookRecap,
  useRecapSettings,
  useSetRecapSettings,
} from '../api/queries'
import {
  RECAP_AFTER_OPTIONS,
  RECAP_NEVER,
  activeRecapWord,
  agoLabel,
  loadDismissedRecap,
  recapIsCurrent,
  recapKey,
  recapWords,
  saveDismissedRecap,
  shouldHoldForRecap,
  shouldOfferRecap,
  shouldRequestRecap,
  type RecapPos,
} from '../utils/recap'

// What the player bar drives while a recap is open: its play/pause button,
// the keyboard shortcut and the sleep timer act on the recap's clip, not
// the book's.
export interface RecapTransport {
  isPlaying: boolean
  play: () => void
  pause: () => void
  // Shown in the bar in place of the chapter/paragraph line.
  label?: string
}

interface Props {
  bookId: string
  // The book as it was when the reader opened it: when it was last read
  // (before this visit saved anything) is what "away" is measured from.
  lastReadAt: number | undefined
  seriesIndex: number
  // Where the reader is now.
  pos: RecapPos
  isPlaying: boolean
  // The panel is opened from the reader's header as well as from the offer.
  open: boolean
  onOpenChange: (open: boolean) => void
  // Where the open recap is rendered: the slot above the paragraph the
  // reader was at (ChapterSection's .recap-slot). Null while that paragraph
  // isn't mounted - it floats above the player bar then, like the offer.
  container: HTMLElement | null
  // Pauses the book while the recap is spoken, and resumes it after.
  onPause: () => void
  onPlay: () => void
  // The book's playback speed, which the recap is spoken at too.
  playbackRate: number
  // Hands the reader the recap's transport while it is open with a clip,
  // null otherwise - the recap has no controls of its own.
  onTransport: (transport: RecapTransport | null) => void
}

// The reader's "previously on" recap (backend httpapi/recap.go): an offer
// (floating above the player bar) when the reader comes back to a book
// after a while - or starts one with a book before it in its series - and
// the recap itself, set into the text above the paragraph they were at:
// its narration, which hands back to the book when it ends, and its text,
// highlighted word by word as it is spoken. Mirrored by
// android's ui/reader/RecapInline.kt - keep the two in step.
export function RecapPanel({
  bookId,
  lastReadAt,
  seriesIndex,
  pos,
  isPlaying,
  open,
  onOpenChange,
  container,
  onPause,
  onPlay,
  playbackRate,
  onTransport,
}: Props) {
  const recapQuery = useBookRecap(bookId)
  const prepare = usePrepareBookRecap(bookId)
  const recap = recapQuery.data?.recap ?? null
  const preparing = !!recapQuery.data?.preparing

  // Frozen at mount: this visit's own position saves move lastReadAt.
  const [opened] = useState(() => ({ at: Date.now(), lastReadAt }))
  // Library-wide; nothing is offered or asked for until it has arrived.
  const settingsQuery = useRecapSettings()
  const setSettings = useSetRecapSettings()
  const afterMinutes = settingsQuery.data?.afterMinutes ?? RECAP_NEVER
  const [dismissed, setDismissed] = useState(() => loadDismissedRecap(bookId))
  // Once the book has played this visit, the reader has picked it back up.
  const [played, setPlayed] = useState(false)
  // Opening the panel plays the recap as soon as it has a clip - at once, or
  // when its narration arrives - unless it was opened to be read.
  const [autoPlay, setAutoPlay] = useState(true)
  const audioRef = useRef<HTMLAudioElement>(null)

  if (isPlaying && !played) setPlayed(true)
  // The book and its recap never talk over each other.
  useEffect(() => {
    if (isPlaying) audioRef.current?.pause()
  }, [isPlaying])

  const current = recapIsCurrent(recap, pos)
  const loaded = recapQuery.data !== undefined && settingsQuery.data !== undefined

  // The clip has no controls of its own: while the recap is open with a
  // clip, the player bar's are its controls ("recap"). The bar's play is
  // also what the recap waits behind: with the recap due - the reader is
  // back after a while and it is ready or being written - play opens it to
  // be heard first ("hold"), and pressed again while it is still on its way
  // ("waiting") skips it and starts the book.
  const [recapPlaying, setRecapPlaying] = useState(false)
  const engaged = open && current && !!recap?.audioUrl
  const waiting = open && autoPlay && !engaged
  const hold =
    !open &&
    !played &&
    loaded &&
    shouldHoldForRecap(
      recap,
      pos,
      { lastReadAt: opened.lastReadAt, seriesIndex },
      opened.at,
      afterMinutes,
      dismissed,
      preparing || !prepare.isIdle,
    )
  const mode = engaged ? 'recap' : waiting ? 'waiting' : hold ? 'hold' : null
  const actions = useRef({ show: () => {}, skip: () => {} })
  useEffect(() => {
    if (mode === 'recap') {
      onTransport({
        isPlaying: recapPlaying,
        play: () => void audioRef.current?.play(),
        pause: () => audioRef.current?.pause(),
        label: 'Previously on…',
      })
    } else if (mode === 'waiting') {
      onTransport({
        isPlaying: false,
        play: () => actions.current.skip(),
        pause: () => {},
        label: 'Preparing the recap - press play again to skip',
      })
    } else if (mode === 'hold') {
      onTransport({ isPlaying: false, play: () => actions.current.show(), pause: () => {} })
    } else {
      onTransport(null)
    }
  }, [mode, recapPlaying, onTransport])
  useEffect(() => () => onTransport(null), [onTransport])
  useEffect(() => {
    if (audioRef.current) audioRef.current.playbackRate = playbackRate
  }, [playbackRate, engaged])

  // Back after a while with nothing ready (the backend usually has one
  // prepared): ask once.
  const requested = useRef(false)
  useEffect(() => {
    if (!loaded || requested.current || preparing) return
    if (
      shouldRequestRecap(
        recap,
        pos,
        { lastReadAt: opened.lastReadAt, seriesIndex },
        opened.at,
        afterMinutes,
      )
    ) {
      requested.current = true
      prepare.mutate(false)
    }
  }, [loaded, preparing, recap, pos, opened, seriesIndex, afterMinutes, prepare])

  const dismiss = () => {
    if (recap) {
      saveDismissedRecap(bookId, recapKey(recap))
      setDismissed(recapKey(recap))
    }
  }
  const show = (play: boolean) => {
    setAutoPlay(play)
    onOpenChange(true)
  }
  // Closed - by hand or by its clip ending - the recap is done with for this
  // visit: play is the book's again.
  const close = () => {
    dismiss()
    setPlayed(true)
    setAutoPlay(true)
    onOpenChange(false)
  }
  // What the bar's play does in the "hold" and "waiting" modes above.
  useEffect(() => {
    actions.current = {
      show: () => show(true),
      skip: () => {
        close()
        onPlay()
      },
    }
  })
  // Opened with nothing for this spot: ask for one, once per opening - even
  // while one is already being prepared, since asking is also what moves a
  // recap the backend was preparing in the background to the front of the
  // queue.
  const askedOnOpen = useRef(false)
  useEffect(() => {
    if (!open) {
      askedOnOpen.current = false
      return
    }
    if (loaded && !current && !askedOnOpen.current) {
      askedOnOpen.current = true
      requested.current = true
      prepare.mutate(false)
    }
  }, [open, loaded, current, prepare])

  // Open with no slot for a moment means there is none (the paragraph's
  // chapter isn't rendered).
  const [floatFallback, setFloatFallback] = useState(false)
  useEffect(() => {
    if (!open || container) return
    const timer = setTimeout(() => setFloatFallback(true), 300)
    return () => {
      clearTimeout(timer)
      setFloatFallback(false)
    }
  }, [open, container])
  // Bring the recap into view where it opened.
  useEffect(() => {
    if (open && container) container.scrollIntoView({ block: 'center', behavior: 'smooth' })
  }, [open, container])

  if (!open) {
    const offer =
      !played && shouldOfferRecap(recap, pos, opened.lastReadAt, opened.at, afterMinutes, dismissed)
    if (!offer || !recap) return null
    return (
      <p className="recap-float recap-offer">
        {recap.kind === 'series'
          ? `Before you start: what happened in ${recap.title}. `
          : `Welcome back${opened.lastReadAt ? ` - you last listened ${agoLabel(opened.lastReadAt, opened.at)}` : ''}. Previously, in ${recap.title}… `}
        {recap.audioUrl && (
          <>
            <button className="text-button" onClick={() => show(true)}>
              Play recap
            </button>{' '}
          </>
        )}
        <button className="text-button" onClick={() => show(false)}>
          Read it
        </button>{' '}
        <button className="text-button" onClick={dismiss}>
          Not now
        </button>
      </p>
    )
  }

  const failedHere = !current && !!recap?.error && recap.chapterIdx === pos.chapterIdx
  const panel = (
    <div className={container ? 'recap-panel recap-inline' : 'recap-float recap-panel'}>
      <div className="recap-panel-header">
        <strong>{current && recap ? `Previously, in ${recap.title}` : 'Previously…'}</strong>
        <button className="text-button" onClick={close}>
          Close
        </button>
      </div>
      {current && recap ? (
        <>
          {recap.audioUrl ? (
            <audio
              ref={audioRef}
              autoPlay={autoPlay}
              src={recap.audioUrl}
              onLoadedMetadata={(e) => (e.currentTarget.playbackRate = playbackRate)}
              onPlay={() => {
                setRecapPlaying(true)
                onPause()
              }}
              onPause={() => setRecapPlaying(false)}
              onEnded={() => {
                setRecapPlaying(false)
                close()
                onPlay()
              }}
            />
          ) : (
            <p className="muted">
              {preparing ? 'Its narration is on the way…' : 'Not narrated yet.'}
            </p>
          )}
          {recap.audioUrl && recap.segments.length > 0 ? (
            <RecapText segments={recap.segments} audioRef={audioRef} />
          ) : (
            recap.text.split('\n\n').map((para, i) => <p key={i}>{para}</p>)
          )}
        </>
      ) : preparing ? (
        <p className="muted">
          {waiting
            ? 'Writing the recap - the book starts after it. Press play again to skip.'
            : 'Writing the recap…'}
        </p>
      ) : failedHere ? (
        <p className="error-text">The recap couldn’t be written: {recap?.error}</p>
      ) : (
        <p className="muted">Nothing to recap here yet.</p>
      )}
      <div className="recap-panel-footer">
        <button
          className="text-button"
          disabled={preparing}
          onClick={() => {
            requested.current = true
            prepare.mutate(true)
          }}
        >
          {preparing ? 'Working…' : current ? 'Write it again' : 'Write one'}
        </button>
        <label className="muted">
          Offer{' '}
          <select
            value={afterMinutes}
            disabled={settingsQuery.data === undefined}
            onChange={(e) => setSettings.mutate(Number(e.target.value))}
          >
            {RECAP_AFTER_OPTIONS.map((o) => (
              <option key={o.minutes} value={o.minutes}>
                {o.label}
              </option>
            ))}
          </select>
        </label>
      </div>
    </div>
  )
  if (container) return createPortal(panel, container)
  // Its paragraph isn't on screen to sit above: float, once that is clear -
  // not for the moment before the slot mounts, when the clip would start
  // here and again there.
  return floatFallback ? panel : null
}

// The recap as it is spoken - a paragraph per stretch of the clip - with
// the word being spoken highlighted, and a click on a word seeking to it.
function RecapText({
  segments,
  audioRef,
}: {
  segments: RecapSegment[]
  audioRef: React.RefObject<HTMLAudioElement | null>
}) {
  const words = useMemo(() => segments.map(recapWords), [segments])
  const times = useMemo(() => words.flat().map((w) => w.time), [words])
  // -1 until the clip has started; follows it frame by frame while it plays.
  const [active, setActive] = useState(-1)
  useEffect(() => {
    let frame = 0
    const tick = () => {
      const audio = audioRef.current
      if (audio && (audio.currentTime > 0 || !audio.paused)) {
        // A beat ahead, as ParagraphText's highlight is.
        setActive(activeRecapWord(times, audio.currentTime + 0.1))
      }
      frame = requestAnimationFrame(tick)
    }
    frame = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(frame)
  }, [audioRef, times])

  // Each segment's first word's place among all of them.
  const bases = useMemo(
    () => words.map((_, si) => words.slice(0, si).reduce((sum, ws) => sum + ws.length, 0)),
    [words],
  )
  return (
    <>
      {segments.map((seg, si) => {
        const nodes: React.ReactNode[] = []
        let cursor = 0
        for (const [wi, w] of words[si].entries()) {
          const i = bases[si] + wi
          if (w.start > cursor) nodes.push(seg.text.slice(cursor, w.start))
          nodes.push(
            <span
              key={i}
              className={'word' + (i === active ? ' word-active' : '')}
              onClick={() => {
                const audio = audioRef.current
                if (!audio) return
                audio.currentTime = w.time
                void audio.play()
              }}
            >
              {w.word}
            </span>,
          )
          cursor = w.end
        }
        if (cursor < seg.text.length) nodes.push(seg.text.slice(cursor))
        return <p key={si}>{nodes}</p>
      })}
    </>
  )
}
