package com.lectable.app.di

import com.lectable.app.data.remote.DynamicBaseUrlInterceptor
import com.lectable.app.data.remote.LectableApi
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
            // This client also backs BookUpdatesSocket's long-lived GET .../ws connection
            // (built as an ordinary Request, so it shares this client rather than getting its
            // own) - without a pingInterval, OkHttp's WebSocket support falls back to treating
            // readTimeout above as a plain idle-read timeout: a book that goes 60s with no
            // paragraph-status push (nothing generating, or just a normal gap between them) gets
            // its socket killed with onFailure, and BookUpdatesSocket's own reconnect (2s delay,
            // see its RECONNECT_DELAY_MS) has no way to redeliver whatever wshub push landed
            // during that dead window - wshub fans updates out only to sockets connected at the
            // moment they're sent, with no backlog/replay. That's what made a paragraph finishing
            // generation while the socket happened to be down look "stuck" until the next full
            // REST refetch (e.g. leaving and reopening the book) resynced actual state. A
            // pingInterval well under readTimeout keeps the socket's read timeout continuously
            // satisfied by pong frames instead, so it only actually disconnects on a real network
            // problem.
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
}
