package com.lectable.app.ui.reader

import android.content.Context
import android.media.AudioAttributes
import android.media.MediaPlayer
import android.os.PowerManager
import android.os.SystemClock
import androidx.lifecycle.SavedStateHandle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import androidx.work.ExistingWorkPolicy
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkManager
import com.lectable.app.data.live.LiveStore
import com.lectable.app.data.remote.LiveResult
import com.lectable.app.data.remote.MediaUrlResolver
import com.lectable.app.data.remote.dto.AudioStatuses
import com.lectable.app.data.remote.dto.BookDetailDto
import com.lectable.app.data.remote.dto.BookRecapDto
import com.lectable.app.data.remote.dto.ChapterNotesDto
import com.lectable.app.data.remote.dto.ChapterNotesResponseDto
import com.lectable.app.data.remote.dto.CharacterNameDto
import com.lectable.app.data.remote.dto.BookmarkDto
import com.lectable.app.data.remote.dto.CharacterVoiceModes
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ChapterMusicDto
import com.lectable.app.data.remote.dto.CustomVoicePresetDto
import com.lectable.app.data.remote.dto.PositionDto
import com.lectable.app.data.remote.dto.SearchResultDto
import com.lectable.app.data.remote.dto.VoicePresetDto
import com.lectable.app.data.remote.dto.VoiceSettingsDto
import com.lectable.app.data.download.DownloadStatus
import com.lectable.app.data.repository.ChapterDownloadState
import com.lectable.app.data.repository.DownloadRepository
import com.lectable.app.data.repository.JobsRepository
import com.lectable.app.data.repository.LibraryRepository
import com.lectable.app.data.repository.VoiceRepository
import com.lectable.app.data.repository.voiceKeyOf
import com.lectable.app.data.settings.ReaderFontFamily
import com.lectable.app.data.settings.DEFAULT_FONT_SIZE_SP
import com.lectable.app.data.settings.DEFAULT_MUSIC_VOLUME
import com.lectable.app.data.settings.PlaybackSettingsRepository
import com.lectable.app.data.settings.ReadingSettingsRepository
import com.lectable.app.data.settings.LightingSettingsRepository
import com.lectable.app.playback.lighting.LightingStatus
import com.lectable.app.playback.BackgroundMusicPlayer
import com.lectable.app.playback.lighting.HueLightingController
import com.lectable.app.playback.AmbienceDownloadWorker
import com.lectable.app.playback.ChapterDownloadWorker
import com.lectable.app.playback.ParagraphPlayer
import com.lectable.app.playback.PlaybackState
import com.lectable.app.playback.SleepTimerOption
import dagger.hilt.android.lifecycle.HiltViewModel
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import kotlinx.coroutines.async
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

private const val POSITION_SAVE_MIN_INTERVAL_MS = 3_000L

// How long after asking the backend to generate a chapter before asking again - a live
// chapter update can arrive several times a second while it's pending, and the backend's own
// `generating` flag only flips once the enqueue has landed.
private const val GENERATE_RETRIGGER_MS = 5_000L

data class ReaderUiState(
    val loading: Boolean = true,
    val book: BookDetailDto? = null,
    // The window of chapters currently mounted in the scroll list - the Android analogue of
    // ReaderPage.tsx's `chapters: Map<number, ChapterDetail>` plus its `{start, end}` range
    // state. Grows via expandUp/expandDown as the reader scrolls near either edge, instead of
    // requiring a chapter to be explicitly picked.
    val loadedChapters: Map<Int, ChapterDetailDto> = emptyMap(),
    val range: IntRange = 0..0,
    val playback: PlaybackState = PlaybackState(),
    val voicePresetName: String? = null,
    // The book's current voice language (e.g. "Auto", "English") - seeded into
    // VoicePickerSheet's language dropdown so switching narrator preset doesn't silently reset
    // it back to "Auto" (see setVoicePreset).
    val voiceLanguage: String = "Auto",
    // Whether this book's paragraphs narrate in per-character voices (when attributed and
    // assigned server-side) instead of always the book's own voice - see backend's
    // store.Book.MultiVoice. Toggled from VoicePickerSheet; the character roster/voice
    // assignment UI itself (the web frontend's Speakers page) isn't ported to Android.
    val multiVoice: Boolean = false,
    // Reader-facing background-music toggle (see backend store.Book.MusicEnabled) - book-wide,
    // toggled from VoicePickerSheet alongside multiVoice above. See [BackgroundMusicPlayer].
    val musicEnabled: Boolean = false,
    // This book's own loaded chapters' background-music regions, keyed by chapterIdx - backs
    // annotations mode's boundary markers (mirrors frontend's ReaderPage.tsx chapterMusicRegions)
    // and is also what feeds [BackgroundMusicPlayer] itself (see onChapterMusic). Populated
    // (and kept live) only while [musicEnabled] is on - see syncMusicSubscriptions.
    val chapterMusic: Map<Int, ChapterMusicDto> = emptyMap(),
    // Set while a region's own standalone preview clip (see previewMusicRegion) is playing - a
    // brand-new, independent player, not [player]/[BackgroundMusicPlayer] - so a reader can
    // audition a region without pausing/ducking narration or the live background mix, mirroring
    // frontend's own MusicRegionPreviewButton.
    val previewingMusicRegionId: String? = null,
    // (chapterIdx, paragraphIdx) -> bookmark id, mirroring ReaderPage.tsx's bookmarkByKey.
    val bookmarksByKey: Map<Pair<Int, Int>, String> = emptyMap(),
    // Android-only reading preferences (see ReadingSettingsRepository) - no web-frontend
    // equivalent to mirror.
    val fontSizeSp: Int = DEFAULT_FONT_SIZE_SP,
    val fontFamily: ReaderFontFamily = ReaderFontFamily.DEFAULT,
    val error: String? = null,
    // Set alongside `error` specifically when it came from a chapter failing to load (as
    // opposed to any of `error`'s other sources - voice/audio actions, etc.) - lets the error
    // snackbar offer a "Retry" action for this one case, where there's an obvious single thing
    // to retry. Cleared whenever that chapter's own fetch next succeeds (including via the
    // retry itself), not just on dismissError.
    val failedChapterIdx: Int? = null,
    // Offline downloads (see DownloadRepository) - voiceKey mirrors the book's current voice
    // settings, downloadStates is per-chapter download status/progress (any voice;
    // ParagraphPlayer itself only uses a local file that matches voiceKey), and
    // bookDownloadProgress is (chaptersComplete, chaptersTotal) for a manual "download book"
    // action in flight, null when none is running.
    val voiceKey: String? = null,
    val downloadStates: Map<Int, ChapterDownloadState> = emptyMap(),
    val bookDownloadProgress: Pair<Int, Int>? = null,
    // The book's saved position when another device moved it while this reader sat paused -
    // see remotePosition. Null otherwise.
    val remotePosition: ParagraphPos? = null,
    // Persisted (ReadingSettingsRepository.annotationsMode) - see setAnnotationsMode.
    val annotationsMode: Boolean = false,
    // Hue room lighting, for the playback bar's lighting toggle - see toggleLighting.
    val lightingEnabled: Boolean = false,
    val lightingConfigured: Boolean = false,
    val lightingStatus: LightingStatus = LightingStatus.OFF,
    // The series roster (names only, live) for the set-speaker picker - kept
    // subscribed so a picker opens instantly; null until the first value arrives.
    val characters: List<CharacterNameDto>? = null,
)

/** The reader's "previously on" recap (backend httpapi/recap.go) - see [ReaderViewModel.recapState]. */
data class RecapUiState(
    val recap: BookRecapDto? = null,
    // One is queued, or being written or spoken.
    val preparing: Boolean = false,
    // Where the reader is, and whether [recap] was written for it (recapIsCurrent).
    val pos: ParagraphPos = 0 to 0,
    val current: Boolean = false,
    // Show the unasked-for offer (shouldOfferRecap, and the book hasn't played this visit).
    val offer: Boolean = false,
    // It is open in the text (RecapInline), and - [engaged] - has a clip for the playback bar's
    // play/pause to drive in place of the book's.
    val open: Boolean = false,
    val engaged: Boolean = false,
    // The recap's own clip is playing (ReaderViewModel.toggleRecapPlayback).
    val playing: Boolean = false,
    // Pressing play should wait for the recap rather than start the book (shouldHoldForRecap,
    // and the book hasn't played this visit); [waiting] - it is open and will play by itself
    // the moment it has a clip.
    val hold: Boolean = false,
    val waiting: Boolean = false,
    // The library-wide offer threshold (RECAP_NEVER until it has arrived).
    val afterMinutes: Int = RECAP_NEVER,
    // When the book was last read as of opening it, before this visit saved anything, and when
    // that was - what "away" is measured between.
    val lastReadAtOnOpen: Long? = null,
    val openedAtMs: Long = 0L,
)

