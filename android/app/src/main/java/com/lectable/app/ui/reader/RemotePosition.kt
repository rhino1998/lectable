package com.lectable.app.ui.reader

/** A (chapterIdx, paragraphIdx) reading position. */
typealias ParagraphPos = Pair<Int, Int>

/** How many of this reader's own recent saves count as echoes - saves are throttled and the
 *  book topic can deliver an older one after a newer one has gone out. */
const val OWN_POSITION_SAVES_KEPT = 20

/**
 * Whether the book's saved position (the live book topic's pos*) was moved by another device -
 * the web reader, another phone - while this reader sat paused: it differs from where this
 * reader is, isn't an echo of one of its own recent saves, and hasn't been dismissed. Without
 * this, resuming here would silently play from the stale spot and overwrite the other device's
 * progress. Mirrors frontend's hooks/useRemotePosition.ts remotePosition.
 */
fun remotePosition(
    saved: ParagraphPos?,
    current: ParagraphPos,
    ownSaves: Collection<ParagraphPos>,
    dismissed: ParagraphPos?,
): ParagraphPos? {
    if (saved == null || saved == current || saved in ownSaves || saved == dismissed) return null
    return saved
}
