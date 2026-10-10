package com.lectable.app.ui.library

import com.lectable.app.data.remote.dto.BookSummaryDto
import org.junit.Assert.assertEquals
import org.junit.Test

class ContinueReadingTest {
    private fun book(id: String, finished: Boolean = false, lastReadAt: Long? = null, series: String? = null, index: Double? = null) =
        BookSummaryDto(id = id, title = id, finished = finished, lastReadAt = lastReadAt, seriesName = series, seriesIndex = index)

    private fun ids(books: List<BookSummaryDto>) = books.joinToString(",") { it.id }

    @Test
    fun listsUnfinishedBooksBeingReadLatestFirst() {
        val books = listOf(
            book("old", lastReadAt = 10),
            book("new", lastReadAt = 30),
            book("done", finished = true, lastReadAt = 40),
            book("unopened"),
        )
        assertEquals("new,old", ids(continueReading(books)))
        assertEquals("new", ids(continueReading(books, limit = 1)))
        assertEquals("", ids(continueReading(listOf(book("only", lastReadAt = 5)))))
    }

    @Test
    fun addsTheNextBookOfASeriesOnceTheOneBeforeItIsFinished() {
        val books = listOf(
            // Listed out of order: the series' own numbering decides.
            book("s3", series = "Saga", index = 3.0),
            book("s1", finished = true, lastReadAt = 20, series = "Saga", index = 1.0),
            book("s2", finished = true, lastReadAt = 50, series = "Saga", index = 2.0),
            book("s4", series = "Saga", index = 4.0),
            book("reading", lastReadAt = 40),
            // Its first book isn't finished, so its second isn't next.
            book("t1", lastReadAt = 5, series = "Trilogy", index = 1.0),
            book("t2", series = "Trilogy", index = 2.0),
            // Marked read without ever being opened: its next book still shows, last.
            book("b1", finished = true, series = "B", index = 1.0),
            book("b2", series = "B", index = 2.0),
        )
        // s3 counts from when s2 was finished; s4 waits for s3.
        assertEquals("s3,reading,t1,b2", ids(continueReading(books)))
    }
}
