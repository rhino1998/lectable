package com.lectable.app.data.download

import com.lectable.app.data.remote.dto.ChapterDetailDto
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json

/**
 * This COMPLETE row with [chapter]'s text/annotations applied (the backend always wins), keeping
 * each paragraph's stored audio fields - the local .wav files aren't touched here, so
 * durationSeconds/audioPointerSeconds/audioHash keep describing them. Adopts [chapter]'s hash
 * only when every paragraph's local audio still matches, i.e. when the row is then fully in sync;
 * otherwise the old hash stays, so OfflineReconciler still sees the chapter as changed and
 * re-downloads the stale audio. Shared by OfflineCacheWriter (live pushes) and OfflineReconciler
 * (reconnect sync) so the two can't drift.
 */
fun DownloadedChapter.mergedWith(chapter: ChapterDetailDto, json: Json): DownloadedChapter {
    val stored = runCatching { json.decodeFromString<List<OfflineParagraph>>(paragraphsJson) }.getOrDefault(emptyList())
        .associateBy { it.idx }
    var audioInSync = chapter.paragraphs.size == stored.size
    val paragraphs = chapter.paragraphs.map { p ->
        val old = stored[p.idx]
        val sameAudio = old != null && old.hasAudioOf(p)
        if (!sameAudio) audioInSync = false
        OfflineParagraph(
            idx = p.idx,
            text = p.text,
            durationSeconds = old?.durationSeconds ?: (p.durationSeconds ?: 0.0),
            inline = p.inline,
            speaker = p.speaker,
            directionMarks = p.directionMarks,
            pronunciationMarks = p.pronunciationMarks,
            isQuote = p.isQuote,
            describesCharacters = p.describesCharacters,
            scareQuote = p.scareQuote,
            emotion = p.emotion,
            audioPointerSeconds = old?.audioPointerSeconds ?: p.audioPointerSeconds,
            contentHash = p.contentHash,
            // A match found by the pre-hash duration heuristic adopts the server's hash, so
            // rows downloaded before hashes existed migrate without re-downloading anything.
            audioHash = if (sameAudio) p.audioHash else old?.audioHash.orEmpty(),
        )
    }
    return copy(
        title = chapter.title,
        paragraphsJson = json.encodeToString(paragraphs),
        contentJson = json.encodeToString(chapter.content),
        hash = if (audioInSync && chapter.hash.isNotEmpty()) chapter.hash else hash,
    )
}
