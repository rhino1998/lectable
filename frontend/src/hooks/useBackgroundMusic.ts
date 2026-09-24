import { useEffect, useRef } from 'react'
import { useChapterMusic } from '../api/queries'
import type { MusicRegion } from '../api/types'

// Fixed, ducked background level - deliberately not a user-facing volume
// control yet (a first version of this feature - see backend
// store.MusicRegion's own doc comment for the generation pipeline this
// plays back). Kept well under narration's own implicit "full volume" so
// music never competes with the words being read.
const MUSIC_GAIN = 0.22

// Half the width of every region-to-region crossfade, in real (wall-
// clock) seconds. Each switch is centred on the paragraph boundary that
// owns it:
//
//   BBBBBB(Ba)|(bA)AAAAAAAA
//
// the incoming region starts this long *before* the boundary (pre-roll -
// see the region-switch effect below) and fades in over twice this, while
// the outgoing one fades out over the same window - so the two cross at
// equal level right at the boundary, never both at full volume and never
// with a gap between them. Applies to every transition, "continuation"
// included: a seeded continuation still only sounds continuous if the
// outgoing clip is exactly at its own tail at the switch, which a looping
// clip (see source.loop below) never guarantees.
const CROSSFADE_HALF_SECONDS = 5
const CROSSFADE_SECONDS = CROSSFADE_HALF_SECONDS * 2

// Equal-power fade shapes (sin/cos quarter-waves) for setValueCurveAtTime -
// a plain linear crossfade dips audibly toward silence at its midpoint,
// which is exactly the boundary this is centred on.
const CURVE_POINTS = 64
const FADE_IN_CURVE = Float32Array.from({ length: CURVE_POINTS }, (_, i) => Math.sin(((i / (CURVE_POINTS - 1)) * Math.PI) / 2))
const FADE_OUT_CURVE = Float32Array.from({ length: CURVE_POINTS }, (_, i) => Math.cos(((i / (CURVE_POINTS - 1)) * Math.PI) / 2))

// Two gain stages per deck, each automated exactly once in its lifetime
// (fadeIn at start, fadeOut at stop) - so a deck switched away from while
// still mid-fade-in never needs its in-flight curve cancelled and
// re-anchored (setValueCurveAtTime throws on overlapping automation);
// the two simply multiply.
interface Deck {
  regionId: string
  fadeIn: GainNode
  fadeOut: GainNode
  source: AudioBufferSourceNode
  stopping: boolean
}

interface UseBackgroundMusicArgs {
  bookId: string
  chapterIdx: number
  // This chapter's own total paragraph count (chapters.get(chapterIdx)?.
  // paragraphs.length ?? 0 in ReaderPage) - purely so the pre-roll check
  // below can tell "paragraphIdx is this chapter's actual last paragraph"
  // apart from merely "the last paragraph a music region happens to
  // cover" (an unscored trailing paragraph must never trigger a
  // cross-chapter pre-roll - see the region-switch effect's own doc
  // comment). 0 (the default for a not-yet-loaded chapter) safely never
  // matches any real paragraphIdx, so this degrades to "never cross-fade
  // into the next chapter early" rather than misfiring.
  chapterParagraphCount: number
  // This book's total chapter count - so the next-chapter music fetch
  // below doesn't bother querying past the book's actual last chapter.
  totalChapters: number
  paragraphIdx: number
  // Elapsed/total seconds of the *currently narrating* paragraph
  // (PlaybackController.currentTime/duration) - purely for the pre-roll
  // check (see CROSSFADE_HALF_SECONDS): how close is playback to crossing
  // into the next paragraph, which is what the switch used to wait for.
  paragraphCurrentTime: number
  paragraphDuration: number
  musicEnabled: boolean
  isPlaying: boolean
  // PlaybackController.playbackRate - narration's own speed control
  // (usePlayback sets this directly on its <audio> elements, which get
  // the browser's own automatic pitch correction for free -
  // HTMLMediaElement.preservesPitch defaults to true). Applied to the
  // active deck's own AudioBufferSourceNode.playbackRate so the music
  // speeds up/slows down together with narration rather than staying at
  // its own native tempo while the words race ahead of or lag behind it.
  //
  // A plain resample (like changing a tape/turntable's speed), not a
  // pitch-preserving time-stretch the way narration's own audio is - a
  // real limitation, not a deliberate shortcut: routing this graph's
  // output through a real <audio> element via
  // ctx.createMediaStreamDestination() (so *it* could carry the actual
  // playbackRate, the same trick that gets narration its own pitch
  // correction) was tried and confirmed, empirically, not to work -
  // HTMLMediaElement.playbackRate has no effect at all on a
  // MediaStream-sourced element (verified directly: setting it reads
  // back as 1 immediately, not even a delayed revert - MediaStream
  // playback has no buffered, seekable timeline for a resample to act
  // on, unlike a normal file-backed src). Genuine pitch-preserving music
  // playback would need real DSP (an AudioWorklet running something like
  // a phase vocoder, or a library such as SoundTouchJS) - not attempted
  // here; this accepts the pitch shift instead, given it plays back well
  // under narration's own volume (MUSIC_GAIN) as texture, not something
  // a reader is meant to listen to critically on its own.
  playbackRate: number
}

