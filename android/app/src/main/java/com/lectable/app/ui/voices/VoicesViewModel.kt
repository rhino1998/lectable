package com.lectable.app.ui.voices

import android.content.Context
import android.media.MediaPlayer
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.lectable.app.data.live.LiveStore
import com.lectable.app.data.remote.MediaUrlResolver
import com.lectable.app.data.remote.dto.CustomVoicePresetDto
import com.lectable.app.data.remote.dto.CustomVoicePresetInputDto
import com.lectable.app.data.remote.dto.VoiceSettingsDto
import com.lectable.app.data.repository.VoiceRepository
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.File
import javax.inject.Inject
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

data class VoicesUiState(
    val loading: Boolean = true,
    val presets: List<CustomVoicePresetDto> = emptyList(),
    val defaultPresetId: String? = null,
    val error: String? = null,
)

/** Manages the app-wide library of custom (user-created) voice presets - the Android analogue
 *  of frontend/src/pages/VoicesPage.tsx. Separate from per-book voice selection, which
 *  ReaderScreen's VoicePickerSheet already covers (it just lists whatever this screen creates,
 *  read-only). */
@HiltViewModel
class VoicesViewModel @Inject constructor(
    @ApplicationContext private val context: Context,
    private val voiceRepository: VoiceRepository,
    private val mediaUrlResolver: MediaUrlResolver,
    liveStore: LiveStore,
) : ViewModel() {

    private val _uiState = MutableStateFlow(VoicesUiState())
    val uiState: StateFlow<VoicesUiState> = _uiState

    // One ad-hoc player for whichever clip was most recently requested (a list row's saved
    // reference clip, or a form's typed-text test) - starting a new one always releases
    // whatever was still playing, same "only one at a time" behavior as the web's plain
    // <audio> elements naturally get from the browser.
    private var player: MediaPlayer? = null

    // Both lists are live topics (see LiveStore) - a save/delete here, or an edit from the web
    // UI, shows up without refetching.
    init {
        viewModelScope.launch {
            liveStore.customVoicePresets().collect { result ->
                _uiState.update {
                    it.copy(
                        loading = result.loading,
                        presets = result.data ?: it.presets,
                        error = result.error?.message,
                    )
                }
            }
        }
        viewModelScope.launch {
            liveStore.defaultVoice().collect { result ->
                result.data?.let { settings -> _uiState.update { it.copy(defaultPresetId = settings.presetId) } }
            }
        }
    }

    /** Sets the app-wide default voice (what a freshly-uploaded book starts with, before its
     *  own per-book voice is ever changed) - distinct from ReaderScreen's VoicePickerSheet,
     *  which only ever changes one specific book's voice. */
    fun setDefaultVoice(preset: CustomVoicePresetDto) {
        viewModelScope.launch {
            runCatching { voiceRepository.updateDefaultVoice(VoiceSettingsDto(preset.id, preset.instruct, "Auto")) }
                .onSuccess { _uiState.update { it.copy(defaultPresetId = preset.id) } }
        }
    }

    fun createPreset(input: CustomVoicePresetInputDto, onSuccess: () -> Unit, onError: (String) -> Unit) {
        viewModelScope.launch {
            runCatching { voiceRepository.createCustomPreset(input) }
                .onSuccess { onSuccess() }
                .onFailure { e -> onError(e.message ?: "Could not save voice") }
        }
    }

    fun updatePreset(id: String, input: CustomVoicePresetInputDto, onSuccess: () -> Unit, onError: (String) -> Unit) {
        viewModelScope.launch {
            runCatching { voiceRepository.updateCustomPreset(id, input) }
                .onSuccess { onSuccess() }
                .onFailure { e -> onError(e.message ?: "Could not save voice") }
        }
    }

    fun deletePreset(id: String) {
        viewModelScope.launch {
            runCatching { voiceRepository.deleteCustomPreset(id) }
                .onSuccess { _uiState.update { it.copy(presets = it.presets.filterNot { p -> p.id == id }) } }
        }
    }

    /** Plays a preset's already-rendered reference clip directly - the Android analogue of the
     *  web list's plain `<audio src={p.audioUrl}>` per-row player. */
    fun playReferenceClip(preset: CustomVoicePresetDto) {
        val url = mediaUrlResolver.resolve(preset.audioUrl) ?: return
        playInternal { setDataSource(url) }
    }

    /** Synthesizes [text] and plays the result once it's downloaded - either with a saved
     *  preset ([presetId] non-null) or, for a voice that isn't saved yet, a direct VoiceDesign
     *  preview of [instruct]. The Android analogue of the web's "Test"/"Preview" buttons.
     *  [onDone] always fires once (success or failure) - the caller's "Synthesizing…" button
     *  state hangs off it rather than assuming the fire-and-forget call already finished. */
    fun testVoice(
        presetId: String?,
        instruct: String,
        text: String,
        seed: Int?,
        onError: (String) -> Unit,
        onDone: () -> Unit,
    ) {
        viewModelScope.launch {
            runCatching {
                val body = if (presetId != null) {
                    voiceRepository.testCustomPreset(presetId, text)
                } else {
                    voiceRepository.testVoiceDesign(instruct, text, seed)
                }
                withContext(Dispatchers.IO) {
                    val file = File(context.cacheDir, "voice-test-${System.currentTimeMillis()}.wav")
                    file.writeBytes(body.bytes())
                    file
                }
            }.onSuccess { file -> playInternal { setDataSource(file.absolutePath) } }
                .onFailure { e -> onError(e.message ?: "Could not synthesize test text") }
            onDone()
        }
    }

    private inline fun playInternal(crossinline configure: MediaPlayer.() -> Unit) {
        player?.release()
        player = MediaPlayer().apply {
            configure()
            setOnPreparedListener { it.start() }
            setOnCompletionListener { mp -> mp.release(); if (player === mp) player = null }
            prepareAsync()
        }
    }

    override fun onCleared() {
        super.onCleared()
        player?.release()
        player = null
    }
}
