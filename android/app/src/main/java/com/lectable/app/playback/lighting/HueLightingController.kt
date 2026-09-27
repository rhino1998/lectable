package com.lectable.app.playback.lighting

import com.lectable.app.data.remote.dto.ChapterMusicDto
import com.lectable.app.data.remote.dto.LightingDto
import com.lectable.app.data.settings.LightingSettings
import com.lectable.app.data.settings.LightingSettingsRepository
import com.lectable.app.playback.ParagraphPlayer
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

enum class LightingStatus { OFF, UNCONFIGURED, CONNECTING, ACTIVE, ERROR }

data class LightingState(
    val status: LightingStatus = LightingStatus.OFF,
    val message: String? = null,
    val cue: LightingDto? = null,
)

/**
 * Drives the listener's Philips Hue lights from the ambience pass's lighting cues (each music
 * region's `lighting`, see backend store.Lighting) as [ParagraphPlayer] moves through a chapter -
 * the Android twin of frontend/src/lighting/controller.ts + hooks/useLighting.ts. App-scoped like
 * [com.lectable.app.playback.BackgroundMusicPlayer], and fed chapter regions the same way
 * (ReaderViewModel.onChapterMusic -> [setChapterMusic]), so the room keeps following the book
 * with the screen off. Observes [ParagraphPlayer.state] itself for the current paragraph and
 * play/pause.
 *
 * The engine runs whenever lighting is enabled and configured; turning it off (or changing
 * bridge/room) stops it and, with [LightingSettings.restoreOnStop], restores the lights.
 */
@Singleton
class HueLightingController @Inject constructor(
    private val paragraphPlayer: ParagraphPlayer,
    private val settingsRepository: LightingSettingsRepository,
) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private val _state = MutableStateFlow(LightingState())
    val state: StateFlow<LightingState> = _state.asStateFlow()

    private var settings = LightingSettings()
    private var engine: LightingEngine? = null
    private var restartJob: Job? = null
    private var bookId: String? = null
    private val chapterMusic = mutableMapOf<Int, ChapterMusicDto>()
    private var position = -1 to -1
    private var playing = false

    init {
        scope.launch {
            settingsRepository.settings.collect { next ->
                val prev = settings
                settings = next
                if (next.enabled != prev.enabled || next.bridgeIp != prev.bridgeIp ||
                    next.username != prev.username || next.certPin != prev.certPin || next.groupId != prev.groupId
                ) {
                    restart()
                } else if (next.brightnessCap != prev.brightnessCap) {
                    engine?.setCap(next.brightnessCap)
                }
            }
        }
        scope.launch {
            paragraphPlayer.state.collect { s ->
                if (s.isPlaying != playing) {
                    playing = s.isPlaying
                    engine?.setPlaying(playing)
                }
                val pos = s.chapterIdx to s.paragraphIdx
                if (pos != position) {
                    position = pos
                    applyCurrentCue()
                }
            }
        }
    }

    /** Whether lighting wants chapter regions fed to it - ReaderViewModel keeps chapterMusic
     *  subscriptions open while this or background music is on. */
    val enabled: Flow<Boolean> = settingsRepository.settings.map { it.enabled }.distinctUntilChanged()

    /** Forgets another book's regions when the reader opens a different one. */
    fun setBook(id: String) {
        if (id != bookId) {
            bookId = id
            chapterMusic.clear()
        }
    }

    fun setChapterMusic(chapterIdx: Int, music: ChapterMusicDto) {
        chapterMusic[chapterIdx] = music
        if (chapterIdx == position.first) applyCurrentCue()
    }

    private fun applyCurrentCue() {
        val (chapterIdx, paragraphIdx) = position
        val region = chapterMusic[chapterIdx]?.regions?.firstOrNull { paragraphIdx in it.startIdx..it.endIdx }
        val cue = region?.lighting ?: return // an unlit region holds whatever is showing
        _state.update { it.copy(cue = cue) }
        engine?.setCue(cue, cut = region.transition != "continuation")
    }

    private fun restart() {
        val previous = restartJob
        restartJob = scope.launch {
            previous?.join()
            engine?.let {
                engine = null
                it.stop(settings.restoreOnStop)
            }
            val s = settings
            when {
                !s.enabled -> {
                    _state.update { it.copy(status = LightingStatus.OFF, message = null) }
                    return@launch
                }
                !s.configured -> {
                    _state.update { it.copy(status = LightingStatus.UNCONFIGURED, message = null) }
                    return@launch
                }
            }
            _state.update { it.copy(status = LightingStatus.CONNECTING, message = null) }
            try {
                val bridge = HueBridgeClient(s.bridgeIp, s.username, s.certPin)
                val group = bridge.groups().firstOrNull { it.id == s.groupId }
                    ?: error("the selected room no longer exists on the bridge")
                val members = bridge.lights().filter { it.id in group.lights }
                val next = LightingEngine(
                    scope = scope,
                    bridge = bridge,
                    groupId = group.id,
                    lights = members,
                    cap = s.brightnessCap,
                    onError = { msg -> _state.update { it.copy(status = LightingStatus.ERROR, message = msg) } },
                    onOk = {
                        if (_state.value.status == LightingStatus.ERROR) {
                            _state.update { it.copy(status = LightingStatus.ACTIVE, message = null) }
                        }
                    },
                )
                next.start()
                next.setPlaying(playing)
                engine = next
                _state.update { it.copy(status = LightingStatus.ACTIVE, message = null) }
                applyCurrentCue()
            } catch (e: Exception) {
                if (e is kotlinx.coroutines.CancellationException) throw e
                _state.update { it.copy(status = LightingStatus.ERROR, message = e.message ?: e.toString()) }
            }
        }
    }
}
