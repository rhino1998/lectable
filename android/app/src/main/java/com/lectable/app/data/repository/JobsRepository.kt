package com.lectable.app.data.repository

import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.dto.JobsSnapshotDto
import javax.inject.Inject
import javax.inject.Singleton

/** Thin wrapper over [LectableApi]'s job-queue endpoints - mirrors client.ts. The live queue
 *  comes from the "jobs" topic ([com.lectable.app.data.live.LiveStore]), not this
 *  repository; these are the mutating actions (cancel one/all, pause/resume, restart worker)
 *  plus a one-off [snapshot] for point-in-time checks. */
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

    suspend fun pauseJobs() = api.pauseJobs()

    suspend fun resumeJobs() = api.resumeJobs()

    suspend fun restartWorker() = api.restartWorker()
}
