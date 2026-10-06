package com.lectable.app.playback

import com.lectable.app.data.remote.dto.AudioStatuses
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ParagraphDto
import kotlin.math.abs

// The decisions ParagraphPlayer makes about which clip to load, queue and call "current",
// kept apart from the ExoPlayer instance acting on them so they can be unit tested on the JVM.

/** How much upcoming audio (media seconds) [ParagraphPlayer] keeps queued behind the current clip. */
internal const val QUEUE_AHEAD_SECONDS = 30.0

/** Cap on queued clips, whatever their durations. */
internal const val MAX_QUEUED_CLIPS = 20

private fun ParagraphDto.hasClip(): Boolean = audioStatus == AudioStatuses.READY && audioUrl != null

/** What [ParagraphPlayer] does when asked to play a paragraph - see [playAction]. */
internal enum class PlayAction {
    /** The chapter (or that paragraph of it) isn't loaded: ask for it and wait. */
    NEED_CHAPTER,

    /** Loaded, but its audio isn't generated yet: pause on it, ask for fresh data and wait. */
    PARK,

    /** Its clip is the one already loaded: seek within it. */
    SEEK,

    /** Another member of the loaded scare-quote merge group, which shares the loaded clip:
     *  seek within it and make this paragraph the current one. */
    SEEK_IN_GROUP,

    /** Load its clip. */
    LOAD,
}

/**
 * What playing [target] takes, given the clip the player holds: [loadedTarget] and
 * [loadedAudioUrl] describe it, and [hasMediaItems] is whether the player still has it at all.
 */
internal fun playAction(
    chapters: Map<Int, ChapterDetailDto>,
    target: Pair<Int, Int>,
    loadedTarget: Pair<Int, Int>?,
    loadedAudioUrl: String?,
    hasMediaItems: Boolean,
): PlayAction {
    val paragraph = chapters[target.first]?.paragraphs?.getOrNull(target.second) ?: return PlayAction.NEED_CHAPTER
    if (!paragraph.hasClip()) return PlayAction.PARK
    if (loadedTarget == target && hasMediaItems) return PlayAction.SEEK
    if (loadedAudioUrl != null && loadedAudioUrl == paragraph.audioUrl && hasMediaItems &&
        loadedTarget != null && sameMergeGroup(chapters, loadedTarget, target)
    ) {
        return PlayAction.SEEK_IN_GROUP
    }
    return PlayAction.LOAD
}

/**
 * Whether [a] and [b] are members of one scare-quote merge group: the same chapter, and every
 * paragraph from one to the other on the same clip. Sharing a URL alone doesn't say so - a line
 * repeated later in the chapter plays the same shared clip (see the backend's clip store), but
 * is its own clip start with its own place in the queue.
 */
internal fun sameMergeGroup(chapters: Map<Int, ChapterDetailDto>, a: Pair<Int, Int>, b: Pair<Int, Int>): Boolean {
    if (a.first != b.first) return false
    val paragraphs = chapters[a.first]?.paragraphs ?: return false
    val url = paragraphs.getOrNull(a.second)?.audioUrl ?: return false
    return (minOf(a.second, b.second)..maxOf(a.second, b.second)).all { paragraphs.getOrNull(it)?.audioUrl == url }
}

/**
 * Where in its clip playing [paragraph] starts. Every "seconds" value tied to a paragraph is an
 * absolute position within that paragraph's own audioUrl, never paragraph-relative: [startSeconds]
 * above zero is a specific point to seek to (a word tap, a saved position); otherwise it is the
 * paragraph's own start within the clip, which for a merge group's non-anchor member isn't zero.
 */
internal fun seekTargetSeconds(paragraph: ParagraphDto, startSeconds: Double): Double =
    if (startSeconds > 0) startSeconds else paragraph.audioPointerSeconds

/**
 * The paragraph whose clip plays after [from]'s (whose clip is [url]): the first paragraph
 * past that clip's scare-quote merge group (whose members all share one clip), crossing into
 * the next chapter when that's already loaded. Null when it isn't ready yet or doesn't start
 * its own clip.
 */
internal fun clipStartAfter(chapters: Map<Int, ChapterDetailDto>, from: Pair<Int, Int>, url: String): Pair<Int, Int>? {
    var (chapterIdx, paragraphIdx) = from
    while (true) {
        val chapter = chapters[chapterIdx] ?: return null
        paragraphIdx++
        if (paragraphIdx >= chapter.paragraphs.size) {
            chapterIdx++
            paragraphIdx = -1
            continue
        }
        val p = chapter.paragraphs[paragraphIdx]
        if (!p.hasClip()) return null
        if (p.audioUrl == url) continue
        return if (p.audioPointerSeconds == 0.0) chapterIdx to paragraphIdx else null
    }
}

/**
 * The paragraphs whose clips should be queued behind the loaded one ([loadedTarget], playing
 * [loadedAudioUrl]), in order: clips are added until [aheadSeconds] of audio or [maxClips] of
 * them are queued, stopping early at the first one that isn't ready.
 */
