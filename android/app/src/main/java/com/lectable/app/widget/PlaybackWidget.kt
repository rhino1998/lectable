package com.lectable.app.widget

import android.content.Context
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.glance.GlanceId
import androidx.glance.GlanceModifier
import androidx.glance.action.ActionParameters
import androidx.glance.action.clickable
import androidx.glance.appwidget.GlanceAppWidget
import androidx.glance.appwidget.action.ActionCallback
import androidx.glance.appwidget.action.actionRunCallback
import androidx.glance.appwidget.provideContent
import androidx.glance.background
import androidx.glance.layout.Alignment
import androidx.glance.layout.Column
import androidx.glance.layout.Row
import androidx.glance.layout.fillMaxSize
import androidx.glance.layout.fillMaxWidth
import androidx.glance.layout.padding
import androidx.glance.text.FontWeight
import androidx.glance.text.Text
import androidx.glance.text.TextStyle
import androidx.glance.unit.ColorProvider
import com.lectable.app.playback.ParagraphPlayer
import dagger.hilt.EntryPoint
import dagger.hilt.InstallIn
import dagger.hilt.android.EntryPointAccessors
import dagger.hilt.components.SingletonComponent

/** Glance/`ActionCallback` classes are instantiated fresh by the system, not through Hilt's own
 *  entry points - this is how they reach the app-scoped [ParagraphPlayer] singleton anyway. */
@EntryPoint
@InstallIn(SingletonComponent::class)
interface ParagraphPlayerEntryPoint {
    fun paragraphPlayer(): ParagraphPlayer
}

internal fun paragraphPlayerFrom(context: Context): ParagraphPlayer =
    EntryPointAccessors.fromApplication(context.applicationContext, ParagraphPlayerEntryPoint::class.java)
        .paragraphPlayer()

/**
 * Home-screen widget for quick resume/pause - reachable without unlocking the phone or opening
 * the app, complementing (not replacing) PlaybackService's fuller notification/lock-screen
 * transport controls. Deliberately just book title + one play/pause toggle, nothing else.
 *
 * Doesn't observe [ParagraphPlayer]'s flows reactively itself - `provideGlance` just reads a
 * snapshot each time it runs, and Glance re-invokes it fresh on every `updateAll()`/`update()`
 * call. [com.lectable.app.LectableApplication] is what actually watches for playback changes
 * and triggers those calls.
 */
class PlaybackWidget : GlanceAppWidget() {
    override suspend fun provideGlance(context: Context, id: GlanceId) {
        val player = paragraphPlayerFrom(context)
        val state = player.state.value
        val title = player.bookTitle.value

        provideContent {
            Column(
                modifier = GlanceModifier
                    .fillMaxSize()
                    .background(ColorProvider(Color(0xFF2A2930)))
                    .padding(12.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    text = title ?: "Lectable",
                    style = TextStyle(color = ColorProvider(Color.White), fontWeight = FontWeight.Medium, fontSize = 14.sp),
                    maxLines = 1,
                )
                Row(
                    modifier = GlanceModifier.fillMaxWidth().padding(top = 8.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Text(
                        text = if (state.isPlaying) "Pause" else "Play",
                        style = TextStyle(color = ColorProvider(Color(0xFFC4B5FD)), fontWeight = FontWeight.Bold, fontSize = 16.sp),
                        modifier = GlanceModifier.clickable(actionRunCallback<TogglePlayPauseAction>()),
                    )
                }
            }
        }
    }
}

class TogglePlayPauseAction : ActionCallback {
    override suspend fun onAction(context: Context, glanceId: GlanceId, parameters: ActionParameters) {
        paragraphPlayerFrom(context).togglePlayPause()
        // togglePlayPause()'s effect (isPlaying flipping) reaches LectableApplication's
        // observer asynchronously (a Player.Listener callback, then a StateFlow emission) -
        // updating explicitly here as well means the tapped label flips immediately instead
        // of waiting on that round trip.
        PlaybackWidget().update(context, glanceId)
    }
}
