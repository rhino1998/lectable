package com.lectable.app.ui.library

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import android.content.ContentResolver
import android.net.Uri
import androidx.work.ExistingWorkPolicy
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkManager
import com.lectable.app.data.discovery.NsdDiscoveryRepository
import com.lectable.app.data.download.DownloadedBook
import com.lectable.app.data.remote.LiveClient
import com.lectable.app.data.remote.MediaUrlResolver
import com.lectable.app.data.remote.dto.BookSummaryDto
import com.lectable.app.data.repository.DownloadRepository
import com.lectable.app.data.repository.LibraryRepository
import com.lectable.app.data.repository.VoiceRepository
import com.lectable.app.data.repository.voiceKeyOf
import com.lectable.app.data.settings.ServerSettingsRepository
import com.lectable.app.playback.ChapterDownloadWorker
import dagger.hilt.android.lifecycle.HiltViewModel
import java.io.File
import javax.inject.Inject
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull

/** [BookSummaryDto] plus its cover pre-resolved for Coil - a network URL (see [MediaUrlResolver])
 *  when the list came from a live fetch, or a local [File] when it's the offline fallback (see
 *  [LibraryViewModel.refresh]); Coil's AsyncImage model accepts either directly. */
// How long LibraryViewModel.autoConfigureServerIfNeeded waits for mDNS to find a backend on
// first launch before giving up and falling back to the compiled-in default.
private const val AUTO_DISCOVERY_TIMEOUT_MS = 4_000L

data class BookListItem(val summary: BookSummaryDto, val cover: Any?)

data class LibraryUiState(
    val books: List<BookListItem> = emptyList(),
    val isLoading: Boolean = false,
    val isUploading: Boolean = false,
    val error: String? = null,
    // True when `books` came from the offline downloaded-book cache (see DownloadRepository)
    // rather than the live books topic - only downloaded books show up, and their
    // progress/generation figures are unknown offline (zeroed, see toOfflineSummary).
    val offline: Boolean = false,
    // Book ids with at least one chapter downloaded locally (see DownloadRepository
    // .observeDownloadedBooks) - gates a book row's long-press "Remove downloaded copy" item,
    // same reasoning as ReaderScreen.kt's own download-done icon only offering it once
    // something's actually there to remove.
    val downloadedBookIds: Set<String> = emptySet(),
)

