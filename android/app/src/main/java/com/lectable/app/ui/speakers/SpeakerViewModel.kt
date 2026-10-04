package com.lectable.app.ui.speakers

import android.media.MediaPlayer
import androidx.lifecycle.SavedStateHandle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.lectable.app.data.live.LiveStore
import com.lectable.app.data.remote.MediaUrlResolver
import com.lectable.app.data.remote.dto.BookDetailDto
import com.lectable.app.data.remote.dto.VoicePresetsDto
import com.lectable.app.data.remote.dto.CharacterVoiceModes
import com.lectable.app.data.remote.dto.CustomVoicePresetDto
import com.lectable.app.data.remote.dto.SpeakerAppearanceDto
import com.lectable.app.data.remote.dto.SpeakerDto
import com.lectable.app.data.remote.dto.VoicePresetDto
import com.lectable.app.data.remote.dto.VoiceSettingsDto
import com.lectable.app.data.repository.LibraryRepository
import com.lectable.app.data.repository.SpeakerRepository
import com.lectable.app.data.repository.VoiceRepository
import dagger.hilt.android.lifecycle.HiltViewModel
import javax.inject.Inject
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class SpeakerUiState(
    val bookTitle: String = "",
    // The book's own (narrator) voice name, for the Narrator row's "Narrates in the book's own
    // voice (X)" line - resolved from [builtinPresets]/[customPresets] against the book's voice
    // settings, same lookup frontend's own bookVoiceName does.
    val bookVoiceName: String? = null,
    val multiVoice: Boolean = false,
    val preprocessing: Boolean = false,
    val speakers: List<SpeakerDto> = emptyList(),
    val loading: Boolean = false,
    val error: String? = null,
    val characterizingIds: Set<String> = emptySet(),
    val generatingVoiceIds: Set<String> = emptySet(),
    val deletingIds: Set<String> = emptySet(),
    val togglingInvalidIds: Set<String> = emptySet(),
    val togglingRoleIds: Set<String> = emptySet(),
    // Which character's "View appearances" is currently expanded - null when none is.
    val appearancesFor: String? = null,
    val appearances: List<SpeakerAppearanceDto> = emptyList(),
    val appearancesLoading: Boolean = false,
    val builtinPresets: List<VoicePresetDto> = emptyList(),
    val customPresets: List<CustomVoicePresetDto> = emptyList(),
    // The relative URL (a SpeakerDto.refAudioUrl or SpeakerAppearanceDto.audioUrl - never
    // resolved) currently playing via [SpeakerViewModel.playUrl], or null when nothing is -
    // lets any row whose own URL matches show a "Stop" button in place of "Play" (see
    // SpeakersScreen's per-row play/stop toggle).
    val playingUrl: String? = null,
)

/** The Android analogue of frontend/src/pages/SpeakersPage.tsx - per-book character roster:
 *  review who's been identified so far (via [LibraryRepository.preprocessBook], already reachable
 *  from the library's own long-press menu), assign/regenerate voices, browse where each character
 *  speaks (reassigning lines to someone else), and turn on multi-voice narration
 *  once satisfied with the assignments. Deliberately scoped down from the web page: no per-chapter
 *  attribution/direction-tag status/individual buttons here (preprocess already
 *  covers the common "just run everything" path, and the reader's own chapter picker long-press
 *  menu covers attribution/tagging one chapter directly - see ReaderViewModel.runChapterPass)
 *  - see android/CLAUDE.md for the fuller reasoning. */
