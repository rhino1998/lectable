package com.lectable.app.ui.settings

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.lectable.app.data.discovery.DiscoveredServer
import com.lectable.app.data.discovery.NsdDiscoveryRepository
import com.lectable.app.data.live.LiveStore
import com.lectable.app.data.repository.DownloadRepository
import com.lectable.app.data.repository.LibraryRepository
import com.lectable.app.data.settings.DEFAULT_FONT_SIZE_SP
import com.lectable.app.data.settings.DEFAULT_LOOKAHEAD_PARAGRAPHS
import com.lectable.app.data.settings.DEFAULT_SPEED_RAMP_SECONDS
import com.lectable.app.data.settings.LightingSettings
import com.lectable.app.data.settings.LightingSettingsRepository
import com.lectable.app.playback.lighting.DiscoveredBridge
import com.lectable.app.playback.lighting.HUE_LINK_BUTTON_NOT_PRESSED
import com.lectable.app.playback.lighting.HueBridgeClient
import com.lectable.app.playback.lighting.HueException
import com.lectable.app.playback.lighting.HueGroup
import com.lectable.app.playback.lighting.HueLightingController
import com.lectable.app.playback.lighting.LightingState
import kotlinx.coroutines.delay
import com.lectable.app.data.settings.PlaybackSettingsRepository
import com.lectable.app.data.settings.ReaderFontFamily
import com.lectable.app.data.settings.ReadingSettingsRepository
import com.lectable.app.data.settings.ServerSettingsRepository
import com.lectable.app.data.settings.ThemePreference
import com.lectable.app.data.settings.ThemeSettingsRepository
import dagger.hilt.android.lifecycle.HiltViewModel
import javax.inject.Inject
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class SettingsUiState(
    val serverUrl: String = "",
    val isValid: Boolean = true,
    val saved: Boolean = false,
    // Offline-downloads summary (see DownloadRepository.totalDownloadedBytes/
    // downloadedBookCount) - refreshed on load and after deleteAllDownloads.
    val downloadedBookCount: Int = 0,
    val downloadedBytes: Long = 0,
    // Philips Hue setup (see HueLightingController) - transient UI state only; the settings
    // themselves live in LightingSettingsRepository.
    val hueBridgeIp: String = "",
    val hueFound: List<DiscoveredBridge> = emptyList(),
    val hueBusy: HueBusy? = null,
    val hueError: String? = null,
    val hueGroups: List<HueGroup>? = null,
    // Why the last change to "Check for new chapters" didn't save.
    val sourceCheckError: String? = null,
)

enum class HueBusy { DISCOVERING, PAIRING }

private const val HUE_PAIR_TIMEOUT_MS = 30_000L
private const val HUE_PAIR_POLL_MS = 1_500L

