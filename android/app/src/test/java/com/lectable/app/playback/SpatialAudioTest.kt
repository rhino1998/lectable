package com.lectable.app.playback

import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ParagraphDto
import java.nio.ByteBuffer
import java.nio.ByteOrder
import kotlin.math.PI
import kotlin.math.abs
import kotlin.math.log10
import kotlin.math.sin
import kotlin.math.sqrt
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class SpatialAudioTest {

    private fun chapter(vararg speakers: Pair<String, Int?>) = ChapterDetailDto(
        idx = 0,
        paragraphs = speakers.mapIndexed { idx, (speaker, number) -> ParagraphDto(idx = idx, speaker = speaker, speakerIndex = number) },
    )

    private val scene = chapter("" to null, "Narrator" to null, "Jonah" to null, "Castle" to null, "Mona" to null, "Castle" to null, "Frank" to null)

    // paragraphSeat

    @Test
    fun `narration and the first-person narrator have no seat`() {
        assertNull(paragraphSeat(scene, 0, "Jonah"))
        assertNull(paragraphSeat(scene, 1, "Jonah"))
        assertNull(paragraphSeat(scene, 2, "Jonah"))
        assertNull(paragraphSeat(scene, 99, "Jonah"))
        assertNull(paragraphSeat(null, 0, "Jonah"))
    }

    @Test
    fun `speakers are seated by first line, the first two on opposite sides`() {
        assertEquals(-1f, paragraphSeat(scene, 3, "Jonah"))
        assertEquals(1f, paragraphSeat(scene, 4, "Jonah"))
        assertEquals(-1f, paragraphSeat(scene, 5, "Jonah"))
        assertEquals(-0.5f, paragraphSeat(scene, 6, "Jonah"))
    }

    @Test
    fun `a third-person book's lead is seated like anyone else`() {
        assertEquals(-1f, paragraphSeat(scene, 2, ""))
        assertEquals(1f, paragraphSeat(scene, 3, ""))
    }

    @Test
    fun `numbered members of a role sit apart`() {
        val guards = chapter("Guard" to 1, "Guard" to 2, "Guard" to 1)
        assertEquals(-1f, paragraphSeat(guards, 0, ""))
        assertEquals(1f, paragraphSeat(guards, 1, ""))
        assertEquals(-1f, paragraphSeat(guards, 2, ""))
    }

    // SpatialMixer

    private val rate = 48_000

    /** Mixes [samples] at [pan] and returns the left and right channels. */
    private fun mix(samples: ShortArray, mode: SpatialMode, pan: Float?): Pair<FloatArray, FloatArray> {
        val input = ByteBuffer.allocate(samples.size * 2).order(ByteOrder.nativeOrder())
        samples.forEach(input::putShort)
        input.flip()
        val output = ByteBuffer.allocate(samples.size * 4).order(ByteOrder.nativeOrder())
        SpatialMixer(rate).mix(input, output, mode, pan)
        assertEquals(0, input.remaining())
        output.flip()
        val left = FloatArray(samples.size)
        val right = FloatArray(samples.size)
        for (i in samples.indices) {
            left[i] = output.short.toFloat()
            right[i] = output.short.toFloat()
        }
        return left to right
    }

    private fun tone(hz: Double, seconds: Double = 0.1) =
        ShortArray((rate * seconds).toInt()) { i -> (8000 * sin(2 * PI * hz * i / rate)).toInt().toShort() }

    private fun rms(samples: FloatArray): Double = sqrt(samples.drop(rate / 100).sumOf { (it * it).toDouble() } / (samples.size - rate / 100))

    private fun db(a: FloatArray, b: FloatArray) = 20 * log10(rms(a) / rms(b))

    @Test
    fun `off and an unplaced voice go to both ears unchanged`() {
        val input = tone(440.0)
        for ((mode, pan) in listOf(SpatialMode.OFF to 0.5f, SpatialMode.OFF to null, SpatialMode.HEADPHONES to null)) {
            val (left, right) = mix(input, mode, pan)
            for (i in input.indices) {
                assertEquals(input[i].toFloat(), left[i])
                assertEquals(input[i].toFloat(), right[i])
            }
        }
    }

    @Test
    fun `stereo is an equal-power pan`() {
        val input = tone(440.0)
        val (centreLeft, centreRight) = mix(input, SpatialMode.STEREO, null)
        assertEquals(0.0, db(centreLeft, centreRight), 0.01)
        val (left, right) = mix(input, SpatialMode.STEREO, -0.5f)
        assertEquals(7.66, db(left, right), 0.05)
        val power = rms(left) * rms(left) + rms(right) * rms(right)
        val centrePower = 2 * rms(centreLeft) * rms(centreLeft)
        assertEquals(1.0, power / centrePower, 0.01)
        val (hardLeft, hardRight) = mix(input, SpatialMode.STEREO, 1f)
        assertTrue(rms(hardLeft) < 1.0)
        assertTrue(rms(hardRight) > 5000)
    }

    @Test
    fun `headphones delays the far ear`() {
        val click = ShortArray(256).also { it[10] = 10_000 }
        val (left, right) = mix(click, SpatialMode.HEADPHONES, -0.5f)
        val firstLeft = left.indexOfFirst { abs(it) > 50 }
        val firstRight = right.indexOfFirst { abs(it) > 50 }
        assertEquals(10, firstLeft)
        // 45 degrees: 0.0875m / 343m/s * (pi/4 + sin(pi/4)) = 381 microseconds = 18.3 samples.
        assertEquals(18, firstRight - firstLeft)
        val (mirrorLeft, mirrorRight) = mix(click, SpatialMode.HEADPHONES, 0.5f)
        assertEquals(18, mirrorLeft.indexOfFirst { abs(it) > 50 } - mirrorRight.indexOfFirst { abs(it) > 50 })
    }

    @Test
    fun `headphones shadows the far ear at high frequencies only`() {
        val (lowLeft, lowRight) = mix(tone(100.0), SpatialMode.HEADPHONES, -0.5f)
        assertEquals(0.0, db(lowLeft, lowRight), 1.0)
        val (highLeft, highRight) = mix(tone(8000.0), SpatialMode.HEADPHONES, -0.5f)
        assertTrue("near ear louder by ${db(highLeft, highRight)} dB", db(highLeft, highRight) > 12.0)
        // The near ear is brighter than the source, the far ear duller.
        val (plain, _) = mix(tone(8000.0), SpatialMode.OFF, null)
        assertTrue(db(highLeft, plain) in 2.0..6.0)
        assertTrue(db(highRight, plain) < -9.0)
    }

    @Test
    fun `width zero puts a seated voice in front`() {
        val (left, right) = mix(tone(2000.0), SpatialMode.HEADPHONES, 0f)
        assertEquals(0.0, db(left, right), 0.01)
    }

    @Test
    fun `a loud voice is clamped, not wrapped`() {
        val loud = ShortArray(2000) { i -> (32_000 * sin(2 * PI * 8000.0 * i / rate)).toInt().toShort() }
        val (left, _) = mix(loud, SpatialMode.HEADPHONES, -1f)
        assertTrue(left.all { it in -32768f..32767f })
        assertTrue(left.max() == 32767f)
    }
}
