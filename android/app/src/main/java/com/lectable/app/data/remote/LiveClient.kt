package com.lectable.app.data.remote

import com.lectable.app.data.settings.ServerSettingsRepository
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.drop
import kotlinx.coroutines.flow.emitAll
import kotlinx.coroutines.flow.flow
import kotlinx.coroutines.flow.flowOn
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.launch
import kotlinx.serialization.DeserializationStrategy
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.int
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put
import kotlinx.serialization.serializer
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener

private const val LINGER_MS = 5_000L
private const val RECONNECT_DELAY_MS = 1_000L
private const val MAX_RECONNECT_DELAY_MS = 10_000L

/** A live topic's failure - [status] is the backend's HTTP-style status (404 "book not found"),
 *  or [NETWORK] when the backend couldn't be reached at all before any value arrived. */
data class LiveError(val status: Int, val message: String) {
    val isNetwork: Boolean get() = status == NETWORK

    companion object {
        const val NETWORK = 0
    }
}

/** One live topic's current state - loading until the first snapshot (or error) arrives, then
 *  the latest value, kept current by patches. */
data class LiveResult<out T>(
    val data: T? = null,
    val error: LiveError? = null,
    val loading: Boolean = true,
    // True on the value emitted when the configured server changes - anything held from before
    // belongs to a different backend and must be dropped, not kept as last-known (see LiveStore).
    val reset: Boolean = false,
)

/**
 * Client for the backend's live-state WebSocket (`GET /api/events` - see backend
 * internal/live and httpapi.registerLiveTopics), the Android counterpart of
 * frontend/src/api/live.ts. Every piece of server state a screen shows is a *topic*
 * ("books", "chapter", "jobs", ...) plus params picking one instance; [observe] yields the
 * topic's full value once, then a fresh value every time the backend pushes a patch -
 * whatever changed it (this app, the web UI, background generation). This replaces every
 * poll loop and "refetch after mutation" the app used to do.
 *
 * App-scoped, one socket shared by every collector (screens and ChapterDownloadWorker alike).
 * Subscriptions are ref-counted per (topic, params) and linger [LINGER_MS] after their last
 * collector goes away, so a quick re-collect (configuration change, back-and-forth navigation)
 * reuses the value instead of refetching. The socket only stays open while something is
 * subscribed; it reconnects with backoff on failure and re-points at a new backend (dropping
 * every cached value) when the server address changes in Settings.
 */
