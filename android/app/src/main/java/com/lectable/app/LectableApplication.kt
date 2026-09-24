package com.lectable.app

import android.app.Application
import androidx.work.Configuration
import androidx.hilt.work.HiltWorkerFactory
import com.lectable.app.data.settings.ServerSettingsRepository
import com.lectable.app.data.sync.OfflineReconciler
import androidx.glance.appwidget.updateAll
import com.lectable.app.playback.ParagraphPlayer
import com.lectable.app.widget.PlaybackWidget
import dagger.hilt.android.HiltAndroidApp
import javax.inject.Inject
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.launchIn
import kotlinx.coroutines.flow.onEach
import kotlinx.coroutines.runBlocking

@HiltAndroidApp
class LectableApplication : Application(), Configuration.Provider {

    @Inject lateinit var serverSettingsRepository: ServerSettingsRepository

    // Lets ChapterDownloadWorker (@HiltWorker) get its DownloadRepository injected the same way
    // everything else in the app does, instead of WorkManager's default reflection-based
    // no-arg-constructor factory - see the Configuration.Provider override below.
    @Inject lateinit var workerFactory: HiltWorkerFactory

    override val workManagerConfiguration: Configuration
        get() = Configuration.Builder().setWorkerFactory(workerFactory).build()

    // Eagerly injecting ParagraphPlayer here - rather than waiting for it to be created
    // lazily whenever the Reader screen first opens, its usual trigger - is a deliberate
    // trade: the home-screen widget (see widget/PlaybackWidget.kt) needs *some* persistent
    // observer to know when to redraw, and constructing the ExoPlayer/MediaSession a little
    // earlier than strictly necessary is cheap next to that.
    @Inject lateinit var paragraphPlayer: ParagraphPlayer

    // Keeps offline downloads reconciled with the backend - see OfflineReconciler.
    @Inject lateinit var offlineReconciler: OfflineReconciler

    private val appScope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    override fun onCreate() {
        super.onCreate()
        // Must finish before the first network call - DynamicBaseUrlInterceptor
        // reads currentBaseUrl() synchronously and this is the only load().
        runBlocking { serverSettingsRepository.load() }

        offlineReconciler.startAutoSync()

        combine(paragraphPlayer.state, paragraphPlayer.bookTitle) { state, title -> state to title }
            .onEach { PlaybackWidget().updateAll(this) }
            .launchIn(appScope)
    }
}
