package com.lectable.app.ui.llm

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.dto.TestLLMRequestDto
import dagger.hilt.android.lifecycle.HiltViewModel
import javax.inject.Inject
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import retrofit2.HttpException

data class LlmTestUiState(
    val systemPrompt: String = "",
    val userPrompt: String = "",
    // Text, not numbers - blank is "unset, use the backend's default" (see TestLLMRequestDto).
    val temp: String = "",
    val maxTokens: String = "",
    val generating: Boolean = false,
    val result: String? = null,
    /** How long the last finished call took, end to end. */
    val seconds: Double? = null,
    val error: String? = null,
) {
    val canRun: Boolean get() = userPrompt.isNotBlank() && !generating
}

/** Sends a raw system/user prompt pair to the backend's embedded speaker-attribution model
 *  (POST /api/llm/test) and shows the text it generates - the Android analogue of
 *  frontend/src/pages/LLMPage.tsx. Nothing is saved; the prompts live as long as the screen. */
@HiltViewModel
class LlmTestViewModel @Inject constructor(
    private val api: LectableApi,
) : ViewModel() {

    private val _uiState = MutableStateFlow(LlmTestUiState())
    val uiState: StateFlow<LlmTestUiState> = _uiState

    private var call: Job? = null

    fun setSystemPrompt(value: String) = _uiState.update { it.copy(systemPrompt = value) }
    fun setUserPrompt(value: String) = _uiState.update { it.copy(userPrompt = value) }
    fun setTemp(value: String) = _uiState.update { it.copy(temp = value) }
    fun setMaxTokens(value: String) = _uiState.update { it.copy(maxTokens = value) }

    fun run() {
        val state = _uiState.value
        if (!state.canRun) return
        _uiState.update { it.copy(generating = true, result = null, seconds = null, error = null) }
        call = viewModelScope.launch {
            val started = System.nanoTime()
            runCatching {
                api.testLLM(
                    TestLLMRequestDto(
                        systemPrompt = state.systemPrompt.trim().ifEmpty { null },
                        userPrompt = state.userPrompt.trim(),
                        temp = state.temp.toDoubleOrNull(),
                        maxTokens = state.maxTokens.toIntOrNull(),
                    ),
                ).text
            }
                .onSuccess { text ->
                    _uiState.update { it.copy(result = text, seconds = (System.nanoTime() - started) / 1e9) }
                }
                .onFailure { e ->
                    if (e is CancellationException) throw e // cancel() already reset the state
                    _uiState.update { it.copy(error = errorMessage(e)) }
                }
            _uiState.update { it.copy(generating = false) }
        }
    }

    /** Stops waiting for the answer. The backend drops the call when the connection closes. */
    fun cancel() {
        call?.cancel()
        _uiState.update { it.copy(generating = false) }
    }

    // The backend's own {"error": "..."} message when it sent one.
    private fun errorMessage(e: Throwable): String {
        if (e is HttpException) {
            runCatching {
                Json.parseToJsonElement(e.response()?.errorBody()?.string().orEmpty())
                    .jsonObject["error"]?.jsonPrimitive?.content
            }.getOrNull()?.let { return it }
        }
        return e.message ?: "Could not generate text"
    }
}