@HiltViewModel
class SpeakerViewModel @Inject constructor(
    savedStateHandle: SavedStateHandle,
    private val libraryRepository: LibraryRepository,
    private val speakerRepository: SpeakerRepository,
    private val voiceRepository: VoiceRepository,
    private val mediaUrlResolver: MediaUrlResolver,
    private val liveStore: LiveStore,
) : ViewModel() {

    private val bookId: String = checkNotNull(savedStateHandle["bookId"])

    private val _uiState = MutableStateFlow(SpeakerUiState())
    val uiState: StateFlow<SpeakerUiState> = _uiState

    // One-shot playback for a character's reference clip or one appearance line's own audio -
    // same plain MediaPlayer pattern as VoicesViewModel.playReferenceClip, no persistent
    // player/queue needed since only ever one preview plays at a time here.
    private var player: MediaPlayer? = null

    /** Plays a relative audio path (a [SpeakerDto.refAudioUrl] or [SpeakerAppearanceDto
     *  .audioUrl]) directly, resolved against whichever server is currently configured. Tapping
     *  the same row's own "Play"/"Stop" button again while it's already the one playing (see
     *  [SpeakerUiState.playingUrl]) calls [stopPlayback] instead of restarting it - a real
     *  toggle, mirroring what the web page's native `<audio controls>` elements already give it
     *  for free. */
    fun playUrl(relativeUrl: String) {
        if (_uiState.value.playingUrl == relativeUrl) {
            stopPlayback()
            return
        }
        val url = mediaUrlResolver.resolve(relativeUrl) ?: return
        player?.release()
        _uiState.update { it.copy(playingUrl = relativeUrl) }
        player = MediaPlayer().apply {
            setDataSource(url)
            setOnPreparedListener { it.start() }
            setOnCompletionListener { mp ->
                mp.release()
                if (player === mp) player = null
                _uiState.update { it.copy(playingUrl = null) }
            }
            prepareAsync()
        }
    }

    /** The explicit "Stop" tap (see [playUrl]'s own doc comment for the toggle path that also
     *  reaches this) - releases the player outright rather than pausing it. These are short
     *  preview clips, not something a listener resumes later from the same position, so "stop"
     *  (always starts over from the beginning next time) is the right verb, not "pause". */
    fun stopPlayback() {
        player?.release()
        player = null
        _uiState.update { it.copy(playingUrl = null) }
    }

    override fun onCleared() {
        super.onCleared()
        player?.release()
        player = null
    }

    // The expanded appearances list's own live subscription - cancelled on
    // collapse, so a closed list stops costing the backend anything.
    private var appearancesJob: Job? = null

    /** Everything this screen shows is a live topic (see [LiveStore]) - the roster's counts
     *  climbing as audio generates, a preprocessing run finishing, a characterization landing,
     *  and every mutation below all arrive on their own, with no poll or refetch. */
    init {
        viewModelScope.launch {
            combine(
                liveStore.book(bookId),
                liveStore.voice(bookId),
                liveStore.speakers(bookId),
                liveStore.voicePresets(),
                liveStore.customVoicePresets(),
            ) { book, voice, speakers, builtins, customs ->
                val builtinList = builtins.data?.presets ?: emptyList()
                val customList = customs.data ?: emptyList()
                val v = voice.data
                _uiState.update { state ->
                    state.copy(
                        bookTitle = book.data?.title ?: state.bookTitle,
                        bookVoiceName = v?.let { vs -> builtinList.find { it.id == vs.presetId }?.name ?: customList.find { it.id == vs.presetId }?.name },
                        multiVoice = v?.let { it.characterVoiceMode != CharacterVoiceModes.NARRATOR } ?: state.multiVoice,
                        preprocessing = book.data?.preprocessing ?: state.preprocessing,
                        speakers = speakers.data?.let(::sortedRoster) ?: state.speakers,
                        builtinPresets = builtinList,
                        customPresets = customList,
                        loading = speakers.loading && state.speakers.isEmpty(),
                        error = (book.error ?: speakers.error)?.message,
                    )
                }
            }.collect {}
        }
    }

    fun toggleMultiVoice(enabled: Boolean) {
        viewModelScope.launch {
            runCatching {
                // Re-fetches first rather than reusing whatever's already in uiState - this
                // screen has no persistent currentVoice cache the way ReaderViewModel does, and
                // the PUT below is a full replace (see VoiceSettingsDto's own doc comment), so a
                // stale in-memory copy here could clobber a preset/instruct/language change made
                // elsewhere (the reader's own VoicePickerSheet) since this screen was last opened.
                val voice = voiceRepository.getVoice(bookId)
                val newMode = if (enabled) CharacterVoiceModes.ASSIGNED else CharacterVoiceModes.NARRATOR
                voiceRepository.updateVoice(bookId, voice.copy(characterVoiceMode = newMode))
            }
                .onSuccess { updated -> _uiState.update { it.copy(multiVoice = updated.characterVoiceMode != CharacterVoiceModes.NARRATOR) } }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Mirrors the library's own long-press "Preprocess", just reachable without leaving this
     *  screen - see LibraryViewModel.preprocessBook's own doc comment for the optimistic-flag/
     *  reasoning - [SpeakerUiState.preprocessing] flips on and off via the live book topic. */
    fun preprocess() {
        viewModelScope.launch {
            runCatching { libraryRepository.preprocessBook(bookId) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** "Provision voices" - the web Speakers page's "Provision used voices" button: one Jobs row
     *  covering every voice this book's lines use that still lacks something (a character's
     *  voice, its reference clip, an emotion variant). The roster's own live topic shows the
     *  results; the snackbar only says what was queued. */
    fun provisionUsedVoices() {
        viewModelScope.launch {
            runCatching { libraryRepository.provisionUsedVoices(bookId) }
                .onSuccess { queued ->
                    val message = if (queued == 0) "Every voice this book uses is ready" else "Provisioning $queued ${if (queued == 1) "voice" else "voices"}"
                    _uiState.update { it.copy(error = message) }
                }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun characterize(characterId: String) {
        viewModelScope.launch {
            _uiState.update { it.copy(characterizingIds = it.characterizingIds + characterId) }
            runCatching { speakerRepository.characterize(bookId, characterId) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
            _uiState.update { it.copy(characterizingIds = it.characterizingIds - characterId) }
        }
    }

    fun generateVoice(characterId: String) {
        viewModelScope.launch {
            _uiState.update { it.copy(generatingVoiceIds = it.generatingVoiceIds + characterId) }
            runCatching { speakerRepository.generateVoice(bookId, characterId) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
            _uiState.update { it.copy(generatingVoiceIds = it.generatingVoiceIds - characterId) }
        }
    }

    fun setCharacterVoice(characterId: String, voicePresetId: String) {
        viewModelScope.launch {
            runCatching { speakerRepository.setCharacterVoice(bookId, characterId, voicePresetId) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Non-destructive, one tap to undo - no confirmation, same as the web page. The row's
     *  badge updates via the live speakers topic. */
    fun setInvalid(characterId: String, invalid: Boolean) {
        viewModelScope.launch {
            _uiState.update { it.copy(togglingInvalidIds = it.togglingInvalidIds + characterId) }
            runCatching { speakerRepository.setCharacterInvalid(bookId, characterId, invalid) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
            _uiState.update { it.copy(togglingInvalidIds = it.togglingInvalidIds - characterId) }
        }
    }

    /** Marks or unmarks a role label - one tap to undo, like setInvalid. */
    fun setRole(characterId: String, role: Boolean) {
        viewModelScope.launch {
            _uiState.update { it.copy(togglingRoleIds = it.togglingRoleIds + characterId) }
            runCatching { speakerRepository.setCharacterRole(bookId, characterId, role) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
            _uiState.update { it.copy(togglingRoleIds = it.togglingRoleIds - characterId) }
        }
    }

    fun deleteCharacter(characterId: String) {
        viewModelScope.launch {
            _uiState.update { it.copy(deletingIds = it.deletingIds + characterId) }
            runCatching { speakerRepository.deleteCharacter(bookId, characterId) }
                .onSuccess {
                    if (_uiState.value.appearancesFor == characterId) closeAppearances()
                }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
            _uiState.update { it.copy(deletingIds = it.deletingIds - characterId) }
        }
    }

    /** Renames a character; [onResult] gets null on success or the reason it was refused, so
     *  the rename dialog can stay open to correct it. The row updates via the live topic. */
    fun renameCharacter(characterId: String, name: String, onResult: (String?) -> Unit) {
        viewModelScope.launch {
            runCatching { speakerRepository.renameCharacter(bookId, characterId, name) }
                .onSuccess { onResult(null) }
                .onFailure { e -> onResult(e.message ?: "Could not rename") }
        }
    }

    fun mergeCharacter(characterId: String, targetName: String) {
        viewModelScope.launch {
            runCatching { speakerRepository.mergeCharacter(bookId, characterId, targetName) }
                .onSuccess {
                    if (_uiState.value.appearancesFor == characterId) closeAppearances()
                }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun deleteSpeakerData() {
        viewModelScope.launch {
            runCatching { speakerRepository.deleteSpeakerData(bookId) }
                .onSuccess {
                    closeAppearances()
                }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** "View appearances" toggle - collapses if [characterId] is already expanded, otherwise
     *  loads and expands it (collapsing whichever other character was open, same one-at-a-time
     *  behavior as frontend's own appearancesFor). */
    fun toggleAppearances(characterId: String) {
        if (_uiState.value.appearancesFor == characterId) {
            closeAppearances()
            return
        }
        _uiState.update { it.copy(appearancesFor = characterId, appearances = emptyList(), appearancesLoading = true) }
        loadAppearances(characterId)
    }

    private fun closeAppearances() {
        appearancesJob?.cancel()
        appearancesJob = null
        _uiState.update { it.copy(appearancesFor = null, appearances = emptyList(), appearancesLoading = false) }
    }

    private fun loadAppearances(characterId: String) {
        appearancesJob?.cancel()
        appearancesJob = viewModelScope.launch {
            liveStore.characterAppearances(bookId, characterId).collect { result ->
                _uiState.update {
                    it.copy(
                        appearances = result.data ?: it.appearances,
                        appearancesLoading = result.loading,
                        error = result.error?.message ?: it.error,
                    )
                }
            }
        }
    }

    /** An appearance row's own "Regenerate"/"Generate" action - the row's reset-to-pending
     *  status (and later its new audio) arrives on the live appearances topic. */
    fun regenerateAppearance(chapterIdx: Int, paragraphIdx: Int) {
        if (_uiState.value.appearancesFor == null) return
        viewModelScope.launch {
            runCatching { libraryRepository.regenerateParagraph(bookId, chapterIdx, paragraphIdx) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** An appearance row's own "Reassign to…" action - the reassigned line is no longer this
     *  character's, so it drops out of the live appearances list, and the roster's paragraph
     *  counts move between the two rows, both on their own. */
    fun reassignAppearance(chapterIdx: Int, paragraphIdx: Int, speaker: String) {
        if (_uiState.value.appearancesFor == null) return
        viewModelScope.launch {
            runCatching { libraryRepository.setParagraphSpeaker(bookId, chapterIdx, paragraphIdx, speaker) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun dismissError() {
        _uiState.update { it.copy(error = null) }
    }
}

/** The roster in display order: the Narrator row first, then everyone else alphabetically
 *  (case-insensitive) - rather than the backend's appearance order, so a name is easy to find.
 *  Every list derived from it (merge/reassign targets) inherits the order. Mirrors frontend's
 *  SpeakersPage sortedRoster. */
internal fun sortedRoster(speakers: List<SpeakerDto>): List<SpeakerDto> =
    speakers.sortedWith(compareBy<SpeakerDto> { it.id.isNotEmpty() || it.name != "Narrator" }.thenBy(String.CASE_INSENSITIVE_ORDER) { it.name })
