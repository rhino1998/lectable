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
import kotlinx.coroutines.launch

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
    private val serverSettingsRepository: ServerSettingsRepository,
) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    // The id this backend reported - or, until it answers, the one it reported last time (see
    // ServerSettingsRepository.knownLibraryId), so downloads can be read while it's still
    // loading or unreachable. Null only for a backend that has never been reached at its
    // current URL - callers that need a synchronous, non-suspending read (e.g.
    // DownloadRepository.localAudioFile) treat that the same as "nothing downloaded".
    private val _libraryId = MutableStateFlow<String?>(null)
    val libraryId: StateFlow<String?> = _libraryId

    // Whether [libraryId] came from the backend itself on this launch, not just remembered.
    @Volatile private var confirmed = false

    // "" if the current backend never set LECTABLE_LIBRARY_NAME (or hasn't resolved yet) - purely
    // cosmetic (see InstanceDto), for a Settings-screen-style "which backend is this" label.
    private val _libraryName = MutableStateFlow("")
    val libraryName: StateFlow<String> = _libraryName

    init {
        // Re-resolves whenever the configured backend changes. A baseUrl that hasn't changed
        // since the last launch still gets one fetch here (this collector's first emission),
        // which is fine: it's one cheap request, and confirms the remembered id is still valid
        // for whatever's actually running there now.
        scope.launch {
            // This is constructed before LectableApplication.onCreate loads the stored URL and
            // remembered id - see ServerSettingsRepository.awaitLoaded.
            serverSettingsRepository.awaitLoaded()
            serverSettingsRepository.baseUrl.collect { refresh() }
        }
    }

    private suspend fun refresh() {
        val baseUrl = serverSettingsRepository.currentBaseUrl()
        confirmed = false
        _libraryId.value = serverSettingsRepository.knownLibraryId(baseUrl)
        _libraryName.value = ""
        runCatching { api.getInstance() }.onSuccess {
            _libraryId.value = it.id
            _libraryName.value = it.name
            confirmed = true
            if (serverSettingsRepository.currentBaseUrl() == baseUrl) serverSettingsRepository.rememberLibraryId(baseUrl, it.id)
        }
    }

    /** Suspends until the current backend's library id is known - for callers that can suspend
     *  and have something to write (most of DownloadRepository). Immediate once the backend has
     *  ever been reached at this URL; one that never has leaves this suspended until [refresh]
     *  (re-triggered by the next baseUrl change, [resolveLibraryId], or a fresh app start)
     *  succeeds. */
    suspend fun currentLibraryId(): String = libraryId.filterNotNull().first()

    /** The current library id as the backend itself reports it, fetching it now if it hasn't
     *  answered yet on this launch - unlike [currentLibraryId], never waits for someone else to
     *  resolve it (an app started offline never re-runs [refresh] on its own) and never settles
     *  for the remembered id: OfflineReconciler deletes downloads the backend no longer has, so
     *  it must know which backend is actually there. Null if it still can't be reached. */
    suspend fun resolveLibraryId(): String? {
        if (!confirmed) refresh()
        return libraryId.value.takeIf { confirmed }
    }
}
