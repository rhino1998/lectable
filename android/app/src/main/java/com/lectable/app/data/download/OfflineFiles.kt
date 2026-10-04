package com.lectable.app.data.download

import com.lectable.app.data.remote.dto.AudioStatuses
import com.lectable.app.data.remote.dto.ParagraphDto
import java.io.File
import java.security.MessageDigest

// Where a downloaded chapter's clips live and how a refresh swaps new ones in, kept apart from
// DownloadRepository's Room and network calls so they can be unit tested on the JVM.

/** Suffix of a clip fetched by an in-progress download/refresh but not yet swapped in - see
 *  [commitStagedClips]. */
internal const val STAGED_SUFFIX = ".part"

/** Paragraph [idx]'s clip in a downloaded chapter's [dir]. Named `.wav` whatever its format. */
internal fun clipFile(dir: File, idx: Int): File = File(dir, "%05d.wav".format(idx))

/** Where a download or refresh puts paragraph [idx]'s clip until the whole chapter is fetched. */
internal fun stagedClipFile(dir: File, idx: Int): File = File(dir, "%05d.wav$STAGED_SUFFIX".format(idx))

/** Drops every staged clip in [dir], leaving the committed ones as they were. */
internal fun discardStagedClips(dir: File) {
    dir.listFiles { f -> f.name.endsWith(STAGED_SUFFIX) }?.forEach { it.delete() }
}

/**
 * The swap that ends a download or refresh: staged clips replace their committed counterparts
 * (rename(2) - atomic, and a player mid-way through the old file keeps its own open handle), and
 * clips of paragraphs the chapter no longer has ([liveParagraphIdx] is every one it does) are
 * dropped.
 */
internal fun commitStagedClips(dir: File, liveParagraphIdx: Collection<Int>) {
    dir.listFiles { f -> f.name.endsWith(STAGED_SUFFIX) }?.forEach { part ->
        part.renameTo(File(dir, part.name.removeSuffix(STAGED_SUFFIX)))
    }
    val liveNames = liveParagraphIdx.mapTo(mutableSetOf()) { clipFile(dir, it).name }
    dir.listFiles { f -> f.name.endsWith(".wav") && f.name !in liveNames }?.forEach { it.delete() }
}

/** A directory name for [value] (a library id, a voice key) - neither is safe or short enough
 *  to be one as it is. */
internal fun downloadDirName(value: String): String {
    val digest = MessageDigest.getInstance("SHA-256").digest(value.toByteArray())
    return digest.joinToString("") { "%02x".format(it) }.take(16)
}

/** How many of [paragraphs] have their current audio among the clips fetched so far. */
internal fun fetchedCount(paragraphs: List<ParagraphDto>, fetched: Map<Int, OfflineParagraph>): Int =
    paragraphs.count { fetched[it.idx]?.hasAudioOf(it) == true }

/**
 * A downloaded chapter's stored paragraphs as the reader's DTOs: every one READY, its audioUrl
 * whatever [clipUrl] gives for the paragraph whose file holds its clip. The members of a
 * scare-quote merge group share their anchor's file - each one's own is a duplicate of the same
 * whole clip (see [OfflineParagraph.audioPointerSeconds]), and the player tells a group by its
 * shared URL.
 */
internal fun List<OfflineParagraph>.toCachedParagraphs(clipUrl: (anchorIdx: Int) -> String): List<ParagraphDto> {
    var clipAnchorIdx = 0
    return map { p ->
        if (p.audioPointerSeconds == 0.0) clipAnchorIdx = p.idx
        ParagraphDto(
            idx = p.idx,
            text = p.text,
            audioStatus = AudioStatuses.READY,
            audioUrl = clipUrl(clipAnchorIdx),
            durationSeconds = p.durationSeconds,
            speaker = p.speaker,
            inline = p.inline,
            directionMarks = p.directionMarks,
            pronunciationMarks = p.pronunciationMarks,
            isQuote = p.isQuote,
            describesCharacters = p.describesCharacters,
            scareQuote = p.scareQuote,
            emotion = p.emotion,
            audioPointerSeconds = p.audioPointerSeconds,
        )
    }
}
