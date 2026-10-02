package com.lectable.app.playback

import android.content.Context
import androidx.hilt.work.HiltWorker
import androidx.work.BackoffPolicy
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import androidx.work.workDataOf
import com.lectable.app.data.repository.DownloadRepository
import dagger.assisted.Assisted
import dagger.assisted.AssistedInject
import java.util.concurrent.TimeUnit
import kotlinx.coroutines.CancellationException

/**
 * Runs [DownloadRepository.downloadBookAmbience] for one book: gets its downloaded chapters'
 * background music and lighting cues onto the device, asking the backend to produce whatever
 * doesn't exist yet. One per book rather than per chapter - it mostly waits on the backend, and
 * a waiting worker per chapter would crowd out the narration downloads themselves.
 */
@HiltWorker
class AmbienceDownloadWorker @AssistedInject constructor(
    @Assisted context: Context,
    @Assisted params: WorkerParameters,
    private val downloadRepository: DownloadRepository,
) : CoroutineWorker(context, params) {

    override suspend fun doWork(): Result {
        val bookId = inputData.getString(KEY_BOOK_ID) ?: return Result.failure()
        val settled = try {
            downloadRepository.downloadBookAmbience(bookId)
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            false
        }
        // Whatever is still missing after the last attempt is picked up by a later run - every
        // offline sync and every finished chapter download starts one.
        return if (settled || runAttemptCount >= MAX_ATTEMPTS) Result.success() else Result.retry()
    }

    companion object {
        private const val KEY_BOOK_ID = "bookId"
        private const val MAX_ATTEMPTS = 6
        private const val RETRY_BACKOFF_SECONDS = 60L

        /** Starts this book's ambience download unless one is already queued or running - which
         *  re-reads the downloaded chapters as it goes, so it covers ones finished meanwhile. */
        fun enqueue(workManager: WorkManager, bookId: String) {
            val request = OneTimeWorkRequestBuilder<AmbienceDownloadWorker>()
                .setInputData(workDataOf(KEY_BOOK_ID to bookId))
                .setConstraints(Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build())
                .setBackoffCriteria(BackoffPolicy.EXPONENTIAL, RETRY_BACKOFF_SECONDS, TimeUnit.SECONDS)
                .build()
            workManager.enqueueUniqueWork("ambience-$bookId", ExistingWorkPolicy.KEEP, request)
        }
    }
}
