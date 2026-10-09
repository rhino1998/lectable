package com.lectable.app.ui.reader

import com.lectable.app.data.remote.dto.BookRecapDto
import com.lectable.app.data.remote.dto.RecapKinds
import com.lectable.app.data.remote.dto.RecapSegmentDto
import com.lectable.app.data.remote.dto.WordTimingDto
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class RecapTest {
    private val hour = 3600L
    private val day = 24 * 60 // minutes
    private val now = 1_000_000_000_000L // ms
    private val nowS = now / 1000

    private fun recap(
        kind: String = RecapKinds.POSITION,
        chapterIdx: Int = 4,
        paragraphIdx: Int = 10,
        throughIdx: Int = 50,
        text: String = "So far.",
    ) = BookRecapDto(kind = kind, chapterIdx = chapterIdx, paragraphIdx = paragraphIdx, throughIdx = throughIdx, title = "Book", text = text)

    @Test
    fun `current for the place it was written for and a little past it`() {
        assertTrue(recapIsCurrent(recap(), 4 to 10))
        assertTrue(recapIsCurrent(recap(), 4 to 50))
        assertFalse(recapIsCurrent(recap(), 4 to 9))
        assertFalse(recapIsCurrent(recap(), 4 to 51))
        assertFalse(recapIsCurrent(recap(), 5 to 10))
        assertFalse(recapIsCurrent(recap(text = ""), 4 to 10))
        assertFalse(recapIsCurrent(null, 4 to 10))
    }

    @Test
    fun `offered after long enough away`() {
        assertTrue(shouldOfferRecap(recap(), 4 to 12, nowS - 30 * hour, now, day, null))
        assertFalse(shouldOfferRecap(recap(), 4 to 12, nowS - 2 * hour, now, day, null))
    }

    @Test
    fun `offered every time when set to immediately`() {
        assertTrue(shouldOfferRecap(recap(), 4 to 12, nowS - 60, now, 0, null))
    }

    @Test
    fun `not offered when turned off, turned down, or stale`() {
        assertFalse(shouldOfferRecap(recap(), 4 to 12, nowS - 30 * hour, now, RECAP_NEVER, null))
        assertFalse(shouldOfferRecap(recap(), 4 to 12, nowS - 30 * hour, now, day, recapKey(recap())))
        assertFalse(shouldOfferRecap(recap(), 6 to 0, nowS - 30 * hour, now, day, null))
    }

    @Test
    fun `the previous book is offered to a reader starting a sequel`() {
        val series = recap(kind = RecapKinds.SERIES, chapterIdx = 0, paragraphIdx = 0, throughIdx = 8)
        assertTrue(shouldOfferRecap(series, 0 to 0, null, now, day, null))
    }

    @Test
    fun `requested when back after a while with nothing ready`() {
        val away = nowS - 30 * hour
        assertTrue(shouldRequestRecap(null, 4 to 12, away, 0.0, now, day))
        assertTrue(shouldRequestRecap(recap(chapterIdx = 2), 4 to 12, away, 0.0, now, day))
        assertFalse(shouldRequestRecap(recap(), 4 to 12, away, 0.0, now, day))
        assertFalse(shouldRequestRecap(null, 4 to 12, nowS - hour, 0.0, now, day))
        assertFalse(shouldRequestRecap(null, 4 to 12, away, 0.0, now, RECAP_NEVER))
        // Failed here: left to be asked for.
        assertFalse(shouldRequestRecap(recap(text = ""), 4 to 12, away, 0.0, now, day))
    }

    @Test
    fun `requested for a sequel never opened`() {
        assertTrue(shouldRequestRecap(null, 0 to 0, null, 3.0, now, day))
        assertFalse(shouldRequestRecap(null, 0 to 0, null, 1.0, now, day))
        assertFalse(shouldRequestRecap(null, 0 to 0, null, 0.0, now, day))
    }

    @Test
    fun `ago label rounds to a natural unit`() {
        assertEquals("just now", agoLabel(nowS - 60, now))
        assertEquals("5 hours ago", agoLabel(nowS - 5 * hour, now))
        assertEquals("1 day ago", agoLabel(nowS - 26 * hour, now))
        assertEquals("3 days ago", agoLabel(nowS - 72 * hour, now))
        assertEquals("3 weeks ago", agoLabel(nowS - 21 * 24 * hour, now))
    }

    @Test
    fun `word times are the aligner's, or spread over the stretch`() {
        val aligned = RecapSegmentDto(
            text = "Previously, in Book.", start = 2.0, end = 4.0,
            words = listOf(WordTimingDto("Previously,", 2.1, 2.5), WordTimingDto("in", 2.6, 2.7), WordTimingDto("Book.", 3.0, 3.4)),
        )
        assertEquals(listOf(2.1, 2.6, 3.0), recapWordTimes(aligned))
        assertEquals(listOf(10.0, 13.0), recapWordTimes(RecapSegmentDto(text = "ab cd", start = 10.0, end = 15.0)))
    }

    @Test
    fun `play waits for a recap that is ready or on its way`() {
        val away = nowS - 30 * hour
        // Ready to be offered.
        assertTrue(shouldHoldForRecap(recap(), 4 to 12, away, 0.0, now, day, null, awaited = false))
        // Not written yet, but asked for.
        assertTrue(shouldHoldForRecap(null, 4 to 12, away, 0.0, now, day, null, awaited = true))
        assertTrue(shouldHoldForRecap(recap(chapterIdx = 2), 4 to 12, away, 0.0, now, day, null, awaited = true))
        assertTrue(shouldHoldForRecap(null, 0 to 0, null, 2.0, now, day, null, awaited = true))
        // Nothing on its way, not away long enough, turned down, failed, or never offered.
        assertFalse(shouldHoldForRecap(null, 4 to 12, away, 0.0, now, day, null, awaited = false))
        assertFalse(shouldHoldForRecap(null, 4 to 12, nowS - hour, 0.0, now, day, null, awaited = true))
        assertFalse(shouldHoldForRecap(recap(), 4 to 12, away, 0.0, now, day, recapKey(recap()), awaited = true))
        assertFalse(shouldHoldForRecap(recap(text = ""), 4 to 12, away, 0.0, now, day, null, awaited = true))
        assertFalse(shouldHoldForRecap(recap(), 4 to 12, away, 0.0, now, RECAP_NEVER, null, awaited = true))
    }
}