@HiltViewModel
class LibraryViewModel @Inject constructor(
    private val repository: LibraryRepository,
    private val mediaUrlResolver: MediaUrlResolver,
    private val downloadRepository: DownloadRepository,
    private val serverSettingsRepository: ServerSettingsRepository,
    private val discoveryRepository: NsdDiscoveryRepository,
    private val voiceRepository: VoiceRepository,
    private val workManager: WorkManager,
    private val liveClient: LiveClient,
) : ViewModel() {

    private val _uiState = MutableStateFlow(LibraryUiState())
    val uiState: StateFlow<LibraryUiState> = _uiState

    private var booksJob: Job? = null

    init {
        viewModelScope.launch {
            autoConfigureServerIfNeeded()
            refresh()
        }
        viewModelScope.launch {
            downloadRepository.observeDownloadedBooks().collect { downloaded ->
                _uiState.update { it.copy(downloadedBookIds = downloaded.map { b -> b.bookId }.toSet()) }
            }
        }
    }

    /** On a fresh install (no server URL ever saved - see ServerSettingsRepository.isConfigured),
     *  tries a short mDNS discovery burst before this screen first subscribes to the book list, so a
     *  real device on the same LAN as a lectable backend can just open the app and go, instead of
     *  needing a manual trip to Settings to type in an IP first - the same discovery Settings
     *  already offers there (NsdDiscoveryRepository), just run once automatically here instead of
     *  only while that screen happens to be open. Picks whichever backend is found first if more
     *  than one answers; the reader can always switch in Settings afterward. A no-op (one cheap
     *  DataStore read) once the server's ever been configured, by hand or by this. Best-effort:
     *  nothing found within [AUTO_DISCOVERY_TIMEOUT_MS] just leaves the compiled-in default in
     *  place, exactly the pre-existing behavior - refresh() then fails/falls back to offline as
     *  it always did, and the reader lands in Settings manually. */
    private suspend fun autoConfigureServerIfNeeded() {
        if (serverSettingsRepository.isConfigured()) return
        discoveryRepository.startDiscovery()
        try {
            val found = withTimeoutOrNull(AUTO_DISCOVERY_TIMEOUT_MS) {
                discoveryRepository.servers.first { it.isNotEmpty() }
            }?.firstOrNull()
            found?.let { server -> serverSettingsRepository.setBaseUrl(server.url) }
        } finally {
            discoveryRepository.stopDiscovery()
        }
    }

    /** (Re)subscribes to the live `books` topic (see [LiveClient]) - every upload, delete,
     *  preprocessing run finishing, or generation progress tick afterward arrives on its own, so
     *  this only needs calling once (and from pull-to-refresh, as a manual retry). */
    fun refresh() {
        booksJob?.cancel()
        booksJob = viewModelScope.launch {
            _uiState.update { it.copy(isLoading = true, error = null) }
            // Show whatever's already downloaded immediately, rather than leaving the reader
            // staring at a blank loading spinner for however long a slow/unreachable backend
            // takes - LibraryScreen's own `isLoading && books.isEmpty()` full-screen spinner
            // check already steps aside the moment `books` is non-empty. Replaced by the live
            // list the moment it actually arrives.
            val downloaded = downloadRepository.observeDownloadedBooks().first()
            if (downloaded.isNotEmpty() && _uiState.value.books.isEmpty()) {
                _uiState.update { it.copy(books = downloaded.toBookListItems(), offline = true) }
            }
            liveClient.observe<List<BookSummaryDto>>("books").collect { result ->
                val books = result.data
                val error = result.error
                when {
                    books != null -> {
                        val items = books.map { BookListItem(it, mediaUrlResolver.resolve(it.coverUrl)) }
                        _uiState.update { it.copy(books = items, isLoading = false, offline = false, error = null) }
                    }
                    // Backend unreachable (offline, or just down) - the eager display above
                    // already covers "something's downloaded"; a real error only needs
                    // reporting when there was nothing to show at all. LiveClient keeps
                    // retrying, so the live list replaces this once the backend's back.
                    error != null -> _uiState.update {
                        if (downloaded.isEmpty()) it.copy(isLoading = false, error = error.message, offline = false) else it.copy(isLoading = false)
                    }
                }
            }
        }
    }

    fun uploadBook(contentResolver: ContentResolver, uri: Uri, cacheDir: File) {
        viewModelScope.launch {
            _uiState.update { it.copy(isUploading = true, error = null) }
            runCatching { repository.uploadBook(contentResolver, uri, cacheDir) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
            _uiState.update { it.copy(isUploading = false) }
        }
    }

    fun deleteBook(id: String) {
        viewModelScope.launch {
            runCatching { repository.deleteBook(id) }
                .onSuccess {
                    // Deleting the book server-side doesn't touch anything downloaded for it -
                    // without this, its audio/images/covers would sit orphaned on disk forever,
                    // with no server-side book left to ever clean them up via the reader's own
                    // per-book download controls. Best-effort: the book itself is already gone
                    // either way, so a failure here just leaves local files to be caught by
                    // Settings' "Delete all downloads" instead of blocking the deletion.
                    runCatching { downloadRepository.deleteBookDownloads(id) }
                }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Long-press menu's "Preprocess" - kicks off the whole-book meta-task (attribution ->
     *  characterization -> voice provisioning -> direction-tagging) server-side, mirroring the
     *  web Library page's own preprocess button. The book's `preprocessing` spinner flips on (and
     *  later off) via the live books topic. */
    fun preprocessBook(bookId: String) {
        viewModelScope.launch {
            runCatching { repository.preprocessBook(bookId) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Long-press menu's "Generate audio" - enqueues server-side TTS generation for every chapter
     *  at once, so a reader can have a whole book ready to listen to without opening it chapter
     *  by chapter first. Fire-and-forget, no local state to update: unlike [preprocessBook], the
     *  backend has no book-level "is generating" flag - progress shows up the same way
     *  any other generation does, via each paragraph's own status once the book is actually
     *  opened. Distinct from [downloadBook] below: this makes the server *render* the audio in
     *  the first place; downloadBook only fetches already-generated audio to this device. */
    fun generateBook(bookId: String) {
        viewModelScope.launch {
            runCatching { repository.generateBook(bookId) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** "Remaining" alongside [generateBook]'s "All" - same fire-and-forget shape, but generates
     *  only from the book's current stored reading position through the end, for a reader who
     *  wants the rest of a book they're partway through ready without re-generating (or waiting
     *  behind) chapters already behind them. */
    fun generateBookRemaining(bookId: String) {
        viewModelScope.launch {
            runCatching { repository.generateBookRemaining(bookId) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Long-press menu's "Download book" - every chapter, pinned (never auto-evicted), same
     *  semantics as ReaderViewModel.downloadBook's own whole-book action, just reachable directly
     *  from the library without opening the book first. KEEP (not REPLACE): re-tapping this must
     *  not re-queue, or discard the progress of, whatever's already downloading/queued for a
     *  chapter - only ones with no work item at all get a fresh one. No live per-chapter progress
     *  surfaced here (unlike the Reader screen's own downloadBook) - the reader can open the book
     *  and check its "jump to chapter" sheet for that; this is a fire-and-forget kickoff. */
    fun downloadBook(bookId: String) = downloadBookFrom(bookId, fromChapterIdx = 0)

    /** "Remaining" alongside [downloadBook]'s "All" - same pinned, fire-and-forget per-chapter
     *  enqueue, just starting at the book's current stored reading position
     *  (BookSummaryDto.posChapterIdx) instead of chapter 0, for a reader who wants the rest of a
     *  book they're partway through available offline without downloading (or re-downloading)
     *  chapters already behind them. Deliberately chapter-grained, not paragraph-grained the way
     *  the backend's own generate-remaining is: ChapterDownloadWorker's own unit of work is
     *  already a whole chapter (see its own pinned-download semantics), and a reader who wants
     *  "the rest of this book" offline almost always still wants the one chapter they're
     *  currently mid-way through in full, not split at the exact paragraph they've heard so far. */
    fun downloadBookRemaining(bookId: String) {
        val book = _uiState.value.books.find { it.summary.id == bookId }?.summary ?: return
        downloadBookFrom(bookId, fromChapterIdx = book.posChapterIdx)
    }

    private fun downloadBookFrom(bookId: String, fromChapterIdx: Int) {
        viewModelScope.launch {
            val book = _uiState.value.books.find { it.summary.id == bookId }?.summary ?: return@launch
            runCatching { voiceRepository.getVoice(bookId) }
                .onSuccess { voice ->
                    val voiceKey = voiceKeyOf(voice.presetId, voice.instruct, voice.language)
                    val tag = ChapterDownloadWorker.bookDownloadTag(bookId)
                    for (idx in fromChapterIdx until book.chapterCount) {
                        val request = OneTimeWorkRequestBuilder<ChapterDownloadWorker>()
                            .setInputData(ChapterDownloadWorker.inputData(bookId, idx, voiceKey, pinned = true))
                            .addTag(tag)
                            .build()
                        workManager.enqueueUniqueWork(ChapterDownloadWorker.bookDownloadWorkName(bookId, idx), ExistingWorkPolicy.KEEP, request)
                    }
                }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Long-press menu's "Remove downloaded copy" - removes this book's local downloaded audio/
     *  images from the device only, same offline-only-cleanup semantics as ReaderViewModel
     *  .deleteLocalCopy (the book itself stays in the library, on the server, exactly as before;
     *  just no longer available offline until downloaded again). No explicit state update needed
     *  beyond the call itself: [LibraryUiState.downloadedBookIds] is observed straight from Room
     *  (see init's downloadRepository.observeDownloadedBooks collector), so it - and therefore
     *  whether this menu item even shows - reflects the removal reactively. */
    fun deleteLocalCopy(bookId: String) {
        viewModelScope.launch {
            runCatching { downloadRepository.deleteBookDownloads(bookId) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun dismissError() {
        _uiState.update { it.copy(error = null) }
    }
}

private fun List<DownloadedBook>.toBookListItems(): List<BookListItem> =
    map { book -> BookListItem(book.toOfflineSummary(), book.localCoverPath?.let(::File)) }

/** Best-effort BookSummaryDto for offline display - progress/generation/estimate figures are
 *  simply unknown without a live backend, so they're zeroed rather than guessed; LibraryScreen's
 *  offline banner (see [LibraryUiState.offline]) is what actually tells the user why. */
private fun DownloadedBook.toOfflineSummary(): BookSummaryDto = BookSummaryDto(
    id = bookId,
    title = title,
    author = author,
    coverUrl = coverUrl,
    chapterCount = chapterCount,
    posChapterIdx = 0,
    posParagraphIdx = 0,
    posSeconds = 0.0,
    progressPercent = 0.0,
    finished = false,
    estimatedTotalSeconds = 0.0,
    estimateCalibrated = false,
    generatedPercent = 0.0,
)