/** What only this visit to the reader knows about its recap - see [RecapUiState]. */
private data class RecapLocal(
    val opened: Boolean = false,
    val openPos: ParagraphPos = 0 to 0,
    val lastReadAtOnOpen: Long? = null,
    val openedAtMs: Long = 0L,
    val seriesIndex: Double = 0.0,
    // The book has played this visit, or the reader turned the recap down: it is no longer
    // offered, and play starts the book.
    val played: Boolean = false,
    // This visit asked the backend for a recap.
    val requested: Boolean = false,
    val open: Boolean = false,
    // Open, and to play by itself once it has a clip.
    val autoPlay: Boolean = false,
    val playing: Boolean = false,
)

@HiltViewModel
class ReaderViewModel @Inject constructor(
    savedStateHandle: SavedStateHandle,
    private val libraryRepository: LibraryRepository,
    private val jobsRepository: JobsRepository,
    private val voiceRepository: VoiceRepository,
    private val readingSettingsRepository: ReadingSettingsRepository,
    private val playbackSettingsRepository: PlaybackSettingsRepository,
    val player: ParagraphPlayer,
    private val backgroundMusicPlayer: BackgroundMusicPlayer,
    private val lightingController: HueLightingController,
    private val lightingSettingsRepository: LightingSettingsRepository,
    private val liveStore: LiveStore,
    private val downloadRepository: DownloadRepository,
    private val workManager: WorkManager,
    private val mediaUrlResolver: MediaUrlResolver,
    @ApplicationContext private val appContext: Context,
) : ViewModel() {

    private val bookId: String = checkNotNull(savedStateHandle["bookId"])

    private val _uiState = MutableStateFlow(ReaderUiState())
    val uiState: StateFlow<ReaderUiState> = _uiState

    private var lastPositionSaveAtMs = 0L

    // Guards expandUp/expandDown against firing multiple overlapping loads for the same edge
    // chapter while scroll-position updates keep arriving before the first one lands.
    private val loadingChapters = mutableSetOf<Int>()

    // One live "chapter" topic subscription per loaded chapter (see subscribeChapter) - every
    // paragraph status/word-timing/annotation change arrives through these.
    private val chapterJobs = mutableMapOf<Int, Job>()
    private val lastGenerateRequestAtMs = mutableMapOf<Int, Long>()

    // Live "chapterMusic" subscriptions, one per loaded chapter while music or Hue lighting is
    // on (both read each region - its clip, its lighting cue) - see
    // syncMusicSubscriptions.
    private val musicJobs = mutableMapOf<Int, Job>()

    // Whether the first live book value has already set up the starting chapter/position -
    // later book-topic updates only refresh uiState.book.
    private var bookInitialized = false

    // Where startFromOfflineCopy put the reader, if it opened the downloaded copy - see
    // adoptLivePosition. Null when it didn't, or when the player kept its own position.
    private var offlineStart: ParagraphPos? = null

    // This reader's own recent position saves (plus where it resumed from), and a dismissed
    // remote position - see remotePosition/withRemotePosition.
    private val ownPositionSaves = ArrayDeque<ParagraphPos>()
    private var dismissedRemotePosition: ParagraphPos? = null

    // The book's last-known-good voice settings as fetched/saved, kept around (rather than
    // re-derived from uiState's display-only voicePresetName/voiceKey - voiceKey is an opaque
    // hash string, not reversible) so setMultiVoice can PUT a full voiceSettingsDTO without
    // clobbering the currently-selected preset/instruct/language, and so setVoicePreset can do
    // the reverse - keep whatever multiVoice was already set to.
    private var currentVoice: VoiceSettingsDto? = null

    // Guards against re-POSTing music scoring for the same chapter more than once per
    // ViewModel lifetime - see onChapterMusic. Local-only, so it does NOT by itself prevent
    // two different ReaderViewModel instances (e.g. leaving and reopening the reader, or a
    // process/config-change recreation) from each independently triggering a scoring POST for
    // the same chapter around the same time - onChapterMusic also checks the live job queue
    // (via jobsRepository) for that, which is the real fix for a chapter whose scoring run is
    // still genuinely in flight server-side when a fresh ViewModel starts watching it with
    // no memory of the earlier trigger. Once one of those checks says "already covered" (either
    // way), this set is what stops this same instance from asking again on every subsequent
    // live update while backend/CLAUDE.md store.MusicRegion's own scoring is still working
    // through a long chapter (which now resumes from where it left off rather than restarting -
    // see backend httpapi.scoreChapterMusic's own doc comment - so a genuine retrigger across
    // instances is expected/harmless progress, not the bug; a *redundant concurrent* one is).
    private val scoringMusicChapters = mutableSetOf<Int>()

    // One ad-hoc player for whichever music region preview was most recently requested - see
    // previewMusicRegion, mirroring VoicesViewModel's own single-clip MediaPlayer pattern.
    private var musicPreviewPlayer: MediaPlayer? = null

    // The recap's own clip - see playRecap.
    private var recapPlayer: MediaPlayer? = null
    private val recapLocal = MutableStateFlow(RecapLocal())

    /** The book's "previously on" recap as the reader shows it: the live recap, whether it is
     *  for where the reader is now, and whether to offer it unasked. */
    val recapState: StateFlow<RecapUiState> = combine(
        liveStore.bookRecap(bookId),
        player.state,
        liveStore.recapSettings().map { it.data?.afterMinutes ?: RECAP_NEVER },
        readingSettingsRepository.dismissedRecap(bookId),
        recapLocal,
    ) { live, playback, afterMinutes, dismissed, local ->
        val recap = live.data?.recap
        // Before the player has a position in this book it sits where the book was left.
        val pos = if (playback.chapterIdx >= 0) playback.chapterIdx to maxOf(playback.paragraphIdx, 0) else local.openPos
        RecapUiState(
            recap = recap,
            preparing = live.data?.preparing == true,
            pos = pos,
            current = recapIsCurrent(recap, pos),
            offer = local.opened && !local.played && !local.playing &&
                shouldOfferRecap(recap, pos, local.lastReadAtOnOpen, local.openedAtMs, afterMinutes, dismissed),
            open = local.open,
            engaged = local.open && recapIsCurrent(recap, pos) && recap?.audioUrl != null,
            playing = local.playing,
            hold = local.opened && !local.played && shouldHoldForRecap(
                recap, pos, local.lastReadAtOnOpen, local.seriesIndex, local.openedAtMs, afterMinutes, dismissed,
                awaited = local.requested || live.data?.preparing == true,
            ),
            waiting = local.open && local.autoPlay,
            afterMinutes = afterMinutes,
            lastReadAtOnOpen = local.lastReadAtOnOpen,
            openedAtMs = local.openedAtMs,
        )
    }.stateIn(viewModelScope, SharingStarted.WhileSubscribed(5_000), RecapUiState())

    init {
        // Mirrors usePlayback's onNeedChapter - fires when playback advances into a chapter
        // that isn't loaded yet (normally just past the loaded window's trailing edge). Grows
        // the window to include it rather than only fetching it standalone, so the newly
        // playing chapter also becomes visible in the scroll list, not just audible.
        player.onNeedChapter = { idx -> growRangeToInclude(idx) }
        player.onPositionUpdate = { chapterIdx, paragraphIdx, seconds -> maybeSavePosition(chapterIdx, paragraphIdx, seconds) }

        viewModelScope.launch {
            player.state.collect { playback -> _uiState.update { withRemotePosition(it.copy(playback = playback)) } }
        }

        // The book starting to play - from the bar, a headset, the notification - while its
        // recap is due waits for the recap instead (holdForRecap); started again while already
        // waiting, it means "skip it". Otherwise the reader has picked the book back up: no
        // more offer this visit, and the book and its recap never talk over each other.
        viewModelScope.launch {
            player.state.map { it.isPlaying }.distinctUntilChanged().collect { playing ->
                if (!playing) return@collect
                val local = recapLocal.value
                if (recapState.value.hold && !local.played && !(local.open && local.autoPlay)) {
                    player.pause()
                    holdForRecap()
                } else {
                    recapLocal.update { it.copy(played = true, autoPlay = false) }
                    stopRecap()
                }
            }
        }
        // A recap opened to be played starts as soon as it has a clip - at once, or when its
        // narration arrives. (Also keeps recapState current for the checks above.)
        viewModelScope.launch {
            recapState.collect { st ->
                if (st.waiting && st.engaged && recapPlayer == null) {
                    recapLocal.update { it.copy(autoPlay = false) }
                    toggleRecapPlayback()
                }
            }
        }

        // Keeps some runway of generated audio ahead of wherever playback currently is
        // (Settings' lookahead paragraph count, default jobs.LookaheadParagraphCount = 25,
        // spanning into later chapters as needed - see jobs.Manager.EnqueueLookahead) - mirrors
        // ReaderPage.tsx's useEffect on [playback.chapterIdx, playback.paragraphIdx].
        viewModelScope.launch {
            player.state.map { it.chapterIdx to it.paragraphIdx }
                .distinctUntilChanged()
                .collect { (chapterIdx, paragraphIdx) ->
                    if (chapterIdx >= 0) {
                        runCatching {
                            val count = playbackSettingsRepository.lookaheadParagraphs.first()
                            libraryRepository.lookahead(bookId, chapterIdx, paragraphIdx, count)
                        }
                    }
                }
        }

        // Background music: keeps one live chapterMusic subscription per loaded chapter while
        // the book-wide toggle is on (and none while it's off). Not gated on annotations mode
        // (local Compose state ReaderScreen owns) - the live mix itself (BackgroundMusicPlayer)
        // needs this data whether or not the reader ever opens annotations mode to see it.
        lightingController.setBook(bookId)
        viewModelScope.launch {
            combine(_uiState, lightingController.enabled) { ui, lighting ->
                if (ui.musicEnabled || lighting) ui.loadedChapters.keys else emptySet()
            }
                .distinctUntilChanged()
                .collect { syncMusicSubscriptions(it) }
        }

        viewModelScope.launch {
            readingSettingsRepository.fontSize.collect { sizeSp -> _uiState.update { it.copy(fontSizeSp = sizeSp) } }
        }
        viewModelScope.launch {
            readingSettingsRepository.fontFamily.collect { family -> _uiState.update { it.copy(fontFamily = family) } }
        }
        viewModelScope.launch {
            liveStore.characters(bookId).collect { result ->
                result.data?.let { roster -> _uiState.update { it.copy(characters = roster) } }
            }
        }
        viewModelScope.launch {
            readingSettingsRepository.annotationsMode.collect { on -> _uiState.update { it.copy(annotationsMode = on) } }
        }
        viewModelScope.launch {
            lightingSettingsRepository.settings.collect { s ->
                _uiState.update { it.copy(lightingEnabled = s.enabled, lightingConfigured = s.configured) }
            }
        }
        viewModelScope.launch {
            lightingController.state.collect { st -> _uiState.update { it.copy(lightingStatus = st.status) } }
        }

        viewModelScope.launch {
            downloadRepository.observeChapterStatuses(bookId).collect { statuses ->
                _uiState.update { it.copy(downloadStates = statuses) }
            }
        }

        loadBook()
    }

    /** Manual "download this book" action - downloads for every
     *  not-yet-downloaded chapter, one WorkManager request each, tagged together so [uiState]'s
     *  bookDownloadProgress can report aggregate completion. Earlier-first generation order is
     *  the backend job queue's own responsibility now (see jobs.Manager's taskHeap.Less, which
     *  sorts same-tier work by book/chapter/paragraph position rather than raw enqueue order) -
     *  this just fires every chapter's request without needing to stagger or otherwise bias when
     *  each one reaches the backend. */
    fun downloadBook() {
        val book = _uiState.value.book ?: return
        val voiceKey = _uiState.value.voiceKey ?: return
        val tag = ChapterDownloadWorker.bookDownloadTag(bookId)
        val alreadyComplete = _uiState.value.downloadStates.filterValues { it.status == DownloadStatus.COMPLETE }.keys
        // Front and back matter isn't narrated unless someone opens it, and a download asks for
        // the chapter's audio - so it is left to be downloaded on its own, if wanted.
        val matter = book.chapters.filter { it.matter }.map { it.idx }.toSet()
        for (idx in 0 until book.chapterCount) {
            if (idx in alreadyComplete || idx in matter) continue
            // KEEP: re-tapping "download book" must not re-queue (or worse, discard the progress
            // of) whatever's already downloading/queued - only chapters with no work item at all
            // get a fresh one.
            enqueueManualDownload(idx, voiceKey, tag, ExistingWorkPolicy.KEEP)
        }
        observeBookDownloadProgress(tag)
    }

    /** Removes this book's local downloaded audio/images from the device - unlike
     *  LibraryViewModel.deleteBook, this never touches the book on the server; it stays in the
     *  library exactly as before, just no longer available offline until downloaded again.
     *  No explicit state update needed beyond the call itself: uiState.downloadStates is
     *  observed straight from Room (see init's downloadRepository.observeChapterStatuses
     *  collector), so it reflects the removal reactively, and ParagraphPlayer's own
     *  DownloadRepository.localAudioFile check naturally falls back to streaming the moment
     *  these rows are gone. */
    fun deleteLocalCopy() {
        viewModelScope.launch {
            runCatching { downloadRepository.deleteBookDownloads(bookId) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Manual single-chapter download - same semantics as
     *  [downloadBook], just for one chapter (e.g. from a per-chapter action in the "jump to
     *  chapter" sheet) rather than the whole book. Reuses the same underlying
     *  [ChapterDownloadWorker]/[DownloadRepository.downloadChapter] path - a whole-book download
     *  is nothing more than this looped across every chapter index. */
    fun downloadChapter(chapterIdx: Int) {
        val voiceKey = _uiState.value.voiceKey ?: return
        val tag = ChapterDownloadWorker.bookDownloadTag(bookId)
        // REPLACE, unlike downloadBook's loop: this is an explicit, single-chapter user action
        // (including retrying one that previously errored, e.g. ran out of already-generated
        // audio to bundle) - ExistingWorkPolicy.KEEP would silently no-op here, since WorkManager
        // treats an already-FAILED work item under this chapter's unique name as "existing" and
        // never re-runs it.
        enqueueManualDownload(chapterIdx, voiceKey, tag, ExistingWorkPolicy.REPLACE)
        observeBookDownloadProgress(tag)
    }

    private fun enqueueManualDownload(chapterIdx: Int, voiceKey: String, tag: String, policy: ExistingWorkPolicy) {
        val request = OneTimeWorkRequestBuilder<ChapterDownloadWorker>()
            .setInputData(ChapterDownloadWorker.inputData(bookId, chapterIdx, voiceKey, pinned = true))
            .addTag(tag)
            .build()
        workManager.enqueueUniqueWork(ChapterDownloadWorker.bookDownloadWorkName(bookId, chapterIdx), policy, request)
    }

    /** Long-press action on a chapter's download icon: cancels it if still in flight, or just
     *  removes it if already complete - either way finishing with the local files and Room row
     *  gone. */
    fun cancelOrDeleteDownload(chapterIdx: Int) {
        workManager.cancelUniqueWork(ChapterDownloadWorker.bookDownloadWorkName(bookId, chapterIdx))
        viewModelScope.launch { downloadRepository.deleteDownload(bookId, chapterIdx) }
    }

    /** Shares one progress readout across both [downloadBook] and [downloadChapter] - they use
     *  the same tag, so re-observing here just widens what getWorkInfosByTagFlow returns rather
     *  than needing separate single-chapter vs whole-book progress state. */
    private fun observeBookDownloadProgress(tag: String) {
        viewModelScope.launch {
            workManager.getWorkInfosByTagFlow(tag).collect { infos ->
                if (infos.isEmpty()) return@collect
                val done = infos.count { it.state.isFinished }
                _uiState.update {
                    it.copy(bookDownloadProgress = if (done < infos.size) done to infos.size else null)
                }
            }
        }
    }

    /** Subscribes to this book's live book/voice/bookmarks topics (see [LiveStore]). Until the
     *  first live book value arrives the reader opens on the downloaded copy, if the book has
     *  one where it would resume; that first value then sets up the starting chapter and
     *  position (or just takes over, see [adoptLivePosition]). If the backend can't be reached
     *  (or the book can't be loaded), the downloaded copy stays - any part of it will do then -
     *  and the live values still take over whenever the backend comes back. */
    private fun loadBook() {
        viewModelScope.launch {
            val live = liveStore.book(bookId)
            var offline = live.value.data == null && startFromOfflineCopy(whileLoading = true)
            syncPendingPosition()
            var failed = false
            live.collect { result ->
                val book = result.data
                val error = result.error
                when {
                    book != null -> {
                        val first = !bookInitialized
                        bookInitialized = true
                        // Before the update below, so a position adopted here isn't first
                        // reported as another device's (see withRemotePosition).
                        if (first && offline) adoptLivePosition(book)
                        if (first && !offline) noteOwnPositionSave(book.posChapterIdx to book.posParagraphIdx)
                        _uiState.update { withRemotePosition(it.copy(loading = false, book = book)) }
                        if (first) openRecap(book)
                        if (first && !offline) startFromLiveBook(book)
                    }
                    error != null && !bookInitialized && !offline && !failed -> {
                        offline = startFromOfflineCopy(whileLoading = false)
                        if (!offline) {
                            failed = true
                            _uiState.update { it.copy(loading = false, error = error.message) }
                        }
                    }
                }
            }
        }
        viewModelScope.launch {
            liveStore.voice(bookId).collect { result ->
                result.data?.let(::applyVoice)
            }
        }
        viewModelScope.launch {
            liveStore.bookmarks(bookId).collect { result ->
                result.data?.let { bookmarks -> _uiState.update { it.copy(bookmarksByKey = bookmarks.toBookmarksByKey()) } }
            }
        }
    }

    private fun startFromLiveBook(book: BookDetailDto) {
        // Reopening the reader while this book keeps playing in the background (or sits paused
        // where it was left): follow the player. The saved position trails it by up to
        // POSITION_SAVE_MIN_INTERVAL_MS, and seeking there jumped playback back and paused it.
        val resumePlayer = player.hasPositionIn(bookId)
        val startChapterIdx = if (resumePlayer) player.state.value.chapterIdx else book.posChapterIdx
        _uiState.update { it.copy(range = startChapterIdx..startChapterIdx) }
        player.setBookInfo(bookId, book.title, book.author, book.coverUrl)
        viewModelScope.launch {
            // Loaded (and fed to the player) before playFrom below, so playFrom finds the
            // chapter already there instead of racing a duplicate load via onNeedChapter.
            awaitChapter(startChapterIdx)
            if (resumePlayer) return@launch
            // Seeks the player without auto-playing - the user presses play explicitly.
            player.playFrom(startChapterIdx, book.posParagraphIdx, book.posSeconds)
            player.pause()
        }
    }

    /** Opens the reader on this book's downloaded copy, returning whether there was one to
     *  open. [whileLoading] is the look-ahead before the backend has answered: only worth it if
     *  the chapter the book resumes in is downloaded, since the live book is about to open
     *  there. Without it (the backend can't be reached) any downloaded chapter will do. */
    private suspend fun startFromOfflineCopy(whileLoading: Boolean): Boolean {
        val cachedBook = downloadRepository.cachedBook(bookId) ?: return false
        // Resume from a queued-but-not-yet-synced position (see savePendingPosition/
        // syncPendingPosition) if there is one, else from the backend's own position as of the
        // last reconcile (see OfflineReconciler) - either only if it points at a chapter
        // actually downloaded, otherwise cachedBook.chapters wouldn't have anything to show for it.
        val downloadedIdx = cachedBook.chapters.mapTo(mutableSetOf()) { it.idx }
        val pending = downloadRepository.pendingPosition(bookId)?.takeIf { it.chapterIdx in downloadedIdx }
        val serverPos = cachedBook.takeIf { pending == null && it.posChapterIdx in downloadedIdx }
        // Same as startFromLiveBook: a player already on this book keeps its own position.
        val resumePlayer = player.hasPositionIn(bookId) && player.state.value.chapterIdx in downloadedIdx
        val resumeChapterIdx = if (resumePlayer) player.state.value.chapterIdx else pending?.chapterIdx ?: serverPos?.posChapterIdx
        // (A player elsewhere in this book is left for startFromLiveBook to follow.)
        if (whileLoading && (resumeChapterIdx == null || (player.hasPositionIn(bookId) && !resumePlayer))) return false
        val startChapterIdx = resumeChapterIdx ?: cachedBook.chapters.firstOrNull()?.idx ?: 0
        _uiState.update { it.copy(loading = false, book = cachedBook, range = startChapterIdx..startChapterIdx, error = null) }
        player.setBookInfo(bookId, cachedBook.title, cachedBook.author, cachedBook.coverUrl)
        awaitChapter(startChapterIdx)
        val voiceKey = downloadRepository.cachedChapterVoiceKey(bookId, startChapterIdx)
        if (voiceKey != null && _uiState.value.voiceKey == null) {
            player.setVoiceKey(voiceKey)
            _uiState.update { it.copy(voiceKey = voiceKey) }
        }
        // The book's music toggle normally arrives with the live voice (applyVoice, which takes
        // over when it does) - until then, as the downloaded chapter last saw it.
        if (currentVoice == null && downloadRepository.cachedChapterMusic(bookId, startChapterIdx)?.enabled == true) {
            _uiState.update { it.copy(musicEnabled = true) }
            backgroundMusicPlayer.setMusicEnabled(true)
        }
        if (resumePlayer) return true
        val startParagraphIdx = pending?.paragraphIdx ?: serverPos?.posParagraphIdx ?: 0
        offlineStart = startChapterIdx to startParagraphIdx
        player.playFrom(startChapterIdx, startParagraphIdx, pending?.seconds ?: serverPos?.posSeconds ?: 0.0)
        player.pause()
        return true
    }

    /** The first live book value arriving after [startFromOfflineCopy] already opened the
     *  reader: moves to the backend's position if that's somewhere else (another device read
     *  on since the downloaded copy last synced) and the reader hasn't been touched meanwhile.
     *  Once it has, the difference is left to the moved-elsewhere notice (see remotePosition). */
    private suspend fun adoptLivePosition(book: BookDetailDto) {
        player.setBookInfo(bookId, book.title, book.author, book.coverUrl)
        val start = offlineStart ?: return
        val playback = player.state.value
        val untouched = !playback.isPlaying && (playback.chapterIdx < 0 || (playback.chapterIdx to playback.paragraphIdx) == start)
        if (!untouched) return
        val livePos = book.posChapterIdx to book.posParagraphIdx
        noteOwnPositionSave(livePos)
        // A position still queued here is newer than the backend's - see syncPendingPosition.
        if (livePos == start || downloadRepository.pendingPosition(bookId) != null) return
        if (livePos.first !in _uiState.value.loadedChapters) resetLoadedChapters(livePos.first)
        viewModelScope.launch {
            awaitChapter(livePos.first)
            player.playFrom(livePos.first, livePos.second, book.posSeconds)
            player.pause()
        }
    }

    /** Applies the live voice topic - from this screen's own voice/multi-voice/music changes or
     *  anyone else's. A voice change resets this book's cached audio server-side, which the
     *  loaded chapters' own live topics then reflect paragraph by paragraph; offline downloads
     *  made under the old voice just stop matching the new voiceKey (see
     *  ParagraphPlayer.playParagraph) until the user deletes them. */
    private fun applyVoice(voice: VoiceSettingsDto) {
        val previous = currentVoice
        currentVoice = voice
        _uiState.update {
            it.copy(
                multiVoice = voice.characterVoiceMode != CharacterVoiceModes.NARRATOR,
                musicEnabled = voice.musicEnabled,
                voiceLanguage = voice.language.takeIf { lang -> lang.isNotBlank() } ?: "Auto",
            )
        }
        if (previous?.musicEnabled != voice.musicEnabled) backgroundMusicPlayer.setMusicEnabled(voice.musicEnabled)
        val key = voiceKeyOf(voice.presetId, voice.instruct, voice.language)
        if (key != _uiState.value.voiceKey) {
            player.setVoiceKey(key)
            _uiState.update { it.copy(voiceKey = key) }
        }
    }

    /** Pushes a locally-queued position (see savePendingPosition) to the server before deciding
     *  where this book actually starts, so a position saved while offline isn't overwritten by
     *  stale server state the moment connectivity comes back - a no-op (and cheap: one local DB
     *  read) if nothing's queued. Best-effort: still offline here too just leaves it queued for
     *  next time, exactly like the failure path that created it. */
    private suspend fun syncPendingPosition() {
        val pending = downloadRepository.pendingPosition(bookId) ?: return
        runCatching { libraryRepository.updatePosition(bookId, PositionDto(pending.chapterIdx, pending.paragraphIdx, pending.seconds)) }
            .onSuccess { downloadRepository.clearPendingPosition(bookId) }
    }

    private suspend fun resolvePresetName(presetId: String): String = coroutineScope {
        // Both queried unconditionally (rather than only checking custom presets when the
        // builtin lookup misses) so this costs one network round trip's worth of wall-clock
        // time instead of up to two on a high-latency connection.
        val builtinDeferred = async { runCatching { voiceRepository.presets() }.getOrNull()?.presets?.find { it.id == presetId }?.name }
        val customDeferred = async { runCatching { voiceRepository.customPresets() }.getOrNull()?.find { it.id == presetId }?.name }
        builtinDeferred.await() ?: customDeferred.await() ?: presetId
    }

    /** Starts (if not already running) this chapter's live subscription, which merges every
     *  value it receives into [ReaderUiState.loadedChapters] - callers are responsible for
     *  widening [ReaderUiState.range] to include it if needed. A fully downloaded chapter (see
     *  DownloadRepository.cachedChapter) is shown first, until the live value replaces it - or
     *  for good, if the backend can't provide one. A cached chapter reports every paragraph
     *  READY, playing from its downloaded clip. */
    private fun subscribeChapter(idx: Int) {
        if (chapterJobs[idx]?.isActive == true) return
        chapterJobs[idx] = viewModelScope.launch {
            val live = liveStore.chapter(bookId, idx)
            if (live.value.data == null && idx !in _uiState.value.loadedChapters) {
                downloadRepository.cachedChapter(bookId, idx)?.let { mergeChapter(idx, it, live = false) }
            }
            live.collect { result ->
                val chapter = result.data
                val error = result.error
                when {
                    chapter != null -> mergeChapter(idx, chapter, live = true)
                    error != null && idx !in _uiState.value.loadedChapters -> {
                        _uiState.update { it.copy(error = error.message, failedChapterIdx = idx) }
                    }
                }
            }
        }
    }

    /** [subscribeChapter], then suspends until the chapter is actually loaded (returning it) or
     *  has failed to (returning null). */
    private suspend fun awaitChapter(idx: Int): ChapterDetailDto? {
        subscribeChapter(idx)
        return _uiState.first { idx in it.loadedChapters || it.failedChapterIdx == idx }.loadedChapters[idx]
    }

    /** Drops every chapter subscription and loaded chapter - for jumps that reset the loaded
     *  window. Resubscribing a chapter afterward replays its lingering value immediately (see
     *  LiveStore), so a jump back into an already-seen chapter doesn't refetch it. */
    private fun resetLoadedChapters(toIdx: Int) {
        player.pause()
        player.clearChapters()
        loadingChapters.clear()
        chapterJobs.values.forEach { it.cancel() }
        chapterJobs.clear()
        _uiState.update {
            it.copy(
                range = toIdx..toIdx,
                loadedChapters = emptyMap(),
                failedChapterIdx = if (it.failedChapterIdx == toIdx) null else it.failedChapterIdx,
            )
        }
    }

    private fun mergeChapter(idx: Int, chapter: ChapterDetailDto, live: Boolean) {
        player.setChapter(chapter)
        // Unconditional, like player.setChapter above - BackgroundMusicPlayer's own cross-chapter
        // pre-roll (see its tick()) needs to know a chapter's real paragraph count regardless of
        // whether music is currently enabled, the same way ParagraphPlayer needs every chapter's
        // paragraphs regardless of whether narration is playing right now.
        backgroundMusicPlayer.setChapterParagraphCount(chapter.idx, chapter.paragraphs.size)
        _uiState.update {
            it.copy(
                loadedChapters = it.loadedChapters + (idx to chapter),
                failedChapterIdx = if (it.failedChapterIdx == idx) null else it.failedChapterIdx,
            )
        }
        if (live) ensureGenerating(chapter)
    }

    /** The error snackbar's "Retry" action for a chapter that failed to load (see
     *  ReaderUiState.failedChapterIdx) - restarts its subscription. (LiveClient already keeps
     *  retrying an unreachable backend on its own; this just re-asks right away.) */
    fun retryChapter(idx: Int) {
        chapterJobs.remove(idx)?.cancel()
        _uiState.update { if (it.failedChapterIdx == idx) it.copy(failedChapterIdx = null, error = null) else it }
        subscribeChapter(idx)
    }

    private fun ensureGenerating(chapter: ChapterDetailDto) {
        val hasPending = chapter.paragraphs.any { it.audioStatus == AudioStatuses.PENDING }
        if (!hasPending || chapter.generating) return
        val now = SystemClock.elapsedRealtime()
        if (now - (lastGenerateRequestAtMs[chapter.idx] ?: 0L) < GENERATE_RETRIGGER_MS) return
        lastGenerateRequestAtMs[chapter.idx] = now
        viewModelScope.launch { runCatching { libraryRepository.generateChapter(bookId, chapter.idx) } }
    }

    /** Grows the loaded window downward by one chapter - called when the reader scrolls near
     *  the bottom of what's loaded (see ReaderScreen's expand-on-scroll effect). */
    fun expandDown() {
        val state = _uiState.value
        val book = state.book ?: return
        val next = state.range.last + 1
        if (next >= book.chapterCount) return
        if (!loadingChapters.add(next)) return
        viewModelScope.launch {
            awaitChapter(next)
            _uiState.update { it.copy(range = it.range.first..maxOf(it.range.last, next)) }
            loadingChapters.remove(next)
        }
    }

    /** Grows the loaded window upward by one chapter - called when the reader scrolls near the
     *  top of what's loaded. */
    fun expandUp() {
        val state = _uiState.value
        val prev = state.range.first - 1
        if (prev < 0) return
        if (!loadingChapters.add(prev)) return
        viewModelScope.launch {
            awaitChapter(prev)
            _uiState.update { it.copy(range = minOf(it.range.first, prev)..it.range.last) }
            loadingChapters.remove(prev)
        }
    }

    /** Widens the loaded window to include [idx] without disturbing the rest of it - used when
     *  playback itself advances into a chapter that isn't loaded yet (see player.onNeedChapter
     *  above), as opposed to expandUp/expandDown's "one edge at a time" scroll-driven growth. */
    private fun growRangeToInclude(idx: Int) {
        if (idx in _uiState.value.range && _uiState.value.loadedChapters.containsKey(idx)) return
        if (!loadingChapters.add(idx)) return
        viewModelScope.launch {
            awaitChapter(idx)
            _uiState.update { it.copy(range = minOf(it.range.first, idx)..maxOf(it.range.last, idx)) }
            loadingChapters.remove(idx)
        }
    }

    private fun maybeSavePosition(chapterIdx: Int, paragraphIdx: Int, seconds: Double) {
        // Throttled on wall-clock time elapsed since the last save, not on `seconds` itself -
        // `seconds` is ExoPlayer's position *within the current paragraph's clip*, which resets
        // to ~0 on every paragraph change, so comparing it against a previous save's `seconds`
        // value (as this used to) would compare across two different clips' timelines: once a
        // paragraph's peak position climbed high enough, a shorter next paragraph could nearly
        // never re-cross that threshold, silently starving position saves for the rest of
        // playback. Mirrors frontend's usePlayback.ts, which throttles the same way (Date.now()
        // against POSITION_REPORT_INTERVAL_MS) for the same reason.
        val now = SystemClock.elapsedRealtime()
        if (now - lastPositionSaveAtMs < POSITION_SAVE_MIN_INTERVAL_MS) return
        lastPositionSaveAtMs = now
        noteOwnPositionSave(chapterIdx to paragraphIdx)
        viewModelScope.launch {
            runCatching { libraryRepository.updatePosition(bookId, PositionDto(chapterIdx, paragraphIdx, seconds)) }
                .onSuccess {
                    // Clears any position queued from an earlier failed save (see
                    // savePendingPosition) - this one just superseded it, successfully.
                    downloadRepository.clearPendingPosition(bookId)
                }
                .onFailure {
                    // No network (or the server's briefly unreachable) - queue it locally rather
                    // than just losing this progress; synced (and used as the offline resume
                    // point in the meantime) the next time the book is opened - see loadBook.
                    downloadRepository.savePendingPosition(bookId, chapterIdx, paragraphIdx, seconds)
                }
        }
    }

    /** The playback bar's play/pause. While the recap is open with a clip it is the recap's (it
     *  has no controls of its own). With the recap due - the reader is back after a while and
     *  it is ready or being written - the first press waits for it and plays it before the
     *  book; pressed again while still waiting, it skips the recap. */
    fun togglePlayPause() {
        val st = recapState.value
        when {
            st.engaged -> toggleRecapPlayback()
            st.waiting -> {
                declineRecap()
                if (!player.state.value.isPlaying) player.togglePlayPause()
            }
            st.hold && !player.state.value.isPlaying -> holdForRecap()
            else -> player.togglePlayPause()
        }
    }

    fun playParagraph(chapterIdx: Int, paragraphIdx: Int, startSeconds: Double = 0.0) {
        player.playFrom(chapterIdx, paragraphIdx, startSeconds)
    }

    /** "Jump to chapter" - resets the loaded window to just this chapter and starts playback at
     *  its first paragraph, mirroring ReaderPage.tsx's jumpToChapter exactly (setRange +
     *  playback.playAt(idx, 0)) rather than only changing what's displayed. */
    fun goToChapter(idx: Int) {
        resetLoadedChapters(idx)
        viewModelScope.launch {
            awaitChapter(idx)
            player.playFrom(idx, 0)
        }
    }

    fun availablePresets(onResult: (List<VoicePresetDto>) -> Unit) {
        viewModelScope.launch {
            runCatching { voiceRepository.presets().presets }.onSuccess(onResult)
        }
    }

    /** Reassigns a displayed paragraph block's dialogue to a different speaker - the reader's own
     *  per-line correction, mirroring the web Speakers page's per-appearance "Reassign to…". Only
     *  metadata: [LibraryRepository.setParagraphSpeaker] leaves this paragraph's already-cached
     *  audio untouched, same as the web action - hearing the new voice still needs a follow-up
     *  "Regenerate paragraph" tap. [paragraphIndices] must already be filtered to this block's own
     *  isQuote segments (see ReaderScreen.kt's ParagraphRow) - the backend 400s on a non-quote
     *  paragraph, since narration/description can't be "spoken" by a character. The long-press
     *  menu's "Speaker: …" row updates via the chapter's live topic. */
    fun setParagraphSpeaker(chapterIdx: Int, paragraphIndices: List<Int>, speaker: String, isNew: Boolean = false) {
        if (paragraphIndices.isEmpty()) return
        viewModelScope.launch {
            runCatching {
                paragraphIndices.forEach { idx -> libraryRepository.setParagraphSpeaker(bookId, chapterIdx, idx, speaker, isNew) }
            }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** The long-press "Mark/unmark as scare quote" action - a reader's own live correction to
     *  the automatic scare-quote pass, mirroring the web reader's own annotations-view context
     *  menu toggle. [paragraphIndices] is the long-pressed isQuote segment, or all of this
     *  block's when the press landed on narration (see [setParagraphSpeaker]'s own doc comment - same gating,
     *  since only quoted dialogue can be marked a scare quote). Unlike [setParagraphSpeaker], the backend call itself immediately
     *  invalidates each paragraph's own already-generated audio - a live correction, not a
     *  passive attribute edit - and the reset-to-pending status arrives on the chapter's live
     *  topic, same as [regenerateParagraph]. */
    fun setScareQuote(chapterIdx: Int, paragraphIndices: List<Int>, scareQuote: Boolean) {
        if (paragraphIndices.isEmpty()) return
        viewModelScope.launch {
            runCatching {
                paragraphIndices.forEach { idx -> libraryRepository.setParagraphScareQuote(bookId, chapterIdx, idx, scareQuote) }
            }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** The long-press "Set emotion" action - a reader's manual override of the emotion pass,
     *  mirroring the web reader's annotations-view context menu. [paragraphIndices] is this
     *  block's targeted real-dialogue segments (isQuote and not a scare quote - the long-pressed
     *  one, or all of them); [emotion] is a
     *  backend internal/emotions id, or "" for neutral. The backend regenerates each line whose
     *  effective emotion changed, and the new status arrives on the chapter's live topic. */
    fun setEmotion(chapterIdx: Int, paragraphIndices: List<Int>, emotion: String) {
        if (paragraphIndices.isEmpty()) return
        viewModelScope.launch {
            runCatching {
                paragraphIndices.forEach { idx -> libraryRepository.setParagraphEmotion(bookId, chapterIdx, idx, emotion) }
            }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun availableCustomPresets(onResult: (List<CustomVoicePresetDto>) -> Unit) {
        viewModelScope.launch {
            runCatching { voiceRepository.customPresets() }.onSuccess(onResult)
        }
    }

    fun availableLanguages(onResult: (List<String>) -> Unit) {
        viewModelScope.launch {
            runCatching { voiceRepository.languages() }.onSuccess(onResult)
        }
    }

    fun setVoicePreset(presetId: String, name: String, instruct: String, language: String) {
        // Builds the PUT off currentVoice via .copy() rather than a bare VoiceSettingsDto(...)
        // literal - the PUT is a full replace, not a patch (see VoiceSettingsDto's own doc
        // comment), so constructing a fresh instance here used to silently reset
        // characterVoiceMode/speechDirection/musicEnabled back to their defaults every time a
        // reader merely picked a different narrator preset, even after the web UI had already
        // configured one of those for this book. Falls back to a bare literal only if currentVoice
        // somehow isn't loaded yet (shouldn't normally happen - the voice picker sheet that calls
        // this only ever opens once the book's voice has loaded).
        val base = currentVoice
        val settings = base?.copy(presetId = presetId, instruct = instruct, language = language)
            ?: VoiceSettingsDto(presetId, instruct, language)
        viewModelScope.launch {
            runCatching {
                voiceRepository.updateVoice(bookId, settings)
            }.onSuccess { updated ->
                currentVoice = updated
                _uiState.update {
                    it.copy(
                        voicePresetName = name,
                        multiVoice = updated.characterVoiceMode != CharacterVoiceModes.NARRATOR,
                        voiceLanguage = language,
                    )
                }
                // Applied right away rather than waiting for the voice topic's own push (which
                // arrives too, and is idempotent) - see applyVoice for what a voice change resets.
                applyVoice(updated)
            }.onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Toggles per-character narration voices for this book (see [ReaderUiState.multiVoice]) -
     *  unlike [setVoicePreset], this never invalidates cached audio or the voiceKey (matches
     *  backend's handleUpdateVoice: nothing to reset, each resolved voice's audio is already
     *  cached under its own id) - see backend/internal/httpapi/voices.go's handleUpdateVoice
     *  doc comment. */
    fun setMultiVoice(enabled: Boolean) {
        val voice = currentVoice ?: return
        val newMode = if (enabled) CharacterVoiceModes.ASSIGNED else CharacterVoiceModes.NARRATOR
        if (voice.characterVoiceMode == newMode) return
        viewModelScope.launch {
            runCatching {
                voiceRepository.updateVoice(bookId, voice.copy(characterVoiceMode = newMode))
            }.onSuccess { updated ->
                currentVoice = updated
                _uiState.update { it.copy(multiVoice = updated.characterVoiceMode != CharacterVoiceModes.NARRATOR) }
            }.onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Starts/stops live chapterMusic subscriptions so exactly [wanted] chapters have one -
     *  driven from init by the loaded chapters while music is enabled. */
    private fun syncMusicSubscriptions(wanted: Set<Int>) {
        (musicJobs.keys - wanted).forEach { idx -> musicJobs.remove(idx)?.cancel() }
        (wanted - musicJobs.keys).forEach { idx ->
            musicJobs[idx] = viewModelScope.launch {
                // A downloaded chapter's stored ambience first, like subscribeChapter.
                val live = liveStore.chapterMusic(bookId, idx)
                if (live.value.data == null) {
                    downloadRepository.cachedChapterMusic(bookId, idx)?.let { onChapterMusic(idx, it, live = false) }
                }
                live.collect { result ->
                    result.data?.let { onChapterMusic(idx, it, live = true) }
                }
            }
        }
    }

    /** Handles one live value of a chapter's background-music regions: merges them into
     *  [ReaderUiState.chapterMusic], feeds them to [BackgroundMusicPlayer], and - the first time
     *  this ViewModel sees this chapter reported not-yet-scored - triggers scoring for it once
     *  (see [scoringMusicChapters]'s own doc comment).
     *
     *  Before actually POSTing a scoring trigger, checks the live job queue ([jobsRepository]) for
     *  an already in-flight/queued "ambience" task for this exact (book, chapter) - not just
     *  [scoringMusicChapters]'s own local memory. Real bug this fixes: without the job-queue
     *  check, a *second* ReaderViewModel for the same book (leaving and reopening the reader, a
     *  config change, process recreation - all common on Android, unlike a long-lived web tab)
     *  starts with an empty [scoringMusicChapters] and no memory of an earlier trigger, so it
     *  would see `!dto.scored` and fire a redundant POST for a chapter whose scoring run is still
     *  genuinely in progress server-side - wasted LLM work contending for the same `poolLLM` slot
     *  as the run already in flight, not a correctness bug (scoring itself is idempotent/resumable
     *  now - see backend httpapi.scoreChapterMusic's own doc comment on why a pause no longer
     *  wipes prior progress), but real waste, and the actual "sometimes" in "it erroneously
     *  retriggers music gen sometimes" - a genuine *fresh* retrigger of a chapter that paused and
     *  is no longer queued anywhere is correct, expected behavior (that's exactly how a busy box
     *  eventually finishes scoring a long chapter), not this bug. */
    private suspend fun onChapterMusic(idx: Int, dto: ChapterMusicDto, live: Boolean) {
        _uiState.update { it.copy(chapterMusic = it.chapterMusic + (idx to dto)) }
        backgroundMusicPlayer.setChapterMusic(idx, dto)
        lightingController.setChapterMusic(idx, dto)
        // A stored copy (see DownloadRepository.cachedChapterMusic) says nothing about what the
        // backend still has to do.
        if (!live) return
        // The ambience pass is done once its music is scored and its lighting sub-pass has lit
        // every region - a chapter scored before lighting existed still gets (just) lit.
        if ((dto.scored && dto.lit) || idx in scoringMusicChapters) return
        val alreadyInFlight = runCatching { jobsRepository.snapshot(bookId, "ambience") }.getOrNull()?.let { snapshot ->
            (snapshot.inFlight + snapshot.queued).any { it.chapterIdx == idx }
        } ?: false
        if (alreadyInFlight) {
            // Someone else (another instance, or this book open elsewhere) already has this
            // chapter's scoring queued/running - just remember that locally so the next several
            // updates (while it's still in flight) don't re-check the job queue every time.
            scoringMusicChapters.add(idx)
            return
        }
        scoringMusicChapters.add(idx)
        runCatching { libraryRepository.runChapterAmbience(bookId, idx) }
    }

    /** Toggles book-wide background music (see [ReaderUiState.musicEnabled]/
     *  [BackgroundMusicPlayer]) - unlike [setVoicePreset], never invalidates cached narration
     *  audio (nothing narration-related changes), so this only PUTs the settings and updates
     *  local state, same "no reload needed" shape [setMultiVoice] already has. */
    fun setMusicEnabled(enabled: Boolean) {
        val voice = currentVoice ?: return
        if (voice.musicEnabled == enabled) return
        viewModelScope.launch {
            runCatching {
                voiceRepository.updateVoice(bookId, voice.copy(musicEnabled = enabled))
            }.onSuccess { updated ->
                applyVoice(updated)
                // Downloaded chapters want their music offline too.
                if (enabled) AmbienceDownloadWorker.enqueue(workManager, bookId)
            }.onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Annotations mode's per-region "Generate"/"Regenerate" action (see backend httpapi
     *  .handleRegenerateMusicRegion) - always allowed regardless of the region's current status.
     *  The boundary marker's own status/preview button follow the chapterMusic topic.
     *  [chapterIdx] isn't needed by the request itself (the region is addressed by id). */
    fun regenerateMusicRegion(chapterIdx: Int, regionId: String) {
        viewModelScope.launch {
            runCatching { libraryRepository.regenerateMusicRegion(regionId) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Auditions one region's own already-generated clip directly - a brand-new, independent
     *  player (not [player]/[BackgroundMusicPlayer]), so this never pauses/ducks narration or the
     *  live background mix, mirroring frontend's own MusicRegionPreviewButton. Tapping the same
     *  region again while it's already previewing stops it instead of restarting it. */
    fun previewMusicRegion(regionId: String, audioUrl: String) {
        if (_uiState.value.previewingMusicRegionId == regionId) {
            stopMusicPreview()
            return
        }
        val url = mediaUrlResolver.resolve(audioUrl) ?: return
        musicPreviewPlayer?.release()
        _uiState.update { it.copy(previewingMusicRegionId = regionId) }
        musicPreviewPlayer = MediaPlayer().apply {
            setDataSource(url)
            setOnPreparedListener { it.start() }
            setOnCompletionListener { mp ->
                mp.release()
                if (musicPreviewPlayer === mp) musicPreviewPlayer = null
                _uiState.update { it.copy(previewingMusicRegionId = null) }
            }
            prepareAsync()
        }
    }

    fun stopMusicPreview() {
        musicPreviewPlayer?.release()
        musicPreviewPlayer = null
        _uiState.update { it.copy(previewingMusicRegionId = null) }
    }

    fun setPlaybackRate(rate: Float) = player.setPlaybackSpeed(rate)

    /** Resolves a chapter content item's relative `imageUrl` (`/api/images/{id}` - see backend's
     *  handleGetImage) to whatever Coil's AsyncImage should load it from - a locally downloaded
     *  [java.io.File] if this book has one cached (see DownloadRepository.localImageFile, which
     *  works the same whether the image came from a live chapter or an offline-reconstructed
     *  one), otherwise an absolute URL against whichever server is currently configured, same as
     *  [ParagraphPlayer]'s own cover-art resolution. */
    fun resolveImageUrl(path: String): Any? = downloadRepository.localImageFile(bookId, path) ?: mediaUrlResolver.resolve(path)

    fun setSleepTimer(option: SleepTimerOption) = player.setSleepTimer(option)

    /** Background music's own device-local volume (see [PlaybackSettingsRepository.musicVolume]) -
     *  read live by [BackgroundMusicPlayer] itself, exposed here only for the reader's slider. */
    val musicVolume: StateFlow<Float> = playbackSettingsRepository.musicVolume
        .stateIn(viewModelScope, SharingStarted.Eagerly, DEFAULT_MUSIC_VOLUME)

    fun setMusicVolume(volume: Float) {
        viewModelScope.launch { playbackSettingsRepository.setMusicVolume(volume) }
    }

    /** Forces one already-generated paragraph to be reset and re-rendered from scratch - unlike
     *  [libraryRepository]'s lookahead call (used elsewhere for "not generated yet" paragraphs),
     *  this hits the dedicated regenerate endpoint, which unconditionally resets the paragraph's
     *  audio status server-side even when it's already AudioReady. The reset-to-pending status
     *  (and later the new audio) arrives on the chapter's live topic. */
    fun regenerateParagraph(chapterIdx: Int, paragraphIdx: Int) {
        viewModelScope.launch {
            runCatching { libraryRepository.regenerateParagraph(bookId, chapterIdx, paragraphIdx) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** The chapter picker's long-press "Generate chapter audio" action - explicitly (re-)kicks
     *  off background generation for a chapter that isn't necessarily loaded or currently
     *  playing, unlike [ensureGenerating]'s own automatic call for whatever chapter is actually
     *  visible. Idempotent server-side (see LibraryRepository.generateChapter's own doc), so this
     *  is harmless even if generation for it is already running. A loaded chapter shows the
     *  progress via its live topic; one that isn't just generates invisibly in the background
     *  until the reader actually scrolls/jumps to it. */
    fun generateChapter(chapterIdx: Int) {
        viewModelScope.launch {
            runCatching { libraryRepository.generateChapter(bookId, chapterIdx) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** The chapter picker's long-press LLM-pass actions - one [ChapterPass] per item, each the
     *  per-chapter equivalent of one of the web Speakers page's own buttons (see
     *  android/CLAUDE.md's scaffold limitations - there's still no Android roster UI, just these
     *  explicit triggers). Every one is a separate fire-and-forget backend job; ordering between
     *  them is the backend's own business (a chapter's attribution waits on its scare-quote
     *  tagging, queuing it itself if it's never run), so nothing here
     *  sequences them. A failure (e.g. the LLM being unconfigured server-side) surfaces as the
     *  reader's error banner. */
    /** The chapter picker's "Skip when listening" / "Read as part of the story": marks a
     *  chapter as front or back matter, or not. The book's live value carries the change. */
    fun setChapterMatter(chapterIdx: Int, matter: Boolean) {
        viewModelScope.launch {
            runCatching { libraryRepository.setChapterMatter(bookId, chapterIdx, matter) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message ?: "Could not change this chapter") } }
        }
    }

    fun runChapterPass(chapterIdx: Int, pass: ChapterPass) {
        viewModelScope.launch {
            runCatching {
                when (pass) {
                    ChapterPass.SCARE_QUOTES -> libraryRepository.retagScareQuotes(bookId, chapterIdx)
                    ChapterPass.ATTRIBUTION -> libraryRepository.attributeSpeakers(bookId, chapterIdx)
                    ChapterPass.AROUND_EDITS -> libraryRepository.attributeAroundEdits(bookId, chapterIdx)
                    ChapterPass.DIRECTIONS -> libraryRepository.tagDirections(bookId, chapterIdx)
                    ChapterPass.PRONUNCIATION -> libraryRepository.resolvePronunciation(bookId, chapterIdx)
                    ChapterPass.AMBIENCE -> libraryRepository.runChapterAmbience(bookId, chapterIdx)
                }
            }.onFailure { e -> _uiState.update { it.copy(error = e.message ?: "${pass.label} failed") } }
        }
    }

    /** Notes how long the reader has been away, as the first live [book] value has it (this
     *  visit's own position saves move lastReadAt), and asks for a recap if they would be
     *  offered one and none is ready - see shouldRequestRecap. */
    private fun openRecap(book: BookDetailDto) {
        val now = System.currentTimeMillis()
        // A player already narrating this book means the reader never left it.
        val following = player.hasPositionIn(bookId) && player.state.value.isPlaying
        recapLocal.update {
            it.copy(
                opened = true,
                openPos = book.posChapterIdx to book.posParagraphIdx,
                lastReadAtOnOpen = book.lastReadAt?.takeIf { at -> at > 0 },
                openedAtMs = now,
                seriesIndex = book.seriesIndex ?: 0.0,
                played = following,
            )
        }
        if (following) return
        viewModelScope.launch {
            runCatching {
                val recap = liveStore.bookRecap(bookId).first { it.data != null }.data?.recap
                val afterMinutes = liveStore.recapSettings().first { it.data != null }.data?.afterMinutes ?: RECAP_NEVER
                val pos = book.posChapterIdx to book.posParagraphIdx
                if (shouldRequestRecap(recap, pos, book.lastReadAt, book.seriesIndex ?: 0.0, now, afterMinutes)) {
                    recapLocal.update { it.copy(requested = true) }
                    libraryRepository.prepareRecap(bookId, force = false)
                }
            }
        }
    }

    /** Asks for the recap of where the reader is now - [force] to write it again. */
    fun prepareRecap(force: Boolean) {
        viewModelScope.launch {
            runCatching { libraryRepository.prepareRecap(bookId, force) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message ?: "Could not ask for the recap") } }
        }
    }

    /** Turns the current recap down: it isn't offered again (the menu still opens it). */
    fun dismissRecap() {
        val recap = recapState.value.recap ?: return
        viewModelScope.launch { readingSettingsRepository.setDismissedRecap(bookId, recapKey(recap)) }
    }

    fun setRecapAfterMinutes(minutes: Int) {
        viewModelScope.launch {
            runCatching { libraryRepository.setRecapAfterMinutes(minutes) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message ?: "Could not save the setting") } }
        }
    }

    /** Opens or closes the recap in the text (RecapInline). Opened with [autoPlay] it plays
     *  as soon as it has a clip; opened with nothing written for this spot, one is asked for -
     *  even while one is being prepared, since asking also moves a recap the backend was
     *  preparing in the background to the front of the queue. Closing stops its clip. */
    fun setRecapOpen(open: Boolean, autoPlay: Boolean = false) {
        recapLocal.update { it.copy(open = open, autoPlay = open && autoPlay) }
        if (!open) {
            stopRecap()
        } else if (!recapState.value.current) {
            prepareRecap(force = false)
        }
    }

    /** Play was pressed with the recap due: opens it to play first (when it ends the book
     *  starts - see toggleRecapPlayback), waiting for it if it is still being written. */
    private fun holdForRecap() = setRecapOpen(true, autoPlay = true)

    /** The reader would rather not have (or wait for) the recap: closes it, and play starts
     *  the book from now on. */
    fun declineRecap() {
        recapLocal.update { it.copy(played = true) }
        setRecapOpen(false)
        dismissRecap()
    }

    /** Plays, pauses or resumes the recap's own clip (a plain one-clip player, like
     *  previewMusicRegion's) with the book paused, at the book's speed. When it ends the recap
     *  closes and the book picks up where it was - "previously on", then the story. */
    fun toggleRecapPlayback() {
        recapPlayer?.let { mp ->
            val nowPlaying = runCatching {
                if (mp.isPlaying) mp.pause() else mp.start()
                mp.isPlaying
            }.getOrDefault(false)
            recapLocal.update { it.copy(playing = nowPlaying) }
            return
        }
        val url = recapState.value.recap?.audioUrl?.let(mediaUrlResolver::resolve) ?: return
        player.pause()
        dismissRecap()
        recapLocal.update { it.copy(playing = true) }
        recapPlayer = MediaPlayer().apply {
            // Kept running with the screen off; released with the player.
            setWakeMode(appContext, PowerManager.PARTIAL_WAKE_LOCK)
            setAudioAttributes(
                AudioAttributes.Builder().setUsage(AudioAttributes.USAGE_MEDIA).setContentType(AudioAttributes.CONTENT_TYPE_SPEECH).build(),
            )
            setDataSource(url)
            setOnPreparedListener { mp ->
                if (recapPlayer !== mp) return@setOnPreparedListener
                runCatching { mp.playbackParams = mp.playbackParams.setSpeed(player.state.value.playbackSpeed) }
                mp.start()
            }
            setOnCompletionListener { mp ->
                if (recapPlayer !== mp) return@setOnCompletionListener
                recapLocal.update { it.copy(played = true) }
                setRecapOpen(false)
                if (!player.state.value.isPlaying) player.togglePlayPause()
            }
            setOnErrorListener { mp, _, _ ->
                if (recapPlayer === mp) {
                    stopRecap()
                    _uiState.update { it.copy(error = "Could not play the recap") }
                }
                true
            }
            prepareAsync()
        }
    }

    /** Seconds into the recap's clip, 0 when it isn't playing - polled for the word highlight. */
    fun recapPositionSeconds(): Double = runCatching { (recapPlayer?.currentPosition ?: 0) / 1000.0 }.getOrDefault(0.0)

    fun stopRecap() {
        recapPlayer?.release()
        recapPlayer = null
        recapLocal.update { if (it.playing) it.copy(playing = false) else it }
    }

    /** One chapter's summary notes (live topic) for [ChapterNotesSheet] - live only while the
     *  sheet collects it. */
    fun chapterNotes(chapterIdx: Int): StateFlow<LiveResult<ChapterNotesResponseDto>> =
        liveStore.chapterNotes(bookId, chapterIdx)

    /** Saves the reader's version of a chapter's notes; [onResult] gets null on success or the
     *  server's reason for refusing them, for the editor to show beside its fields. */
    fun saveChapterNotes(chapterIdx: Int, notes: ChapterNotesDto, onResult: (String?) -> Unit) {
        viewModelScope.launch {
            val result = runCatching { libraryRepository.saveChapterNotes(bookId, chapterIdx, notes) }
            onResult(result.exceptionOrNull()?.let { it.message ?: "Could not save the notes" })
        }
    }

    /** Replaces edited notes with a fresh summary pass (the sheet confirms first). */
    fun regenerateChapterNotes(chapterIdx: Int) {
        viewModelScope.launch {
            runCatching { libraryRepository.regenerateChapterNotes(bookId, chapterIdx) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message ?: "Could not regenerate the notes") } }
        }
    }

    /** Wipes this book's every generated audio file (any voice) - the reader-facing "Delete
     *  generated audio" action, mirroring frontend's VoicePanel. Every loaded chapter's paragraphs
     *  flip back to pending via their live topics. */
    fun deleteBookAudio() {
        viewModelScope.launch {
            runCatching { libraryRepository.deleteBookAudio(bookId) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun toggleBookmark(chapterIdx: Int, paragraphIdx: Int) {
        val key = chapterIdx to paragraphIdx
        val existingId = _uiState.value.bookmarksByKey[key]
        viewModelScope.launch {
            if (existingId != null) {
                runCatching { libraryRepository.deleteBookmark(existingId) }
                    .onSuccess { _uiState.update { it.copy(bookmarksByKey = it.bookmarksByKey - key) } }
            } else {
                runCatching { libraryRepository.createBookmark(bookId, chapterIdx, paragraphIdx) }
                    .onSuccess { id -> _uiState.update { it.copy(bookmarksByKey = it.bookmarksByKey + (key to id)) } }
            }
        }
    }

    fun availableBookmarks(onResult: (List<BookmarkDto>) -> Unit) {
        viewModelScope.launch {
            runCatching { libraryRepository.listBookmarks(bookId) }.onSuccess(onResult)
        }
    }

    fun updateBookmarkNote(id: String, note: String, onSuccess: () -> Unit) {
        viewModelScope.launch {
            runCatching { libraryRepository.updateBookmarkNote(id, note) }.onSuccess { onSuccess() }
        }
    }

    fun deleteBookmark(id: String) {
        viewModelScope.launch {
            runCatching { libraryRepository.deleteBookmark(id) }
                .onSuccess { _uiState.update { it.copy(bookmarksByKey = it.bookmarksByKey.filterValues { v -> v != id }) } }
        }
    }

    /** "Jump to bookmark" - the Android analogue of ReaderPage.tsx's jumpToParagraph: resets the
     *  loaded window to just that chapter and starts playback at the exact bookmarked
     *  paragraph, not just the chapter's first one (unlike goToChapter). */
    fun jumpToParagraph(chapterIdx: Int, paragraphIdx: Int) {
        resetLoadedChapters(chapterIdx)
        viewModelScope.launch {
            awaitChapter(chapterIdx)
            player.playFrom(chapterIdx, paragraphIdx)
        }
    }

    private fun noteOwnPositionSave(pos: ParagraphPos) {
        if (pos in ownPositionSaves) return
        ownPositionSaves.addLast(pos)
        while (ownPositionSaves.size > OWN_POSITION_SAVES_KEPT) ownPositionSaves.removeFirst()
    }

    /** Recomputes [ReaderUiState.remotePosition] - only while paused: whoever is actively
     *  listening here owns the position. */
    private fun withRemotePosition(state: ReaderUiState): ReaderUiState {
        val book = state.book
        val playback = state.playback
        val remote = if (book == null || playback.isPlaying || playback.chapterIdx < 0) {
            null
        } else {
            remotePosition(
                book.posChapterIdx to book.posParagraphIdx,
                playback.chapterIdx to playback.paragraphIdx,
                ownPositionSaves,
                dismissedRemotePosition,
            )
        }
        return if (remote == state.remotePosition) state else state.copy(remotePosition = remote)
    }

    /** "Jump there" on the moved-elsewhere notice. */
    fun jumpToRemotePosition() {
        val (chapterIdx, paragraphIdx) = _uiState.value.remotePosition ?: return
        noteOwnPositionSave(chapterIdx to paragraphIdx)
        _uiState.update { withRemotePosition(it) }
        jumpToParagraph(chapterIdx, paragraphIdx)
    }

    /** "Stay here" - keeps this reader's position until the other device moves again. */
    fun dismissRemotePosition() {
        dismissedRemotePosition = _uiState.value.remotePosition ?: return
        _uiState.update { withRemotePosition(it) }
    }

    fun setAnnotationsMode(on: Boolean) {
        _uiState.update { it.copy(annotationsMode = on) }
        viewModelScope.launch { readingSettingsRepository.setAnnotationsMode(on) }
    }

    /** The playback bar's lighting button: flips Hue lighting on/off (HueLightingController
     *  follows the setting). Returns false - nothing toggled - when no bridge/room is set up
     *  yet, so the caller can open the lighting screen instead. */
    fun toggleLighting(): Boolean {
        val state = _uiState.value
        if (!state.lightingConfigured) return false
        viewModelScope.launch {
            lightingSettingsRepository.setEnabled(!state.lightingEnabled)
            // Downloaded chapters want their lighting cues offline too.
            if (!state.lightingEnabled) AmbienceDownloadWorker.enqueue(workManager, bookId)
        }
        return true
    }

    fun dismissError() {
        _uiState.update { it.copy(error = null) }
    }

    fun searchBook(query: String, byMeaning: Boolean, onResult: (List<SearchResultDto>) -> Unit) {
        viewModelScope.launch {
            runCatching { libraryRepository.searchBook(bookId, query, byMeaning) }
                // A failed search still has to end the spinner.
                .onFailure { onResult(emptyList()) }
                .onSuccess(onResult)
        }
    }

    override fun onCleared() {
        super.onCleared()
        musicPreviewPlayer?.release()
        musicPreviewPlayer = null
        recapPlayer?.release()
        recapPlayer = null
        // player/backgroundMusicPlayer are deliberately NOT released here - both are app-scoped
        // singletons (see ParagraphPlayer's class doc) so background/lock-screen playback (and
        // the live background-music mix) survives leaving this screen, unlike everything else
        // this ViewModel owns.
    }
}

private fun List<BookmarkDto>.toBookmarksByKey(): Map<Pair<Int, Int>, String> =
    associate { (it.chapterIdx to it.paragraphIdx) to it.id }

/** One of the chapter picker's long-press LLM passes - see [ReaderViewModel.runChapterPass].
 *  Declared in the order a chapter's passes run on the backend (scare quotes first, since
 *  attribution waits on it), which is also the menu's order. */
enum class ChapterPass(val label: String) {
    SCARE_QUOTES("Tag scare quotes"),
    ATTRIBUTION("Attribute speakers"),
    AROUND_EDITS("Re-attribute around my fixes"),
    DIRECTIONS("Label emotions"),
    PRONUNCIATION("Resolve pronunciation"),
    AMBIENCE("Run ambience (music + lighting)"),
}
