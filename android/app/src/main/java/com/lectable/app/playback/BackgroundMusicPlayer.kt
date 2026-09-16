package com.lectable.app.playback

import android.content.Context
import androidx.media3.common.AudioAttributes
import androidx.media3.common.C
import androidx.media3.common.MediaItem
import androidx.media3.common.PlaybackParameters
import androidx.media3.exoplayer.ExoPlayer
import com.lectable.app.data.remote.MediaUrlResolver
import com.lectable.app.data.remote.dto.AudioStatus
import com.lectable.app.data.remote.dto.ChapterMusicDto
import com.lectable.app.data.remote.dto.MusicRegionDto
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

// Fixed, ducked background level - deliberately not a user-facing volume control yet (a first
// version of this feature, mirroring frontend/src/hooks/useBackgroundMusic.ts's own MUSIC_GAIN).
// Kept well under narration's own implicit "full volume" so music never competes with the words
// being read.
private const val MUSIC_GAIN = 0.22f

// How long a "cut" transition's own crossfade runs (one deck's own gain, fading the outgoing
// clip out while the incoming one fades in) - mirrors useBackgroundMusic.ts's own
// CUT_CROSSFADE_SECONDS exactly. A "continuation" transition needs none of this - see
// switchToDeck's own doc comment.
private const val CROSSFADE_SECONDS = 1.5f

// How long the *master* bus itself takes to fade in/out when the reader flips the book-wide
// background-music toggle on/off - a separate, shorter ramp from CROSSFADE_SECONDS above,
// mirroring useBackgroundMusic.ts's own master-gain effect (which hardcodes this same "+ 0.8" -
// see its own doc comment on why toggling availability gets its own fade instead of an abrupt
// cut). Kept as two independently-multiplied gain stages (masterLevel × Deck.ownLevel, see
// applyVolume) rather than one shared level, the same two-GainNode-stage shape that hook's own
// Web Audio graph uses (a deck's own gain feeding into one master gain) - collapsing the two into
// a single ramp would make a region crossfade that happens to overlap a toggle race against the
// wrong duration/target.
private const val MASTER_FADE_SECONDS = 0.8f

// How much of a region's own clip duration it overlaps its neighbor by, on both ends - mirrors
// useBackgroundMusic.ts's own REGION_OVERLAP_FRACTION exactly (renamed there from an earlier
// PRE_ROLL_FRACTION once post-roll below was added), used two ways:
//   - Pre-roll (see tick's own upcoming-region check): the *upcoming* region starts playing this
//     fraction of its own duration before the paragraph boundary that "owns" it actually arrives,
//     rather than only once paragraphIdx literally crosses into it - which would leave a "cut"
//     transition's own CROSSFADE_SECONDS fade (or a fresh region's cold start) only beginning
//     *after* that paragraph's narration had already started.
//   - Post-roll (see stopDeckAfter's own delaySeconds parameter): the *outgoing* region,
//     symmetrically, keeps playing at full volume this same fraction of its own duration past
//     that same boundary before its own fade-out even begins, rather than being cut off right at
//     the instant the next region takes over - a genuine overlap window with the incoming clip,
//     not just a same-instant crossfade.
private const val REGION_OVERLAP_FRACTION = 0.05

// Caps regionOverlapSeconds below - a very long region shouldn't overlap its neighbor by more
// than this many real seconds even though REGION_OVERLAP_FRACTION alone would ask for more; an
// overlap this long is no longer "smoothing a transition", it's two clips genuinely both playing
// at once for an awkwardly long stretch. Mirrors useBackgroundMusic.ts's own MAX_OVERLAP_SECONDS.
private const val MAX_OVERLAP_SECONDS = 5.0

// The actual pre-roll/post-roll overlap for a region of [durationSeconds] - REGION_OVERLAP_FRACTION
// of its own length, capped at MAX_OVERLAP_SECONDS, whichever is lower. Shared by both call sites
// (tick's own pre-roll check and outgoingPostRollSeconds) so the two stay in sync by construction
// rather than each re-deriving it.
private fun regionOverlapSeconds(durationSeconds: Double): Double = minOf(durationSeconds * REGION_OVERLAP_FRACTION, MAX_OVERLAP_SECONDS)

