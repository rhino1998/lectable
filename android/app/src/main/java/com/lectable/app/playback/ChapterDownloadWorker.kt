package com.lectable.app.playback

import android.content.Context
import androidx.hilt.work.HiltWorker
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import androidx.work.workDataOf
import com.lectable.app.data.repository.DownloadRepository
import dagger.assisted.Assisted
import dagger.assisted.AssistedInject
import kotlinx.coroutines.CancellationException

/**
 * Runs one chapter's [DownloadRepository.downloadChapter] as background work - one enqueued
 * request per chapter of a manual "download this book"/"download chapter" action - see
 * ReaderViewModel/LibraryViewModel.
 */
@HiltWorker
class ChapterDownloadWorker @AssistedInject constructor(
    @Assisted context: Context,
    @Assisted params: WorkerParameters,
    private val downloadRepository: DownloadRepository,
) : CoroutineWorker(context, params) {

    override suspend fun doWork(): Result {
        val bookId = inputData.getString(KEY_BOOK_ID) ?: return Result.failure()
        val chapterIdx = inputData.getInt(KEY_CHAPTER_IDX, -1)
        if (chapterIdx < 0) return Result.failure()
        val voiceKey = inputData.getString(KEY_VOICE_KEY) ?: return Result.failure()
        val pinned = inputData.getBoolean(KEY_PINNED, false)

        return try {
            downloadRepository.downloadChapter(bookId, chapterIdx, voiceKey, pinned) { ready, total ->
                setProgressAsync(workDataOf(KEY_READY to ready, KEY_TOTAL to total))
            }
            Result.success()
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            Result.failure(workDataOf(KEY_ERROR to e.message))
        }
    }

    companion object {
        const val KEY_BOOK_ID = "bookId"
        const val KEY_CHAPTER_IDX = "chapterIdx"
        const val KEY_VOICE_KEY = "voiceKey"
        const val KEY_PINNED = "pinned"
        const val KEY_READY = "ready"
        const val KEY_TOTAL = "total"
        const val KEY_ERROR = "error"

        fun inputData(bookId: String, chapterIdx: Int, voiceKey: String, pinned: Boolean) = workDataOf(
            KEY_BOOK_ID to bookId,
            KEY_CHAPTER_IDX to chapterIdx,
            KEY_VOICE_KEY to voiceKey,
            KEY_PINNED to pinned,
        )

        /** Unique work name for a manual whole-book download - one chapter's worker per name
         *  suffix, chained so WorkManager can report/cancel the set together. */
        fun bookDownloadWorkName(bookId: String, chapterIdx: Int): String = "download-$bookId-$chapterIdx"

        /** Common tag across a manual whole-book download's per-chapter work requests, so
         *  ReaderViewModel can observe aggregate progress via WorkManager.getWorkInfosByTagFlow
         *  instead of tracking N individual work ids itself. */
        fun bookDownloadTag(bookId: String): String = "download-book-$bookId"
    }
}
