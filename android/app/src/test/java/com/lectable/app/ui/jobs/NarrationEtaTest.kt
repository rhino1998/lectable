package com.lectable.app.ui.jobs

import com.lectable.app.data.remote.dto.BookNarrationDto
import com.lectable.app.data.remote.dto.JobsSnapshotDto
import com.lectable.app.data.remote.dto.QueueTaskDto
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class NarrationEtaTest {
    private fun left(book: String, chars: Int) = BookNarrationDto(bookId = book, bookTitle = book.uppercase(), chars = chars)

    @Test
    fun nullWithoutRateOrNarration() {
        assertNull(narrationEta(null))
        assertNull(narrationEta(JobsSnapshotDto(narrationLeft = listOf(left("a", 100)))))
        assertNull(
            narrationEta(
                JobsSnapshotDto(narrationCharsPerSecond = 10.0, queued = listOf(QueueTaskDto(kind = "ambience"))),
            ),
        )
    }

    // The queue's task list is capped, so the estimate reads the backend's per-book sums.
    @Test
    fun sumsPerBookAtRate() {
        val eta = narrationEta(
            JobsSnapshotDto(
                narrationCharsPerSecond = 20.0,
                narrationRealtimeFactor = 1.5,
                queued = listOf(QueueTaskDto(kind = "voice_clone", bookId = "a", chars = 400)),
                queuedTotal = 30_000,
                narrationLeft = listOf(left("a", 600), left("b", 1200)),
            ),
        )!!
        assertEquals(90.0, eta.totalSeconds, 1e-9)
        assertEquals(1.5, eta.realtimeFactor, 1e-9)
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
