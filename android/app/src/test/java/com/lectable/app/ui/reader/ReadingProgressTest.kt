package com.lectable.app.ui.reader

import com.lectable.app.data.remote.dto.ChapterSummaryDto
import org.junit.Assert.assertEquals
import org.junit.Test

class ReadingProgressTest {
    private val book = listOf(
        ChapterSummaryDto(idx = 0, paragraphCount = 40, matter = true),
        ChapterSummaryDto(idx = 1, paragraphCount = 10),
        ChapterSummaryDto(idx = 2, paragraphCount = 5, matter = true),
        ChapterSummaryDto(idx = 3, paragraphCount = 10),
        ChapterSummaryDto(idx = 4, paragraphCount = 3, matter = true),
    )

    @Test
    fun `matter counts for nothing`() {
        assertEquals(0.0, readingProgressPercent(book, 1, 0), 0.0)
        assertEquals(50.0, readingProgressPercent(book, 2, 3), 0.0)
        assertEquals(75.0, readingProgressPercent(book, 3, 5), 0.0)
        assertEquals(100.0, readingProgressPercent(book, 4, 1), 0.0)
    }

    @Test
    fun `a book with no matter is the plain sum`() {
        val plain = book.map { it.copy(matter = false) }
        assertEquals(20.0 / 68 * 100, readingProgressPercent(plain, 0, 20), 1e-9)
    }

    @Test
    fun `an empty book is at zero`() {
        assertEquals(0.0, readingProgressPercent(emptyList(), 0, 0), 0.0)
    }
}