@Singleton
class LiveClient @Inject constructor(
    private val okHttpClient: OkHttpClient,
    private val serverSettingsRepository: ServerSettingsRepository,
    private val json: Json,
) {
    private class Entry(val topic: String, val params: JsonObject) {
        var refs = 0
        val state = MutableStateFlow(LiveResult<JsonElement>())
        // True between a (re)subscribe being sent and its snapshot/error arriving - a patch
        // can't apply before then.
        var awaitingSnapshot = true
        var lingerJob: Job? = null
    }

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val lock = Any()
    private val entries = mutableMapOf<String, Entry>()
    private var socket: WebSocket? = null
    private var open = false
    private var reconnectJob: Job? = null
    private var reconnectDelayMs = RECONNECT_DELAY_MS

    init {
        scope.launch {
            serverSettingsRepository.baseUrl.drop(1).distinctUntilChanged().collect { reconnectToNewServer() }
        }
    }

    /** Observes one topic instance as raw JSON - see the reified [observe] overload for the
     *  usual decoded form. Cold: subscribes when collected, releases when the collector is
     *  cancelled. */
    fun observeJson(topic: String, params: Map<String, Any> = emptyMap()): Flow<LiveResult<JsonElement>> = flow {
        val paramsJson = buildJsonObject {
            params.toSortedMap().forEach { (k, v) ->
                when (v) {
                    is Number -> put(k, v)
                    else -> put(k, v.toString())
                }
            }
        }
        val key = "$topic:$paramsJson"
        val entry = retain(key, topic, paramsJson)
        try {
            emitAll(entry.state)
        } finally {
            release(key, entry)
        }
    }

    fun <T> observe(topic: String, params: Map<String, Any>, deserializer: DeserializationStrategy<T>): Flow<LiveResult<T>> =
        observeJson(topic, params).distinctUntilChanged().map { raw ->
            val data = raw.data ?: return@map LiveResult(data = null, error = raw.error, loading = raw.loading, reset = raw.reset)
            runCatching { json.decodeFromJsonElement(deserializer, data) }.fold(
                onSuccess = { LiveResult(data = it, error = raw.error, loading = false) },
                onFailure = { e -> LiveResult(data = null, error = LiveError(500, "decode $topic: ${e.message}"), loading = false) },
            )
        }
            // A whole chapter's paragraphs (text + word timings) decode on every patch while it
            // generates - keep that off whichever (usually main) dispatcher is collecting.
            .flowOn(Dispatchers.Default)

    inline fun <reified T> observe(topic: String, params: Map<String, Any> = emptyMap()): Flow<LiveResult<T>> =
        observe(topic, params, serializer<T>())

    private fun retain(key: String, topic: String, params: JsonObject): Entry = synchronized(lock) {
        val entry = entries.getOrPut(key) {
            Entry(topic, params).also { sendSubscribeLocked(key, it) }
        }
        entry.lingerJob?.cancel()
        entry.lingerJob = null
        entry.refs++
        ensureSocketLocked()
        entry
    }

    private fun release(key: String, entry: Entry) = synchronized(lock) {
        entry.refs--
        if (entry.refs > 0) return@synchronized
        entry.lingerJob = scope.launch {
            delay(LINGER_MS)
            synchronized(lock) {
                if (entry.refs > 0 || entries[key] !== entry) return@synchronized
                entries.remove(key)
                sendLocked(buildJsonObject { put("type", "unsubscribe"); put("id", key) })
                if (entries.isEmpty()) {
                    reconnectJob?.cancel()
                    reconnectJob = null
                    socket?.close(1000, null)
                    socket = null
                    open = false
                }
            }
        }
    }

    private fun ensureSocketLocked() {
        if (socket != null || reconnectJob != null) return
        val base = serverSettingsRepository.currentBaseUrl()
        val wsUrl = base.replaceFirst("http://", "ws://").replaceFirst("https://", "wss://").trimEnd('/') + "/api/events"
        val request = runCatching { Request.Builder().url(wsUrl).build() }.getOrElse { e ->
            failPendingLocked("Invalid server address: ${e.message}")
            return
        }
        lateinit var ws: WebSocket
        ws = okHttpClient.newWebSocket(
            request,
            object : WebSocketListener() {
                override fun onOpen(webSocket: WebSocket, response: Response) = synchronized(lock) {
                    if (socket !== ws) return@synchronized
                    open = true
                    reconnectDelayMs = RECONNECT_DELAY_MS
                    for ((key, entry) in entries) {
                        entry.awaitingSnapshot = true
                        sendSubscribeLocked(key, entry)
                    }
                }

                override fun onMessage(webSocket: WebSocket, text: String) {
                    if (socket !== ws) return
                    runCatching { handle(json.parseToJsonElement(text).jsonObject) }
                }

                override fun onClosed(webSocket: WebSocket, code: Int, reason: String) = onDisconnected(ws, reason)

                override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) =
                    onDisconnected(ws, t.message ?: t.javaClass.simpleName)
            },
        )
        socket = ws
    }

    private fun onDisconnected(ws: WebSocket, reason: String) = synchronized(lock) {
        if (socket !== ws) return@synchronized
        socket = null
        open = false
        failPendingLocked("Can't reach the server: $reason")
        if (entries.isEmpty()) return@synchronized
        val wait = reconnectDelayMs
        reconnectDelayMs = (reconnectDelayMs * 2).coerceAtMost(MAX_RECONNECT_DELAY_MS)
        reconnectJob = scope.launch {
            delay(wait)
            synchronized(lock) {
                reconnectJob = null
                if (entries.isNotEmpty()) ensureSocketLocked()
            }
        }
    }

    /** Entries that never got a value report a network error (so screens can fall back to
     *  offline copies); ones that already have data keep showing it until the reconnect's
     *  fresh snapshot replaces it. */
    private fun failPendingLocked(message: String) {
        for (entry in entries.values) {
            if (entry.state.value.data == null) {
                entry.state.value = LiveResult(data = null, error = LiveError(LiveError.NETWORK, message), loading = false)
            }
        }
    }

    private fun reconnectToNewServer() = synchronized(lock) {
        socket?.close(1000, null)
        socket = null
        open = false
        reconnectJob?.cancel()
        reconnectJob = null
        reconnectDelayMs = RECONNECT_DELAY_MS
        for (entry in entries.values) {
            entry.awaitingSnapshot = true
            entry.state.value = LiveResult(reset = true)
        }
        if (entries.isNotEmpty()) ensureSocketLocked()
    }

    private fun handle(msg: JsonObject) {
        val id = msg["id"]?.jsonPrimitive?.content ?: return
        synchronized(lock) {
            val entry = entries[id] ?: return
            when (msg["type"]?.jsonPrimitive?.content) {
                "snapshot" -> {
                    entry.awaitingSnapshot = false
                    entry.state.value = LiveResult(data = msg["data"], error = null, loading = false)
                }
                "error" -> {
                    entry.awaitingSnapshot = false
                    entry.state.value = LiveResult(
                        data = null,
                        error = LiveError(msg["status"]?.jsonPrimitive?.int ?: 500, msg["error"]?.jsonPrimitive?.content ?: "error"),
                        loading = false,
                    )
                }
                "patch" -> {
                    val current = entry.state.value.data
                    if (entry.awaitingSnapshot || current == null) return
                    val ops = msg["ops"]?.jsonArray ?: return
                    val next = runCatching { applyLiveOps(current, ops) }.getOrElse {
                        // Our copy diverged somehow - resubscribing gets a fresh snapshot rather
                        // than compounding the error.
                        entry.awaitingSnapshot = true
                        sendSubscribeLocked(id, entry)
                        return
                    }
                    entry.state.value = entry.state.value.copy(data = next)
                }
            }
        }
    }

    private fun sendSubscribeLocked(key: String, entry: Entry) {
        sendLocked(
            buildJsonObject {
                put("type", "subscribe")
                put("id", key)
                put("topic", entry.topic)
                put("params", entry.params)
            },
        )
    }

    private fun sendLocked(msg: JsonObject) {
        // Not open yet: onOpen (re)sends every entry's subscribe itself.
        if (open) socket?.send(msg.toString())
    }
}

