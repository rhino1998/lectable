package com.lectable.app.data.download

import androidx.room.ColumnInfo
import androidx.room.Entity

/**
 * A reading position [ReaderViewModel][com.lectable.app.ui.reader.ReaderViewModel] couldn't
 * report to the server yet (no network - see DownloadRepository.savePendingPosition), queued
 * here instead of just being silently dropped. Keyed by (libraryId, bookId) - only the latest
 * position matters, so a newer save simply overwrites this row rather than accumulating a
 * history. Synced (and then deleted) the next time the book is opened with a reachable backend -
 * see ReaderViewModel.loadBook - and also used directly as the *offline* resume point itself,
 * since without it a book opened while still offline would otherwise always restart from its
 * first downloaded chapter (DownloadRepository.cachedBook carries no position of its own).
 */
@Entity(tableName = "pending_positions", primaryKeys = ["libraryId", "bookId"])
data class PendingPosition(
    @ColumnInfo(defaultValue = "") val libraryId: String,
    val bookId: String,
    val chapterIdx: Int,
    val paragraphIdx: Int,
    val seconds: Double,
)
