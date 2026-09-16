package com.lectable.app.data.remote

import com.lectable.app.data.settings.ServerSettingsRepository
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.filterNotNull
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.launchIn
import kotlinx.coroutines.flow.onEach

/**
 * Resolves and caches the currently-configured backend's stable identity (see
 * backend/internal/httpapi/instance.go), so locally-cached data (offline downloads - see
 * DownloadRepository) can be scoped per-backend rather than just per-book-id, which alone is
 * only unique within one backend's own database - the app can be pointed at different backends
 * over its lifetime (see android/CLAUDE.md's "Server address").
 *
 * Deliberately a separate class from [ServerSettingsRepository] rather than folding this in
 * there: [ServerSettingsRepository] is a dependency of [DynamicBaseUrlInterceptor], which
 * [LectableApi]'s OkHttpClient is built from - giving ServerSettingsRepository its own
 * LectableApi dependency to fetch this would create a circular Dagger graph
 * (ServerSettingsRepository -> LectableApi -> OkHttpClient -> DynamicBaseUrlInterceptor ->
 * ServerSettingsRepository). Sitting a layer above both avoids that entirely.
 */
@Singleton
class BackendIdentityRepository @Inject constructor(
    private val api: LectableApi,
    serverSettingsRepository: ServerSettingsRepository,
) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    // Null until resolved (or between a base-URL change and the next successful fetch) - callers
    // that need a synchronous, non-suspending read (e.g. ParagraphPlayer.playParagraph, which
    // can't suspend) treat null the same as "no local file available yet", falling back to
    // streaming exactly like an unresolved bookId/voiceKey already does there.
    private val _libraryId = MutableStateFlow<String?>(null)
    val libraryId: StateFlow<String?> = _libraryId

    // "" if the current backend never set LIBRARY_NAME (or hasn't resolved yet) - purely
    // cosmetic (see InstanceDto), for a Settings-screen-style "which backend is this" label.
    private val _libraryName = MutableStateFlow("")
    val libraryName: StateFlow<String> = _libraryName

    init {
        // Re-resolves whenever the configured backend changes, including the very first value
        // once ServerSettingsRepository.load() has run (see LectableApplication.onCreate) -
        // StateFlow already only emits on actual change (no distinctUntilChanged needed), but a
        // baseUrl that hasn't changed since app start still gets one fetch here regardless (this
        // collector's first emission), which is fine: it's one cheap request, and confirms the
        // cached value is still valid for whatever's actually running there now.
        serverSettingsRepository.baseUrl
            .onEach { refresh() }
            .launchIn(scope)
    }

    private suspend fun refresh() {
        _libraryId.value = null
        _libraryName.value = ""
        runCatching { api.getInstance() }.onSuccess {
            _libraryId.value = it.id
            _libraryName.value = it.name
        }
    }

    /** Suspends until the current backend's library id is known - for callers that can suspend
     *  (most of DownloadRepository). A backend that's genuinely unreachable just leaves this
     *  suspended until connectivity returns and [refresh] (re-triggered by the next baseUrl
     *  change, or a fresh app start) succeeds. */
    suspend fun currentLibraryId(): String = libraryId.filterNotNull().first()
}
