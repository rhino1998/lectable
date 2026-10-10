package com.lectable.app.ui.library

import com.lectable.app.data.remote.dto.BookSummaryDto

// The library's "Continue reading" section, mirroring frontend/src/utils/continueReading.ts:
// unfinished books that are being read, and the next book of a series - an unfinished one whose
// predecessor in the series is finished - whether or not it has been opened.

/** How many books the section lists. */
internal const val CONTINUE_READING_LIMIT = 5

/** The section's books, most recent first: a book counts from when it was last read, a series'
 *  next book also from when the one before it was (so it turns up as that one is finished).
 *  Empty for a one-book library, and offline - downloaded copies don't record when they were
 *  read - unless a series has a next book. */
internal fun continueReading(books: List<BookSummaryDto>, limit: Int = CONTINUE_READING_LIMIT): List<BookSummaryDto> {
    if (books.size < 2) return emptyList()
    val at = mutableMapOf<String, Long>()
    for (b in books) {
        val read = b.lastReadAt ?: 0
        if (!b.finished && read > 0) at[b.id] = read
    }
    books.filter { !it.seriesName.isNullOrEmpty() }
        .groupBy { it.seriesName }
        .values
        .forEach { series ->
            series.sortedBy { it.seriesIndex ?: 0.0 }.zipWithNext { before, next ->
                if (before.finished && !next.finished) {
                    at[next.id] = maxOf(at[next.id] ?: 0, before.lastReadAt ?: 0)
                }
            }
        }
    return books.filter { it.id in at }.sortedByDescending { at.getValue(it.id) }.take(limit)
}
