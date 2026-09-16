package com.lectable.app.data.remote

import com.lectable.app.data.remote.dto.ParagraphUpdateDto
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
 * The Android analogue of frontend/src/hooks/useBookUpdates.ts: subscribes to real-time
 * paragraph status pushes for a book over `GET /api/books/{id}/ws` (backend: wshub), instead
 * of polling `GET /api/books/{id}/chapters/{idx}` on an interval to notice a paragraph go
 * pending -> generating -> ready. One connection per mounted reader (this is
 * `@ViewModelScoped`, same as [com.lectable.app.playback.ParagraphPlayer]), auto-reconnecting
 * on close/failure like the web version.
 */
@ViewModelScoped
class BookUpdatesSocket @Inject constructor(
    private val okHttpClient: OkHttpClient,
    private val serverSettingsRepository: ServerSettingsRepository,
    private val json: Json,
) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private var webSocket: WebSocket? = null
    private var reconnectJob: Job? = null
    private var stopped = true

    var onUpdate: ((ParagraphUpdateDto) -> Unit)? = null

    fun connect(bookId: String) {
        stopped = false
        openSocket(bookId)
    }

    private fun openSocket(bookId: String) {
        val base = serverSettingsRepository.currentBaseUrl()
        val wsUrl = base.replaceFirst("http://", "ws://").replaceFirst("https://", "wss://")
            .trimEnd('/') + "/api/books/$bookId/ws"
        val request = Request.Builder().url(wsUrl).build()
        webSocket = okHttpClient.newWebSocket(
            request,
            object : WebSocketListener() {
                override fun onMessage(webSocket: WebSocket, text: String) {
                    runCatching { json.decodeFromString<ParagraphUpdateDto>(text) }
                        .onSuccess { onUpdate?.invoke(it) }
                }

                override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                    scheduleReconnect(bookId)
                }

                override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                    scheduleReconnect(bookId)
                }
            },
        )
    }

    private fun scheduleReconnect(bookId: String) {
        if (stopped) return
        reconnectJob?.cancel()
        reconnectJob = scope.launch {
            delay(RECONNECT_DELAY_MS)
            if (!stopped) openSocket(bookId)
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
