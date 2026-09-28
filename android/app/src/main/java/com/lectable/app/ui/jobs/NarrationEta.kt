package com.lectable.app.ui.jobs

import com.lectable.app.data.remote.dto.JobsSnapshotDto
import kotlin.math.roundToInt

/** One book's share of [NarrationEta]. */
data class BookEta(val bookId: String, val bookTitle: String, val chars: Int, val seconds: Double)

/**
 * Time left for the narration already queued or in flight - mirrors frontend's
 * utils/narrationEta.ts: every voice_clone/voice_design task's paragraph text
 * ([com.lectable.app.data.remote.dto.QueueTaskDto.chars]) at the backend's recently
 * measured rate ([JobsSnapshotDto.narrationCharsPerSecond], all generation slots
 * together). Only covers what's enqueued.
 */
data class NarrationEta(val charsPerSecond: Double, val totalSeconds: Double, val books: List<BookEta>)

/** Null while the backend has no rate to offer or nothing narration-shaped is queued. */
fun narrationEta(snapshot: JobsSnapshotDto?): NarrationEta? {
    val rate = snapshot?.narrationCharsPerSecond ?: 0.0
    if (snapshot == null || rate <= 0.0) return null
    val byBook = LinkedHashMap<String, Pair<String, Int>>()
    for (t in snapshot.inFlight + snapshot.queued) {
        val chars = t.chars ?: 0
        if (chars <= 0) continue
        val (title, sum) = byBook[t.bookId] ?: (t.bookTitle to 0)
        byBook[t.bookId] = title to sum + chars
    }
    if (byBook.isEmpty()) return null
    val books = byBook.map { (id, v) -> BookEta(id, v.first, v.second, v.second / rate) }
        .sortedByDescending { it.chars }
    return NarrationEta(rate, books.sumOf { it.chars } / rate, books)
}

/** "<1m", "38m", "2h", "1h 25m". */
fun formatEta(seconds: Double): String {
    if (seconds < 60) return "<1m"
    val totalMinutes = (seconds / 60).roundToInt()
    val h = totalMinutes / 60
    val m = totalMinutes % 60
    return when {
        h == 0 -> "${m}m"
        m == 0 -> "${h}h"
        else -> "${h}h ${m}m"
    }
}
