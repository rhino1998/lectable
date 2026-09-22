package com.lectable.app.ui.reader

import android.media.MediaPlayer
import android.os.SystemClock
import androidx.lifecycle.SavedStateHandle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import androidx.work.Constraints
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkManager
import com.lectable.app.data.remote.BookUpdatesSocket
import com.lectable.app.data.remote.MediaUrlResolver
import com.lectable.app.data.remote.dto.AudioStatus
import com.lectable.app.data.remote.dto.BookDetailDto
import com.lectable.app.data.remote.dto.BookmarkDto
import com.lectable.app.data.remote.dto.CHARACTER_VOICE_MODE_ASSIGNED
import com.lectable.app.data.remote.dto.CHARACTER_VOICE_MODE_NARRATOR
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ChapterMusicDto
import com.lectable.app.data.remote.dto.CustomVoicePresetDto
import com.lectable.app.data.remote.dto.ParagraphUpdateDto
import com.lectable.app.data.remote.dto.PositionDto
import com.lectable.app.data.remote.dto.SearchResultDto
import com.lectable.app.data.remote.dto.SpeakerDto
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
import com.lectable.app.data.settings.ReadingSettingsRepository
import com.lectable.app.playback.BackgroundMusicPlayer
import com.lectable.app.playback.ChapterDownloadWorker
import com.lectable.app.playback.ParagraphPlayer
import com.lectable.app.playback.PlaybackState
import com.lectable.app.playback.SleepTimerOption
import dagger.hilt.android.lifecycle.HiltViewModel
import javax.inject.Inject
import kotlinx.coroutines.async
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

private const val POSITION_SAVE_MIN_INTERVAL_MS = 3_000L

// Matches frontend's chapterQueryOptions poll interval (see frontend/CLAUDE.md) - the safety
// net's own tick rate, see the poll loop in init below for why one exists at all.
private const val CHAPTER_POLL_INTERVAL_MS = 1_500L

// Background-music regions are few per chapter and generate slowly - no websocket push for this
// yet (see backend httpapi.handleGetChapterMusic's own doc comment), so a slower poll than
// [CHAPTER_POLL_INTERVAL_MS] is fine, mirroring frontend's own chapterMusicQueryOptions.
private const val CHAPTER_MUSIC_POLL_INTERVAL_MS = 3_000L

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
    // and is also what feeds [BackgroundMusicPlayer] itself (see refreshChapterMusic). Populated
    // (and kept polling) only while [musicEnabled] is on - see the poll loop in init.
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
    // settings, downloadStates is per-chapter download status/progress/pinned (any voice;
    // ParagraphPlayer itself only uses a local file that matches voiceKey), and
    // bookDownloadProgress is (chaptersComplete, chaptersTotal) for a manual "download book"
    // action in flight, null when none is running.
    val voiceKey: String? = null,
    val downloadStates: Map<Int, ChapterDownloadState> = emptyMap(),
    val bookDownloadProgress: Pair<Int, Int>? = null,
)

