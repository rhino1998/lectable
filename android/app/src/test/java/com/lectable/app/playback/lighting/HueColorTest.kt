package com.lectable.app.playback.lighting

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class HueColorTest {
    @Test
    fun parsesHex() {
        assertEquals(Rgb(1.0, 0.0, 0.0), parseHex("#ff0000"))
        assertEquals(Rgb(0.0, 0.0, 1.0), parseHex("0000FF"))
        assertNull(parseHex("orange"))
        assertNull(parseHex(null))
    }

    @Test
    fun convertsPrimariesToXy() {
        // Philips' Wide RGB D65 primaries.
        assertEquals(listOf(0.7006, 0.2993), rgbToXy(Rgb(1.0, 0.0, 0.0)))
        assertEquals(listOf(0.3127, 0.329), rgbToXy(Rgb(0.0, 0.0, 0.0)))
    }

    @Test
    fun scalesBrightnessByCap() {
        assertEquals(254, hueBrightness(100.0, 1f))
        assertEquals(128, hueBrightness(50.0, 1f))
        assertEquals(1, hueBrightness(0.0, 1f))
        assertEquals(hueBrightness(25.0, 1f), hueBrightness(50.0, 0.5f))
    }
}
