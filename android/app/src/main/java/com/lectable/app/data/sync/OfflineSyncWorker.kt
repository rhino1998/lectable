package com.lectable.app.data.sync

import android.content.Context
import androidx.hilt.work.HiltWorker
import androidx.work.CoroutineWorker
import androidx.work.WorkerParameters
import dagger.assisted.Assisted
import dagger.assisted.AssistedInject
import java.io.IOException
import kotlinx.coroutines.CancellationException
import retrofit2.HttpException

/** Runs one [OfflineReconciler.reconcile] pass as background work - see
 *  [OfflineReconciler.startAutoSync] for when. An unreachable backend (the phone is on some
 *  other network) or a server error retries with backoff; anything else fails the attempt. */
@HiltWorker
class OfflineSyncWorker @AssistedInject constructor(
    @Assisted context: Context,
    @Assisted params: WorkerParameters,
    private val reconciler: OfflineReconciler,
) : CoroutineWorker(context, params) {

    override suspend fun doWork(): Result = try {
        reconciler.reconcile()
        Result.success()
    } catch (e: CancellationException) {
        throw e
    } catch (e: IOException) {
        Result.retry()
    } catch (e: HttpException) {
        if (e.code() >= 500) Result.retry() else Result.failure()
    }
}
