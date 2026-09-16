package com.lectable.app.data.repository

import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.dto.JobsSnapshotDto
import javax.inject.Inject
import javax.inject.Singleton

/** Thin wrapper over [LectableApi]'s job-queue endpoints - mirrors client.ts. Live updates come
 *  from [com.lectable.app.data.remote.JobsSocket], not this repository; these are just the
 *  fallback initial snapshot and the mutating actions (cancel one/all, pause/resume, restart
 *  worker). */
@Singleton
class JobsRepository @Inject constructor(
    private val api: LectableApi,
) {
    suspend fun snapshot(): JobsSnapshotDto = api.jobsSnapshot()

    suspend fun cancelJob(id: String) {
        api.cancelJob(id)
    }

    /** Returns how many tasks were actually canceled, for the confirming snackbar. */
    suspend fun cancelAllJobs(): Int = api.cancelAllJobs().canceled

    suspend fun pauseJobs(): Boolean = api.pauseJobs().paused

    suspend fun resumeJobs(): Boolean = api.resumeJobs().paused

    suspend fun restartWorker(): Boolean = api.restartWorker().restarted
}
