package com.lectable.app.data.download

import androidx.work.WorkManager
import com.lectable.app.data.remote.BackendIdentityRepository
import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.repository.DownloadRepository
import com.lectable.app.playback.ChapterDownloadWorker
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.withContext

/**
 * "Remove downloaded copy" for a book - takes the book back off this device *and* stops
 * everything a download of it set going, so it stays removed:
 *  - its [ChapterDownloadWorker]s (a whole-book download queues one per chapter, and the
 *    offline sync's refreshes share their unique names) are cancelled, since one still queued
 *    or running would write the chapter straight back after the delete;
 *  - its ambience worker, which asks the backend to score/render music for downloaded chapters;
 *  - the backend's chapter-generation jobs those downloads asked for (each
 *    ChapterDownloadWorker starts with POST .../chapters/{idx}/generate), so the job queue
 *    isn't left rendering a book nobody wants offline any more. Only chapters this device was
 *    still downloading are cancelled - a chapter's generation asked for from anywhere else is
 *    left alone.
 * Cancelling backend jobs is best-effort: offline, the local copy is removed all the same.
 */
@Singleton
class BookDownloadRemover @Inject constructor(
    private val api: LectableApi,
    private val workManager: WorkManager,
    private val chapterDao: DownloadedChapterDao,
    private val bookDao: DownloadedBookDao,
    private val backendIdentity: BackendIdentityRepository,
    private val downloadRepository: DownloadRepository,
) {
    suspend fun remove(bookId: String) = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        // Read before cancelling: once cancelled, the work no longer says which chapters were
        // still being downloaded.
        val generationJobs = runCatching {
            api.jobsSnapshot(bookId = bookId, kind = GENERATE_CHAPTER_KIND)
                .let { it.inFlight + it.queued }
                .filter { it.bookId == bookId }
        }.getOrDefault(emptyList())
        val unfinished = chapterDao.forBook(libraryId, bookId)
            .filter { it.status != DownloadStatus.COMPLETE }
            .mapTo(mutableSetOf()) { it.chapterIdx }
        for (job in generationJobs) {
            val infos = workManager.getWorkInfosForUniqueWorkFlow(
                ChapterDownloadWorker.bookDownloadWorkName(bookId, job.chapterIdx),
            ).first()
            if (infos.any { !it.state.isFinished }) unfinished += job.chapterIdx
        }

        workManager.cancelAllWorkByTag(ChapterDownloadWorker.bookDownloadTag(bookId))
        for (row in chapterDao.forBook(libraryId, bookId)) {
            workManager.cancelUniqueWork(ChapterDownloadWorker.bookDownloadWorkName(bookId, row.chapterIdx))
        }
        workManager.cancelUniqueWork(ambienceWorkName(bookId))

        downloadRepository.deleteBookDownloads(bookId)

        for (job in generationJobs) {
            if (job.chapterIdx in unfinished) runCatching { api.cancelJob(job.id) }
        }
    }

    /** Every downloaded book on the current library - Settings' "Delete all downloads". */
    suspend fun removeAll() = withContext(Dispatchers.IO) {
        bookDao.bookIdsForLibrary(backendIdentity.currentLibraryId()).forEach { remove(it) }
    }

    private companion object {
        // jobs.Manager.EnqueueChapter's group kind - what POST .../chapters/{idx}/generate queues.
        const val GENERATE_CHAPTER_KIND = "pipeline_generate_chapter"

        fun ambienceWorkName(bookId: String) = "ambience-$bookId"
    }
}
