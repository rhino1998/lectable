package com.lectable.app.ui.reader

import com.lectable.app.data.remote.dto.BookRecapDto
import com.lectable.app.data.remote.dto.RecapKinds
import com.lectable.app.data.remote.dto.RecapSegmentDto

/** How long a reader must have been away from a book before its recap is offered unasked -
 *  minutes to label: 0 offers it every time the book is opened, [RECAP_NEVER] never. Mirrors
 *  frontend's utils/recap.ts (as does the rest of this file - keep the two in step). */
const val RECAP_NEVER = -1
val RECAP_AFTER_OPTIONS = listOf(
    0 to "immediately",
    15 to "after 15 minutes away",
    60 to "after an hour away",
    4 * 60 to "after 4 hours away",
    12 * 60 to "after 12 hours away",
    24 * 60 to "after a day away",
    3 * 24 * 60 to "after 3 days away",
    7 * 24 * 60 to "after a week away",
    RECAP_NEVER to "never",
)

/** Names the recap a reader turned down - what it is of and the place it was written for - so
 *  the offer doesn't come back for it. */
fun recapKey(recap: BookRecapDto): String = "${recap.kind}:${recap.chapterIdx}:${recap.paragraphIdx}"

/** The recap was written for where the reader is now: it tells nothing past their position
 *  and isn't from far behind it. */
fun recapIsCurrent(recap: BookRecapDto?, pos: ParagraphPos): Boolean =
    recap != null && recap.text.isNotEmpty() && recap.chapterIdx == pos.first &&
        recap.paragraphIdx <= pos.second && pos.second <= recap.throughIdx

/** The book was last read ([lastReadAt], unix seconds; null for a book never opened) at least
 *  [afterMinutes] before [nowMs] - any time before, for 0. */
fun awayLongEnough(lastReadAt: Long?, nowMs: Long, afterMinutes: Int): Boolean =
    afterMinutes >= 0 && lastReadAt != null && lastReadAt > 0 && nowMs / 1000 - lastReadAt >= afterMinutes * 60L

/**
 * Whether the reader is offered a recap they didn't ask for: it is current, they haven't
 * turned it down, and either they have been away long enough ([lastReadAtOnOpen]: as it was
 * when they opened the book, before this visit saved anything) or it is the recap of the
 * previous book, offered whenever a sequel is started.
 */
fun shouldOfferRecap(
    recap: BookRecapDto?,
    pos: ParagraphPos,
    lastReadAtOnOpen: Long?,
    nowMs: Long,
    afterMinutes: Int,
    dismissed: String?,
): Boolean {
    if (afterMinutes < 0 || recap == null || !recapIsCurrent(recap, pos)) return false
    if (recapKey(recap) == dismissed) return false
    return recap.kind == RecapKinds.SERIES || awayLongEnough(lastReadAtOnOpen, nowMs, afterMinutes)
}

/**
 * On opening a book, whether to ask the backend for a recap: the reader would be offered one
 * and none is ready for where they are - back after long enough, or starting a book that has
 * one before it in its series. (The backend usually has it ready already; it prepares one
 * whenever a book has been left alone for a while.)
 */
fun shouldRequestRecap(
    recap: BookRecapDto?,
    pos: ParagraphPos,
    lastReadAt: Long?,
    seriesIndex: Double,
    nowMs: Long,
    afterMinutes: Int,
): Boolean {
    if (afterMinutes < 0 || recapIsCurrent(recap, pos)) return false
    // Failed here already: left for the reader to ask again.
    if (recap != null && recap.text.isEmpty() && recap.chapterIdx == pos.first) return false
    val startingSequel = (lastReadAt == null || lastReadAt == 0L) && seriesIndex > 1
    return startingSequel || awayLongEnough(lastReadAt, nowMs, afterMinutes)
}

/** How long before [nowMs] a unix-seconds time was, roughly. */
fun agoLabel(at: Long, nowMs: Long): String {
    val hours = maxOf(0.0, (nowMs / 1000 - at) / 3600.0)
    if (hours < 1) return "just now"
    val h = Math.round(hours)
    if (hours < 24) return "$h hour${if (h == 1L) "" else "s"} ago"
    val days = Math.round(hours / 24)
    if (days < 14) return "$days day${if (days == 1L) "" else "s"} ago"
    if (days < 60) return "${Math.round(days / 7.0)} weeks ago"
    return "${Math.round(days / 30.0)} months ago"
}

/** When each word of one stretch of a recap's clip is spoken, in seconds into the clip, one per
 *  [tokenizeWords] token of its text: the aligner's own times when it has one per word,
 *  otherwise spread over the stretch by position in the text. */
fun recapWordTimes(segment: RecapSegmentDto): List<Double> =
    wordStartTimes(tokenizeWords(segment.text), segment.words, segment.text.length, maxOf(0.0, segment.end - segment.start), segment.start)