/**
 * Applies backend live patch ops (see backend internal/live.Op) to [doc]. JsonObject/JsonArray
 * are immutable, so only the containers along each changed path are copied - untouched
 * subtrees stay the same instances.
 */
fun applyLiveOps(doc: JsonElement, ops: JsonArray): JsonElement {
    var out = doc
    for (op in ops) {
        val o = op.jsonObject
        val path = o["path"]?.jsonArray ?: JsonArray(emptyList())
        out = applyAt(out, o, path, 0)
    }
    return out
}

private fun applyAt(node: JsonElement, op: JsonObject, path: JsonArray, depth: Int): JsonElement {
    val kind = op["op"]!!.jsonPrimitive.content
    if (depth == path.size) {
        return when (kind) {
            "set" -> op["value"]!!
            "arr" -> {
                val prev = node.jsonArray
                val items = mutableListOf<JsonElement>()
                for (item in op["items"]!!.jsonArray) {
                    if (item is JsonArray) {
                        val start = item[0].jsonPrimitive.int
                        val end = item[1].jsonPrimitive.int
                        for (i in start until end) items += prev[i]
                    } else {
                        items += item.jsonObject["v"]!!
                    }
                }
                JsonArray(items)
            }
            else -> error("live: $kind op with empty path")
        }
    }
    val key = path[depth].jsonPrimitive
    return when (node) {
        is JsonArray -> {
            val i = key.int
            JsonArray(node.toMutableList().also { it[i] = applyAt(node[i], op, path, depth + 1) })
        }
        is JsonObject -> {
            val k = key.content
            if (kind == "del" && depth == path.size - 1) {
                JsonObject(node - k)
            } else {
                JsonObject(node.toMutableMap().also { it[k] = applyAt(node[k] ?: JsonNull, op, path, depth + 1) })
            }
        }
        else -> error("live: path through a scalar")
    }
}