@HiltViewModel
class SettingsViewModel @Inject constructor(
    private val serverSettingsRepository: ServerSettingsRepository,
    private val themeSettingsRepository: ThemeSettingsRepository,
    private val readingSettingsRepository: ReadingSettingsRepository,
    private val playbackSettingsRepository: PlaybackSettingsRepository,
    private val discoveryRepository: NsdDiscoveryRepository,
    private val downloadRepository: DownloadRepository,
    private val lightingSettingsRepository: LightingSettingsRepository,
    private val libraryRepository: LibraryRepository,
    liveStore: LiveStore,
    lightingController: HueLightingController,
) : ViewModel() {

    private val _uiState = MutableStateFlow(SettingsUiState(serverUrl = serverSettingsRepository.currentBaseUrl()))
    val uiState: StateFlow<SettingsUiState> = _uiState

    val lightingSettings: StateFlow<LightingSettings> = lightingSettingsRepository.settings
        .stateIn(viewModelScope, SharingStarted.Eagerly, LightingSettings())

    val lightingState: StateFlow<LightingState> = lightingController.state

    /** How often books imported from websites are checked for new entries (the backend's
     *  sourceSettings live topic, library-wide); null until the backend has said. */
    val sourceCheckEveryHours: StateFlow<Int?> = liveStore.sourceSettings()
        .map { it.data?.checkEveryHours }
        .stateIn(viewModelScope, SharingStarted.Eagerly, null)

    fun setSourceCheckEveryHours(hours: Int) {
        viewModelScope.launch {
            _uiState.update { it.copy(sourceCheckError = null) }
            runCatching { libraryRepository.setSourceCheckEveryHours(hours) }
                .onFailure { e -> _uiState.update { it.copy(sourceCheckError = e.message ?: "Could not save the setting") } }
        }
    }

    init {
        refreshDownloadsSummary()
        viewModelScope.launch {
            // Seed the IP field and load rooms whenever a pairing is (or becomes) present.
            lightingSettingsRepository.settings.collect { s ->
                if (_uiState.value.hueBridgeIp.isEmpty()) _uiState.update { it.copy(hueBridgeIp = s.bridgeIp) }
                if (s.paired && _uiState.value.hueGroups == null) loadHueGroups(s)
            }
        }
    }

    private fun loadHueGroups(s: LightingSettings) {
        viewModelScope.launch {
            runCatching { HueBridgeClient(s.bridgeIp, s.username, s.certPin).groups() }
                .onSuccess { groups -> _uiState.update { it.copy(hueGroups = groups) } }
                .onFailure { e -> _uiState.update { it.copy(hueError = e.message) } }
        }
    }

    fun onHueBridgeIpChanged(ip: String) {
        _uiState.update { it.copy(hueBridgeIp = ip, hueError = null) }
    }

    fun discoverHueBridges() {
        viewModelScope.launch {
            _uiState.update { it.copy(hueBusy = HueBusy.DISCOVERING, hueError = null) }
            runCatching { HueBridgeClient.discover() }
                .onSuccess { found ->
                    _uiState.update {
                        it.copy(
                            hueFound = found,
                            hueBridgeIp = if (found.size == 1) found[0].ip else it.hueBridgeIp,
                            hueError = if (found.isEmpty()) "No bridge found - enter its IP address instead." else null,
                        )
                    }
                }
                .onFailure { e -> _uiState.update { it.copy(hueError = "Discovery failed (${e.message}) - enter the bridge's IP instead.") } }
            _uiState.update { it.copy(hueBusy = null) }
        }
    }

    /** Polls the bridge until its link button is pressed, then saves the pairing. */
    fun pairHueBridge() {
        val ip = _uiState.value.hueBridgeIp.trim()
        if (ip.isEmpty()) return
        viewModelScope.launch {
            _uiState.update { it.copy(hueBusy = HueBusy.PAIRING, hueError = null) }
            val deadline = System.currentTimeMillis() + HUE_PAIR_TIMEOUT_MS
            try {
                while (true) {
                    try {
                        val pairing = HueBridgeClient.pair(ip, "android")
                        _uiState.update { it.copy(hueGroups = null) }
                        lightingSettingsRepository.setPairing(ip, pairing.username, pairing.certPin)
                        break
                    } catch (e: HueException) {
                        if (e.type != HUE_LINK_BUTTON_NOT_PRESSED) throw e
                        if (System.currentTimeMillis() > deadline) error("timed out waiting for the link button")
                        delay(HUE_PAIR_POLL_MS)
                    }
                }
            } catch (e: Exception) {
                if (e is kotlinx.coroutines.CancellationException) throw e
                _uiState.update { it.copy(hueError = "Pairing failed: ${e.message}") }
            } finally {
                _uiState.update { it.copy(hueBusy = null) }
            }
        }
    }

    fun forgetHueBridge() {
        _uiState.update { it.copy(hueGroups = null) }
        viewModelScope.launch { lightingSettingsRepository.forgetBridge() }
    }

    fun setLightingEnabled(enabled: Boolean) {
        viewModelScope.launch { lightingSettingsRepository.setEnabled(enabled) }
    }

    fun setHueGroup(groupId: String) {
        viewModelScope.launch { lightingSettingsRepository.setGroupId(groupId) }
    }

    fun setLightBrightnessCap(cap: Float) {
        viewModelScope.launch { lightingSettingsRepository.setBrightnessCap(cap) }
    }

    fun setRestoreLightsOnStop(restore: Boolean) {
        viewModelScope.launch { lightingSettingsRepository.setRestoreOnStop(restore) }
    }

    private fun refreshDownloadsSummary() {
        viewModelScope.launch {
            val count = downloadRepository.downloadedBookCount()
            val bytes = downloadRepository.totalDownloadedBytes()
            _uiState.update { it.copy(downloadedBookCount = count, downloadedBytes = bytes) }
        }
    }

    /** Settings' "Delete all downloads" action - removes every downloaded book across the
     *  current library at once. */
    fun deleteAllDownloads() {
        viewModelScope.launch {
            downloadRepository.deleteAllDownloads()
            refreshDownloadsSummary()
        }
    }

    val discoveredServers: StateFlow<List<DiscoveredServer>> = discoveryRepository.servers

    val themePreference: StateFlow<ThemePreference> = themeSettingsRepository.theme
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), ThemePreference.SYSTEM)

    val fontSize: StateFlow<Int> = readingSettingsRepository.fontSize
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), DEFAULT_FONT_SIZE_SP)

    val fontFamily: StateFlow<ReaderFontFamily> = readingSettingsRepository.fontFamily
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), ReaderFontFamily.DEFAULT)

    val lookaheadParagraphs: StateFlow<Int> = playbackSettingsRepository.lookaheadParagraphs
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), DEFAULT_LOOKAHEAD_PARAGRAPHS)

    val speedRampSeconds: StateFlow<Float> = playbackSettingsRepository.speedRampSeconds
        .stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), DEFAULT_SPEED_RAMP_SECONDS)

    fun setSpeedRampSeconds(seconds: Float) {
        viewModelScope.launch { playbackSettingsRepository.setSpeedRampSeconds(seconds) }
    }

    fun setTheme(theme: ThemePreference) {
        viewModelScope.launch { themeSettingsRepository.setTheme(theme) }
    }

    fun setFontSize(sizeSp: Int) {
        viewModelScope.launch { readingSettingsRepository.setFontSize(sizeSp) }
    }

    fun setFontFamily(family: ReaderFontFamily) {
        viewModelScope.launch { readingSettingsRepository.setFontFamily(family) }
    }

    fun setLookaheadParagraphs(count: Int) {
        viewModelScope.launch { playbackSettingsRepository.setLookaheadParagraphs(count) }
    }

    fun onUrlChanged(url: String) {
        _uiState.update { it.copy(serverUrl = url, isValid = true, saved = false) }
    }

    fun save() {
        val url = _uiState.value.serverUrl
        if (!ServerSettingsRepository.isValid(url)) {
            _uiState.update { it.copy(isValid = false) }
            return
        }
        viewModelScope.launch {
            serverSettingsRepository.setBaseUrl(url)
            _uiState.update { it.copy(saved = true) }
        }
    }

    /** Called while the Settings screen is visible - see SettingsScreen's DisposableEffect. */
    fun startDiscovery() = discoveryRepository.startDiscovery()

    fun stopDiscovery() = discoveryRepository.stopDiscovery()

    /** Picking a discovered server both fills the field and saves it immediately. */
    fun selectDiscoveredServer(server: DiscoveredServer) {
        onUrlChanged(server.url)
        save()
    }

    override fun onCleared() {
        discoveryRepository.stopDiscovery()
        super.onCleared()
    }
}
