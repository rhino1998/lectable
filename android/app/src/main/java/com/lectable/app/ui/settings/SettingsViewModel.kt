package com.lectable.app.ui.settings

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.lectable.app.data.discovery.DiscoveredServer
import com.lectable.app.data.discovery.NsdDiscoveryRepository
import com.lectable.app.data.repository.DownloadRepository
import com.lectable.app.data.settings.DEFAULT_FONT_SIZE_SP
import com.lectable.app.data.settings.DEFAULT_LOOKAHEAD_PARAGRAPHS
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
)

@HiltViewModel
class SettingsViewModel @Inject constructor(
    private val serverSettingsRepository: ServerSettingsRepository,
    private val themeSettingsRepository: ThemeSettingsRepository,
    private val readingSettingsRepository: ReadingSettingsRepository,
    private val playbackSettingsRepository: PlaybackSettingsRepository,
    private val discoveryRepository: NsdDiscoveryRepository,
    private val downloadRepository: DownloadRepository,
) : ViewModel() {

    private val _uiState = MutableStateFlow(SettingsUiState(serverUrl = serverSettingsRepository.currentBaseUrl()))
    val uiState: StateFlow<SettingsUiState> = _uiState

    init {
        refreshDownloadsSummary()
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
