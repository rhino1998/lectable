package com.lectable.app.data.settings

import android.content.Context
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.floatPreferencesKey
import androidx.datastore.preferences.core.intPreferencesKey
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import com.lectable.app.playback.DEFAULT_SPATIAL_WIDTH
import com.lectable.app.playback.MAX_SPATIAL_WIDTH
import com.lectable.app.playback.SpatialMode
import dagger.hilt.android.qualifiers.ApplicationContext
import java.util.UUID
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map

private val Context.playbackDataStore by preferencesDataStore(name = "playback_settings")
private val PLAYBACK_SPEED_KEY = floatPreferencesKey("speed")
private val LOOKAHEAD_PARAGRAPHS_KEY = intPreferencesKey("lookahead_paragraphs")
private val MUSIC_VOLUME_KEY = floatPreferencesKey("music_volume")
private val SPEED_RAMP_SECONDS_KEY = floatPreferencesKey("speed_ramp_seconds")
private val CLIENT_ID_KEY = stringPreferencesKey("client_id")
private val SPATIAL_MODE_KEY = stringPreferencesKey("spatial_mode")
private val SPATIAL_WIDTH_KEY = floatPreferencesKey("spatial_width")

/** How gradually a playback-speed change takes effect while narration is playing: the seconds
 *  it takes to move 0.5x toward the new speed (so 1x to 3x at the default takes 4s). 0 applies
 *  a change at once. See [com.lectable.app.playback.ParagraphPlayer]'s speed ramp. */
const val DEFAULT_SPEED_RAMP_SECONDS = 1f
const val MAX_SPEED_RAMP_SECONDS = 5f
const val SPEED_RAMP_SECONDS_STEP = 0.25f

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

    /** This installation's reader id: random, made once and kept. Sent with lookahead calls so
     *  the backend tracks each reader's window on its own (LookaheadRequestDto.clientId). */
    suspend fun clientId(): String {
        context.playbackDataStore.data.first()[CLIENT_ID_KEY]?.let { return it }
        val fresh = UUID.randomUUID().toString()
        var id = fresh
        context.playbackDataStore.edit { prefs -> id = prefs[CLIENT_ID_KEY] ?: fresh.also { prefs[CLIENT_ID_KEY] = it } }
        return id
    }

    val lookaheadParagraphs: Flow<Int> = context.playbackDataStore.data.map { prefs ->
        (prefs[LOOKAHEAD_PARAGRAPHS_KEY] ?: DEFAULT_LOOKAHEAD_PARAGRAPHS).coerceIn(MIN_LOOKAHEAD_PARAGRAPHS, MAX_LOOKAHEAD_PARAGRAPHS)
    }

    suspend fun setLookaheadParagraphs(count: Int) {
        context.playbackDataStore.edit { prefs ->
            prefs[LOOKAHEAD_PARAGRAPHS_KEY] = count.coerceIn(MIN_LOOKAHEAD_PARAGRAPHS, MAX_LOOKAHEAD_PARAGRAPHS)
        }
    }

    val speedRampSeconds: Flow<Float> = context.playbackDataStore.data.map { prefs ->
        (prefs[SPEED_RAMP_SECONDS_KEY] ?: DEFAULT_SPEED_RAMP_SECONDS).coerceIn(0f, MAX_SPEED_RAMP_SECONDS)
    }

    suspend fun setSpeedRampSeconds(seconds: Float) {
        context.playbackDataStore.edit { prefs -> prefs[SPEED_RAMP_SECONDS_KEY] = seconds.coerceIn(0f, MAX_SPEED_RAMP_SECONDS) }
    }

    /** Speaker positions - see [com.lectable.app.playback.SpatialMode]. Per device, since
     *  whether they suit depends on what it plays through. */
    val spatialMode: Flow<SpatialMode> = context.playbackDataStore.data.map { prefs -> SpatialMode.fromKey(prefs[SPATIAL_MODE_KEY]) }

    suspend fun setSpatialMode(mode: SpatialMode) {
        context.playbackDataStore.edit { prefs -> prefs[SPATIAL_MODE_KEY] = mode.key }
    }

    /** How far apart the seats are, 0..[MAX_SPATIAL_WIDTH]. */
    val spatialWidth: Flow<Float> = context.playbackDataStore.data.map { prefs ->
        (prefs[SPATIAL_WIDTH_KEY] ?: DEFAULT_SPATIAL_WIDTH).coerceIn(0f, MAX_SPATIAL_WIDTH)
    }

    suspend fun setSpatialWidth(width: Float) {
        context.playbackDataStore.edit { prefs -> prefs[SPATIAL_WIDTH_KEY] = width.coerceIn(0f, MAX_SPATIAL_WIDTH) }
    }

    val musicVolume: Flow<Float> = context.playbackDataStore.data.map { prefs ->
        (prefs[MUSIC_VOLUME_KEY] ?: DEFAULT_MUSIC_VOLUME).coerceIn(0f, 1f)
    }

    suspend fun setMusicVolume(volume: Float) {
        context.playbackDataStore.edit { prefs -> prefs[MUSIC_VOLUME_KEY] = volume.coerceIn(0f, 1f) }
    }
}
