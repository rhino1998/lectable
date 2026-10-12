package com.lectable.app.playback

import com.lectable.app.data.remote.dto.ChapterDetailDto
import java.nio.ByteBuffer
import kotlin.math.PI
import kotlin.math.abs
import kotlin.math.cos
import kotlin.math.roundToInt
import kotlin.math.sin

/**
 * Speaker positions: each character is heard from a seat to the left or right of a centred
 * narrator - the Android twin of `frontend/src/utils/spatialAudio.ts`, with the same seating
 * rule ([paragraphSeat]) and spread. Clips stay mono and nothing comes from the backend; the
 * position is applied to the decoded audio as it plays ([SpatialAudioProcessor]). A
 * per-device setting ([com.lectable.app.data.settings.PlaybackSettingsRepository.spatialMode]).
 *
 * [HEADPHONES] is the web's "hrtf-head": binaural speakers around a narrator left unplaced,
 * so narration is heard inside the head. There is no twin of the web's plain "hrtf" (narrator
 * placed in front): this model has no cue that tells front from inside the head, see
 * [SpatialMixer].
 */
enum class SpatialMode(val key: String, val label: String) {
    OFF("off", "Off"),
    STEREO("stereo", "Left and right"),
    HEADPHONES("hrtf-head", "Headphones"),
    ;

    companion object {
        fun fromKey(key: String?): SpatialMode = entries.firstOrNull { it.key == key } ?: OFF
    }
}

/** How far the outermost seat sits from centre at width 1: a pan of 1 is hard left/right, and
 *  in [SpatialMode.HEADPHONES] 90 degrees, so 0.5 is 45 degrees off centre. */
const val SPATIAL_MAX_PAN = 0.5f

/** The width setting multiplies [SPATIAL_MAX_PAN]: 0 puts everyone in the centre,
 *  [MAX_SPATIAL_WIDTH] puts the outer seats hard left/right. */
const val DEFAULT_SPATIAL_WIDTH = 1f
const val MAX_SPATIAL_WIDTH = 1f / SPATIAL_MAX_PAN

// Seats in the order a chapter's speakers take them: the first two voices heard sit far apart
// on opposite sides, later ones fill in between.
private val SEATS = floatArrayOf(-1f, 1f, -0.5f, 0.5f, -0.75f, 0.75f, -0.25f, 0.25f)

private fun seatKey(speaker: String, speakerIndex: Int?): String =
    if (speakerIndex != null && speakerIndex != 0) "$speaker#$speakerIndex" else speaker

/**
 * The seat paragraph [idx] of [chapter] is heard from, -1 (outermost left) to 1, or null for a
 * line with no place in the room: narration, an unattributed line and [centerSpeaker] (a
 * first-person book's narrator). Anyone else has the seat they took by order of first line in
 * the chapter.
 */
internal fun paragraphSeat(chapter: ChapterDetailDto?, idx: Int, centerSpeaker: String): Float? {
    val target = chapter?.paragraphs?.getOrNull(idx) ?: return null
    fun centred(speaker: String?) = speaker.isNullOrEmpty() || speaker == "Narrator" || speaker == centerSpeaker
    if (centred(target.speaker)) return null
    val wanted = seatKey(target.speaker.orEmpty(), target.speakerIndex)
    val seen = mutableSetOf<String>()
    for (p in chapter.paragraphs) {
        if (centred(p.speaker)) continue
        val key = seatKey(p.speaker.orEmpty(), p.speakerIndex)
        if (key == wanted) break
        seen += key
    }
    return SEATS[seen.size % SEATS.size]
}

// A spherical head: its radius over the speed of sound, in seconds.
private const val HEAD_SECONDS = 0.0875f / 343f

// The head-shadow filter's lowest high-frequency gain, and the angle from an ear (radians)
// where it is reached - Brown and Duda's values.
private const val SHADOW_MIN = 0.1f
private const val SHADOW_MIN_ANGLE = (150.0 * PI / 180.0).toFloat()

// Longest interaural delay is under 0.7ms: 256 samples covers every rate up to 192kHz.
private const val HISTORY = 256

