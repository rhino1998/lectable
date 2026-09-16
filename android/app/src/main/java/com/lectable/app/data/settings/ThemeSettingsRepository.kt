package com.lectable.app.data.settings

import android.content.Context
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.map

enum class ThemePreference { SYSTEM, LIGHT, DARK }

fun ThemePreference.next(): ThemePreference = when (this) {
    ThemePreference.SYSTEM -> ThemePreference.LIGHT
    ThemePreference.LIGHT -> ThemePreference.DARK
    ThemePreference.DARK -> ThemePreference.SYSTEM
}

private val Context.themeDataStore by preferencesDataStore(name = "theme_settings")
private val THEME_KEY = stringPreferencesKey("theme")

/**
 * The Android analogue of frontend/src/hooks/useTheme.ts: a three-way
 * system/light/dark preference (not just a light/dark bool), since
 * "system" - deferring to the OS setting - is a real state worth being
 * able to return to, not just a default. Separate DataStore file from
 * [ServerSettingsRepository] since it has nothing to do with networking
 * and doesn't need that repository's synchronous-preload treatment - every
 * reader here is a Flow collected by Compose, so async is fine.
 */
@Singleton
class ThemeSettingsRepository @Inject constructor(
    @ApplicationContext private val context: Context,
) {
    val theme: Flow<ThemePreference> = context.themeDataStore.data.map { prefs ->
        when (prefs[THEME_KEY]) {
            "light" -> ThemePreference.LIGHT
            "dark" -> ThemePreference.DARK
            else -> ThemePreference.SYSTEM
        }
    }

    suspend fun setTheme(theme: ThemePreference) {
        context.themeDataStore.edit { prefs ->
            when (theme) {
                ThemePreference.LIGHT -> prefs[THEME_KEY] = "light"
                ThemePreference.DARK -> prefs[THEME_KEY] = "dark"
                ThemePreference.SYSTEM -> prefs.remove(THEME_KEY)
            }
        }
    }
}
