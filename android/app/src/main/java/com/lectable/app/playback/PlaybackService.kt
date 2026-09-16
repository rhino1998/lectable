package com.lectable.app.playback

import android.content.Intent
import androidx.media3.session.MediaSession
import androidx.media3.session.MediaSessionService
import dagger.hilt.android.AndroidEntryPoint
import javax.inject.Inject

/**
 * Hosts [ParagraphPlayer]'s [MediaSession] as a foreground service, the Android analogue of
 * the web player's ability to keep narrating in a background tab - here that means surviving
 * backgrounding/screen-off and exposing system-level play/pause (notification, lock screen,
 * headset/Bluetooth buttons), which `MediaSessionService` + Media3's default notification
 * provider implement automatically for whatever session is registered with it.
 *
 * Doesn't own the player itself - [ParagraphPlayer] is `@Singleton`-scoped and already exists
 * (and starts this service) the moment playback begins, so this just exposes its session and
 * defers to it for teardown.
 */
@AndroidEntryPoint
class PlaybackService : MediaSessionService() {

    @Inject lateinit var paragraphPlayer: ParagraphPlayer

    override fun onCreate() {
        super.onCreate()
        // Media3's automatic play/pause-aware notification (and the startForeground() call
        // that must land within seconds of ContextCompat.startForegroundService() in
        // ParagraphPlayer, or the system kills this service with
        // ForegroundServiceDidNotStartInTimeException) only activates for sessions this
        // service is explicitly tracking - onGetSession() alone only answers a *connecting
        // controller*, which nothing here ever creates, since the UI talks to
        // ParagraphPlayer's ExoPlayer directly rather than through a MediaController. Session
        // and player were also built with the app context (this service didn't exist yet,
        // being a singleton created on first playback), so registering has to happen
        // explicitly rather than the auto-registration a session built with a running
        // service's own context would get.
        addSession(paragraphPlayer.mediaSession)
        // Playback is very likely already under way by the time onCreate() runs (that's what
        // triggered starting this service) - forces the notification/foreground state to
        // reflect that immediately instead of waiting on the next player event.
        onUpdateNotification(paragraphPlayer.mediaSession, /* startInForegroundRequired= */ true)
    }

    override fun onGetSession(controllerInfo: MediaSession.ControllerInfo): MediaSession =
        paragraphPlayer.mediaSession

    // Swiping the app away from recents shouldn't cut off narration that's actually playing
    // (same as any other music/podcast app) - only tears the service down if it wasn't.
    // Deliberately doesn't release ParagraphPlayer here (unlike the usual MediaSessionService
    // sample pattern of owning the player itself) - it's an app-scoped singleton that can
    // outlive many start/stop cycles of this service within the same process, so releasing it
    // on a stop this service itself requested would break the next playback attempt.
    override fun onTaskRemoved(rootIntent: Intent?) {
        super.onTaskRemoved(rootIntent)
        if (!paragraphPlayer.state.value.isPlaying) {
            stopSelf()
        }
    }

    override fun onDestroy() {
        removeSession(paragraphPlayer.mediaSession)
        super.onDestroy()
    }
}
