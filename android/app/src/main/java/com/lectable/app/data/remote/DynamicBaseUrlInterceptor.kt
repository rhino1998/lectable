package com.lectable.app.data.remote

import com.lectable.app.data.settings.ServerSettingsRepository
import javax.inject.Inject
import javax.inject.Singleton
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.Interceptor
import okhttp3.Response

/**
 * Retrofit is built once against a placeholder base URL (see
 * di.NetworkModule); this interceptor rewrites every outgoing request's
 * scheme/host/port to whatever the user last set in Settings, read fresh
 * from [ServerSettingsRepository] on each call. That lets the backend
 * address change at runtime without rebuilding the Retrofit/OkHttp stack.
 */
@Singleton
class DynamicBaseUrlInterceptor @Inject constructor(
    private val settingsRepository: ServerSettingsRepository,
) : Interceptor {
    override fun intercept(chain: Interceptor.Chain): Response {
        val original = chain.request()
        val configured = settingsRepository.currentBaseUrl().toHttpUrl()
        val newUrl = original.url.newBuilder()
            .scheme(configured.scheme)
            .host(configured.host)
            .port(configured.port)
            .build()
        return chain.proceed(original.newBuilder().url(newUrl).build())
    }
}
