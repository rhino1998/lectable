package com.lectable.app.data.settings

import android.content.Context
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.floatPreferencesKey
import androidx.datastore.preferences.core.intPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.map

private val Context.playbackDataStore by preferencesDataStore(name = "playback_settings")
private val PLAYBACK_SPEED_KEY = floatPreferencesKey("speed")
private val LOOKAHEAD_PARAGRAPHS_KEY = intPreferencesKey("lookahead_paragraphs")
private val MUSIC_VOLUME_KEY = floatPreferencesKey("music_volume")

/** Background music's own volume (0..1, the ExoPlayer volume [com.lectable.app.playback
 *  .BackgroundMusicPlayer] mixes it in at, relative to narration's full volume) - set from the
 *  reader's PlaybackBar music button (long-press). Defaults well under narration, mirroring
 *  frontend useBackgroundMusic.ts's own fixed MUSIC_GAIN. */
const val DEFAULT_MUSIC_VOLUME = 0.22f

/** How many paragraphs ahead of playback the backend is asked to generate (the lookahead
 *  request's paragraphCount). Default matches backend jobs.LookaheadParagraphCount; the max
 *  matches jobs.MaxLookaheadParagraphCount, which the backend clamps to anyway. */
const val MIN_LOOKAHEAD_PARAGRAPHS = 5
const val MAX_LOOKAHEAD_PARAGRAPHS = 1000
const val LOOKAHEAD_PARAGRAPHS_STEP = 5
const val DEFAULT_LOOKAHEAD_PARAGRAPHS = 25

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

    val lookaheadParagraphs: Flow<Int> = context.playbackDataStore.data.map { prefs ->
        (prefs[LOOKAHEAD_PARAGRAPHS_KEY] ?: DEFAULT_LOOKAHEAD_PARAGRAPHS).coerceIn(MIN_LOOKAHEAD_PARAGRAPHS, MAX_LOOKAHEAD_PARAGRAPHS)
    }

    suspend fun setLookaheadParagraphs(count: Int) {
        context.playbackDataStore.edit { prefs ->
            prefs[LOOKAHEAD_PARAGRAPHS_KEY] = count.coerceIn(MIN_LOOKAHEAD_PARAGRAPHS, MAX_LOOKAHEAD_PARAGRAPHS)
        }
    }

    val musicVolume: Flow<Float> = context.playbackDataStore.data.map { prefs ->
        (prefs[MUSIC_VOLUME_KEY] ?: DEFAULT_MUSIC_VOLUME).coerceIn(0f, 1f)
    }

    suspend fun setMusicVolume(volume: Float) {
        context.playbackDataStore.edit { prefs -> prefs[MUSIC_VOLUME_KEY] = volume.coerceIn(0f, 1f) }
    }
}
