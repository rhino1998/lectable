package com.lectable.app.playback

import android.content.Context
import android.os.SystemClock
import androidx.media3.common.AudioAttributes
import androidx.media3.common.C
import androidx.media3.common.MediaItem
import androidx.media3.common.PlaybackParameters
import androidx.media3.common.Player
import androidx.media3.exoplayer.ExoPlayer
import com.lectable.app.data.remote.MediaUrlResolver
import com.lectable.app.data.remote.dto.AudioStatuses
import com.lectable.app.data.remote.dto.ChapterMusicDto
import com.lectable.app.data.remote.dto.MusicRegionDto
import com.lectable.app.data.settings.DEFAULT_MUSIC_VOLUME
import com.lectable.app.data.settings.PlaybackSettingsRepository
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlin.math.PI
import kotlin.math.cos
import kotlin.math.sin
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

// Half the width of every region-to-region crossfade, in real seconds - mirrors
// useBackgroundMusic.ts's own CROSSFADE_HALF_SECONDS exactly, including why every switch is
// centred on the paragraph boundary that owns it:
//
//   BBBBBB(Ba)|(bA)AAAAAAAA
//
// the incoming region starts this long before the boundary (pre-roll, see tick) and fades in over
// twice this while the outgoing one fades out over the same window, so the two cross at equal
// level right at the boundary - never both at full volume, never a gap. Applies to "continuation"
// regions too (see that hook's own doc comment).
private const val CROSSFADE_HALF_SECONDS = 5f
private const val CROSSFADE_SECONDS = CROSSFADE_HALF_SECONDS * 2

// How long the *master* bus itself takes to fade in/out when the reader flips the book-wide
// background-music toggle on/off - a separate, shorter ramp from CROSSFADE_SECONDS above,
// mirroring useBackgroundMusic.ts's own master-gain effect (which hardcodes this same "+ 0.8").
// Kept as a separate multiplied gain stage from each Deck's own fade levels (see applyVolume), the
// same shape that hook's own Web Audio graph uses.
private const val MASTER_FADE_SECONDS = 0.8f

private const val TICK_MS = 250L
private const val FADE_STEP_MS = 40L

private class Deck(val player: ExoPlayer) {
    var regionId: String? = null
    // Two independent fade stages, each 0..1 *progress* (not gain - see applyVolume for the
    // equal-power shaping), mirroring useBackgroundMusic.ts's own per-deck fadeIn/fadeOut
    // GainNodes: a deck switched away from while still mid-fade-in keeps fading in underneath its
    // fade-out rather than having one shared level cancelled and re-anchored, and the two
    // simply multiply.
    var fadeInProgress: Float = 0f
    var fadeOutProgress: Float = 0f
    var fadeInJob: Job? = null
    var fadeOutJob: Job? = null
}