private const val TICK_MS = 250L
private const val FADE_STEP_MS = 40L

private class Deck(val player: ExoPlayer) {
    var regionId: String? = null
    var fadeJob: Job? = null
    // This deck's own crossfade progress, 0..1 - independent of [BackgroundMusicPlayer
    // .masterLevel]; the two multiply together (see applyVolume) to get the actual ExoPlayer
    // volume, mirroring how useBackgroundMusic.ts's per-deck GainNode feeds into one master
    // GainNode downstream rather than driving the AudioContext's destination directly.
    var ownLevel: Float = 0f
    // The region currently loaded into this deck's own [MusicRegionDto.durationSeconds] - used
    // purely to compute this deck's own post-roll delay when it becomes the *outgoing* deck of a
    // later switch (see stopDeckAfter). Server-reported, not measured locally: unlike
    // useBackgroundMusic.ts (which reads its already-decoded AudioBuffer.duration directly),
    // ExoPlayer has no synchronous local duration until well after prepare() - close enough for a
    // fade-timing fraction that only needs to be roughly right.
    var durationSeconds: Double = 0.0
}

/**
 * Mixes a chapter's own generated background-music regions in underneath narration, as
 * [paragraphIndex][ParagraphPlayer] advances through them - the Android analogue of
 * frontend/src/hooks/useBackgroundMusic.ts. The backend only ever hands over per-region clips
 * (see [com.lectable.app.data.remote.LectableApi.getChapterMusic]), never a chapter-length
 * stitched track, so this owns the actual mixing client-side.
 *
 * Runs two independent [ExoPlayer] "decks" (mirroring that hook's two Web Audio
 * GainNode/AudioBufferSourceNode pairs) so a region switch can crossfade the outgoing clip into
 * the incoming one instead of cutting abruptly - only a genuine "continuation" region (its own
 * first chunk generated seeded from the immediately preceding region's own tail, see backend
 * store.MusicTransition) skips the fade for a hard, instant cut, since the seeded generation is
 * what already makes it sound continuous.
 *
 * App-scoped ([Singleton]), like [ParagraphPlayer] - owns its own coroutine scope/tick loop
 * entirely independent of any screen's lifecycle, observing [paragraphPlayer]'s own
 * [ParagraphPlayer.state]/[ParagraphPlayer.positionMs]/[ParagraphPlayer.durationMs] directly
 * rather than requiring a ViewModel to push position updates every tick - the same "app-scoped,
 * self-contained" shape [ParagraphPlayer] itself already has.
 */