// useBackgroundMusic mixes a chapter's own generated music regions in
// underneath narration, entirely client-side (Web Audio), as paragraphIdx
// advances through them - the "frontend mixes live" delivery this feature
// was built around: the backend only ever hands over per-region clips
// (api.getChapterMusic), never a chapter-length stitched track. Runs
// alongside - and is entirely independent of - usePlayback's own plain
// <audio> element; this owns a second, parallel audio graph rather than
// touching that one at all.
//
// Every region switch is the same equal-power crossfade centred on the
// paragraph boundary (see CROSSFADE_HALF_SECONDS), regardless of the
// region's own Transition - that only affects how the backend seeds the
// region's generation (store.MusicTransition), not playback.
export function useBackgroundMusic({
  bookId,
  chapterIdx,
  chapterParagraphCount,
  totalChapters,
  paragraphIdx,
  paragraphCurrentTime,
  paragraphDuration,
  musicEnabled,
  isPlaying,
  playbackRate,
}: UseBackgroundMusicArgs) {
  const { data } = useChapterMusic(bookId, chapterIdx, musicEnabled)
  // Fetched purely so the pre-roll check below can look one region past
  // this chapter's own last one, into the very next chapter's first
  // region - a chapter's own regions never actually span into the next
  // one (see backend store.MusicRegion's own doc comment: scoring always
  // partitions one chapter's paragraphs), so without this, a chapter
  // boundary always played through a beat of silence (or an abrupt cold
  // start) instead of crossfading the way an ordinary in-chapter region
  // switch already does. `enabled` also requires a next chapter to
  // actually exist - the book's last chapter has none to fetch.
  const { data: nextChapterData } = useChapterMusic(bookId, chapterIdx + 1, musicEnabled && chapterIdx + 1 < totalChapters)

  const ctxRef = useRef<AudioContext | null>(null)
  const masterGainRef = useRef<GainNode | null>(null)
  const activeDeckRef = useRef<Deck | null>(null)
  const bufferCacheRef = useRef<Map<string, AudioBuffer>>(new Map())
  // Dedupes concurrent getBuffer calls for the same not-yet-cached region -
  // the pre-roll prefetch below re-checks bufferCacheRef on every
  // paragraphCurrentTime tick (several times a second) until a region's
  // buffer actually lands there, so without this, every one of those
  // ticks before the first fetch resolves would kick off its own
  // redundant fetch+decode of the same clip.
  const pendingFetchRef = useRef<Map<string, Promise<AudioBuffer>>>(new Map())
  const switchTokenRef = useRef(0)
  // Tracks whether the *current* (chapterIdx, paragraphIdx) was reached by
  // a jump (the reader clicking a word/paragraph, PlaybackController.
  // playAt) rather than natural sequential playback - see the pre-roll
  // guard in the switch effect below for why that distinction matters.
  // lastParagraphPosRef is only updated when paragraphIdx/chapterIdx
  // actually change, deliberately not on every run of that effect (which
  // also fires on every paragraphCurrentTime tick, many times a second,
  // for the pre-roll check itself) - otherwise a same-paragraph tick
  // would compare paragraphIdx against an already-advanced "expected
  // next" value left over from the *previous* tick and misread itself as
  // a jump. isJumpRef is likewise only ever written at that same moment,
  // so its value stays correct for every tick spent sitting on the
  // paragraph a jump (or a natural advance) landed on, until the next
  // real transition re-evaluates it.
  const lastParagraphPosRef = useRef<{ chapterIdx: number; paragraphIdx: number } | null>(null)
  const isJumpRef = useRef(false)

  function ensureContext(): AudioContext {
    let ctx = ctxRef.current
    if (!ctx) {
      ctx = new AudioContext()
      const master = ctx.createGain()
      // The master-volume fade effect below only reacts to musicEnabled
      // *changes*, and bails out early while masterGainRef.current is
      // still null (see its own guard) - so on this hook's very first
      // mount, that effect always runs before this lazy creation ever
      // happens (ensureContext is only called from the region-switch
      // effect, declared after it), finds no gain node yet, and does
      // nothing. If musicEnabled is already true at that point (the
      // ordinary case - the toggle was on before this chapter's first
      // ready region showed up) and never flips again, that effect never
      // gets a second chance to ramp this node up - it would otherwise
      // sit muted at 0 forever, with every other part of the pipeline
      // (fetch, decode, start) working and nothing audible. Starting it
      // at the correct target immediately, rather than always 0, closes
      // that gap.
      master.gain.value = musicEnabled ? MUSIC_GAIN : 0
      master.connect(ctx.destination)
      ctxRef.current = ctx
      masterGainRef.current = master
      // The play/pause effect below only reacts to isPlaying/musicEnabled
      // *changes* and is a no-op while ctxRef.current is still null (see
      // its own early return) - so a context created here, lazily, well
      // after narration already started (the common case: scoring/
      // generation catches up to a chapter mid-playback, long after the
      // reader's own initial "Play" gesture) would otherwise sit
      // permanently suspended with nothing left to ever resume it.
      // Resuming immediately if playback should already be audible closes
      // that gap; a no-op if the context happens to start already running.
      if (isPlaying && musicEnabled) void ctx.resume()
    }
    return ctx
  }

  // Fades deck out (equal-power) over fadeSeconds starting right now, then
  // stops it. Only ever acts once per deck (see Deck's own doc comment) -
  // its fadeOut stage has no prior automation to collide with.
  function stopDeck(deck: Deck, fadeSeconds: number) {
    const ctx = ctxRef.current
    if (!ctx || deck.stopping) return
    deck.stopping = true
    const now = ctx.currentTime
    if (fadeSeconds <= 0) {
      deck.source.stop(now)
      return
    }
    deck.fadeOut.gain.setValueCurveAtTime(FADE_OUT_CURVE, now, fadeSeconds)
    deck.source.stop(now + fadeSeconds + 0.05)
  }

  // Play/pause: suspend/resume the whole context so a source node's own
  // playback position freezes exactly where it was - AudioBufferSourceNode
  // has no native pause(), this is the standard workaround. Only matters
  // once a context actually exists (ensureContext runs lazily, from the
  // region-switch effect below, not on mount - most chapters never turn
  // music on at all).
  useEffect(() => {
    const ctx = ctxRef.current
    if (!ctx) return
    if (isPlaying && musicEnabled) void ctx.resume()
    else void ctx.suspend()
  }, [isPlaying, musicEnabled])

  // Live speed changes: the region-switch effect below only sets a new
  // source's own playbackRate once, at creation time - a rate change
  // while that same source is still the active deck (the reader adjusts
  // narration's own speed mid-paragraph, not at a region switch) would
  // otherwise never reach it. AudioBufferSourceNode.playbackRate is a
  // live AudioParam, safe to update on an already-started source.
  useEffect(() => {
    activeDeckRef.current?.source.playbackRate.setValueAtTime(playbackRate, ctxRef.current?.currentTime ?? 0)
  }, [playbackRate])

  // Fades the master bus in/out when music availability changes (the
  // reader toggled it off, or this chapter has none at all) rather than an
  // abrupt cut - and fully stops the active deck once faded out, so an
  // hours-long reading session doesn't leave silently-muted source nodes
  // accumulating.
  useEffect(() => {
    const master = masterGainRef.current
    const ctx = ctxRef.current
    if (!master || !ctx) return
    const target = musicEnabled ? MUSIC_GAIN : 0
    master.gain.cancelScheduledValues(ctx.currentTime)
    master.gain.setValueAtTime(master.gain.value, ctx.currentTime)
    master.gain.linearRampToValueAtTime(target, ctx.currentTime + 0.8)
    if (!musicEnabled && activeDeckRef.current) {
      stopDeck(activeDeckRef.current, 0.8)
      activeDeckRef.current = null
    }
  }, [musicEnabled])

  // getBuffer fetches+decodes+caches region's own clip (or returns the
  // already-cached one synchronously-resolved) - factored out of the
  // switch effect below so the pre-roll prefetch can warm the cache
  // ahead of the actual switch decision without duplicating this. Dedupes
  // against an already-in-flight fetch for the same region (see
  // pendingFetchRef) rather than starting a fresh one every call.
  function getBuffer(ctx: AudioContext, region: NonNullable<typeof data>['regions'][number]): Promise<AudioBuffer> {
    const cached = bufferCacheRef.current.get(region.id)
    if (cached) return Promise.resolve(cached)
    const pending = pendingFetchRef.current.get(region.id)
    if (pending) return pending
    const promise = fetch(region.audioUrl!)
      .then((res) => res.arrayBuffer())
      .then((bytes) => ctx.decodeAudioData(bytes))
      .then((buffer) => {
        bufferCacheRef.current.set(region.id, buffer)
        pendingFetchRef.current.delete(region.id)
        return buffer
      })
      .catch((err) => {
        pendingFetchRef.current.delete(region.id)
        throw err
      })
    pendingFetchRef.current.set(region.id, promise)
    return promise
  }

  // The actual region-switch: which region should be playing right now,
  // and is it already what's playing.
  useEffect(() => {
    if (!musicEnabled || !data || data.regions.length === 0) return

    // Re-evaluate isJumpRef only on a genuine (chapterIdx, paragraphIdx)
    // transition (see lastParagraphPosRef's own doc comment) - the very
    // first run (lastParagraphPosRef still null) is never a jump, nothing
    // to have jumped from yet.
    const last = lastParagraphPosRef.current
    if (last === null) {
      isJumpRef.current = false
    } else if (last.chapterIdx !== chapterIdx || last.paragraphIdx !== paragraphIdx) {
      isJumpRef.current = !(last.chapterIdx === chapterIdx && paragraphIdx === last.paragraphIdx + 1)
    }
    lastParagraphPosRef.current = { chapterIdx, paragraphIdx }
    const isJump = isJumpRef.current

    const currentIdx = data.regions.findIndex(
      (r) => paragraphIdx >= r.startIdx && paragraphIdx <= r.endIdx && r.status === 'ready' && r.audioUrl,
    )

    // Pre-roll: if the very next paragraph is where a new region begins,
    // start warming its clip into the cache the moment we enter the
    // paragraph right before it, and switch to it once the current
    // paragraph has CROSSFADE_HALF_SECONDS of real time left - so the
    // crossfade (see CROSSFADE_HALF_SECONDS) is centred on the boundary.
    // paragraphCurrentTime/paragraphDuration are narration media time,
    // hence the division by playbackRate; the crossfade itself runs on the
    // AudioContext's own real-time clock.
    //
    // The "next region" isn't always in this same chapter's own `data` -
    // if paragraphIdx is the chapter's actual last paragraph (not merely
    // the last one some region happens to cover - an unscored trailing
    // paragraph must never trigger this early), the upcoming region is
    // instead nextChapterData's own first region.
    //
    // Skipped entirely on a jump: paragraphCurrentTime/paragraphDuration
    // describe whatever paragraph was playing *before* the jump for at
    // least one tick (PlaybackController.playAt resets currentTime but
    // the new paragraph's own duration can lag a beat behind, and even
    // once both are fresh they say nothing about a position the reader
    // didn't arrive at by simply reading through it). A jump always
    // resolves to whichever region strictly covers where the reader
    // actually clicked (currentIdx), immediately.
    let region: MusicRegion | null = currentIdx === -1 ? null : data.regions[currentIdx]
    if (!isJump) {
      const isLastParagraphOfChapter = chapterParagraphCount > 0 && paragraphIdx === chapterParagraphCount - 1
      const upcoming =
        data.regions.find((r) => r.startIdx === paragraphIdx + 1 && r.status === 'ready' && r.audioUrl) ??
        (isLastParagraphOfChapter ? nextChapterData?.regions.find((r) => r.startIdx === 0 && r.status === 'ready' && r.audioUrl) : undefined)
      if (upcoming) {
        if (!bufferCacheRef.current.has(upcoming.id)) {
          void getBuffer(ensureContext(), upcoming)
        } else if (paragraphDuration > 0) {
          const remainingRealSeconds = (paragraphDuration - paragraphCurrentTime) / (playbackRate || 1)
          if (remainingRealSeconds <= CROSSFADE_HALF_SECONDS) {
            region = upcoming
          }
        }
      }
    }

    const outgoing = activeDeckRef.current

    if (!region) {
      // No ready region covers the current paragraph (scoring/generation
      // hasn't caught up yet, or narration has moved past whatever was
      // scored) - fade out whatever was playing rather than leaving it
      // running under the wrong paragraphs.
      if (outgoing) {
        stopDeck(outgoing, CROSSFADE_SECONDS)
        activeDeckRef.current = null
      }
      return
    }
    if (region.id === outgoing?.regionId) return

    const ctx = ensureContext()
    const token = ++switchTokenRef.current
    const incoming = region

    getBuffer(ctx, incoming)
      .then((buffer) => {
        if (switchTokenRef.current !== token) return // superseded by a later switch

        const fadeOut = ctx.createGain()
        fadeOut.connect(masterGainRef.current!)
        const fadeIn = ctx.createGain()
        fadeIn.connect(fadeOut)
        const source = ctx.createBufferSource()
        source.buffer = buffer
        // A region's paragraphs can outlast its own clip - musicgen renders
        // every clip with an already-crossfaded loop seam for exactly this
        // (see its trimToLoopableClip), so looping is gapless as-is. The
        // deck only ever ends via stopDeck.
        source.loop = true
        source.playbackRate.value = playbackRate
        source.connect(fadeIn)

        // Every switch crossfades, "continuation" included - see
        // CROSSFADE_HALF_SECONDS. A late switch (a jump, or the upcoming
        // clip's fetch not landing in time for pre-roll) runs the same
        // full-length crossfade, just starting from now.
        const now = ctx.currentTime
        fadeIn.gain.setValueCurveAtTime(FADE_IN_CURVE, now, CROSSFADE_SECONDS)
        if (outgoing) stopDeck(outgoing, CROSSFADE_SECONDS)
        source.start(now)
        activeDeckRef.current = { regionId: incoming.id, fadeIn, fadeOut, source, stopping: false }
      })
      .catch(() => {
        // A failed fetch/decode just leaves whatever was playing before in
        // place (or silence) - background music is a nicety, never worth
        // surfacing an error to the reader over.
      })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [musicEnabled, data, nextChapterData, chapterParagraphCount, paragraphIdx, paragraphCurrentTime, paragraphDuration, playbackRate])

  // Full teardown on unmount (leaving the reader, or switching books).
  useEffect(() => {
    return () => {
      if (activeDeckRef.current) {
        try {
          activeDeckRef.current.source.stop()
        } catch {
          // Already stopped/ended - nothing to clean up.
        }
      }
      void ctxRef.current?.close()
    }
  }, [])
}
