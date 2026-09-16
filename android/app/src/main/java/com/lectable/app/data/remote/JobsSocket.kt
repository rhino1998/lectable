package com.lectable.app.data.remote

import com.lectable.app.data.remote.dto.JobsSnapshotDto
import com.lectable.app.data.settings.ServerSettingsRepository
import dagger.hilt.android.scopes.ViewModelScoped
import javax.inject.Inject
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.serialization.decodeFromString
import kotlinx.serialization.json.Json
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener

private const val RECONNECT_DELAY_MS = 2000L

/**
 * The Android analogue of frontend/src/hooks/useJobsUpdates.ts: subscribes to the backend's
 * shared job queue over `GET /api/jobs/ws` (backend: jobs.Manager.SubscribeChanges), replacing
 * polling `GET /api/jobs` on an interval. Unlike [BookUpdatesSocket] (which only ever pushes
 * incremental per-paragraph patches), every message here already *is* the complete current
 * state - sent once immediately on connect and again after every change - so there's nothing to
 * merge; [JobsViewModel][com.lectable.app.ui.jobs.JobsViewModel] just replaces its state
 * wholesale on each one. `@ViewModelScoped`, one connection per mounted Jobs screen - auto-
 * reconnecting on close/failure like every other socket in this app.
 */
@ViewModelScoped
class JobsSocket @Inject constructor(
    private val okHttpClient: OkHttpClient,
    private val serverSettingsRepository: ServerSettingsRepository,
    private val json: Json,
) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private var webSocket: WebSocket? = null
    private var reconnectJob: Job? = null
    private var stopped = true

    var onSnapshot: ((JobsSnapshotDto) -> Unit)? = null

    fun connect() {
        stopped = false
        openSocket()
    }

    private fun openSocket() {
        val base = serverSettingsRepository.currentBaseUrl()
        val wsUrl = base.replaceFirst("http://", "ws://").replaceFirst("https://", "wss://")
            .trimEnd('/') + "/api/jobs/ws"
        val request = Request.Builder().url(wsUrl).build()
        webSocket = okHttpClient.newWebSocket(
            request,
            object : WebSocketListener() {
                override fun onMessage(webSocket: WebSocket, text: String) {
                    runCatching { json.decodeFromString<JobsSnapshotDto>(text) }
                        .onSuccess { onSnapshot?.invoke(it) }
                }

                override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                    scheduleReconnect()
                }

                override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                    scheduleReconnect()
                }
            },
        )
    }

    private fun scheduleReconnect() {
        if (stopped) return
        reconnectJob?.cancel()
        reconnectJob = scope.launch {
            delay(RECONNECT_DELAY_MS)
            if (!stopped) openSocket()
        }
    }

    fun close() {
        stopped = true
        reconnectJob?.cancel()
        webSocket?.close(1000, null)
        webSocket = null
    }

    fun release() {
        close()
        scope.cancel()
    }
}
