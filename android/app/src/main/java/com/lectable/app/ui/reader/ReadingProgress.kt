package com.lectable.app.ui.reader

import com.lectable.app.data.remote.dto.ChapterSummaryDto

/**
 * How far through the book a position is, in percent: paragraphs behind it over paragraphs in
 * all, counting story chapters only - front and back matter ([ChapterSummaryDto.matter]) counts
 * for nothing. Mirrors the backend's toBookSummary (and the web's readingProgress) so the live
 * figure in the reader and the saved one in the library agree.
 */
internal fun readingProgressPercent(chapters: List<ChapterSummaryDto>, chapterIdx: Int, paragraphIdx: Int): Double {
    var total = 0
    var completed = 0
    for (c in chapters) {
        if (c.matter) continue
        total += c.paragraphCount
        when {
            c.idx < chapterIdx -> completed += c.paragraphCount
            c.idx == chapterIdx -> completed += paragraphIdx.coerceAtMost(c.paragraphCount)
        }
    }
    return if (total > 0) (completed.toDouble() / total * 100).coerceAtMost(100.0) else 0.0
}
