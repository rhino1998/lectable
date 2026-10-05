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
 * together). Only covers what's enqueued. [realtimeFactor] is that same rate in seconds of audio
 * per wall-clock second ([JobsSnapshotDto.narrationRealtimeFactor]), 0 if unknown.
 */
data class NarrationEta(
    val charsPerSecond: Double,
    val realtimeFactor: Double,
    val totalSeconds: Double,
    val books: List<BookEta>,
)

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
    return NarrationEta(rate, snapshot.narrationRealtimeFactor ?: 0.0, books.sumOf { it.chars } / rate, books)
}

/** The speed to estimate not-yet-queued work at: the live rate while narration runs, else the
 *  last run's; 0 when nothing has been measured since the backend started. Mirrors frontend's
 *  narrationRate. */
fun narrationRate(snapshot: JobsSnapshotDto?): Double =
    narrationRate(snapshot?.narrationCharsPerSecond, snapshot?.lastNarrationCharsPerSecond)

/** The same choice from the two rates themselves ([JobsSummaryDto] carries them too). */
fun narrationRate(charsPerSecond: Double?, lastCharsPerSecond: Double?): Double {
    val live = charsPerSecond ?: 0.0
    return if (live > 0) live else lastCharsPerSecond ?: 0.0
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
