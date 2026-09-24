package com.lectable.app.ui.reader

import android.media.MediaPlayer
import android.os.SystemClock
import androidx.lifecycle.SavedStateHandle
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import androidx.work.ExistingWorkPolicy
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkManager
import com.lectable.app.data.live.LiveStore
import com.lectable.app.data.remote.MediaUrlResolver
import com.lectable.app.data.remote.dto.AudioStatus
import com.lectable.app.data.remote.dto.BookDetailDto
import com.lectable.app.data.remote.dto.BookmarkDto
import com.lectable.app.data.remote.dto.CHARACTER_VOICE_MODE_ASSIGNED
import com.lectable.app.data.remote.dto.CHARACTER_VOICE_MODE_NARRATOR
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ChapterMusicDto
import com.lectable.app.data.remote.dto.CustomVoicePresetDto
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
import com.lectable.app.data.settings.DEFAULT_MUSIC_VOLUME
import com.lectable.app.data.settings.PlaybackSettingsRepository
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
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
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
    private val liveStore: LiveStore,
    private val downloadRepository: DownloadRepository,
    private val workManager: WorkManager,
    private val mediaUrlResolver: MediaUrlResolver,
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

    // Live "chapterMusic" subscriptions, one per loaded chapter while music is on - see
    // syncMusicSubscriptions.
    private val musicJobs = mutableMapOf<Int, Job>()

    // Whether the first book value (live, or the offline fallback) has already set up the
    // starting chapter/position - later book-topic updates only refresh uiState.book.
    private var bookInitialized = false

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
        viewModelScope.launch {
            _uiState.map { if (it.musicEnabled) it.loadedChapters.keys else emptySet() }
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

    /** Subscribes to this book's live book/voice/bookmarks topics (see [LiveStore]). The first
     *  book value sets up the starting chapter and position; if the backend can't be reached (or
     *  the book can't be loaded) before that, falls back to a fully offline-downloaded copy -
     *  and if the backend comes back later, the live values simply take over. */
    private fun loadBook() {
        viewModelScope.launch {
            syncPendingPosition()
            liveStore.book(bookId).collect { result ->
                val book = result.data
                val error = result.error
                when {
                    book != null -> {
                        _uiState.update { it.copy(loading = false, book = book) }
                        if (!bookInitialized) {
                            bookInitialized = true
                            startFromLiveBook(book)
                        }
                    }
                    error != null && !bookInitialized -> {
                        bookInitialized = true
                        startFromOfflineCopy(error.message)
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
        val startChapterIdx = book.posChapterIdx
        _uiState.update { it.copy(range = startChapterIdx..startChapterIdx) }
        player.setBookInfo(bookId, book.title, book.author, book.coverUrl)
        viewModelScope.launch {
            // Loaded (and fed to the player) before playFrom below, so playFrom finds the
            // chapter already there instead of racing a duplicate load via onNeedChapter.
            awaitChapter(startChapterIdx)
            // Seeks the player without auto-playing - the user presses play explicitly.
            player.playFrom(startChapterIdx, book.posParagraphIdx, book.posSeconds)
            player.pause()
        }
    }

    private suspend fun startFromOfflineCopy(errorMessage: String) {
        val cachedBook = downloadRepository.cachedBook(bookId)
        if (cachedBook == null) {
            _uiState.update { it.copy(loading = false, error = errorMessage) }
            return
        }
        // Resume from a queued-but-not-yet-synced position (see savePendingPosition/
        // syncPendingPosition) if there is one *and* it points at a chapter actually
        // downloaded - otherwise cachedBook.chapters wouldn't have anything to show for it.
        val pending = downloadRepository.pendingPosition(bookId)
            ?.takeIf { p -> cachedBook.chapters.any { it.idx == p.chapterIdx } }
        val startChapterIdx = pending?.chapterIdx ?: (cachedBook.chapters.firstOrNull()?.idx ?: 0)
        _uiState.update { it.copy(loading = false, book = cachedBook, range = startChapterIdx..startChapterIdx, error = null) }
        player.setBookInfo(bookId, cachedBook.title, cachedBook.author, cachedBook.coverUrl)
        awaitChapter(startChapterIdx)
        val voiceKey = downloadRepository.cachedChapterVoiceKey(bookId, startChapterIdx)
        if (voiceKey != null && _uiState.value.voiceKey == null) {
            player.setVoiceKey(voiceKey)
            _uiState.update { it.copy(voiceKey = voiceKey) }
        }
        player.playFrom(startChapterIdx, pending?.paragraphIdx ?: 0, pending?.seconds ?: 0.0)
        player.pause()
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
                multiVoice = voice.characterVoiceMode != CHARACTER_VOICE_MODE_NARRATOR,
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
     *  widening [ReaderUiState.range] to include it if needed. If the backend can't provide it
     *  (unreachable, or an error) before any live value arrived, falls back to a fully
     *  offline-downloaded copy (see DownloadRepository.cachedChapter) - a cached chapter always
     *  reports every paragraph READY with no audioUrl, so playback resolves through
     *  ParagraphPlayer's local-file check instead. */
    private fun subscribeChapter(idx: Int) {
        if (chapterJobs[idx]?.isActive == true) return
        chapterJobs[idx] = viewModelScope.launch {
            liveStore.chapter(bookId, idx).collect { result ->
                val chapter = result.data
                val error = result.error
                when {
                    chapter != null -> mergeChapter(idx, chapter, live = true)
                    error != null && idx !in _uiState.value.loadedChapters -> {
                        val cached = downloadRepository.cachedChapter(bookId, idx)
                        if (cached != null) {
                            mergeChapter(idx, cached, live = false)
                        } else {
                            _uiState.update { it.copy(error = error.message, failedChapterIdx = idx) }
                        }
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
        val hasPending = chapter.paragraphs.any { it.audioStatus == AudioStatus.PENDING }
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
     *  paragraph, since narration/description can't be "spoken" by a character. The long-press
     *  menu's "Speaker: …" row updates via the chapter's live topic. */
    fun setParagraphSpeaker(chapterIdx: Int, paragraphIndices: List<Int>, speaker: String) {
        if (paragraphIndices.isEmpty()) return
        viewModelScope.launch {
            runCatching {
                paragraphIndices.forEach { idx -> libraryRepository.setParagraphSpeaker(bookId, chapterIdx, idx, speaker) }
            }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** The long-press "Mark/unmark as scare quote" action - a reader's own live correction to
     *  the automatic scare-quote pass, mirroring the web reader's own annotations-view context
     *  menu toggle. [paragraphIndices] is this block's own isQuote segments (see
     *  [setParagraphSpeaker]'s own doc comment - same gating, since only quoted dialogue can be
     *  marked a scare quote). Unlike [setParagraphSpeaker], the backend call itself immediately
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

    /** [setParagraphSpeaker]'s own counterpart for a *description* - reassigns which character
     *  [paragraphIndices] (this block's own segments whose describesCharacters already lists
     *  [from] - see ReaderScreen.kt's DescriptionPickerTarget) narrate a description of, from
     *  [from] to [to] ("" clears it, describing no one). Leaves any *other* character one of
     *  these segments might also describe untouched - see LibraryRepository
     *  .setParagraphDescription. The change arrives on the chapter's live topic, same as
     *  [setParagraphSpeaker]. */
    fun reassignDescription(chapterIdx: Int, paragraphIndices: List<Int>, from: String, to: String) {
        if (paragraphIndices.isEmpty()) return
        viewModelScope.launch {
            runCatching {
                paragraphIndices.forEach { idx -> libraryRepository.setParagraphDescription(bookId, chapterIdx, idx, from, to) }
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
                        multiVoice = updated.characterVoiceMode != CHARACTER_VOICE_MODE_NARRATOR,
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

    /** Starts/stops live chapterMusic subscriptions so exactly [wanted] chapters have one -
     *  driven from init by the loaded chapters while music is enabled. */
    private fun syncMusicSubscriptions(wanted: Set<Int>) {
        (musicJobs.keys - wanted).forEach { idx -> musicJobs.remove(idx)?.cancel() }
        (wanted - musicJobs.keys).forEach { idx ->
            musicJobs[idx] = viewModelScope.launch {
                liveStore.chapterMusic(bookId, idx).collect { result ->
                    result.data?.let { onChapterMusic(idx, it) }
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
    private suspend fun onChapterMusic(idx: Int, dto: ChapterMusicDto) {
        _uiState.update { it.copy(chapterMusic = it.chapterMusic + (idx to dto)) }
        backgroundMusicPlayer.setChapterMusic(idx, dto)
        if (dto.scored || idx in scoringMusicChapters) return
        val alreadyInFlight = runCatching { jobsRepository.snapshot() }.getOrNull()?.let { snapshot ->
            (snapshot.inFlight + snapshot.queued).any { it.kind == "music_scoring" && it.bookId == bookId && it.chapterIdx == idx }
        } ?: false
        if (alreadyInFlight) {
            // Someone else (another instance, or this book open elsewhere) already has this
            // chapter's scoring queued/running - just remember that locally so the next several
            // updates (while it's still in flight) don't re-check the job queue every time.
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
                applyVoice(updated)
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
     *  them is the backend's own business (a chapter's attribution and description tagging both
     *  wait on its scare-quote tagging, queuing it themselves if it's never run), so nothing here
     *  sequences them. A failure (e.g. tag-directions 400ing on a non-Higgs voice, or the LLM
     *  being unconfigured server-side) surfaces as the reader's error banner. */
    fun runChapterPass(chapterIdx: Int, pass: ChapterPass) {
        viewModelScope.launch {
            runCatching {
                when (pass) {
                    ChapterPass.SCARE_QUOTES -> libraryRepository.retagScareQuotes(bookId, chapterIdx)
                    ChapterPass.ATTRIBUTION -> libraryRepository.attributeSpeakers(bookId, chapterIdx)
                    ChapterPass.DESCRIPTIONS -> libraryRepository.retagDescriptions(bookId, chapterIdx)
                    ChapterPass.DIRECTIONS -> libraryRepository.tagDirections(bookId, chapterIdx)
                    ChapterPass.PRONUNCIATION -> libraryRepository.resolvePronunciation(bookId, chapterIdx)
                    ChapterPass.MUSIC -> libraryRepository.scoreChapterMusic(bookId, chapterIdx)
                }
            }.onFailure { e -> _uiState.update { it.copy(error = e.message ?: "${pass.label} failed") } }
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

/** One of the chapter picker's long-press LLM passes - see [ReaderViewModel.runChapterPass].
 *  Declared in the order a chapter's passes run on the backend (scare quotes first, since
 *  attribution and description tagging wait on it), which is also the menu's order. */
enum class ChapterPass(val label: String) {
    SCARE_QUOTES("Tag scare quotes"),
    ATTRIBUTION("Attribute speakers"),
    DESCRIPTIONS("Tag descriptions"),
    DIRECTIONS("Tag speech directions"),
    PRONUNCIATION("Resolve pronunciation"),
    MUSIC("Score background music"),
}