internal fun clipsToQueue(
    chapters: Map<Int, ChapterDetailDto>,
    loadedTarget: Pair<Int, Int>?,
    loadedAudioUrl: String?,
    aheadSeconds: Double = QUEUE_AHEAD_SECONDS,
    maxClips: Int = MAX_QUEUED_CLIPS,
): List<Pair<Int, Int>> {
    val wanted = mutableListOf<Pair<Int, Int>>()
    var from = loadedTarget
    var url = loadedAudioUrl
    var queuedSeconds = 0.0
    while (from != null && url != null && queuedSeconds < aheadSeconds && wanted.size < maxClips) {
        val next = clipStartAfter(chapters, from, url) ?: break
        wanted += next
        val paragraph = chapters[next.first]?.paragraphs?.getOrNull(next.second)
        queuedSeconds += paragraph?.durationSeconds ?: 0.0
        from = next
        url = paragraph?.audioUrl
    }
    return wanted
}

/** The paragraph and clip a queued player item was built for - see [clipMediaId]. */
internal data class QueuedClip(val chapterIdx: Int, val paragraphIdx: Int, val audioUrl: String?)

/** A player item's id: which paragraph and clip it is, so a clip queued ahead of time can be
 *  checked against the chapter's current data when it starts. */
internal fun clipMediaId(chapterIdx: Int, paragraphIdx: Int, audioUrl: String): String = "$chapterIdx:$paragraphIdx:$audioUrl"

/** Reads back a [clipMediaId]; null if [mediaId] isn't one. */
internal fun parseClipMediaId(mediaId: String): QueuedClip? {
    val parts = mediaId.split(":", limit = 3)
    val chapterIdx = parts.getOrNull(0)?.toIntOrNull() ?: return null
    val paragraphIdx = parts.getOrNull(1)?.toIntOrNull() ?: return null
    return QueuedClip(chapterIdx, paragraphIdx, parts.getOrNull(2))
}

/** Whether the clip a queued item was built for is still [paragraph]'s audio (it may have been
 *  regenerated, or dropped from the chapter, since it was queued). */
internal fun QueuedClip.isCurrentFor(paragraph: ParagraphDto?): Boolean =
    paragraph != null && paragraph.audioStatus == AudioStatuses.READY && paragraph.audioUrl == audioUrl

/**
 * Whether playback at [positionMs] within paragraph [paragraphIdx]'s clip has reached the next
 * paragraph - true only when that one is a later member of the same scare-quote merge group
 * (same audioUrl) and its start within the shared clip has been passed. A group's real end is a
 * different clip, which the player's own end-of-clip handling covers.
 */
internal fun reachedNextMergeMember(chapter: ChapterDetailDto, paragraphIdx: Int, positionMs: Long): Boolean {
    val current = chapter.paragraphs.getOrNull(paragraphIdx) ?: return false
    if (!current.hasClip()) return false
    val next = chapter.paragraphs.getOrNull(paragraphIdx + 1) ?: return false
    if (next.audioStatus != AudioStatuses.READY || next.audioUrl != current.audioUrl) return false
    return positionMs >= (next.audioPointerSeconds * 1000).toLong()
}

/** [chapter]'s whole narration length, for a scrubber spanning the chapter rather than one clip. */
internal fun chapterDurationMs(chapter: ChapterDetailDto): Long =
    (chapter.paragraphs.sumOf { it.durationSeconds ?: 0.0 } * 1000).toLong()

/**
 * Position within [chapter] of playback at [clipPositionMs] in paragraph [paragraphIdx]'s clip.
 * [clipPositionMs] is absolute within the clip, so for a scare-quote merge group member it
 * includes everything before that paragraph's own start in the shared clip - which the earlier
 * paragraphs' durations already account for, so it is taken back out.
 */
internal fun chapterPositionMs(chapter: ChapterDetailDto, paragraphIdx: Int, clipPositionMs: Long): Long {
    val elapsedBeforeSeconds = chapter.paragraphs.take(paragraphIdx.coerceAtLeast(0)).sumOf { it.durationSeconds ?: 0.0 }
    val pointerOffsetMs = ((chapter.paragraphs.getOrNull(paragraphIdx)?.audioPointerSeconds ?: 0.0) * 1000).toLong()
    return (elapsedBeforeSeconds * 1000).toLong() + (clipPositionMs - pointerOffsetMs).coerceAtLeast(0)
}

/** How far one tick of a speed ramp moves the speed: 0.5x per [rampSeconds], or the whole way
 *  at once when ramping is off. */
internal fun speedRampStep(rampSeconds: Float, tickMs: Long): Float =
    (if (rampSeconds > 0f) 0.5f / rampSeconds else Float.MAX_VALUE) * tickMs / 1000f

/** The speed one ramp tick after [applied] on the way to [target]: [target] itself once it is
 *  within [step]. */
internal fun rampedSpeed(applied: Float, target: Float, step: Float): Float =
    if (abs(target - applied) <= step) target else applied + if (target > applied) step else -step
