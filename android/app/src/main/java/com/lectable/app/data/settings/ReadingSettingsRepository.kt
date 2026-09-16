package com.lectable.app.data.settings

import android.content.Context
import androidx.compose.ui.text.font.FontFamily
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.intPreferencesKey
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.map

/** No web-frontend equivalent to mirror (index.css's paragraph text is a fixed
 *  size/font) - this is an Android-only reading preference. A plain whole-number sp size
 *  (not a preset scale multiplier) - see MIN_FONT_SIZE_SP/MAX_FONT_SIZE_SP/
 *  DEFAULT_FONT_SIZE_SP below and SettingsScreen.kt's slider, which sets this directly. */
const val MIN_FONT_SIZE_SP = 12
const val MAX_FONT_SIZE_SP = 32
const val DEFAULT_FONT_SIZE_SP = 16

/** Android's own built-in generic font families, not bundled font files - keeps this
 *  to "a few fonts" without adding font assets/licensing to track. */
enum class ReaderFontFamily(val label: String) {
    DEFAULT("Default"),
    SERIF("Serif"),
    SANS_SERIF("Sans"),
    MONOSPACE("Mono"),
}

fun ReaderFontFamily.toComposeFontFamily(): FontFamily = when (this) {
    ReaderFontFamily.DEFAULT -> FontFamily.Default
    ReaderFontFamily.SERIF -> FontFamily.Serif
    ReaderFontFamily.SANS_SERIF -> FontFamily.SansSerif
    ReaderFontFamily.MONOSPACE -> FontFamily.Monospace
}

private val Context.readingDataStore by preferencesDataStore(name = "reading_settings")
// Still named "font_size" on disk even though it moved from a preset enum's .name to a plain
// Int sp value - an old stored preset name fails the Int read below and just falls back to
// DEFAULT_FONT_SIZE_SP, same "not worth a migration" treatment this app's own DuckDB schema
// gets (see backend/CLAUDE.md) - nothing here is worth preserving across that change.
private val FONT_SIZE_KEY = intPreferencesKey("font_size")
private val FONT_FAMILY_KEY = stringPreferencesKey("font_family")

/** Reader typography preferences (font size/family for paragraph text in ReaderScreen) -
 *  same DataStore-backed pattern as [ThemeSettingsRepository], kept in its own file since
 *  it's an unrelated preference. */
@Singleton
class ReadingSettingsRepository @Inject constructor(
    @ApplicationContext private val context: Context,
) {
    /** Whole-number sp size, clamped to [MIN_FONT_SIZE_SP]/[MAX_FONT_SIZE_SP] in case a stored
     *  value ever ends up outside that range (a future narrower bound, or hand-edited prefs). */
    val fontSize: Flow<Int> = context.readingDataStore.data.map { prefs ->
        (prefs[FONT_SIZE_KEY] ?: DEFAULT_FONT_SIZE_SP).coerceIn(MIN_FONT_SIZE_SP, MAX_FONT_SIZE_SP)
    }

    val fontFamily: Flow<ReaderFontFamily> = context.readingDataStore.data.map { prefs ->
        prefs[FONT_FAMILY_KEY]?.let { name -> runCatching { ReaderFontFamily.valueOf(name) }.getOrNull() } ?: ReaderFontFamily.DEFAULT
    }

    suspend fun setFontSize(sizeSp: Int) {
        context.readingDataStore.edit { prefs -> prefs[FONT_SIZE_KEY] = sizeSp.coerceIn(MIN_FONT_SIZE_SP, MAX_FONT_SIZE_SP) }
    }

    suspend fun setFontFamily(family: ReaderFontFamily) {
        context.readingDataStore.edit { prefs -> prefs[FONT_FAMILY_KEY] = family.name }
    }
}
