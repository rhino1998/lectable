package com.lectable.app.data.download

import androidx.room.ColumnInfo
import androidx.room.Entity

/** Plain string constants rather than a Room-`@TypeConverter`-requiring enum, same "just a
 *  string column" convention the backend's own AudioPending/AudioReady/etc. statuses use. */
object DownloadStatus {
    const val DOWNLOADING = "downloading"
    const val COMPLETE = "complete"
    const val ERROR = "error"
}

/**
 * One chapter's downloaded-for-offline-playback state, keyed by (libraryId, bookId, chapterIdx).
 * [libraryId] scopes this to one specific backend (see BackendIdentityRepository) - the app can
 * be pointed at different backends over its lifetime, and a bookId alone is only unique within
 * one backend's own database. [voiceKey] mirrors the backend's own voice-invalidation rule (see
 * root CLAUDE.md: changing a book's voice resets its cached audio) - it's the same (presetId,
 * instruct, language) tuple the backend hashes into its `voiceId` (see
 * backend/internal/store/store.go's VoiceID), just compared as a raw string here rather than
 * reproducing the SHA256 client-side, since all we need is staleness detection, not
 * byte-identical keys. [pinned] is vestigial: it distinguished manual downloads from the
 * since-removed automatic next-chapter prefetch (which evicted unpinned rows once playback moved
 * past them). Every download is manual now, so nothing reads it - kept only to avoid a Room
 * schema migration.
 */
@Entity(tableName = "downloaded_chapters", primaryKeys = ["libraryId", "bookId", "chapterIdx"])
data class DownloadedChapter(
    @ColumnInfo(defaultValue = "") val libraryId: String,
    val bookId: String,
    val chapterIdx: Int,
    val voiceKey: String,
    val pinned: Boolean,
    val status: String,
    // Chapter title + paragraph text/duration (JSON-serialized List<OfflineParagraph>) - present
    // once COMPLETE, "" / "[]" until then. Lets a downloaded chapter be opened for offline
    // reading (see DownloadRepository.cachedChapter), not just played - the point of downloading
    // it in the first place.
    @ColumnInfo(defaultValue = "") val title: String = "",
    @ColumnInfo(defaultValue = "[]") val paragraphsJson: String = "[]",
    // This chapter's content items in original document order (JSON-serialized
    // List<ContentItemDto> - text items interleaved with images), present once COMPLETE. Kept
    // separately from paragraphsJson (rather than re-synthesized as one text item per paragraph)
    // specifically so a downloaded chapter's images survive for offline reading too - see
    // DownloadRepository.cachedChapter/downloadChapter and localImageFile.
    @ColumnInfo(defaultValue = "[]") val contentJson: String = "[]",
    // Paragraph-level progress while status == DOWNLOADING (each paragraph is fetched
    // individually - see DownloadRepository.downloadChapter) - lets the UI show a determinate
    // "3 of 12" ring instead of an indeterminate spinner. Meaningless once COMPLETE (by then
    // readyParagraphs == totalParagraphs) or ERROR.
    @ColumnInfo(defaultValue = "0") val readyParagraphs: Int = 0,
    @ColumnInfo(defaultValue = "0") val totalParagraphs: Int = 0,
    @ColumnInfo(defaultValue = "0") val totalBytes: Long = 0,
    @ColumnInfo(defaultValue = "0") val lastAccessedAt: Long = 0,
    // The backend's hash for this chapter (ChapterDetailDto.hash) as of the content/audio this
    // row actually holds - compared against BookManifestDto's per-chapter hashes by
    // OfflineReconciler to find what changed server-side. "" (never matches) for a row
    // downloaded before hashes existed.
    @ColumnInfo(defaultValue = "") val hash: String = "",
    // Directory holding this chapter's %05d.wav files - always
    // context.filesDir/downloads/<libraryId-hash>/<bookId>/<chapterIdx>/<voiceKey-hash>/ in
    // practice (see DownloadRepository.chapterDir), stored explicitly rather than recomputed so
    // a future storage-layout change doesn't orphan already-downloaded files.
    val localDir: String,
)