/**
 * Turns mono 16-bit PCM into stereo with the voice placed at a pan. Stereo mode is an
 * equal-power pan. Headphones mode is a spherical-head model (Brown and Duda, 1998): the far
 * ear hears the voice later (the interaural delay) and duller (a one-pole head-shadow filter
 * per ear), which is what places a sound to one side on headphones. It models no outer ear, so
 * unlike a measured HRTF it can't put a voice in front of or behind the listener.
 */
internal class SpatialMixer(private val sampleRate: Int) {
    private val history = FloatArray(HISTORY)
    private var writeIdx = 0
    private val left = Shadow()
    private val right = Shadow()

    private var mode = SpatialMode.OFF
    private var pan: Float? = null
    private var leftGain = 1f
    private var rightGain = 1f
    private var leftDelay = 0f
    private var rightDelay = 0f

    /** Reads all of [input] (mono) and writes twice as much to [output] (stereo), the voice at
     *  [pan] (-1 hard left to 1 hard right), or with no place of its own when null. */
    fun mix(input: ByteBuffer, output: ByteBuffer, mode: SpatialMode, pan: Float?) {
        if (mode != this.mode || pan != this.pan) place(mode, pan)
        val binaural = mode == SpatialMode.HEADPHONES && pan != null
        while (input.remaining() >= 2) {
            val x = input.short.toFloat()
            history[writeIdx] = x
            if (binaural) {
                output.putShort(pcm(left.step(tap(leftDelay))))
                output.putShort(pcm(right.step(tap(rightDelay))))
            } else {
                output.putShort(pcm(x * leftGain))
                output.putShort(pcm(x * rightGain))
            }
            writeIdx = (writeIdx + 1) and (HISTORY - 1)
        }
    }

    private fun place(mode: SpatialMode, pan: Float?) {
        this.mode = mode
        this.pan = pan
        leftGain = 1f
        rightGain = 1f
        val p = (pan ?: 0f).coerceIn(-1f, 1f)
        when (mode) {
            SpatialMode.OFF -> Unit
            SpatialMode.STEREO -> {
                val angle = (p + 1f) * (PI / 4).toFloat()
                leftGain = cos(angle)
                rightGain = sin(angle)
            }
            SpatialMode.HEADPHONES -> {
                if (pan == null) return
                val azimuth = p * (PI / 2).toFloat()
                val delay = HEAD_SECONDS * (abs(azimuth) + sin(abs(azimuth))) * sampleRate
                leftDelay = if (azimuth > 0f) delay else 0f
                rightDelay = if (azimuth < 0f) delay else 0f
                left.aim((PI / 2).toFloat() + azimuth, sampleRate)
                right.aim((PI / 2).toFloat() - azimuth, sampleRate)
            }
        }
    }

    /** The input [delay] samples ago, between samples by linear interpolation. */
    private fun tap(delay: Float): Float {
        val whole = delay.toInt()
        val part = delay - whole
        val a = history[(writeIdx - whole) and (HISTORY - 1)]
        if (part == 0f) return a
        val b = history[(writeIdx - whole - 1) and (HISTORY - 1)]
        return a + (b - a) * part
    }

    private fun pcm(sample: Float): Short = sample.roundToInt().coerceIn(Short.MIN_VALUE.toInt(), Short.MAX_VALUE.toInt()).toShort()

    /** One ear's head shadow: unity at low frequencies, rising to a gain of alpha at high ones
     *  - above 1 facing the source, well under 1 on the far side of the head. */
    private class Shadow {
        private var b0 = 1f
        private var b1 = 0f
        private var a1 = 0f
        private var x1 = 0f
        private var y1 = 0f

        /** [angle] is between the source and this ear's axis, radians. */
        fun aim(angle: Float, sampleRate: Int) {
            val alpha = (1f + SHADOW_MIN / 2f) + (1f - SHADOW_MIN / 2f) * cos(angle / SHADOW_MIN_ANGLE * PI.toFloat())
            val w0 = 1f / HEAD_SECONDS
            val fs = sampleRate.toFloat()
            b0 = (w0 + alpha * fs) / (w0 + fs)
            b1 = (w0 - alpha * fs) / (w0 + fs)
            a1 = (w0 - fs) / (w0 + fs)
        }

        fun step(x: Float): Float {
            val y = b0 * x + b1 * x1 - a1 * y1
            x1 = x
            y1 = y
            return y
        }
    }
}
