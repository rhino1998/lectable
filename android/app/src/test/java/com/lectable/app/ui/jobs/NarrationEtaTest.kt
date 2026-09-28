package com.lectable.app.ui.jobs

import com.lectable.app.data.remote.dto.JobsSnapshotDto
import com.lectable.app.data.remote.dto.QueueTaskDto
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class NarrationEtaTest {
    private fun task(book: String = "a", chars: Int? = null, kind: String = "voice_clone") =
        QueueTaskDto(kind = kind, bookId = book, bookTitle = book.uppercase(), chars = chars)

    @Test
    fun nullWithoutRateOrNarration() {
        assertNull(narrationEta(null))
        assertNull(narrationEta(JobsSnapshotDto(queued = listOf(task(chars = 100)))))
        assertNull(
            narrationEta(
                JobsSnapshotDto(narrationCharsPerSecond = 10.0, queued = listOf(task(kind = "ambience"))),
            ),
        )
    }

    @Test
    fun sumsPerBookAtRate() {
        val eta = narrationEta(
            JobsSnapshotDto(
                narrationCharsPerSecond = 20.0,
                inFlight = listOf(task(chars = 200)),
                queued = listOf(task(chars = 400), task(book = "b", chars = 1200)),
            ),
        )!!
        assertEquals(90.0, eta.totalSeconds, 1e-9)
        assertEquals(listOf("b", "a"), eta.books.map { it.bookId })
        assertEquals(60.0, eta.books[0].seconds, 1e-9)
        assertEquals(600, eta.books[1].chars)
    }

    @Test
    fun rateFallsBackToLastRun() {
        assertEquals(0.0, narrationRate(null), 0.0)
        assertEquals(12.0, narrationRate(JobsSnapshotDto(lastNarrationCharsPerSecond = 12.0)), 0.0)
        assertEquals(
            20.0,
            narrationRate(JobsSnapshotDto(narrationCharsPerSecond = 20.0, lastNarrationCharsPerSecond = 12.0)),
            0.0,
        )
    }

    @Test
    fun formats() {
        assertEquals("<1m", formatEta(20.0))
        assertEquals("2m", formatEta(90.0))
        assertEquals("1h", formatEta(3600.0))
        assertEquals("1h 25m", formatEta(3600.0 + 25 * 60))
    }
}
