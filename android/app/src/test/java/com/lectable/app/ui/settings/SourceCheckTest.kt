package com.lectable.app.ui.settings

import org.junit.Assert.assertEquals
import org.junit.Test

class SourceCheckTest {
    @Test
    fun labelsTheOfferedIntervals() {
        assertEquals(
            listOf("Never", "Every 6 hours", "Every 12 hours", "Daily", "Every 2 days", "Weekly"),
            SOURCE_CHECK_HOURS.map(::sourceCheckLabel),
        )
    }

    @Test
    fun labelsAnyOtherIntervalInHours() {
        assertEquals("Every 36 hours", sourceCheckLabel(36))
        assertEquals("Every hour", sourceCheckLabel(1))
    }

    // A value set from elsewhere still shows in the menu, where it belongs by length.
    @Test
    fun choicesIncludeAnOddStoredValue() {
        assertEquals(SOURCE_CHECK_HOURS, sourceCheckChoices(null))
        assertEquals(SOURCE_CHECK_HOURS, sourceCheckChoices(24))
        assertEquals(SOURCE_CHECK_HOURS, sourceCheckChoices(-1))
        assertEquals(listOf(0, 6, 12, 24, 36, 48, 168), sourceCheckChoices(36))
        assertEquals(listOf(0, 6, 12, 24, 48, 168, 720), sourceCheckChoices(720))
    }
}