@Singleton
class BackgroundMusicPlayer @Inject constructor(
    @ApplicationContext private val context: Context,
    private val mediaUrlResolver: MediaUrlResolver,
    private val paragraphPlayer: ParagraphPlayer,
) {
    private fun buildDeck(): Deck {
        val player = ExoPlayer.Builder(context)
            .setAudioAttributes(
                AudioAttributes.Builder()
                    .setUsage(C.USAGE_MEDIA)
                    .setContentType(C.AUDIO_CONTENT_TYPE_MUSIC)
                    .build(),
                // Background music never independently ducks/gets ducked by other apps - it
                // already sits well under narration's own volume as texture (MUSIC_GAIN), and
                // [ParagraphPlayer]'s own narration player is what actually owns audio-focus
                // duck/pause behavior for this app.
                false,
            )
            .build()
        player.volume = 0f
        return player.let(::Deck)
    }

    private val deckA = buildDeck()
    private val deckB = buildDeck()
    private var activeDeck: Deck? = null
    private fun idleDeck(): Deck = if (activeDeck === deckA) deckB else deckA

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)

    // chapterIdx -> its own latest-known regions, fed by ReaderViewModel as its own
    // ChapterMusicDto polling lands - see ReaderViewModel.refreshChapterMusic.
    private val chapterMusic = mutableMapOf<Int, ChapterMusicDto>()
    // chapterIdx -> its own real paragraph count, fed by ReaderViewModel.fetchAndMergeChapter
    // unconditionally (same as ParagraphPlayer.setChapter) - purely so tick()'s pre-roll check can
    // tell "paragraphIdx is this chapter's actual last paragraph" apart from merely "the last
    // paragraph some region happens to cover" (an unscored trailing paragraph must never trigger
    // an early cross-chapter switch - see setChapterParagraphCount's own doc comment).
    private val chapterParagraphCounts = mutableMapOf<Int, Int>()
    private var musicEnabled = false
    private var isPlaying = false
    private var playbackRate = 1f

    // The master bus level (0f..MUSIC_GAIN, not 0f..1f - see applyVolume), ramped via
    // [fadeMasterLevel] whenever [setMusicEnabled] toggles - see MASTER_FADE_SECONDS's own doc
    // comment for why this is a separate stage from each Deck's own ownLevel.
    private var masterLevel = 0f
    private var masterFadeJob: Job? = null

    // Tracks whether the *current* (chapterIdx, paragraphIdx) was reached by a jump (the reader
    // tapping a word/paragraph, or jumping chapters) rather than natural sequential playback -
    // mirrors useBackgroundMusic.ts's own isJumpRef/lastParagraphPosRef exactly, including why it
    // matters: pre-roll timing and "is this a genuine adjacent hand-off" (for a seamless
    // continuation) both only make sense measured against natural forward progress.
    private var lastPos: Pair<Int, Int>? = null
    private var isJump = false

    init {
        scope.launch {
            paragraphPlayer.state.collect { s ->
                val wasPlaying = isPlaying
                isPlaying = s.isPlaying
                playbackRate = s.playbackSpeed
                if (isPlaying != wasPlaying) applyPlayPause()
                applyPlaybackRate()
            }
        }
        scope.launch {
            while (isActive) {
                delay(TICK_MS)
                if (musicEnabled) tick()
            }
        }
    }

    /** Call whenever a chapter's own background-music regions load or refresh (polling, or a
     *  reader's own "Regenerate" tap) - see ReaderViewModel.refreshChapterMusic. */
    fun setChapterMusic(chapterIdx: Int, music: ChapterMusicDto) {
        chapterMusic[chapterIdx] = music
    }

    /** Call whenever a chapter's real paragraph count becomes known (a chapter load/refresh) -
     *  see [chapterParagraphCounts]'s own doc comment for why tick() needs this at all, distinct
     *  from [chapterMusic] (whose regions may not yet cover every paragraph, or any). */
    fun setChapterParagraphCount(chapterIdx: Int, count: Int) {
        chapterParagraphCounts[chapterIdx] = count
    }

    /** Call whenever the book's own background-music toggle (VoiceSettingsDto.musicEnabled) is
     *  known or changes - book-wide, not gated on any chapter actually being loaded, mirroring
     *  useBackgroundMusic.ts's own musicEnabled prop. Fades the master bus in/out over
     *  [MASTER_FADE_SECONDS] rather than snapping it, same as that hook's own master-gain effect
     *  - and, when disabling, stops whatever deck was active once that fade actually finishes
     *  (not immediately), so the toggle audibly fades to silence instead of cutting off mid-note. */
    fun setMusicEnabled(enabled: Boolean) {
        if (musicEnabled == enabled) return
        musicEnabled = enabled
        applyPlayPause()
        fadeMasterLevel(if (enabled) MUSIC_GAIN else 0f)
        if (!enabled) {
            val deck = activeDeck
            activeDeck = null
            lastPos = null
            if (deck != null) {
                scope.launch {
                    delay((MASTER_FADE_SECONDS * 1000).toLong())
                    stopDeckNow(deck)
                }
            }
        }
    }

    private fun applyPlayPause() {
        activeDeck?.player?.playWhenReady = isPlaying && musicEnabled
    }

    /** Applied to BOTH decks, not just [activeDeck] - a rate change mid-crossfade (or mid-post-
     *  roll, see [stopDeckAfter]) must still reach whichever deck is still playing out its own
     *  outgoing tail, not only the incoming one; setting it on an idle/stopped deck too is
     *  harmless. */
    private fun applyPlaybackRate() {
        // A plain resample (pitch shifts with speed, like Web Audio's own
        // AudioBufferSourceNode.playbackRate), deliberately NOT pitch-preserving the way
        // [ParagraphPlayer]'s own narration speed control is - see useBackgroundMusic.ts's own
        // playbackRate doc comment for why: a voice cloned from a slow/brisk reference
        // legitimately needs pitch preservation to sound natural, but background music plays back
        // well under narration's own volume as texture, not something a reader listens to
        // critically - a minor, deliberately-accepted pitch shift at non-1x speeds.
        val params = PlaybackParameters(playbackRate, playbackRate)
        deckA.player.playbackParameters = params
        deckB.player.playbackParameters = params
    }

    private fun tick() {
        val playback = paragraphPlayer.state.value
        val chapterIdx = playback.chapterIdx
        val paragraphIdx = playback.paragraphIdx
        if (chapterIdx < 0) return

        val last = lastPos
        isJump = if (last == null) false else !(last.first == chapterIdx && paragraphIdx == last.second + 1)
        lastPos = chapterIdx to paragraphIdx

        val regions = chapterMusic[chapterIdx]?.regions.orEmpty()
        val currentIdx = regions.indexOfFirst {
            paragraphIdx in it.startIdx..it.endIdx && it.status == AudioStatus.READY && it.audioUrl != null
        }

        // targetRegions/targetRegion track which chapter's own regions array the eventual switch
        // target actually belongs to - not always this chapter's own `regions` (see the
        // cross-chapter pre-roll below), so [immediatelyPrecedingId] below can still correctly
        // look up "the region right before this one" from the *right* array.
        var targetRegions = regions
        var targetRegion = regions.getOrNull(currentIdx)

        // Pre-roll: warm/switch into the *next* paragraph's own region slightly before playback
        // actually crosses into it, so its own fade/start is already underway by the time it
        // needs to be audible - see REGION_OVERLAP_FRACTION's own doc comment. Skipped entirely
        // on a jump, same reasoning as useBackgroundMusic.ts's own pre-roll guard: the
        // currently-loaded clip's position/duration describe wherever the reader landed, not a
        // natural read-through of the region leading up to it, so comparing them against an
        // upcoming region's own duration would be comparing unrelated numbers.
        //
        // The "next region" isn't always in this same chapter's own `regions` - if paragraphIdx is
        // this chapter's actual last paragraph (not merely the last one some region happens to
        // cover - chapterParagraphCounts is what tells the two apart; an unscored trailing
        // paragraph must never trigger this early), the upcoming region is instead the *next*
        // chapter's own first region, read straight from [chapterMusic] (already fetched for every
        // loaded chapter, not just the currently-playing one - see ReaderViewModel's own poll
        // loop). A region's own Transition never actually spans chapters in practice (server-side
        // scoring has no cross-chapter context to seed a "continuation" from), so a cross-chapter
        // switch always falls back to an ordinary crossfade below rather than the seamless cut -
        // no special-casing needed for that, it falls out of immediatelyPrecedingId naturally
        // being null for a chapter's own first region (targetRegions.indexOf(region) == 0 there).
        if (!isJump) {
            val upcomingIdx = regions.indexOfFirst { it.startIdx == paragraphIdx + 1 && it.status == AudioStatus.READY && it.audioUrl != null }
            val chapterParagraphCount = chapterParagraphCounts[chapterIdx] ?: 0
            val isLastParagraphOfChapter = chapterParagraphCount > 0 && paragraphIdx == chapterParagraphCount - 1
            var upcomingRegions: List<MusicRegionDto>? = null
            var upcoming: MusicRegionDto? = null
            if (upcomingIdx != -1) {
                upcomingRegions = regions
                upcoming = regions[upcomingIdx]
            } else if (isLastParagraphOfChapter) {
                val nextRegions = chapterMusic[chapterIdx + 1]?.regions.orEmpty()
                val first = nextRegions.firstOrNull { it.startIdx == 0 && it.status == AudioStatus.READY && it.audioUrl != null }
                if (first != null) {
                    upcomingRegions = nextRegions
                    upcoming = first
                }
            }
            if (upcoming != null && upcomingRegions != null) {
                val durationMs = paragraphPlayer.durationMs()
                if (durationMs > 0 && upcoming.durationSeconds > 0) {
                    val remainingMs = durationMs - paragraphPlayer.positionMs()
                    if (remainingMs <= regionOverlapSeconds(upcoming.durationSeconds) * 1000) {
                        targetRegions = upcomingRegions
                        targetRegion = upcoming
                    }
                }
            }
        }

        val region = targetRegion
        val outgoing = activeDeck
        // Post-roll (see regionOverlapSeconds/stopDeckAfter's own delaySeconds): how long outgoing
        // itself keeps playing, past this switch, before its own fade-out even starts - the same
        // overlap the incoming region above got started early by. 0 if outgoing was never
        // actually given a region (nothing to delay).
        val outgoingPostRollSeconds = if (outgoing != null && outgoing.durationSeconds > 0) {
            regionOverlapSeconds(outgoing.durationSeconds).toFloat()
        } else {
            0f
        }

        if (region == null) {
            // No ready region covers the current paragraph (scoring/generation hasn't caught up
            // yet, or narration has moved past whatever was scored) - fade out whatever was
            // playing rather than leaving it running under the wrong paragraphs.
            if (outgoing != null) {
                stopDeckAfter(outgoing, outgoingPostRollSeconds, CROSSFADE_SECONDS)
                activeDeck = null
            }
            return
        }
        if (region.id == outgoing?.regionId) return

        // Only a genuine adjacent hand-off (what's currently playing is literally the region
        // right before this one, in the SAME chapter's own regions array) counts as a real
        // continuation - see useBackgroundMusic.ts's own seamless computation for the full
        // reasoning (a jump landing on a "continuation"-tagged region has no seeded relationship
        // to whatever's on the other deck, so it still falls back to a crossfade).
        val targetOwnIdx = targetRegions.indexOf(region)
        val immediatelyPrecedingId = if (targetOwnIdx > 0) targetRegions[targetOwnIdx - 1].id else null
        val seamless = !isJump && region.transition == "continuation" && outgoing?.regionId == immediatelyPrecedingId
        switchToDeck(region, outgoing, seamless, outgoingPostRollSeconds)
    }

    /** [outgoingPostRollSeconds] is [outgoing]'s own post-roll delay (see REGION_OVERLAP_FRACTION)
     *  - ignored entirely for a seamless continuation, which stops [outgoing] immediately with no
     *  delay/fade at all (see that branch's own doc comment for why). */
    private fun switchToDeck(region: MusicRegionDto, outgoing: Deck?, seamless: Boolean, outgoingPostRollSeconds: Float) {
        val uri = mediaUrlResolver.resolve(region.audioUrl) ?: return
        val next = idleDeck()
        next.fadeJob?.cancel()
        next.regionId = region.id
        next.durationSeconds = region.durationSeconds
        next.player.setMediaItem(MediaItem.fromUri(uri))
        next.player.playbackParameters = PlaybackParameters(playbackRate, playbackRate)
        next.player.prepare()
        next.player.playWhenReady = isPlaying && musicEnabled

        if (seamless) {
            next.ownLevel = 1f
            applyVolume(next)
            // No post-roll here, unlike the crossfade branch below: a seamless continuation's
            // whole point is that its first chunk was generated seeded from outgoing's own tail
            // specifically so the two can play back to back with an instant cut - letting
            // outgoing linger past that cut would mean genuinely playing both at once, an audible
            // doubling of content that was designed to already flow into itself, not two
            // independent clips that need the overlap to disguise a seam.
            outgoing?.let { stopDeckAfter(it, delaySeconds = 0f, fadeSeconds = 0f) }
        } else {
            next.ownLevel = 0f
            applyVolume(next)
            next.fadeJob = scope.launch { fadeOwnLevel(next, 0f, 1f, CROSSFADE_SECONDS) }
            outgoing?.let { stopDeckAfter(it, outgoingPostRollSeconds, CROSSFADE_SECONDS) }
        }
        activeDeck = next
    }

    /** Stops [deck] - holding it at its current volume for [delaySeconds] (the post-roll window,
     *  see REGION_OVERLAP_FRACTION) before its own [fadeSeconds]-long fade-out even begins, then
     *  fully stopping it - mirrors useBackgroundMusic.ts's own stopDeck(fadeSeconds, delaySeconds)
     *  exactly, including that an immediate stop ([fadeSeconds] <= 0) still respects
     *  [delaySeconds] rather than cutting off right away. */
    private fun stopDeckAfter(deck: Deck, delaySeconds: Float, fadeSeconds: Float) {
        deck.fadeJob?.cancel()
        deck.fadeJob = scope.launch {
            if (delaySeconds > 0f) delay((delaySeconds * 1000).toLong())
            if (fadeSeconds > 0f) fadeOwnLevel(deck, deck.ownLevel, 0f, fadeSeconds)
            stopDeckNow(deck)
        }
    }

    private fun stopDeckNow(deck: Deck) {
        deck.fadeJob?.cancel()
        deck.player.stop()
        deck.ownLevel = 0f
        deck.player.volume = 0f
        deck.regionId = null
        deck.durationSeconds = 0.0
    }

    /** Ramps [deck]'s own crossfade level (not the master bus - see applyVolume) linearly from
     *  [from] to [to] over [seconds], applying it to the deck's real ExoPlayer volume on every
     *  step so it stays correctly multiplied against whatever [masterLevel] is doing at the same
     *  time (the two ramps are entirely independent - see MASTER_FADE_SECONDS's own doc comment). */
    private suspend fun fadeOwnLevel(deck: Deck, from: Float, to: Float, seconds: Float) {
        val steps = ((seconds * 1000) / FADE_STEP_MS).toInt().coerceAtLeast(1)
        for (i in 1..steps) {
            deck.ownLevel = from + (to - from) * (i.toFloat() / steps)
            applyVolume(deck)
            delay(FADE_STEP_MS)
        }
        deck.ownLevel = to
        applyVolume(deck)
    }

    /** Ramps the master bus level linearly to [target] over [MASTER_FADE_SECONDS], re-applying it
     *  to both decks on every step (see applyVolume) - whichever is actually active picks up the
     *  change live; applying it to the idle one too is harmless. */
    private fun fadeMasterLevel(target: Float) {
        masterFadeJob?.cancel()
        val from = masterLevel
        masterFadeJob = scope.launch {
            val steps = ((MASTER_FADE_SECONDS * 1000) / FADE_STEP_MS).toInt().coerceAtLeast(1)
            for (i in 1..steps) {
                masterLevel = from + (target - from) * (i.toFloat() / steps)
                applyVolume(deckA)
                applyVolume(deckB)
                delay(FADE_STEP_MS)
            }
            masterLevel = target
            applyVolume(deckA)
            applyVolume(deckB)
        }
    }

    /** The actual ExoPlayer volume applied to [deck] is always its own crossfade level times the
     *  master bus level - see Deck.ownLevel's own doc comment for why these are two independent
     *  multiplied stages rather than one shared value. */
    private fun applyVolume(deck: Deck) {
        deck.player.volume = (deck.ownLevel * masterLevel).coerceIn(0f, MUSIC_GAIN)
    }
}
