package com.lectable.app.di

import com.lectable.app.data.remote.DynamicBaseUrlInterceptor
import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.MediaApi
import dagger.Module
import dagger.Provides
import dagger.hilt.InstallIn
import dagger.hilt.components.SingletonComponent
import java.util.concurrent.TimeUnit
import javax.inject.Singleton
import kotlinx.serialization.json.Json
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.logging.HttpLoggingInterceptor
import com.jakewharton.retrofit2.converter.kotlinx.serialization.asConverterFactory
import retrofit2.Retrofit

@Module
@InstallIn(SingletonComponent::class)
object NetworkModule {

    @Provides
    @Singleton
    fun provideJson(): Json = Json {
        ignoreUnknownKeys = true
        encodeDefaults = true
    }

    @Provides
    @Singleton
    fun provideOkHttpClient(dynamicBaseUrlInterceptor: DynamicBaseUrlInterceptor): OkHttpClient =
        OkHttpClient.Builder()
            .addInterceptor(dynamicBaseUrlInterceptor)
            .addInterceptor(
                HttpLoggingInterceptor().apply { level = HttpLoggingInterceptor.Level.BASIC },
            )
            // Paragraph audio fetches and voice-preset synthesis can take a while on a
            // single-GPU box (see tts-service/CLAUDE.md) - generous timeouts avoid
            // spurious failures rather than tuning for a snappy worst case.
            .connectTimeout(10, TimeUnit.SECONDS)
            .readTimeout(60, TimeUnit.SECONDS)
            .writeTimeout(60, TimeUnit.SECONDS)
            // This client also backs LiveClient's long-lived GET /api/events WebSocket (built as
            // an ordinary Request, so it shares this client). Without a pingInterval, OkHttp
            // treats readTimeout above as a plain idle-read timeout on a WebSocket too: a quiet
            // 60s (nothing changing server-side) would kill the socket with onFailure. A
            // pingInterval well under readTimeout keeps it satisfied by pong frames instead, so
            // it only disconnects on a real network problem. (A disconnect is recoverable anyway -
            // LiveClient resubscribes and gets fresh snapshots - but churning it every quiet
            // minute is pointless.)
            .pingInterval(20, TimeUnit.SECONDS)
            .build()

    @Provides
    @Singleton
    fun provideRetrofit(okHttpClient: OkHttpClient, json: Json): Retrofit =
        Retrofit.Builder()
            // Placeholder - DynamicBaseUrlInterceptor rewrites the real host per request.
            .baseUrl("http://localhost/")
            .client(okHttpClient)
            .addConverterFactory(json.asConverterFactory("application/json".toMediaType()))
            .build()

    @Provides
    @Singleton
    fun provideLectableApi(retrofit: Retrofit): LectableApi = retrofit.create(LectableApi::class.java)

    @Provides
    @Singleton
    fun provideMediaApi(retrofit: Retrofit): MediaApi = retrofit.create(MediaApi::class.java)
}
