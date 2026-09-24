package com.lectable.app.data.sync

import androidx.work.BackoffPolicy
import androidx.work.Constraints
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import com.lectable.app.data.download.DownloadStatus
import com.lectable.app.data.download.DownloadedBookDao
import com.lectable.app.data.download.DownloadedChapterDao
import com.lectable.app.data.download.mergedWith
import com.lectable.app.data.remote.BackendIdentityRepository
import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.LiveClient
import com.lectable.app.data.repository.DownloadRepository
import com.lectable.app.data.repository.voiceKeyOf
import com.lectable.app.playback.ChapterDownloadWorker
import java.io.IOException
import java.util.concurrent.TimeUnit
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.filter
import kotlinx.coroutines.flow.launchIn
import kotlinx.coroutines.flow.onEach
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import retrofit2.HttpException

/**
 * Brings the offline download cache back in line with the backend after the phone was away:
 * whatever changed server-side meanwhile (a book deleted, renamed or re-covered, chapters
 * re-attributed/re-annotated, paragraphs regenerated, the book's voice or clone model changed,
 * the reading position moved on another device) replaces the local copy - the backend always
 * wins, nothing local is merged back.
 *
 * Walks the backend's offline-sync hash tree (backend httpapi/manifest.go) top-down so an
 * unchanged book costs one request:
 *  1. `GET /api/books` - a downloaded book missing from it was deleted; drop its downloads.
 *  2. Per downloaded book, `GET .../manifest?chapters=<downloaded>` - the root hash matching
 *     DownloadedBook.hash means nothing downloaded changed; only the position is taken.
 *  3. Otherwise per downloaded chapter, its hash vs DownloadedChapter.hash; a mismatch fetches
 *     just that chapter, applies its text/annotations right away ([mergedWith]) and - only if
 *     some paragraph's audioHash moved - queues a [ChapterDownloadWorker] refresh, which
 *     re-fetches only those paragraphs' clips (see DownloadRepository.downloadChapter).
 * The root is recorded only once every chapter matches, so a book with refreshes still in
 * flight is simply re-checked (cheaply, chapter by chapter) next time.
 *
 * Runs as [OfflineSyncWorker]: whenever [LiveClient] reconnects (the phone is back on the
 * backend's network) and periodically in the background - see [startAutoSync].
 */