/**
 * Mixes a chapter's own generated background-music regions in underneath narration, as
 * [paragraphIndex][ParagraphPlayer] advances through them - the Android analogue of
 * frontend/src/hooks/useBackgroundMusic.ts. The backend only ever hands over per-region clips
 * (see [com.lectable.app.data.remote.LectableApi.getChapterMusic]), never a chapter-length
 * stitched track, so this owns the actual mixing client-side.
 *
 * Runs two independent [ExoPlayer] "decks" (mirroring that hook's two Web Audio
 * GainNode/AudioBufferSourceNode pairs) so every region switch can crossfade the outgoing clip
 * into the incoming one - see CROSSFADE_HALF_SECONDS.
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
    private val playbackSettingsRepository: PlaybackSettingsRepository,
) {
    private fun buildDeck(): Deck {
        val player = ExoPlayer.Builder(context)
            .setAudioAttributes(
                AudioAttributes.Builder()
                    .setUsage(C.USAGE_MEDIA)
                    .setContentType(C.AUDIO_CONTENT_TYPE_MUSIC)
                    .build(),
                // Background music never independently ducks/gets ducked by other apps - it
                // already sits well under narration's own volume as texture (musicVolume), and
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
    // Prefers a deck with nothing loaded at all over one still fading out a previous region -
    // only reuses (and so cuts off) a fading deck when a third region arrives while both are busy.
    private fun idleDeck(): Deck {
        val other = if (activeDeck === deckA) deckB else deckA
        return listOf(deckA, deckB).firstOrNull { it !== activeDeck && it.regionId == null } ?: other
    }

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

    // The reader's own chosen music volume (PlaybackSettingsRepository.musicVolume, 0..1) - the
    // final multiplied stage in applyVolume, applied live to both decks whenever it changes.
    private var musicVolume = DEFAULT_MUSIC_VOLUME

    // The master bus level (0f..1f, see applyVolume), ramped via
    // [fadeMasterLevel] whenever [setMusicEnabled] toggles - see MASTER_FADE_SECONDS's own doc
    // comment for why this is a separate stage from each Deck's own fade levels.
    private var masterLevel = 0f
    private var masterFadeJob: Job? = null

    // Tracks whether the *current* (chapterIdx, paragraphIdx) was reached by a jump (the reader
    // tapping a word/paragraph, or jumping chapters) rather than natural sequential playback -
    // mirrors useBackgroundMusic.ts's own isJumpRef/lastParagraphPosRef exactly, including why it
    // matters: pre-roll timing only makes sense measured against natural forward progress.
    private var lastPos: Pair<Int, Int>? = null
    private var isJump = false

    init {
        scope.launch {
            playbackSettingsRepository.musicVolume.collect { volume ->
                musicVolume = volume
                applyVolume(deckA)
                applyVolume(deckB)
            }
        }
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
     *  [MASTER_FADE_SECONDS] rather than snapping it, same as that hook's own master-gain effect. */
    fun setMusicEnabled(enabled: Boolean) {
        if (musicEnabled == enabled) return
        musicEnabled = enabled
        applyPlayPause()
        fadeMasterLevel(if (enabled) 1f else 0f)
        if (!enabled) {
            // applyPlayPause above has already paused both decks, so there's nothing audible left
            // to fade - stop them outright (including one still mid-crossfade-out, which would
            // otherwise sit paused with its fade frozen until music was re-enabled).
            activeDeck = null
            lastPos = null
            stopDeckNow(deckA)
            stopDeckNow(deckB)
        }
    }

    /** Applied to BOTH decks, not just [activeDeck] - pausing narration mid-crossfade must pause
     *  the outgoing deck too, not leave it playing out its tail alone. An unloaded deck has no
     *  media item, so playWhenReady on it is a no-op. */
    private fun applyPlayPause() {
        val play = isPlaying && musicEnabled
        deckA.player.playWhenReady = play
        deckB.player.playWhenReady = play
    }

    /** Applied to BOTH decks, not just [activeDeck] - a rate change mid-crossfade must still
     *  reach whichever deck is still playing out its own outgoing tail, not only the incoming one; setting it on an idle/stopped deck too is
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

        // Re-evaluate isJump only on a genuine (chapterIdx, paragraphIdx) transition, mirroring
        // useBackgroundMusic.ts's own isJumpRef - re-deriving it every tick would compare the
        // current paragraph against itself and misread every tick after a paragraph's first as a
        // jump, which silently disabled pre-roll below.
        val pos = chapterIdx to paragraphIdx
        val last = lastPos
        if (last == null) {
            isJump = false
        } else if (last != pos) {
            isJump = !(last.first == chapterIdx && paragraphIdx == last.second + 1)
        }
        lastPos = pos

        val regions = chapterMusic[chapterIdx]?.regions.orEmpty()
        var region = regions.firstOrNull {
            paragraphIdx in it.startIdx..it.endIdx && it.status == AudioStatuses.READY && it.audioUrl != null
        }

        // Pre-roll: switch into the *next* paragraph's own region once the current paragraph has
        // CROSSFADE_HALF_SECONDS of real time left, so the crossfade is centred on the boundary -
        // see useBackgroundMusic.ts's own pre-roll for the full reasoning, including why this is
        // skipped on a jump and why a chapter's actual last paragraph looks ahead into the *next*
        // chapter's own first region (read straight from [chapterMusic], already fetched for
        // every loaded chapter - see ReaderViewModel's own poll loop). Paragraph position/duration
        // are narration media time, hence the division by playbackRate.
        if (!isJump) {
            val chapterParagraphCount = chapterParagraphCounts[chapterIdx] ?: 0
            val isLastParagraphOfChapter = chapterParagraphCount > 0 && paragraphIdx == chapterParagraphCount - 1
            val upcoming = regions.firstOrNull { it.startIdx == paragraphIdx + 1 && it.status == AudioStatuses.READY && it.audioUrl != null }
                ?: if (isLastParagraphOfChapter) {
                    chapterMusic[chapterIdx + 1]?.regions.orEmpty()
                        .firstOrNull { it.startIdx == 0 && it.status == AudioStatuses.READY && it.audioUrl != null }
                } else {
                    null
                }
            if (upcoming != null) {
                val durationMs = paragraphPlayer.durationMs()
                if (durationMs > 0) {
                    val remainingRealMs = (durationMs - paragraphPlayer.positionMs()) / playbackRate.coerceAtLeast(0.01f)
                    if (remainingRealMs <= CROSSFADE_HALF_SECONDS * 1000) region = upcoming
                }
            }
        }

        val outgoing = activeDeck

        if (region == null) {
            // No ready region covers the current paragraph (scoring/generation hasn't caught up
            // yet, or narration has moved past whatever was scored) - fade out whatever was
            // playing rather than leaving it running under the wrong paragraphs.
            if (outgoing != null) {
                fadeOutAndStop(outgoing)
                activeDeck = null
            }
            return
        }
        if (region.id == outgoing?.regionId) return

        switchToDeck(region, outgoing)
    }

    /** Every switch is the same full-length crossfade (see CROSSFADE_HALF_SECONDS) - a late one (a
     *  jump) just starts it from now rather than centred on a boundary. */
    private fun switchToDeck(region: MusicRegionDto, outgoing: Deck?) {
        val uri = mediaUrlResolver.resolve(region.audioUrl) ?: return
        val next = idleDeck()
        next.fadeInJob?.cancel()
        next.fadeOutJob?.cancel()
        next.regionId = region.id
        next.fadeInProgress = 0f
        next.fadeOutProgress = 0f
        applyVolume(next)
        next.player.setMediaItem(MediaItem.fromUri(uri))
        // A region's paragraphs can outlast its own clip - backend musicgen renders every clip with
        // an already-crossfaded loop seam for exactly this, same as useBackgroundMusic.ts's own
        // source.loop. The deck only ever stops via fadeOutAndStop/stopDeckNow.
        next.player.repeatMode = Player.REPEAT_MODE_ONE
        next.player.playbackParameters = PlaybackParameters(playbackRate, playbackRate)
        next.player.prepare()
        next.player.playWhenReady = isPlaying && musicEnabled

        next.fadeInJob = scope.launch {
            runFade(next, CROSSFADE_SECONDS) { next.fadeInProgress = it }
        }
        outgoing?.let(::fadeOutAndStop)
        activeDeck = next
    }

    /** Fades [deck] out over CROSSFADE_SECONDS, then fully stops it. Its fade-in (if still running)
     *  is left alone - see [Deck]'s own doc comment. */
    private fun fadeOutAndStop(deck: Deck) {
        deck.fadeOutJob?.cancel()
        deck.fadeOutJob = scope.launch {
            runFade(deck, CROSSFADE_SECONDS) { deck.fadeOutProgress = maxOf(deck.fadeOutProgress, it) }
            stopDeckNow(deck)
        }
    }

    private fun stopDeckNow(deck: Deck) {
        deck.fadeInJob?.cancel()
        deck.fadeOutJob?.cancel()
        deck.player.stop()
        deck.player.clearMediaItems()
        deck.fadeInProgress = 0f
        deck.fadeOutProgress = 0f
        deck.player.volume = 0f
        deck.regionId = null
    }

    /** Drives one fade stage of [deck] from 0 to 1 over [seconds] of *audible* time: the fade clock
     *  only advances while [deck]'s own player is actually playing, so pausing narration (or the
     *  incoming clip still buffering) freezes the crossfade where it is - the Android analogue of
     *  Web Audio's suspended AudioContext freezing its own scheduled ramps. A player that errored
     *  or went idle can never resume, so the fade just completes. */
    private suspend fun runFade(deck: Deck, seconds: Float, setProgress: (Float) -> Unit) {
        val totalMs = seconds * 1000
        var elapsedMs = 0f
        var lastTick = SystemClock.elapsedRealtime()
        while (elapsedMs < totalMs) {
            delay(FADE_STEP_MS)
            val now = SystemClock.elapsedRealtime()
            if (deck.player.isPlaying) {
                elapsedMs += now - lastTick
            } else if (deck.player.playbackState == Player.STATE_IDLE || deck.player.playbackState == Player.STATE_ENDED) {
                elapsedMs = totalMs
            }
            lastTick = now
            setProgress((elapsedMs / totalMs).coerceAtMost(1f))
            applyVolume(deck)
        }
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

    /** The actual ExoPlayer volume applied to [deck]: its two fade stages shaped equal-power
     *  (sin/cos quarter-waves - a linear crossfade dips audibly at its midpoint, which is exactly
     *  the boundary it's centred on), times the master bus level, times the reader's own [musicVolume]. */
    private fun applyVolume(deck: Deck) {
        val inGain = sin(deck.fadeInProgress * PI / 2).toFloat()
        val outGain = cos(deck.fadeOutProgress * PI / 2).toFloat()
        deck.player.volume = (inGain * outGain * masterLevel * musicVolume).coerceIn(0f, 1f)
    }
}
