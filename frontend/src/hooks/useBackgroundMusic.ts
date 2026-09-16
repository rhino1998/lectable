import { useEffect, useRef } from 'react'
import { useChapterMusic } from '../api/queries'
import type { MusicRegion } from '../api/types'

// Fixed, ducked background level - deliberately not a user-facing volume
// control yet (a first version of this feature - see backend
// store.MusicRegion's own doc comment for the generation pipeline this
// plays back). Kept well under narration's own implicit "full volume" so
// music never competes with the words being read.
const MUSIC_GAIN = 0.22

// How long a "cut" transition's crossfade runs - short and deliberately
// unseeded (see internal/musicgen's own package doc comment): the two
// clips either side of a cut share no generation-time relationship, so a
// longer fade would just prolong an audible mismatch rather than smooth
// it. A "continuation" transition needs none of this at all - its first
// chunk was generated seeded from the previous region's own tail
// specifically so the two can play back to back with a hard, instant cut.
const CUT_CROSSFADE_SECONDS = 1.5

// How much of a region's own clip duration it overlaps its neighbor by,
// on both ends - used two ways:
//   - Pre-roll (see the region-switch effect below): the *upcoming*
//     region starts playing this fraction of its own duration before the
//     paragraph boundary that "owns" it actually arrives, rather than
//     only once paragraphIdx literally crosses into it (which left a
//     "cut" transition's own CUT_CROSSFADE_SECONDS fade, or a fresh
//     source.start for the chapter's very first region, beginning only
//     *after* that paragraph's narration had already started).
//   - Post-roll (see stopDeck's own delaySeconds parameter): the
//     *outgoing* region, symmetrically, keeps playing this same fraction
//     of its own duration past that same boundary before its own
//     fade-out even begins, rather than being cut off right at the
//     instant the next region takes over.
const REGION_OVERLAP_FRACTION = 0.05

// Caps regionOverlapSeconds below - a very long region (this app's clips
// aren't normally more than a minute or so, but nothing enforces that)
// shouldn't overlap its neighbor by more than this many real seconds even
// though REGION_OVERLAP_FRACTION alone would ask for more; an overlap
// this long is no longer "smoothing a transition", it's two clips
// genuinely both playing at once for an awkwardly long stretch.
const MAX_OVERLAP_SECONDS = 5

// The actual pre-roll/post-roll overlap for a region of `durationSeconds`
// - REGION_OVERLAP_FRACTION of its own length, capped at
// MAX_OVERLAP_SECONDS, whichever is lower. Shared by both call sites (the
// region-switch effect's own pre-roll check and its outgoingPostRoll) so
// the two stay in sync by construction rather than each re-deriving it.
function regionOverlapSeconds(durationSeconds: number): number {
  return Math.min(durationSeconds * REGION_OVERLAP_FRACTION, MAX_OVERLAP_SECONDS)
}

interface Deck {
  regionId: string
  gain: GainNode
  source: AudioBufferSourceNode
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
  // check (see REGION_OVERLAP_FRACTION): how close is playback to crossing
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
// A region's own Transition ("cut" vs "continuation" - see
// store.MusicTransition) decides how the switch into it sounds: a
// "continuation" region was generated with its first chunk seeded from
// the immediately preceding region's own tail (internal/musicgen), so
// switching into it is a hard, instant cut with no fade at all - the
// seeded generation is what already makes it sound continuous. A "cut"
// region (or any switch that isn't actually a genuine adjacent hand-off -
// e.g. the reader jumped/seeked past several regions at once) gets a
// short crossfade instead, since there's nothing tying the two clips
// together.
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

