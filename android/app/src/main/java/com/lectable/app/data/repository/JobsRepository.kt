package com.lectable.app.data.repository

import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.dto.JobTier
import com.lectable.app.data.remote.dto.JobTierRequestDto
import com.lectable.app.data.remote.dto.JobsSnapshotDto
import javax.inject.Inject
import javax.inject.Singleton

/** Thin wrapper over [LectableApi]'s job-queue endpoints - mirrors client.ts. The live queue
 *  comes from the "jobsQueue" topic ([com.lectable.app.data.live.LiveStore]), not this
 *  repository; these are the mutating actions (cancel one/all, raise priority, pause/resume, restart worker)
 *  plus a one-off [snapshot] for point-in-time checks. */
@Singleton
class JobsRepository @Inject constructor(
    private val api: LectableApi,
) {
    /** One book's queued and in-flight tasks of one kind - never the whole queue, which can
     *  be tens of megabytes. */
    suspend fun snapshot(bookId: String, kind: String): JobsSnapshotDto = api.jobsSnapshot(bookId = bookId, kind = kind)

    suspend fun cancelJob(id: String) {
        api.cancelJob(id)
    }

    /** Raises a task's priority; the backend refuses a tier that isn't more urgent than its own. */
    suspend fun setJobTier(id: String, tier: JobTier) {
        api.setJobTier(id, JobTierRequestDto(tier = tier))
    }

    /** Returns how many tasks were actually canceled, for the confirming snackbar. */
    suspend fun cancelAllJobs(): Int = api.cancelAllJobs().canceled

    suspend fun pauseJobs() = api.pauseJobs()

    suspend fun resumeJobs() = api.resumeJobs()

    suspend fun restartWorker() = api.restartWorker()
}
