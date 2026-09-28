package com.lectable.app.ui.lexicon

import android.content.Context
import android.media.MediaPlayer
import androidx.lifecycle.SavedStateHandle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.lectable.app.data.live.LiveStore
import com.lectable.app.data.remote.dto.LexiconEntryDto
import com.lectable.app.data.remote.dto.LexiconSuggestionDto
import com.lectable.app.data.remote.dto.UpsertLexiconRequestDto
import com.lectable.app.data.repository.LexiconRepository
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.File
import javax.inject.Inject
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

data class LexiconUiState(
    val bookTitle: String = "",
    // The series the rules are shared across ("" for a standalone book).
    val seriesName: String = "",
    val entries: List<LexiconEntryDto> = emptyList(),
    val suggestions: List<LexiconSuggestionDto> = emptyList(),
    val loading: Boolean = true,
    val error: String? = null,
    val saving: Boolean = false,
    // "form" for the unsaved rule being edited, or a saved entry's id - whichever preview is
    // rendering right now.
    val previewingKey: String? = null,
)

/** The Android analogue of frontend's components/speakers/LexiconPanel.tsx: the book's
 *  series-wide pronunciation lexicon ("say Nyx as Nicks"), with listen-before-save previews in
 *  the narrator's voice and roster-name suggestions. Saving/removing a rule regenerates every
 *  line it matches, server-side (backend httpapi/lexicon.go). */
@HiltViewModel
class LexiconViewModel @Inject constructor(
    savedStateHandle: SavedStateHandle,
    @ApplicationContext private val context: Context,
    private val repository: LexiconRepository,
    liveStore: LiveStore,
) : ViewModel() {

    private val bookId: String = checkNotNull(savedStateHandle["bookId"])

    private val _uiState = MutableStateFlow(LexiconUiState())
    val uiState: StateFlow<LexiconUiState> = _uiState

    private var player: MediaPlayer? = null

    init {
        viewModelScope.launch {
            combine(
                liveStore.book(bookId),
                liveStore.lexicon(bookId),
                liveStore.lexiconSuggestions(bookId),
            ) { book, lexicon, suggestions -> Triple(book, lexicon, suggestions) }
                .collect { (book, lexicon, suggestions) ->
                    _uiState.update {
                        it.copy(
                            bookTitle = book.data?.title ?: it.bookTitle,
                            seriesName = book.data?.seriesName ?: it.seriesName,
                            entries = lexicon.data ?: it.entries,
                            suggestions = suggestions.data ?: it.suggestions,
                            loading = lexicon.data == null && lexicon.error == null,
                            error = it.error ?: lexicon.error?.message,
                        )
                    }
                }
        }
    }

    /** Adds the rule, or replaces the one for the same word; [onSaved] clears the form. */
    fun save(rule: UpsertLexiconRequestDto, onSaved: () -> Unit) {
        if (rule.term.isBlank() || rule.replacement.isBlank()) return
        _uiState.update { it.copy(saving = true) }
        viewModelScope.launch {
            runCatching { repository.upsert(bookId, rule.trimmed()) }
                .onSuccess { onSaved() }
                .onFailure { e -> _uiState.update { it.copy(error = e.message ?: "Could not save this rule") } }
            _uiState.update { it.copy(saving = false) }
        }
    }

    fun delete(entry: LexiconEntryDto) {
        viewModelScope.launch {
            runCatching { repository.delete(bookId, entry.id) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message ?: "Could not remove this rule") } }
        }
    }

    /** Plays the narrator reading the first line the rule matches, with it applied. */
    fun preview(key: String, rule: UpsertLexiconRequestDto) {
        if (rule.term.isBlank() || rule.replacement.isBlank() || _uiState.value.previewingKey != null) return
        _uiState.update { it.copy(previewingKey = key) }
        viewModelScope.launch {
            runCatching {
                val body = repository.preview(bookId, rule.trimmed())
                withContext(Dispatchers.IO) {
                    File(context.cacheDir, "lexicon-preview.wav").apply { writeBytes(body.bytes()) }
                }
            }.onSuccess { file ->
                player?.release()
                player = MediaPlayer().apply {
                    setDataSource(file.absolutePath)
                    setOnPreparedListener { it.start() }
                    setOnCompletionListener { mp -> mp.release(); if (player === mp) player = null }
                    prepareAsync()
                }
            }.onFailure { e -> _uiState.update { it.copy(error = e.message ?: "Could not preview this rule") } }
            _uiState.update { it.copy(previewingKey = null) }
        }
    }

    fun dismissError() {
        _uiState.update { it.copy(error = null) }
    }

    override fun onCleared() {
        super.onCleared()
        player?.release()
        player = null
    }
}

private fun UpsertLexiconRequestDto.trimmed() = copy(term = term.trim(), replacement = replacement.trim())

/** A saved rule as the request that would recreate it - for previewing and editing. */
fun LexiconEntryDto.toRequest() = UpsertLexiconRequestDto(
    term = term,
    replacement = replacement,
    caseSensitive = caseSensitive,
    prefix = prefix,
    global = global,
)
