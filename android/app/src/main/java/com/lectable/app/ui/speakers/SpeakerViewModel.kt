package com.lectable.app.ui.speakers

import android.media.MediaPlayer
import androidx.lifecycle.SavedStateHandle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.lectable.app.data.live.LiveStore
import com.lectable.app.data.remote.MediaUrlResolver
import com.lectable.app.data.remote.dto.BookDetailDto
import com.lectable.app.data.remote.dto.VoicePresetsDto
import com.lectable.app.data.remote.dto.CHARACTER_VOICE_MODE_ASSIGNED
import com.lectable.app.data.remote.dto.CHARACTER_VOICE_MODE_NARRATOR
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
    // Which character's "View appearances" is currently expanded - null when none is.
    val appearancesFor: String? = null,
    val appearances: List<SpeakerAppearanceDto> = emptyList(),
    val appearancesLoading: Boolean = false,
    // [appearancesFor]'s own description-tagging counterpart - independently expandable, so a
    // reader can have both a character's appearances and their descriptions open at once.
    val descriptionsFor: String? = null,
    val descriptions: List<SpeakerAppearanceDto> = emptyList(),
    val descriptionsLoading: Boolean = false,
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
 *  speaks or is described (reassigning either to someone else), and turn on multi-voice narration
 *  once satisfied with the assignments. Deliberately scoped down from the web page: no per-chapter
 *  attribution/description/direction-tag status/individual buttons here (preprocess already
 *  covers the common "just run everything" path, and the reader's own chapter picker long-press
 *  menu covers attribution/tagging one chapter directly - see ReaderViewModel.attributeChapter)
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

    // The expanded appearances/descriptions lists' own live subscriptions - cancelled on
    // collapse, so a closed list stops costing the backend anything.
    private var appearancesJob: Job? = null
    private var descriptionsJob: Job? = null

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
                        multiVoice = v?.let { it.characterVoiceMode != CHARACTER_VOICE_MODE_NARRATOR } ?: state.multiVoice,
                        preprocessing = book.data?.preprocessing ?: state.preprocessing,
                        speakers = speakers.data ?: state.speakers,
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
                val newMode = if (enabled) CHARACTER_VOICE_MODE_ASSIGNED else CHARACTER_VOICE_MODE_NARRATOR
                voiceRepository.updateVoice(bookId, voice.copy(characterVoiceMode = newMode))
            }
                .onSuccess { updated -> _uiState.update { it.copy(multiVoice = updated.characterVoiceMode != CHARACTER_VOICE_MODE_NARRATOR) } }
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

    fun deleteCharacter(characterId: String) {
        viewModelScope.launch {
            _uiState.update { it.copy(deletingIds = it.deletingIds + characterId) }
            runCatching { speakerRepository.deleteCharacter(bookId, characterId) }
                .onSuccess {
                    if (_uiState.value.appearancesFor == characterId) closeAppearances()
                    if (_uiState.value.descriptionsFor == characterId) closeDescriptions()
                }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
            _uiState.update { it.copy(deletingIds = it.deletingIds - characterId) }
        }
    }

    fun mergeCharacter(characterId: String, targetName: String) {
        viewModelScope.launch {
            runCatching { speakerRepository.mergeCharacter(bookId, characterId, targetName) }
                .onSuccess {
                    if (_uiState.value.appearancesFor == characterId) closeAppearances()
                    if (_uiState.value.descriptionsFor == characterId) closeDescriptions()
                }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun deleteSpeakerData() {
        viewModelScope.launch {
            runCatching { speakerRepository.deleteSpeakerData(bookId) }
                .onSuccess {
                    closeAppearances()
                    closeDescriptions()
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

    /** "View descriptions" toggle - [toggleAppearances]'s own counterpart, independently
     *  expandable (a character's appearances and descriptions can be open at the same time,
     *  unlike frontend, which only tracks one of each per character but still lets both be open
     *  simultaneously for the same reason). */
    fun toggleDescriptions(characterId: String) {
        if (_uiState.value.descriptionsFor == characterId) {
            closeDescriptions()
            return
        }
        _uiState.update { it.copy(descriptionsFor = characterId, descriptions = emptyList(), descriptionsLoading = true) }
        loadDescriptions(characterId)
    }

    private fun closeDescriptions() {
        descriptionsJob?.cancel()
        descriptionsJob = null
        _uiState.update { it.copy(descriptionsFor = null, descriptions = emptyList(), descriptionsLoading = false) }
    }

    private fun loadDescriptions(characterId: String) {
        descriptionsJob?.cancel()
        descriptionsJob = viewModelScope.launch {
            liveStore.characterDescriptions(bookId, characterId).collect { result ->
                _uiState.update {
                    it.copy(
                        descriptions = result.data ?: it.descriptions,
                        descriptionsLoading = result.loading,
                        error = result.error?.message ?: it.error,
                    )
                }
            }
        }
    }

    /** A description row's own "Reassign to…" action - moves this paragraph off the currently
     *  expanded character's own describes-list and onto targetName's instead ("Narrator"/""
     *  clears it, describing no one), the same [chapterIdx]/[paragraphIdx]-addressed shape
     *  [reassignAppearance] uses. Looks the current character's own name up from the roster
     *  (the backend call is name-keyed, not id-keyed - see LibraryRepository.setParagraphDescription).
     *  The reassigned paragraph is no longer this character's, so it drops out of the live
     *  list (and a brand-new target character shows up in the roster) on its own. */
    fun reassignDescription(chapterIdx: Int, paragraphIdx: Int, targetName: String) {
        val characterId = _uiState.value.descriptionsFor ?: return
        val fromName = _uiState.value.speakers.find { it.id == characterId }?.name ?: return
        viewModelScope.launch {
            runCatching { libraryRepository.setParagraphDescription(bookId, chapterIdx, paragraphIdx, fromName, targetName) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun dismissError() {
        _uiState.update { it.copy(error = null) }
    }
}
