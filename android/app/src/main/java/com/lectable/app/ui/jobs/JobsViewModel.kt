package com.lectable.app.ui.jobs

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.lectable.app.data.remote.LiveClient
import com.lectable.app.data.remote.dto.CustomVoicePresetDto
import com.lectable.app.data.remote.dto.JobsSnapshotDto
import com.lectable.app.data.remote.dto.QueueTaskDto
import com.lectable.app.data.repository.JobsRepository
import com.lectable.app.data.remote.dto.VoicePresetsDto
import dagger.hilt.android.lifecycle.HiltViewModel
import javax.inject.Inject
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class JobsUiState(
    val loading: Boolean = true,
    val snapshot: JobsSnapshotDto? = null,
    val error: String? = null,
    // Tracked separately from a generic "is a cancel in flight" bool so two different rows'
    // cancel buttons don't show each other's spinner/disabled state if tapped in quick
    // succession - mirrors frontend's JobsPage.tsx cancelingId.
    val cancelingId: String? = null,
    // presetId -> friendly name, resolved from the built-in/custom preset lists VoicesScreen
    // already fetches elsewhere - a task with no presetId (a pure custom instruct, or one of
    // the two LLM kinds) falls back to a preview of its own instruct text instead.
    val presetNames: Map<String, String> = emptyMap(),
    // True for the whole duration of a pause/resume call - covers the (usually brief) gap
    // before the jobs topic's own push of the new paused state arrives, same reasoning
    // frontend's usePauseJobs/useResumeJobs give for their own invalidation.
    val togglingPause: Boolean = false,
    // True for the whole duration of a restartWorker call, which blocks server-side until the
    // new worker is confirmed healthy (can take a few seconds) - mirrors frontend's
    // restartWorker.isPending, shown as a spinning restart icon.
    val restarting: Boolean = false,
)

/**
 * Live view of the backend's shared job queue (backend/internal/jobs.Manager) - the Android
 * analogue of frontend/src/pages/JobsPage.tsx. The queue and both preset lists (for friendly
 * voice names) are live topics (see [LiveClient]), so every change - a task starting,
 * finishing, a pause/resume from anywhere - shows up without polling.
 */
@HiltViewModel
class JobsViewModel @Inject constructor(
    private val repository: JobsRepository,
    liveClient: LiveClient,
) : ViewModel() {

    private val _uiState = MutableStateFlow(JobsUiState())
    val uiState: StateFlow<JobsUiState> = _uiState

    init {
        viewModelScope.launch {
            liveClient.observe<JobsSnapshotDto>("jobs").collect { result ->
                _uiState.update {
                    it.copy(
                        snapshot = result.data ?: it.snapshot,
                        loading = result.loading,
                        error = result.error?.message ?: it.error,
                    )
                }
            }
        }
        viewModelScope.launch {
            combine(
                liveClient.observe<VoicePresetsDto>("voicePresets"),
                liveClient.observe<List<CustomVoicePresetDto>>("customVoicePresets"),
            ) { builtins, customs ->
                buildMap {
                    builtins.data?.presets?.forEach { put(it.id, it.name) }
                    customs.data?.forEach { put(it.id, it.name) }
                }
            }.collect { names -> _uiState.update { it.copy(presetNames = names) } }
        }
    }

    fun cancelJob(task: QueueTaskDto) {
        _uiState.update { it.copy(cancelingId = task.id, error = null) }
        viewModelScope.launch {
            runCatching { repository.cancelJob(task.id) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message ?: "Could not cancel this job") } }
            _uiState.update { it.copy(cancelingId = null) }
        }
    }

    fun cancelAll() {
        viewModelScope.launch {
            runCatching { repository.cancelAllJobs() }
                .onFailure { e -> _uiState.update { it.copy(error = e.message ?: "Could not cancel all jobs") } }
        }
    }

    /** Job queue screen's pause/resume toggle - not destructive (unlike cancel/cancel all), so
     *  no confirmation prompt: nothing already in flight is touched, only whether the queue
     *  dispatches anything *new*. */
    fun togglePause() {
        val paused = _uiState.value.snapshot?.paused ?: return
        _uiState.update { it.copy(togglingPause = true, error = null) }
        viewModelScope.launch {
            runCatching { if (paused) repository.resumeJobs() else repository.pauseJobs() }
                .onFailure { e ->
                    _uiState.update {
                        it.copy(error = e.message ?: "Could not ${if (paused) "resume" else "pause"} the queue")
                    }
                }
            _uiState.update { it.copy(togglingPause = false) }
        }
    }

    /** Kills and respawns ttsworker right now - the same thing the backend's own watchdog does
     *  automatically on an RSS breach or crash, just triggered manually. Disruptive to whatever
     *  is in flight (the restart blocks every proxied TTS/LLM call until the new worker is
     *  healthy), so JobsScreen confirms first, same as cancel all. */
    fun restartWorker() {
        _uiState.update { it.copy(restarting = true, error = null) }
        viewModelScope.launch {
            runCatching { repository.restartWorker() }
                .onFailure { e -> _uiState.update { it.copy(error = e.message ?: "Could not restart ttsworker") } }
            _uiState.update { it.copy(restarting = false) }
        }
    }

    fun dismissError() {
        _uiState.update { it.copy(error = null) }
    }

}

/** Friendly display name for a target/voice column - mirrors JobsPage.tsx's targetLabel/
 *  voiceName. [targetLabel] is [QueueTaskDto.label] when present (a character name, or a
 *  preview sequence tag), otherwise the chapter title/number, since label-less kinds are always
 *  chapter-scoped. */
fun targetLabel(t: QueueTaskDto): String = t.label?.takeIf { it.isNotEmpty() } ?: (t.chapterTitle.takeIf { it.isNotEmpty() } ?: "Chapter ${t.chapterIdx + 1}")

private const val INSTRUCT_PREVIEW_LENGTH = 40

fun voiceName(t: QueueTaskDto, presetNames: Map<String, String>): String {
    if (t.presetId.isNotEmpty()) return presetNames[t.presetId] ?: t.presetId
    if (t.instruct.isEmpty()) return "—"
    return if (t.instruct.length > INSTRUCT_PREVIEW_LENGTH) t.instruct.take(INSTRUCT_PREVIEW_LENGTH) + "…" else t.instruct
}

/** Human-readable label for a task's kind - falls back to the raw string for anything not yet
 *  known here (see QueueTaskDto.kind's own doc comment on why kind isn't a closed enum). */
fun kindLabel(kind: String): String = when (kind) {
    "voice_clone" -> "Clone"
    "voice_design" -> "Design"
    "voice_design_preview" -> "Design Preview"
    "voice_provision" -> "Voice Provision"
    "speaker_attribution" -> "Attribution"
    "speaker_characterization" -> "Characterization"
    "pipeline_generate_chapter" -> "Generate: Chapter"
    "pipeline_generate_book" -> "Generate: All"
    "pipeline_generate_remaining" -> "Generate: Remaining"
    else -> kind
}
