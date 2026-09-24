package com.lectable.app.data.download

import androidx.room.ColumnInfo
import androidx.room.Entity
import com.lectable.app.data.remote.dto.AudioStatus
import com.lectable.app.data.remote.dto.DirectionMarkDto
import com.lectable.app.data.remote.dto.ParagraphDto
import com.lectable.app.data.remote.dto.PronunciationMarkDto
import kotlinx.serialization.Serializable

/**
 * A downloaded book's metadata, cached so the library list and reader can show/open it without a
 * live network call (see DownloadRepository.cacheBookMetadata, called whenever a chapter
 * download runs). Keyed by (libraryId, bookId) - see DownloadedChapter's doc for why libraryId
 * is part of the key. [coverUrl] is kept as the original relative network path (e.g.
 * "/api/books/{id}/cover", same shape BookDetailDto.coverUrl always carries) so a reconstructed
 * BookDetailDto round-trips through the same MediaUrlResolver path other code already expects -
 * [localCoverPath] is the actual downloaded image bytes on disk, used directly for offline
 * library browsing since MediaUrlResolver can't do anything with [coverUrl] when there's no
 * network to resolve it against.
 */
@Entity(tableName = "downloaded_books", primaryKeys = ["libraryId", "bookId"])
data class DownloadedBook(
    @ColumnInfo(defaultValue = "") val libraryId: String,
    val bookId: String,
    val title: String,
    val author: String,
    val coverUrl: String?,
    val localCoverPath: String?,
    val chapterCount: Int,
    // The root of the backend's offline-sync hash tree (BookManifestDto.hash) as of the last
    // reconcile that left every downloaded chapter matching it - lets OfflineReconciler skip an
    // unchanged book after one request. "" until then.
    @ColumnInfo(defaultValue = "") val hash: String = "",
    // The backend's reading position as of the last reconcile - the offline resume point when
    // there's no newer, locally-queued PendingPosition (see DownloadRepository.cachedBook).
    @ColumnInfo(defaultValue = "0") val posChapterIdx: Int = 0,
    @ColumnInfo(defaultValue = "0") val posParagraphIdx: Int = 0,
    @ColumnInfo(defaultValue = "0") val posSeconds: Double = 0.0,
)

/** One paragraph's offline-readable content, serialized into DownloadedChapter.paragraphsJson -
 *  just enough to reconstruct a ChapterDetailDto for offline reading (see
 *  DownloadRepository.cachedChapter). No audioStatus/audioUrl: a cached chapter is only ever
 *  written once every paragraph in it is actually downloaded, so callers can assume READY with a
 *  local file for every one of these. [inline]/[speaker] mirror ParagraphDto's own fields
 *  (defaulted so an already-downloaded chapter's stored JSON, serialized before these existed,
 *  still decodes) so a downloaded chapter keeps its Inline-joined visual blocks and speaker hints
 *  when read offline, not just its plain paragraph text. */
@Serializable
data class OfflineParagraph(
    val idx: Int,
    val text: String,
    val durationSeconds: Double,
    val inline: Boolean = false,
    val speaker: String? = null,
    val directionMarks: List<DirectionMarkDto> = emptyList(),
    val pronunciationMarks: List<PronunciationMarkDto> = emptyList(),
    val isQuote: Boolean = false,
    val describesCharacters: List<String> = emptyList(),
    val scareQuote: Boolean = false,
    // Mirrors ParagraphDto.audioPointerSeconds - see its own doc comment. Each downloaded
    // paragraph still gets its own local .wav file (downloadChapter fetches by this paragraph's
    // own audioUrl, which for a scare-quote merge group's non-anchor member already resolves to
    // the group's one shared clip - so its own local copy is a full, redundant duplicate of that
    // same clip, not a slice), so this offset is exactly as meaningful for seeking into it
    // offline as it is for the streamed original.
    val audioPointerSeconds: Double = 0.0,
    // ParagraphDto.contentHash/audioHash as of what's stored here - [audioHash] describes the
    // downloaded .wav, not whatever the backend has since regenerated. "" for a paragraph
    // stored before hashes existed (see hasAudioOf).
    val contentHash: String = "",
    val audioHash: String = "",
)

/** Whether this stored paragraph's local .wav is still the audio [live] would serve - by
 *  audioHash when both sides have one, otherwise (a row stored before hashes existed) by the
 *  duration/pointer heuristic that predates them. Never true for a live paragraph whose audio
 *  isn't ready. */
fun OfflineParagraph.hasAudioOf(live: ParagraphDto): Boolean {
    if (live.audioStatus != AudioStatus.READY) return false
    if (audioHash.isNotEmpty() && live.audioHash.isNotEmpty()) return audioHash == live.audioHash
    return durationSeconds == (live.durationSeconds ?: 0.0) && audioPointerSeconds == live.audioPointerSeconds
}
