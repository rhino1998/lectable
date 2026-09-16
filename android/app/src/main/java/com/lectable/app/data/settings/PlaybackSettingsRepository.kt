package com.lectable.app.data.settings

import android.content.Context
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.floatPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.map

private val Context.playbackDataStore by preferencesDataStore(name = "playback_settings")
private val PLAYBACK_SPEED_KEY = floatPreferencesKey("speed")

/**
 * Persists the user's chosen playback speed across app restarts - same DataStore-backed
 * pattern as [ThemeSettingsRepository]/[ReadingSettingsRepository], kept in its own file since
 * it's an unrelated preference. Read reactively by [com.lectable.app.playback.ParagraphPlayer]
 * (a Flow collected once at startup, not a synchronous preload like
 * [ServerSettingsRepository] - a brief default-speed flash before the first collection lands
 * is harmless, unlike a network call racing an unset base URL).
 */
@Singleton
class PlaybackSettingsRepository @Inject constructor(
    @ApplicationContext private val context: Context,
) {
    val speed: Flow<Float> = context.playbackDataStore.data.map { prefs -> prefs[PLAYBACK_SPEED_KEY] ?: 1f }

    suspend fun setSpeed(speed: Float) {
        context.playbackDataStore.edit { prefs -> prefs[PLAYBACK_SPEED_KEY] = speed }
    }
}