@HiltViewModel
class ReaderViewModel @Inject constructor(
    savedStateHandle: SavedStateHandle,
    private val libraryRepository: LibraryRepository,
    private val jobsRepository: JobsRepository,
    private val voiceRepository: VoiceRepository,
    private val readingSettingsRepository: ReadingSettingsRepository,
    val player: ParagraphPlayer,
    private val backgroundMusicPlayer: BackgroundMusicPlayer,
    private val bookUpdatesSocket: BookUpdatesSocket,
    private val downloadRepository: DownloadRepository,
    private val workManager: WorkManager,
    private val mediaUrlResolver: MediaUrlResolver,
) : ViewModel() {

    private val bookId: String = checkNotNull(savedStateHandle["bookId"])

    private val _uiState = MutableStateFlow(ReaderUiState())
    val uiState: StateFlow<ReaderUiState> = _uiState

    private var lastPositionSaveAtMs = 0L

    // Tracks the previously-current chapter across player.state updates, purely so the
    // auto-prefetch collector below knows which chapter playback just moved past (see init).
    private var previousChapterIdx: Int? = null

    // Guards expandUp/expandDown against firing multiple overlapping fetches for the same edge
    // chapter while scroll-position updates keep arriving before the first fetch finishes.
    private val loadingChapters = mutableSetOf<Int>()

    // The book's last-known-good voice settings as fetched/saved, kept around (rather than
    // re-derived from uiState's display-only voicePresetName/voiceKey - voiceKey is an opaque
    // hash string, not reversible) so setMultiVoice can PUT a full voiceSettingsDTO without
    // clobbering the currently-selected preset/instruct/language, and so setVoicePreset can do
    // the reverse - keep whatever multiVoice was already set to.
    private var currentVoice: VoiceSettingsDto? = null

    // Guards against re-POSTing music scoring for the same chapter more than once per
    // ViewModel lifetime - see refreshChapterMusic. Local-only, so it does NOT by itself prevent
    // two different ReaderViewModel instances (e.g. leaving and reopening the reader, or a
    // process/config-change recreation) from each independently triggering a scoring POST for
    // the same chapter around the same time - refreshChapterMusic also checks the live job queue
    // (via jobsRepository) for that, which is the real fix for a chapter whose scoring run is
    // still genuinely in flight server-side when a fresh ViewModel starts polling it fresh with
    // no memory of the earlier trigger. Once one of those checks says "already covered" (either
    // way), this set is what stops this same instance from asking again on every subsequent
    // ~3s poll tick while backend/CLAUDE.md store.MusicRegion's own scoring is still working
    // through a long chapter (which now resumes from where it left off rather than restarting -
    // see backend httpapi.scoreChapterMusic's own doc comment - so a genuine retrigger across
    // instances is expected/harmless progress, not the bug; a *redundant concurrent* one is).
    private val scoringMusicChapters = mutableSetOf<Int>()

    // One ad-hoc player for whichever music region preview was most recently requested - see
    // previewMusicRegion, mirroring VoicesViewModel's own single-clip MediaPlayer pattern.
    private var musicPreviewPlayer: MediaPlayer? = null

    init {
        // Mirrors usePlayback's onNeedChapter - fires when playback advances into a chapter
        // that isn't loaded yet (normally just past the loaded window's trailing edge). Grows
        // the window to include it rather than only fetching it standalone, so the newly
        // playing chapter also becomes visible in the scroll list, not just audible.
        player.onNeedChapter = { idx -> growRangeToInclude(idx) }
        player.onPositionUpdate = { chapterIdx, paragraphIdx, seconds -> maybeSavePosition(chapterIdx, paragraphIdx, seconds) }

        viewModelScope.launch {
            player.state.collect { playback -> _uiState.update { it.copy(playback = playback) } }
        }

        // Keeps some runway of generated audio ahead of wherever playback currently is
        // (backend caps this at jobs.LookaheadParagraphCount = 25 paragraphs, spanning into
        // later chapters as needed - see jobs.Manager.EnqueueLookahead) - mirrors
        // ReaderPage.tsx's useEffect on [playback.chapterIdx, playback.paragraphIdx].
        viewModelScope.launch {
            player.state.map { it.chapterIdx to it.paragraphIdx }
                .distinctUntilChanged()
                .collect { (chapterIdx, paragraphIdx) ->
                    if (chapterIdx >= 0) {
                        runCatching { libraryRepository.lookahead(bookId, chapterIdx, paragraphIdx) }
                    }
                }
        }

        // Real-time paragraph status pushes (see BookUpdatesSocket) - the Android analogue of
        // frontend/src/hooks/useBookUpdates.ts, replacing the polling this used to do... in the
        // common case. wshub (backend) fans a push out only to sockets connected at the exact
        // moment it's sent, with no backlog/replay - so any single missed one (a brief
        // reconnect window, or a chapter that wasn't in loadedChapters yet when the server sent
        // it - see applyParagraphUpdate's own early-return) leaves that paragraph showing stale
        // status indefinitely, since nothing else ever asks the server again. The web frontend
        // never hits this: chapterQueryOptions (see frontend/CLAUDE.md) *also* polls every
        // chapter query with anything still pending/generating, socket or no socket, so a missed
        // push there just means the next 1.5s poll tick catches it instead. The loop below is
        // that same safety net, ported here rather than assumed away - see fetchAndMergeChapter.
        bookUpdatesSocket.onUpdate = { update -> applyParagraphUpdate(update) }
        bookUpdatesSocket.connect(bookId)
        viewModelScope.launch {
            while (isActive) {
                delay(CHAPTER_POLL_INTERVAL_MS)
                _uiState.value.loadedChapters.values
                    .filter { c -> c.paragraphs.any { it.audioStatus == AudioStatus.PENDING || it.audioStatus == AudioStatus.GENERATING } }
                    .forEach { c -> launch { fetchAndMergeChapter(c.idx) } }
            }
        }

        // Background-music polling: fetches (and, the first time, triggers scoring for) every
        // loaded chapter's own regions while the book-wide toggle is on, and keeps polling any
        // chapter that isn't fully settled yet (not yet scored, or a region still pending/
        // generating) - mirrors frontend's own chapterMusicQueryOptions refetchInterval. Unlike
        // [CHAPTER_POLL_INTERVAL_MS]'s own narration-audio poll, this isn't gated on annotations
        // mode being on (that's local Compose state ReaderScreen owns, not this ViewModel) - the
        // live background-music mix itself (BackgroundMusicPlayer) needs this data regardless of
        // whether the reader ever opens annotations mode to see it drawn.
        viewModelScope.launch {
            while (isActive) {
                delay(CHAPTER_MUSIC_POLL_INTERVAL_MS)
                if (!_uiState.value.musicEnabled) continue
                _uiState.value.loadedChapters.keys.forEach { idx ->
                    val dto = _uiState.value.chapterMusic[idx]
                    val stillWorking = dto == null || !dto.scored || dto.regions.any {
                        it.status == AudioStatus.PENDING || it.status == AudioStatus.GENERATING
                    }
                    if (stillWorking) launch { refreshChapterMusic(idx) }
                }
            }
        }

        viewModelScope.launch {
            readingSettingsRepository.fontSize.collect { sizeSp -> _uiState.update { it.copy(fontSizeSp = sizeSp) } }
        }
        viewModelScope.launch {
            readingSettingsRepository.fontFamily.collect { family -> _uiState.update { it.copy(fontFamily = family) } }
        }

        viewModelScope.launch {
            downloadRepository.observeChapterStatuses(bookId).collect { statuses ->
                _uiState.update { it.copy(downloadStates = statuses) }
            }
        }

        // Ahead-of-playback prefetch + eviction: mirrors the backend's own lookahead concept
        // (jobs.Manager.EnqueueLookahead), but at whole-chapter/download granularity. Keeps at
        // most "current + next 1" auto-downloaded chapters materially cached - see
        // DownloadRepository.evictIfUnpinned's doc for why a global size/LRU cap is deliberately
        // out of scope for v1.
        viewModelScope.launch {
            player.state.map { it.chapterIdx }.distinctUntilChanged().collect { chapterIdx ->
                if (chapterIdx < 0) return@collect
                val prev = previousChapterIdx
                previousChapterIdx = chapterIdx
                if (prev != null && prev != chapterIdx) {
                    launch { downloadRepository.evictIfUnpinned(bookId, prev) }
                }
                val book = _uiState.value.book ?: return@collect
                val voiceKey = _uiState.value.voiceKey ?: return@collect
                val nextIdx = chapterIdx + 1
                if (nextIdx < book.chapterCount && !downloadRepository.isDownloaded(bookId, nextIdx, voiceKey)) {
                    enqueuePrefetch(nextIdx, voiceKey)
                }
            }
        }

        loadBook()
    }

    private fun enqueuePrefetch(chapterIdx: Int, voiceKey: String) {
        val request = OneTimeWorkRequestBuilder<ChapterDownloadWorker>()
            .setInputData(ChapterDownloadWorker.inputData(bookId, chapterIdx, voiceKey, pinned = false))
            // Wi-Fi only - an automatic prefetch the user never asked for shouldn't silently
            // burn cellular data, unlike an explicit "download this book" action.
            .setConstraints(Constraints.Builder().setRequiredNetworkType(NetworkType.UNMETERED).build())
            .build()
        workManager.enqueueUniqueWork(ChapterDownloadWorker.prefetchWorkName(bookId), ExistingWorkPolicy.REPLACE, request)
    }

    /** Manual "download this book" action - pinned (never auto-evicted) downloads for every
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
        for (idx in 0 until book.chapterCount) {
            if (idx in alreadyComplete) continue
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

    /** Manual single-chapter download - same pinned/never-auto-evicted semantics as
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

    /** Long-press action on a chapter's download icon: cancels it if still in flight (whichever
     *  of the manual/whole-book or auto-prefetch unique work applies - see [ChapterDownloadState
     *  .pinned]), or just removes it if already complete - either way finishing with the local
     *  files and Room row gone, overriding pinning since this is an explicit user request. */
    fun cancelOrDeleteDownload(chapterIdx: Int) {
        val pinned = _uiState.value.downloadStates[chapterIdx]?.pinned ?: true
        if (pinned) {
            workManager.cancelUniqueWork(ChapterDownloadWorker.bookDownloadWorkName(bookId, chapterIdx))
        } else {
            workManager.cancelUniqueWork(ChapterDownloadWorker.prefetchWorkName(bookId))
        }
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

    /** Patches one paragraph's status into whichever loaded chapter it belongs to (both the UI
     *  state and the player's cached copy) without a full re-fetch - see [BookUpdatesSocket]. */
    private fun applyParagraphUpdate(update: ParagraphUpdateDto) {
        val chapter = _uiState.value.loadedChapters[update.chapterIdx] ?: return
        val updatedParagraphs = chapter.paragraphs.map { p ->
            if (p.idx == update.paragraphIdx) {
                p.copy(
                    audioStatus = update.audioStatus,
                    audioError = update.audioError,
                    durationSeconds = update.durationSeconds,
                    audioUrl = update.audioUrl,
                    // Always applied alongside audioUrl/durationSeconds, not merged/preserved
                    // the way words is below - a scare-quote merge group member's own pointer
                    // offset only ever means something at the exact moment its audioUrl/
                    // audioStatus change together, so a stale value must never survive past
                    // this same update (see ParagraphUpdateDto.audioPointerSeconds's own doc
                    // comment - this was the actual bug behind a merge-group member appearing
                    // to play/seek wrong until the chapter was fully refetched from scratch).
                    audioPointerSeconds = update.audioPointerSeconds,
                    // Alignment arrives as a separate, later push after the paragraph is
                    // already "ready" - null here means "not what this update is about", not
                    // "no words", so it must not clobber whatever alignment already landed.
                    words = update.words ?: p.words,
                )
            } else {
                p
            }
        }
        val updatedChapter = chapter.copy(paragraphs = updatedParagraphs)
        player.setChapter(updatedChapter)
        _uiState.update { it.copy(loadedChapters = it.loadedChapters + (update.chapterIdx to updatedChapter)) }
    }

    private fun loadBook() {
        viewModelScope.launch {
            syncPendingPosition()
            runCatching {
                // book/position are independent - fetched concurrently rather than one after
                // another, since each round trip pays real network latency on a LAN connection
                // (as opposed to the emulator's ~instant loopback), and stacking them serially
                // is the main reason opening a book feels slow over a real network.
                coroutineScope {
                    val bookDeferred = async { libraryRepository.getBook(bookId) }
                    val positionDeferred = async { runCatching { libraryRepository.getPosition(bookId) }.getOrNull() }
                    bookDeferred.await() to positionDeferred.await()
                }
            }.onSuccess { (book, position) ->
                val startChapterIdx = position?.chapterIdx ?: book.posChapterIdx
                _uiState.update { it.copy(loading = false, book = book, range = startChapterIdx..startChapterIdx) }
                player.setBookInfo(bookId, book.title, book.author, book.coverUrl)

                // Chapter text and the voice/preset-name label (just for the subtitle under the
                // book title) are unrelated - fetched concurrently so the reader isn't kept
                // waiting on the slower, multi-call preset name lookup before any text shows.
                launch {
                    // Fetched (and fed to the player) before playFrom below, so playFrom finds
                    // the chapter already loaded instead of racing a duplicate fetch via
                    // onNeedChapter.
                    fetchAndMergeChapter(startChapterIdx)
                    val startParagraphIdx = position?.paragraphIdx ?: book.posParagraphIdx
                    val startSeconds = position?.seconds ?: book.posSeconds
                    // Seeks the player without auto-playing - the user presses play explicitly.
                    player.playFrom(startChapterIdx, startParagraphIdx, startSeconds)
                    player.pause()
                }
                launch {
                    val voice = runCatching { voiceRepository.getVoice(bookId) }.getOrNull()
                    currentVoice = voice
                    _uiState.update {
                        it.copy(
                            multiVoice = voice?.let { v -> v.characterVoiceMode != CHARACTER_VOICE_MODE_NARRATOR } ?: false,
                            musicEnabled = voice?.musicEnabled ?: false,
                            voiceLanguage = voice?.language?.takeIf { lang -> lang.isNotBlank() } ?: "Auto",
                        )
                    }
                    backgroundMusicPlayer.setMusicEnabled(voice?.musicEnabled ?: false)
                    if (voice?.musicEnabled == true) {
                        _uiState.value.loadedChapters.keys.forEach { idx -> launch { refreshChapterMusic(idx) } }
                    }
                    if (voice != null) {
                        val key = voiceKeyOf(voice.presetId, voice.instruct, voice.language)
                        player.setVoiceKey(key)
                        _uiState.update { it.copy(voiceKey = key) }
                    }
                }
                launch {
                    val bookmarks = runCatching { libraryRepository.listBookmarks(bookId) }.getOrNull() ?: return@launch
                    _uiState.update { it.copy(bookmarksByKey = bookmarks.toBookmarksByKey()) }
                }
            }.onFailure { e ->
                // Backend unreachable - fall back to a fully offline-downloaded book if this one
                // is (see DownloadRepository.cachedBook) rather than just showing an error for a
                // book the device actually has a complete local copy of.
                val cachedBook = downloadRepository.cachedBook(bookId)
                if (cachedBook != null) {
                    // Resume from a queued-but-not-yet-synced position (see
                    // savePendingPosition/syncPendingPosition above) if there is one *and* it
                    // points at a chapter actually downloaded - otherwise cachedBook.chapters
                    // wouldn't have anything to fetch for it. Without this, opening a book while
                    // still offline always restarted from its first downloaded chapter, even if
                    // the reader had gotten further before losing signal.
                    val pending = downloadRepository.pendingPosition(bookId)
                        ?.takeIf { p -> cachedBook.chapters.any { it.idx == p.chapterIdx } }
                    val startChapterIdx = pending?.chapterIdx ?: (cachedBook.chapters.firstOrNull()?.idx ?: 0)
                    _uiState.update { it.copy(loading = false, book = cachedBook, range = startChapterIdx..startChapterIdx, error = null) }
                    player.setBookInfo(bookId, cachedBook.title, cachedBook.author, cachedBook.coverUrl)
                    launch {
                        fetchAndMergeChapter(startChapterIdx)
                        val voiceKey = downloadRepository.cachedChapterVoiceKey(bookId, startChapterIdx)
                        if (voiceKey != null) {
                            player.setVoiceKey(voiceKey)
                            _uiState.update { it.copy(voiceKey = voiceKey) }
                        }
                        player.playFrom(startChapterIdx, pending?.paragraphIdx ?: 0, pending?.seconds ?: 0.0)
                        player.pause()
                    }
                } else {
                    _uiState.update { it.copy(loading = false, error = e.message) }
                }
            }
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

    /** Fetches a chapter and merges it into [ReaderUiState.loadedChapters] - callers are
     *  responsible for widening [ReaderUiState.range] to include it if needed. Falls back to a
     *  fully offline-downloaded copy (see DownloadRepository.cachedChapter) if the live fetch
     *  fails - a cached chapter always reports every paragraph READY with no audioUrl, so
     *  ensureGenerating below naturally no-ops for it (nothing pending to trigger generation
     *  for) and playback resolves through ParagraphPlayer's local-file check instead. */
    private suspend fun fetchAndMergeChapter(idx: Int): ChapterDetailDto? {
        val liveResult = runCatching { libraryRepository.getChapter(bookId, idx) }
        val chapter = liveResult.getOrNull() ?: downloadRepository.cachedChapter(bookId, idx)
        if (chapter == null) {
            liveResult.exceptionOrNull()?.let { e -> _uiState.update { it.copy(error = e.message, failedChapterIdx = idx) } }
            return null
        }
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
        ensureGenerating(chapter)
        return chapter
    }

    /** The error snackbar's "Retry" action for a chapter that failed to load (see
     *  ReaderUiState.failedChapterIdx) - just re-runs the same fetch that failed. */
    fun retryChapter(idx: Int) {
        viewModelScope.launch { fetchAndMergeChapter(idx) }
    }

    private fun ensureGenerating(chapter: ChapterDetailDto) {
        val hasPending = chapter.paragraphs.any { it.audioStatus == AudioStatus.PENDING }
        if (hasPending && !chapter.generating) {
            viewModelScope.launch { runCatching { libraryRepository.generateChapter(bookId, chapter.idx) } }
        }
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
            fetchAndMergeChapter(next)
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
            fetchAndMergeChapter(prev)
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
            fetchAndMergeChapter(idx)
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

    fun togglePlayPause() = player.togglePlayPause()

    fun playParagraph(chapterIdx: Int, paragraphIdx: Int, startSeconds: Double = 0.0) {
        player.playFrom(chapterIdx, paragraphIdx, startSeconds)
    }

    /** "Jump to chapter" - resets the loaded window to just this chapter and starts playback at
     *  its first paragraph, mirroring ReaderPage.tsx's jumpToChapter exactly (setRange +
     *  playback.playAt(idx, 0)) rather than only changing what's displayed. */
    fun goToChapter(idx: Int) {
        player.pause()
        player.clearChapters()
        loadingChapters.clear()
        _uiState.update { it.copy(range = idx..idx, loadedChapters = emptyMap()) }
        viewModelScope.launch {
            fetchAndMergeChapter(idx)
            player.playFrom(idx, 0)
        }
    }

    fun availablePresets(onResult: (List<VoicePresetDto>) -> Unit) {
        viewModelScope.launch {
            runCatching { voiceRepository.presets().presets }.onSuccess(onResult)
        }
    }

    /** Populates the long-press "Set speaker" picker (SpeakerPickerSheet) - Narrator plus every
     *  attributed character, mirroring the web Speakers page's own reassignTargets. */
    fun availableSpeakers(onResult: (List<SpeakerDto>) -> Unit) {
        viewModelScope.launch {
            runCatching { libraryRepository.listSpeakers(bookId) }.onSuccess(onResult)
        }
    }

    /** Reassigns a displayed paragraph block's dialogue to a different speaker - the reader's own
     *  per-line correction, mirroring the web Speakers page's per-appearance "Reassign to…". Only
     *  metadata: [LibraryRepository.setParagraphSpeaker] leaves this paragraph's already-cached
     *  audio untouched, same as the web action - hearing the new voice still needs a follow-up
     *  "Regenerate paragraph" tap. [paragraphIndices] must already be filtered to this block's own
     *  isQuote segments (see ReaderScreen.kt's ParagraphRow) - the backend 400s on a non-quote
     *  paragraph, since narration/description can't be "spoken" by a character. Refetches the
     *  chapter on success so the long-press menu's "Speaker: …" row updates immediately, same
     *  reload-after-mutation pattern as [regenerateParagraph]/[deleteBookAudio]. */
    fun setParagraphSpeaker(chapterIdx: Int, paragraphIndices: List<Int>, speaker: String) {
        if (paragraphIndices.isEmpty()) return
        viewModelScope.launch {
            runCatching {
                paragraphIndices.forEach { idx -> libraryRepository.setParagraphSpeaker(bookId, chapterIdx, idx, speaker) }
            }
                .onSuccess { fetchAndMergeChapter(chapterIdx) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** The long-press "Mark/unmark as scare quote" action - a reader's own live correction to
     *  the automatic scare-quote pass, mirroring the web reader's own annotations-view context
     *  menu toggle. [paragraphIndices] is this block's own isQuote segments (see
     *  [setParagraphSpeaker]'s own doc comment - same gating, since only quoted dialogue can be
     *  marked a scare quote). Unlike [setParagraphSpeaker], the backend call itself immediately
     *  invalidates each paragraph's own already-generated audio - a live correction, not a
     *  passive attribute edit - so this refetch also picks up the reset-to-pending status right
     *  away, same as [regenerateParagraph]. */
    fun setScareQuote(chapterIdx: Int, paragraphIndices: List<Int>, scareQuote: Boolean) {
        if (paragraphIndices.isEmpty()) return
        viewModelScope.launch {
            runCatching {
                paragraphIndices.forEach { idx -> libraryRepository.setParagraphScareQuote(bookId, chapterIdx, idx, scareQuote) }
            }
                .onSuccess { fetchAndMergeChapter(chapterIdx) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** [setParagraphSpeaker]'s own counterpart for a *description* - reassigns which character
     *  [paragraphIndices] (this block's own segments whose describesCharacters already lists
     *  [from] - see ReaderScreen.kt's DescriptionPickerTarget) narrate a description of, from
     *  [from] to [to] ("" clears it, describing no one). Leaves any *other* character one of
     *  these segments might also describe untouched - see LibraryRepository
     *  .setParagraphDescription. Refetches the chapter on success, same reload-after-mutation
     *  pattern as [setParagraphSpeaker]. */
    fun reassignDescription(chapterIdx: Int, paragraphIndices: List<Int>, from: String, to: String) {
        if (paragraphIndices.isEmpty()) return
        viewModelScope.launch {
            runCatching {
                paragraphIndices.forEach { idx -> libraryRepository.setParagraphDescription(bookId, chapterIdx, idx, from, to) }
            }
                .onSuccess { fetchAndMergeChapter(chapterIdx) }
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
                        multiVoice = updated.characterVoiceMode != CHARACTER_VOICE_MODE_NARRATOR,
                        voiceLanguage = language,
                    )
                }
                // Changing voice invalidates this book's cached audio (see backend/CLAUDE.md) -
                // reload every currently-loaded chapter so paragraph statuses reflect the reset,
                // not just whichever one is currently playing. Also invalidates any offline
                // downloads made under the old voice - they simply stop matching the new
                // voiceKey (see ParagraphPlayer.playParagraph) rather than needing an explicit
                // purge; stale files are cleaned up lazily the next time eviction runs for them.
                val key = voiceKeyOf(presetId, instruct, language)
                player.setVoiceKey(key)
                _uiState.update { it.copy(voiceKey = key) }
                _uiState.value.loadedChapters.keys.forEach { idx ->
                    launch { fetchAndMergeChapter(idx) }
                }
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
        val newMode = if (enabled) CHARACTER_VOICE_MODE_ASSIGNED else CHARACTER_VOICE_MODE_NARRATOR
        if (voice.characterVoiceMode == newMode) return
        viewModelScope.launch {
            runCatching {
                voiceRepository.updateVoice(bookId, voice.copy(characterVoiceMode = newMode))
            }.onSuccess { updated ->
                currentVoice = updated
                _uiState.update { it.copy(multiVoice = updated.characterVoiceMode != CHARACTER_VOICE_MODE_NARRATOR) }
            }.onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Fetches one chapter's own background-music regions, merges them into [ReaderUiState
     *  .chapterMusic], feeds them to [BackgroundMusicPlayer], and - the first time this ViewModel
     *  sees this chapter reported not-yet-scored - triggers scoring for it once (see
     *  [scoringMusicChapters]'s own doc comment). Called both from the poll loop in init and
     *  directly after an explicit mutation (toggling music on, regenerating a region) for
     *  immediate feedback, same "reload after mutation" pattern [fetchAndMergeChapter]'s own
     *  callers use.
     *
     *  Before actually POSTing a scoring trigger, checks the live job queue ([jobsRepository]) for
     *  an already in-flight/queued "music_scoring" task for this exact (book, chapter) - not just
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
    private suspend fun refreshChapterMusic(idx: Int) {
        val dto = runCatching { libraryRepository.getChapterMusic(bookId, idx) }.getOrNull() ?: return
        _uiState.update { it.copy(chapterMusic = it.chapterMusic + (idx to dto)) }
        backgroundMusicPlayer.setChapterMusic(idx, dto)
        if (dto.scored || idx in scoringMusicChapters) return
        val alreadyInFlight = runCatching { jobsRepository.snapshot() }.getOrNull()?.let { snapshot ->
            (snapshot.inFlight + snapshot.queued).any { it.kind == "music_scoring" && it.bookId == bookId && it.chapterIdx == idx }
        } ?: false
        if (alreadyInFlight) {
            // Someone else (another instance, or this book open elsewhere) already has this
            // chapter's scoring queued/running - just remember that locally so the next several
            // poll ticks (while it's still in flight) don't re-check the job queue every time.
            scoringMusicChapters.add(idx)
            return
        }
        scoringMusicChapters.add(idx)
        runCatching { libraryRepository.scoreChapterMusic(bookId, idx) }
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
                currentVoice = updated
                _uiState.update { it.copy(musicEnabled = updated.musicEnabled) }
                backgroundMusicPlayer.setMusicEnabled(updated.musicEnabled)
                if (updated.musicEnabled) {
                    _uiState.value.loadedChapters.keys.forEach { idx -> launch { refreshChapterMusic(idx) } }
                }
            }.onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Annotations mode's per-region "Generate"/"Regenerate" action (see backend httpapi
     *  .handleRegenerateMusicRegion) - always allowed regardless of the region's current status.
     *  Refetches this chapter's music afterward so the boundary marker's own status/preview
     *  button reflect the reset immediately, same reload-after-mutation pattern as
     *  [regenerateParagraph]. */
    fun regenerateMusicRegion(chapterIdx: Int, regionId: String) {
        viewModelScope.launch {
            runCatching { libraryRepository.regenerateMusicRegion(regionId) }
                .onSuccess { refreshChapterMusic(chapterIdx) }
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

    /** Forces one already-generated paragraph to be reset and re-rendered from scratch - unlike
     *  [libraryRepository]'s lookahead call (used elsewhere for "not generated yet" paragraphs),
     *  this hits the dedicated regenerate endpoint, which unconditionally resets the paragraph's
     *  audio status server-side even when it's already AudioReady. Refetches the chapter on
     *  success so the local copy immediately reflects the reset-to-pending status (and stale
     *  cached audioUrl) instead of waiting on BookUpdatesSocket/the poll fallback to notice,
     *  mirroring [deleteBookAudio]'s own post-mutation reload. */
    fun regenerateParagraph(chapterIdx: Int, paragraphIdx: Int) {
        viewModelScope.launch {
            runCatching { libraryRepository.regenerateParagraph(bookId, chapterIdx, paragraphIdx) }
                .onSuccess { fetchAndMergeChapter(chapterIdx) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** The chapter picker's long-press "Generate chapter audio" action - explicitly (re-)kicks
     *  off background generation for a chapter that isn't necessarily loaded or currently
     *  playing, unlike [ensureGenerating]'s own automatic call for whatever chapter is actually
     *  visible. Idempotent server-side (see LibraryRepository.generateChapter's own doc), so this
     *  is harmless even if generation for it is already running. Only refetches if the chapter
     *  happens to already be loaded - one that isn't just starts generating invisibly in the
     *  background until the reader actually scrolls/jumps to it. */
    fun generateChapter(chapterIdx: Int) {
        viewModelScope.launch {
            runCatching { libraryRepository.generateChapter(bookId, chapterIdx) }
                .onSuccess { if (chapterIdx in _uiState.value.loadedChapters) fetchAndMergeChapter(chapterIdx) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** The chapter picker's long-press "Attribute & tag chapter" action - fires this one
     *  chapter's own speaker attribution, description tagging, and speech-direction/
     *  pronunciation tagging, the per-chapter equivalent of the web Speakers page's three
     *  individual buttons (see android/CLAUDE.md's scaffold limitations - there's still no
     *  Android roster UI, just this explicit trigger). Issued in the same order the backend's own
     *  whole-book preprocess pipeline runs them in, but - like that pipeline - each is
     *  best-effort/independent: attribute-speakers and tag-directions are fire-and-forget queued
     *  work anyway, so one failing (e.g. tag-directions 400ing on a non-Higgs voice, or the
     *  feature being unconfigured server-side) doesn't stop the others from being requested. */
    fun attributeChapter(chapterIdx: Int) {
        viewModelScope.launch {
            val errors = mutableListOf<String>()
            runCatching { libraryRepository.attributeSpeakers(bookId, chapterIdx) }
                .onFailure { e -> errors += e.message ?: "Speaker attribution failed" }
            runCatching { libraryRepository.retagDescriptions(bookId, chapterIdx) }
                .onFailure { e -> errors += e.message ?: "Description tagging failed" }
            runCatching { libraryRepository.tagDirections(bookId, chapterIdx) }
                .onFailure { e -> errors += e.message ?: "Direction tagging failed" }
            if (errors.isNotEmpty()) _uiState.update { it.copy(error = errors.joinToString("; ")) }
        }
    }

    /** Wipes this book's every generated audio file (any voice) - the reader-facing "Delete
     *  generated audio" action, mirroring frontend's VoicePanel. Every currently-loaded chapter
     *  is refetched afterward so paragraph statuses reflect the reset immediately, same as
     *  setVoicePreset's own reload after a voice change. */
    fun deleteBookAudio() {
        viewModelScope.launch {
            runCatching { libraryRepository.deleteBookAudio(bookId) }
                .onSuccess {
                    _uiState.value.loadedChapters.keys.forEach { idx -> launch { fetchAndMergeChapter(idx) } }
                }
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
        player.pause()
        player.clearChapters()
        loadingChapters.clear()
        _uiState.update { it.copy(range = chapterIdx..chapterIdx, loadedChapters = emptyMap()) }
        viewModelScope.launch {
            fetchAndMergeChapter(chapterIdx)
            player.playFrom(chapterIdx, paragraphIdx)
        }
    }

    fun dismissError() {
        _uiState.update { it.copy(error = null) }
    }

    fun searchBook(query: String, onResult: (List<SearchResultDto>) -> Unit) {
        viewModelScope.launch {
            runCatching { libraryRepository.searchBook(bookId, query) }.onSuccess(onResult)
        }
    }

    override fun onCleared() {
        super.onCleared()
        bookUpdatesSocket.release()
        musicPreviewPlayer?.release()
        musicPreviewPlayer = null
        // player/backgroundMusicPlayer are deliberately NOT released here - both are app-scoped
        // singletons (see ParagraphPlayer's class doc) so background/lock-screen playback (and
        // the live background-music mix) survives leaving this screen, unlike everything else
        // this ViewModel owns.
    }
}

private fun List<BookmarkDto>.toBookmarksByKey(): Map<Pair<Int, Int>, String> =
    associate { (it.chapterIdx to it.paragraphIdx) to it.id }
