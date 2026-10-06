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

/** Extension of a downloaded clip. Format-neutral on purpose: the backend serves Ogg Opus, or a
 *  legacy WAV it hasn't converted yet, and ExoPlayer picks the extractor by sniffing content. */
internal const val CLIP_EXTENSION = ".audio"

/** What clips were named before [CLIP_EXTENSION], whatever their format - see
 *  [renameLegacyClips]. */
internal const val LEGACY_CLIP_EXTENSION = ".wav"

private fun clipName(idx: Int, extension: String) = "%05d".format(idx) + extension

/** The paragraph index a clip file name is for, or null if [name] isn't a clip's. */
private fun clipIdx(name: String): Int? {
    val stem = name.removeSuffix(CLIP_EXTENSION).takeIf { it != name } ?: name.removeSuffix(LEGACY_CLIP_EXTENSION).takeIf { it != name }
    return stem?.takeIf { it.length == 5 }?.toIntOrNull()
}

/** Paragraph [idx]'s clip in a downloaded chapter's [dir] - under its legacy name while that is
 *  the only copy there (a directory [renameLegacyClips] hasn't reached yet). */
internal fun clipFile(dir: File, idx: Int): File {
    val file = File(dir, clipName(idx, CLIP_EXTENSION))
    if (file.exists()) return file
    return File(dir, clipName(idx, LEGACY_CLIP_EXTENSION)).takeIf { it.exists() } ?: file
}

/** Where a download or refresh puts paragraph [idx]'s clip until the whole chapter is fetched. */
internal fun stagedClipFile(dir: File, idx: Int): File = File(dir, clipName(idx, CLIP_EXTENSION) + STAGED_SUFFIX)

/** Every committed clip in [dir], under either name. */
internal fun clipFiles(dir: File): List<File> = dir.listFiles { f -> clipIdx(f.name) != null }.orEmpty().toList()

/** Drops every staged clip in [dir], leaving the committed ones as they were. */
internal fun discardStagedClips(dir: File) {
    dir.listFiles { f -> f.name.endsWith(STAGED_SUFFIX) }?.forEach { it.delete() }
}

/**
 * The swap that ends a download or refresh: staged clips replace their committed counterparts
 * (rename(2) - atomic, and a player mid-way through the old file keeps its own open handle), and
 * clips of paragraphs the chapter no longer has ([liveParagraphIdx] is every one it does) are
 * dropped. A legacy-named clip a staged one replaces goes too.
 */
internal fun commitStagedClips(dir: File, liveParagraphIdx: Collection<Int>) {
    dir.listFiles { f -> f.name.endsWith(STAGED_SUFFIX) }?.forEach { part ->
        val committed = File(dir, part.name.removeSuffix(STAGED_SUFFIX))
        if (part.renameTo(committed)) File(dir, committed.name.removeSuffix(CLIP_EXTENSION) + LEGACY_CLIP_EXTENSION).delete()
    }
    val live = liveParagraphIdx.toSet()
    clipFiles(dir).filter { clipIdx(it.name) !in live }.forEach { it.delete() }
}

/**
 * Renames every legacy-named clip under [downloadsRoot] (`00012.wav`) to its [CLIP_EXTENSION]
 * name, returning how many it moved. Run once at startup; until it has reached a directory,
 * [clipFile] still finds that directory's clips. Only names of exactly that shape are touched.
 */
internal fun renameLegacyClips(downloadsRoot: File): Int {
    var renamed = 0
    downloadsRoot.walkTopDown().filter { it.isFile && it.name.endsWith(LEGACY_CLIP_EXTENSION) }.forEach { legacy ->
        val idx = clipIdx(legacy.name) ?: return@forEach
        val target = File(legacy.parentFile, clipName(idx, CLIP_EXTENSION))
        if (target.exists()) legacy.delete() else if (legacy.renameTo(target)) renamed++
    }
    return renamed
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
            label = p.label,
            table = p.table,
            inline = p.inline,
            directionMarks = p.directionMarks,
            pronunciationMarks = p.pronunciationMarks,
            isQuote = p.isQuote,
            scareQuote = p.scareQuote,
            emotion = p.emotion,
            audioPointerSeconds = p.audioPointerSeconds,
        )
    }
}
