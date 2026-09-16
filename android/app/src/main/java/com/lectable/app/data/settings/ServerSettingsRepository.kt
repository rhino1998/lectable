package com.lectable.app.data.settings

import android.content.Context
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import com.lectable.app.BuildConfig
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.runBlocking

private val Context.settingsDataStore by preferencesDataStore(name = "settings")
private val SERVER_URL_KEY = stringPreferencesKey("server_url")

/**
 * The backend is a self-hosted, single-user service on the user's own
 * network (no auth, no service discovery) - so unlike the web frontend,
 * which gets its origin for free from Vite's dev proxy, the app needs the
 * user to type in where it lives. Persisted via DataStore and exposed as a
 * hot [StateFlow] so [com.lectable.app.data.remote.DynamicBaseUrlInterceptor]
 * can read the current value on every request without suspending.
 */
@Singleton
class ServerSettingsRepository @Inject constructor(
    @ApplicationContext private val context: Context,
) {
    private val dataStore get() = context.settingsDataStore

    private val _baseUrl = MutableStateFlow(BuildConfig.DEFAULT_SERVER_URL)
    val baseUrl: StateFlow<String> = _baseUrl

    suspend fun load() {
        val stored = dataStore.data.map { it[SERVER_URL_KEY] }.first()
        if (stored != null) _baseUrl.value = stored
    }

    /** Whether a server URL has ever actually been saved - by hand in Settings, by picking a
     *  discovered one there, or by LibraryViewModel's own first-launch auto-discovery - as
     *  opposed to [baseUrl] just still holding [BuildConfig.DEFAULT_SERVER_URL] because nothing
     *  has been. */
    suspend fun isConfigured(): Boolean = dataStore.data.map { it[SERVER_URL_KEY] }.first() != null

    suspend fun setBaseUrl(url: String) {
        val normalized = normalize(url)
        dataStore.edit { it[SERVER_URL_KEY] = normalized }
        _baseUrl.value = normalized
    }

    /** Synchronous snapshot for the interceptor - always already loaded by the time requests fire. */
    fun currentBaseUrl(): String = _baseUrl.value

    companion object {
        fun normalize(url: String): String {
            val trimmed = url.trim()
            val withScheme = if (trimmed.startsWith("http://") || trimmed.startsWith("https://")) {
                trimmed
            } else {
                "http://$trimmed"
            }
            return if (withScheme.endsWith("/")) withScheme else "$withScheme/"
        }

        fun isValid(url: String): Boolean = runCatching {
            val normalized = normalize(url)
            val parsed = java.net.URI(normalized)
            parsed.host != null && parsed.host.isNotBlank()
        }.getOrDefault(false)
    }
}

/** Blocking variant only for Application.onCreate's synchronous DataStore preload - see LectableApplication. */
fun ServerSettingsRepository.loadBlocking() = runBlocking { load() }