@Singleton
class OfflineReconciler @Inject constructor(
    private val api: LectableApi,
    private val chapterDao: DownloadedChapterDao,
    private val bookDao: DownloadedBookDao,
    private val downloadRepository: DownloadRepository,
    private val backendIdentity: BackendIdentityRepository,
    private val liveClient: LiveClient,
    private val workManager: WorkManager,
    private val json: Json,
) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    /** Schedules reconciles for the app's lifetime - called once from LectableApplication. */
    fun startAutoSync() {
        workManager.enqueueUniquePeriodicWork(
            PERIODIC_WORK_NAME,
            ExistingPeriodicWorkPolicy.KEEP,
            PeriodicWorkRequestBuilder<OfflineSyncWorker>(PERIODIC_INTERVAL_HOURS, TimeUnit.HOURS)
                .setConstraints(networkConstraint)
                .build(),
        )
        liveClient.connected.filter { it }.onEach { requestSync() }.launchIn(scope)
    }

    /** Queues a reconcile now. REPLACE rather than KEEP: a previous request still waiting out
     *  its retry backoff shouldn't delay this one now that the backend is known reachable, and
     *  a reconcile interrupted part-way is harmless - every step is idempotent. */
    fun requestSync() {
        workManager.enqueueUniqueWork(
            ONE_SHOT_WORK_NAME,
            ExistingWorkPolicy.REPLACE,
            OneTimeWorkRequestBuilder<OfflineSyncWorker>()
                .setConstraints(networkConstraint)
                .setBackoffCriteria(BackoffPolicy.EXPONENTIAL, RETRY_BACKOFF_SECONDS, TimeUnit.SECONDS)
                .build(),
        )
    }

    /** One full pass over every downloaded book on the current backend. Throws (IOException/
     *  HttpException) if the backend can't be reached, leaving everything local untouched. */
    suspend fun reconcile() = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.resolveLibraryId() ?: throw IOException("Can't reach the server")
        val downloaded = bookDao.bookIdsForLibrary(libraryId)
        if (downloaded.isEmpty()) return@withContext
        val live = api.listBooks().mapTo(mutableSetOf()) { it.id }
        for (bookId in downloaded) {
            if (bookId !in live) {
                downloadRepository.deleteBookDownloads(bookId)
                continue
            }
            reconcileBook(libraryId, bookId)
        }
    }

    private suspend fun reconcileBook(libraryId: String, bookId: String) {
        val book = bookDao.get(libraryId, bookId) ?: return
        // Only COMPLETE chapters: one still DOWNLOADING is fetching current state already, and
        // an ERROR one is the user's to retry.
        val rows = chapterDao.forBook(libraryId, bookId).filter { it.status == DownloadStatus.COMPLETE }
        if (rows.isEmpty()) return
        val manifest = try {
            api.getBookManifest(bookId, rows.joinToString(",") { it.chapterIdx.toString() })
        } catch (e: HttpException) {
            // Never deletes on a 404 here - listBooks above is the only authority on a book
            // being gone (next pass catches one deleted just now), and a backend too old to
            // have this endpoint 404s every book.
            if (e.code() == HTTP_NOT_FOUND) return
            throw e
        }

        var inSync = true
        if (manifest.hash != book.hash) {
            // Title/author/cover - cheap next to chapter work, so just refetched whenever the
            // root moved rather than tracked as a separate node.
            runCatching { downloadRepository.cacheBookMetadata(bookId) }
            val remote = manifest.chapters.associate { it.idx to it.hash }
            var voiceKey: String? = null
            for (row in rows) {
                val remoteHash = remote[row.chapterIdx]
                if (remoteHash == null) {
                    // The chapter no longer exists server-side.
                    downloadRepository.deleteDownload(bookId, row.chapterIdx)
                    continue
                }
                if (remoteHash == row.hash) continue
                val chapter = api.getChapter(bookId, row.chapterIdx)
                val merged = row.mergedWith(chapter, json)
                if (merged != row) chapterDao.upsert(merged)
                val key = voiceKey ?: api.getVoice(bookId).let { voiceKeyOf(it.presetId, it.instruct, it.language) }.also { voiceKey = it }
                // Only the text/annotations moved - the merge above already brought it in sync.
                if (merged.hash == chapter.hash && row.voiceKey == key) continue
                inSync = false
                enqueueRefresh(bookId, row.chapterIdx, key)
            }
        }

        // Re-read: cacheBookMetadata above may have replaced the row.
        val current = bookDao.get(libraryId, bookId) ?: return
        val updated = current.copy(
            hash = if (inSync) manifest.hash else current.hash,
            posChapterIdx = manifest.posChapterIdx,
            posParagraphIdx = manifest.posParagraphIdx,
            posSeconds = manifest.posSeconds,
        )
        if (updated != current) bookDao.upsert(updated)
    }

    /** Same unique work name as a manual download of this chapter, with KEEP - a download the
     *  user already has in flight is fetching current state anyway. */
    private fun enqueueRefresh(bookId: String, chapterIdx: Int, voiceKey: String) {
        val request = OneTimeWorkRequestBuilder<ChapterDownloadWorker>()
            .setInputData(ChapterDownloadWorker.inputData(bookId, chapterIdx, voiceKey, pinned = true))
            .setConstraints(networkConstraint)
            .addTag(REFRESH_TAG)
            .build()
        workManager.enqueueUniqueWork(ChapterDownloadWorker.bookDownloadWorkName(bookId, chapterIdx), ExistingWorkPolicy.KEEP, request)
    }

    private companion object {
        const val ONE_SHOT_WORK_NAME = "offline-sync"
        const val PERIODIC_WORK_NAME = "offline-sync-periodic"
        const val REFRESH_TAG = "offline-refresh"
        // Keeps downloads fresh while the app sits in the background on the home network, so a
        // phone that leaves it carries recent state even if the app wasn't opened first.
        const val PERIODIC_INTERVAL_HOURS = 6L
        const val RETRY_BACKOFF_SECONDS = 30L
        const val HTTP_NOT_FOUND = 404

        val networkConstraint: Constraints = Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build()
    }
}