  // delaySeconds (default 0) is stopDeck's own "post-roll" - see
  // REGION_OVERLAP_FRACTION's own doc comment: rather than starting
  // deck's fade-out right now, it holds at its current gain until
  // now+delaySeconds and only starts fading (and, for the fadeSeconds<=0
  // case, only actually stops) from there - the exact mirror of the
  // region-switch effect's own pre-roll, which starts the *incoming*
  // region early by the same kind of amount. Callers pass 0 (the
  // ordinary case) for a switch that should behave exactly as before.
  function stopDeck(deck: Deck, fadeSeconds: number, delaySeconds = 0) {
    const ctx = ctxRef.current
    if (!ctx) return
    const now = ctx.currentTime
    const fadeStart = now + delaySeconds
    if (fadeSeconds <= 0) {
      deck.gain.gain.cancelScheduledValues(fadeStart)
      deck.gain.gain.setValueAtTime(0, fadeStart)
      deck.source.stop(fadeStart)
      return
    }
    deck.gain.gain.cancelScheduledValues(now)
    deck.gain.gain.setValueAtTime(deck.gain.gain.value, fadeStart)
    deck.gain.gain.linearRampToValueAtTime(0, fadeStart + fadeSeconds)
    deck.source.stop(fadeStart + fadeSeconds + 0.1)
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

    // Pre-roll: a region switch used to fire only once paragraphIdx
    // literally crossed into it - meaning a "cut" transition's own
    // CUT_CROSSFADE_SECONDS fade (or the chapter's very first region
    // starting cold) only ever began *after* the paragraph that owns it
    // had already started narrating. If the very next paragraph is where
    // a new region begins, start warming its clip into the cache the
    // moment we enter the paragraph right before it, and once that
    // clip's own duration is known, switch to it REGION_OVERLAP_FRACTION
    // of its own length before the current paragraph is actually due to
    // finish - so by the time narration crosses the real boundary, this
    // region's own fade/start is already well underway.
    //
    // The "next region" isn't always in this same chapter's own `data` -
    // if paragraphIdx is the chapter's actual last paragraph (not merely
    // the last one some region happens to cover - an unscored trailing
    // paragraph must never trigger this early), the upcoming region is
    // instead nextChapterData's own first region. A region's Transition
    // never actually spans chapters in practice (server-side scoring has
    // no cross-chapter context to seed a "continuation" from - each
    // chapter's own regions are scored independently), so a cross-chapter
    // switch always falls back to an ordinary crossfade below rather than
    // the seamless cut - no special-casing needed for that, it falls out
    // of immediatelyPrecedingId naturally being null for a chapter's own
    // first region (see below).
    //
    // Skipped entirely on a jump: paragraphCurrentTime/paragraphDuration
    // describe whatever paragraph was playing *before* the jump for at
    // least one tick (PlaybackController.playAt resets currentTime but
    // the new paragraph's own duration can lag a beat behind, and even
    // once both are fresh they say nothing about a position the reader
    // didn't arrive at by simply reading through it) - comparing them
    // against an upcoming region's own duration here would be comparing
    // unrelated numbers. A jump always resolves to whichever region
    // strictly covers where the reader actually clicked (currentIdx),
    // immediately - correct is more important than early for a jump the
    // way it isn't for the ordinary case pre-roll targets.
    let target: { region: MusicRegion; regions: MusicRegion[] } | null =
      currentIdx === -1 ? null : { region: data.regions[currentIdx], regions: data.regions }
    if (!isJump) {
      const upcomingInChapterIdx = data.regions.findIndex((r) => r.startIdx === paragraphIdx + 1 && r.status === 'ready' && r.audioUrl)
      const isLastParagraphOfChapter = chapterParagraphCount > 0 && paragraphIdx === chapterParagraphCount - 1
      const upcoming =
        upcomingInChapterIdx !== -1
          ? { region: data.regions[upcomingInChapterIdx], regions: data.regions }
          : isLastParagraphOfChapter && nextChapterData
            ? (() => {
                const first = nextChapterData.regions.find((r) => r.startIdx === 0 && r.status === 'ready' && r.audioUrl)
                return first ? { region: first, regions: nextChapterData.regions } : null
              })()
            : null
      if (upcoming) {
        const cachedUpcoming = bufferCacheRef.current.get(upcoming.region.id)
        if (!cachedUpcoming) {
          void getBuffer(ensureContext(), upcoming.region)
        } else if (paragraphDuration > 0) {
          const remaining = paragraphDuration - paragraphCurrentTime
          if (remaining <= regionOverlapSeconds(cachedUpcoming.duration)) {
            target = upcoming
          }
        }
      }
    }

    const region = target?.region ?? null
    const outgoing = activeDeckRef.current
    // Post-roll (see regionOverlapSeconds/stopDeck's own delaySeconds): how
    // long outgoing itself keeps playing, past this switch, before its own
    // fade-out even starts - the same overlap the incoming region above got
    // started early by. buffer is only unset if this deck was somehow
    // never actually started; 0 falls back to the old, immediate behavior.
    const outgoingPostRoll = outgoing?.source.buffer ? regionOverlapSeconds(outgoing.source.buffer.duration) : 0

    if (!region) {
      // No ready region covers the current paragraph (scoring/generation
      // hasn't caught up yet, or narration has moved past whatever was
      // scored) - fade out whatever was playing rather than leaving it
      // running under the wrong paragraphs.
      if (outgoing) {
        stopDeck(outgoing, CUT_CROSSFADE_SECONDS, outgoingPostRoll)
        activeDeckRef.current = null
      }
      return
    }
    if (region.id === outgoing?.regionId) return

    const ctx = ensureContext()
    const token = ++switchTokenRef.current
    // Only a genuine adjacent hand-off (what's currently playing is
    // literally the region right before this one, in the SAME chapter's
    // own regions array - see target's own doc comment above for why a
    // cross-chapter target's own "regions" is a different chapter's array
    // entirely, whose own indexOf(region) here is always 0) counts as a
    // real continuation - a region tagged "continuation" whose actual
    // predecessor isn't playing (the reader jumped/seeked past several
    // regions, nothing was playing at all, or this is a chapter's own
    // first region) has no seeded relationship to whatever's on the other
    // deck, so it falls back to a crossfade. Never seamless on a jump
    // even if outgoing happens to genuinely be the immediately-preceding
    // region by coincidence: the seeded continuation only sounds right
    // when the outgoing clip actually played through to near its own
    // tail first, which a jump - landing here from some arbitrary
    // elsewhere, not from reading through the preceding region - never
    // guarantees.
    // target is guaranteed non-null here (region was derived from it, and
    // the !region check above already returned if that were null).
    const targetOwnIdx = target!.regions.indexOf(region)
    const immediatelyPrecedingId = targetOwnIdx > 0 ? target!.regions[targetOwnIdx - 1].id : null
    const seamless = !isJump && region.transition === 'continuation' && outgoing?.regionId === immediatelyPrecedingId

    getBuffer(ctx, region)
      .then((buffer) => {
        if (switchTokenRef.current !== token) return // superseded by a later switch

        const gain = ctx.createGain()
        gain.connect(masterGainRef.current!)
        const source = ctx.createBufferSource()
        source.buffer = buffer
        source.playbackRate.value = playbackRate
        source.connect(gain)

        const now = ctx.currentTime
        if (seamless) {
          gain.gain.setValueAtTime(1, now)
          // No post-roll here, unlike the crossfade branch below: a
          // seamless continuation's whole point is that its first chunk
          // was generated seeded from outgoing's own tail specifically so
          // the two can play back to back with an instant cut - letting
          // outgoing linger past that cut would mean genuinely playing
          // both at once, an audible doubling of content that was
          // designed to already flow into itself, not two independent
          // clips that need the overlap to disguise a seam.
          if (outgoing) stopDeck(outgoing, 0)
        } else {
          gain.gain.setValueAtTime(0, now)
          gain.gain.linearRampToValueAtTime(1, now + CUT_CROSSFADE_SECONDS)
          if (outgoing) stopDeck(outgoing, CUT_CROSSFADE_SECONDS, outgoingPostRoll)
        }
        source.start(now)
        activeDeckRef.current = { regionId: region.id, gain, source }
      })
      .catch(() => {
        // A failed fetch/decode just leaves whatever was playing before in
        // place (or silence) - background music is a nicety, never worth
        // surfacing an error to the reader over.
      })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [musicEnabled, data, nextChapterData, chapterParagraphCount, paragraphIdx, paragraphCurrentTime, paragraphDuration])

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
