package com.lectable.app.data.remote

import com.lectable.app.data.settings.ServerSettingsRepository
import javax.inject.Inject
import javax.inject.Singleton

/**
 * Covers and paragraph audio aren't fetched through Retrofit endpoints -
 * they're relative paths embedded in DTOs (e.g. `coverUrl: "/api/books/{id}/cover"`,
 * see backend/internal/httpapi/books.go) that Coil and Media3 need as full
 * URLs. This resolves them against the currently configured server, same as
 * [DynamicBaseUrlInterceptor] does for Retrofit calls.
 */
@Singleton
class MediaUrlResolver @Inject constructor(
    private val settingsRepository: ServerSettingsRepository,
) {
    fun resolve(relativePath: String?): String? {
        if (relativePath.isNullOrBlank()) return null
        val base = settingsRepository.currentBaseUrl().trimEnd('/')
        val path = if (relativePath.startsWith("/")) relativePath else "/$relativePath"
        return base + path
    }
}
