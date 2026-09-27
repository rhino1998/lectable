package com.lectable.app.data.settings

import android.content.Context
import androidx.datastore.preferences.core.booleanPreferencesKey
import androidx.datastore.preferences.core.edit
import androidx.datastore.preferences.core.floatPreferencesKey
import androidx.datastore.preferences.core.stringPreferencesKey
import androidx.datastore.preferences.preferencesDataStore
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.map

private val Context.lightingDataStore by preferencesDataStore(name = "lighting_settings")
private val ENABLED_KEY = booleanPreferencesKey("enabled")
private val BRIDGE_IP_KEY = stringPreferencesKey("bridge_ip")
private val USERNAME_KEY = stringPreferencesKey("username")
private val CERT_PIN_KEY = stringPreferencesKey("cert_pin")
private val GROUP_ID_KEY = stringPreferencesKey("group_id")
private val BRIGHTNESS_CAP_KEY = floatPreferencesKey("brightness_cap")
private val RESTORE_ON_STOP_KEY = booleanPreferencesKey("restore_on_stop")

const val MIN_LIGHT_BRIGHTNESS_CAP = 0.1f
const val DEFAULT_LIGHT_BRIGHTNESS_CAP = 0.8f

/** The listener's Philips Hue setup - this device's own pairing with this network's bridge, so it
 *  lives here rather than on the backend (which never talks to a light - see backend
 *  store.Lighting). Mirrors frontend/src/lighting/controller.ts's LightingSettings. */
data class LightingSettings(
    val enabled: Boolean = false,
    val bridgeIp: String = "",
    val username: String = "",
    // The bridge certificate's public-key pin from pairing - see HueBridgeClient's BridgeTrust.
    val certPin: String = "",
    val groupId: String = "",
    val brightnessCap: Float = DEFAULT_LIGHT_BRIGHTNESS_CAP,
    val restoreOnStop: Boolean = true,
) {
    val paired: Boolean get() = bridgeIp.isNotBlank() && username.isNotBlank()
    val configured: Boolean get() = paired && groupId.isNotBlank()
}

/** DataStore-backed [LightingSettings] - same pattern as [PlaybackSettingsRepository]. Read by
 *  [com.lectable.app.playback.lighting.HueLightingController] and edited from Settings. */
@Singleton
class LightingSettingsRepository @Inject constructor(
    @ApplicationContext private val context: Context,
) {
    val settings: Flow<LightingSettings> = context.lightingDataStore.data.map { prefs ->
        LightingSettings(
            enabled = prefs[ENABLED_KEY] ?: false,
            bridgeIp = prefs[BRIDGE_IP_KEY] ?: "",
            username = prefs[USERNAME_KEY] ?: "",
            certPin = prefs[CERT_PIN_KEY] ?: "",
            groupId = prefs[GROUP_ID_KEY] ?: "",
            brightnessCap = (prefs[BRIGHTNESS_CAP_KEY] ?: DEFAULT_LIGHT_BRIGHTNESS_CAP).coerceIn(MIN_LIGHT_BRIGHTNESS_CAP, 1f),
            restoreOnStop = prefs[RESTORE_ON_STOP_KEY] ?: true,
        )
    }

    suspend fun setEnabled(enabled: Boolean) {
        context.lightingDataStore.edit { it[ENABLED_KEY] = enabled }
    }

    /** A fresh pairing - the room picked on any previous bridge no longer applies. */
    suspend fun setPairing(bridgeIp: String, username: String, certPin: String) {
        context.lightingDataStore.edit {
            it[BRIDGE_IP_KEY] = bridgeIp
            it[USERNAME_KEY] = username
            it[CERT_PIN_KEY] = certPin
            it[GROUP_ID_KEY] = ""
        }
    }

    suspend fun forgetBridge() {
        context.lightingDataStore.edit {
            it[USERNAME_KEY] = ""
            it[CERT_PIN_KEY] = ""
            it[GROUP_ID_KEY] = ""
            it[ENABLED_KEY] = false
        }
    }

    suspend fun setGroupId(groupId: String) {
        context.lightingDataStore.edit { it[GROUP_ID_KEY] = groupId }
    }

    suspend fun setBrightnessCap(cap: Float) {
        context.lightingDataStore.edit { it[BRIGHTNESS_CAP_KEY] = cap.coerceIn(MIN_LIGHT_BRIGHTNESS_CAP, 1f) }
    }

    suspend fun setRestoreOnStop(restore: Boolean) {
        context.lightingDataStore.edit { it[RESTORE_ON_STOP_KEY] = restore }
    }
}
