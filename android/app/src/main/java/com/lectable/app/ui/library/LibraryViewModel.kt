package com.lectable.app.ui.library

import com.lectable.app.ui.jobs.narrationRate
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import android.content.ContentResolver
import android.net.Uri
import androidx.work.ExistingWorkPolicy
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkManager
import com.lectable.app.data.discovery.NsdDiscoveryRepository
import com.lectable.app.data.download.BookDownloadRemover
import com.lectable.app.data.download.DownloadedBook
import com.lectable.app.data.live.LiveStore
import com.lectable.app.data.remote.MediaUrlResolver
import com.lectable.app.data.remote.dto.BookSummaryDto
import com.lectable.app.data.remote.dto.ImportPreviewResponseDto
import com.lectable.app.data.repository.DownloadRepository
import com.lectable.app.data.repository.LibraryRepository
import com.lectable.app.data.repository.VoiceRepository
import com.lectable.app.data.repository.voiceKeyOf
import com.lectable.app.data.settings.ServerSettingsRepository
import com.lectable.app.playback.ChapterDownloadWorker
import dagger.hilt.android.lifecycle.HiltViewModel
import java.io.File
import javax.inject.Inject
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull

/** [BookSummaryDto] plus its cover pre-resolved for Coil - a network URL (see [MediaUrlResolver])
 *  when the list is the live one, or a local [File] when it's the downloaded copy (see
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
    // Something to tell the reader that isn't a failure ("Checking 3 books for new chapters"),
    // shown the way an error is.
    val notice: String? = null,
    // True when `books` came from the offline downloaded-book cache (see DownloadRepository)
    // rather than the live books topic - only downloaded books show up, and their
    // progress/generation figures are unknown offline (zeroed, see toOfflineSummary). With
    // isLoading still set, the live list just hasn't arrived yet.
    val offline: Boolean = false,
    // Book ids with at least one chapter downloaded locally (see DownloadRepository
    // .observeDownloadedBooks) - gates a book row's long-press "Remove downloaded copy" item,
    // same reasoning as ReaderScreen.kt's own download-done icon only offering it once
    // something's actually there to remove.
    val downloadedBookIds: Set<String> = emptySet(),
    // Measured narration speed in chars/s for each row's "to go" estimate - see
    // ui/jobs/NarrationEta.kt's narrationRate. 0 until anything has been measured.
    val narrationRate: Double = 0.0,
    // "Import from a link" -> "Choose chapters…" (ImportPreview.kt), shown while any of these
    // is set: the request in flight, the link as the backend last described it, and what went
    // wrong with the last request. Which chapters are ticked is LibraryViewModel
    // .previewSelection, kept out of here so a tick doesn't recompose the library.
    val previewBusy: PreviewBusy? = null,
    val preview: ImportPreviewResponseDto? = null,
    val previewError: String? = null,
)

/** What the import preview is waiting on. */
enum class PreviewBusy { LOOKING_UP, SEARCHING_ARCHIVE, ATTACHING_EPUB, IMPORTING }

@HiltViewModel
class LibraryViewModel @Inject constructor(
    private val repository: LibraryRepository,
    private val mediaUrlResolver: MediaUrlResolver,
    private val downloadRepository: DownloadRepository,
    private val bookDownloadRemover: BookDownloadRemover,
    private val serverSettingsRepository: ServerSettingsRepository,
    private val discoveryRepository: NsdDiscoveryRepository,
    private val voiceRepository: VoiceRepository,
    private val workManager: WorkManager,
    private val liveStore: LiveStore,
) : ViewModel() {

    private val _uiState = MutableStateFlow(LibraryUiState())
    val uiState: StateFlow<LibraryUiState> = _uiState

    private var booksJob: Job? = null

    // Ids of the previewed chapters ticked for import (see LibraryUiState.preview).
    private val _previewSelection = MutableStateFlow<Set<String>>(emptySet())
    val previewSelection: StateFlow<Set<String>> = _previewSelection

    private var previewJob: Job? = null

    init {
        viewModelScope.launch {
            autoConfigureServerIfNeeded()
            refresh()
        }
        viewModelScope.launch {
            // The summary, never the full queue: decoding every queued task here ran the app
            // out of memory on launch once three books were queued for generation.
            liveStore.jobsSummary().collect { result ->
                val rate = narrationRate(result.data?.narrationCharsPerSecond, result.data?.lastNarrationCharsPerSecond)
                if (rate != _uiState.value.narrationRate) _uiState.update { it.copy(narrationRate = rate) }
            }
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

    /** (Re)subscribes to the live `books` topic (see [LiveStore]) - every upload, delete,
     *  preprocessing run finishing, or generation progress tick afterward arrives on its own, so
     *  this only needs calling once (and from pull-to-refresh, as a manual retry). */
    fun refresh() {
        booksJob?.cancel()
        booksJob = viewModelScope.launch {
            _uiState.update { it.copy(isLoading = true, error = null) }
            combine(liveStore.books(), downloadRepository.observeDownloadedBooks()) { result, downloaded -> result to downloaded }
                .collect { (result, downloaded) ->
                    val books = result.data
                    val error = result.error
                    when {
                        books != null -> {
                            val items = books.map { BookListItem(it, mediaUrlResolver.resolve(it.coverUrl)) }
                            _uiState.update { it.copy(books = items, isLoading = false, offline = false, error = null) }
                        }
                        // Until the live list arrives, show whatever's downloaded rather than
                        // a blank loading spinner for however long a slow/unreachable backend
                        // takes (LibraryScreen says which of the two it is, by isLoading).
                        // LiveClient keeps retrying, so the live list replaces this once the
                        // backend's back.
                        downloaded.isNotEmpty() -> _uiState.update {
                            it.copy(books = downloaded.toBookListItems(), isLoading = error == null, offline = true, error = null)
                        }
                        // Backend unreachable with nothing downloaded to show instead.
                        error != null -> _uiState.update { it.copy(isLoading = false, error = error.message, offline = false) }
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

    /** "Add book" -> "Import from a link": the book appears on the live books topic as soon
     *  as its first chapter is in, and keeps growing from there (see BookSummaryDto.source). */
    fun importBookUrl(url: String) {
        viewModelScope.launch {
            _uiState.update { it.copy(isUploading = true, error = null) }
            runCatching { repository.importBookUrl(url.trim()) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
            _uiState.update { it.copy(isUploading = false) }
        }
    }

    /** "Import from a link" -> "Choose chapters…": looks the link over (nothing is imported)
     *  and opens the preview on its chapter list. */
    fun previewImport(url: String) {
        previewJob?.cancel()
        previewJob = null
        _previewSelection.value = emptySet()
        _uiState.update { it.copy(preview = null) }
        runPreview(PreviewBusy.LOOKING_UP) { repository.previewImport(url = url.trim(), previewId = null, searchArchive = false) }
    }

    /** The preview's "Search the Internet Archive": adds the chapters the site has delisted and
     *  the archive kept a copy of. Slow - up to a minute. */
    fun searchPreviewArchive() {
        val id = _uiState.value.preview?.id ?: return
        runPreview(PreviewBusy.SEARCHING_ARCHIVE) { repository.previewImport(url = null, previewId = id, searchArchive = true) }
    }

    /** The preview's "Attach an epub": its chapters stand in for ones the site won't serve. */
    fun attachPreviewEpub(contentResolver: ContentResolver, uri: Uri, cacheDir: File) {
        val id = _uiState.value.preview?.id ?: return
        runPreview(PreviewBusy.ATTACHING_EPUB) {
            repository.previewImport(url = null, previewId = id, searchArchive = false, contentResolver = contentResolver, epub = uri, cacheDir = cacheDir)
        }
    }

    /** One preview request at a time. A failure leaves the list and the selection as they were,
     *  with the error beside them. */
    private fun runPreview(busy: PreviewBusy, request: suspend () -> ImportPreviewResponseDto) {
        if (previewJob?.isActive == true) return
        previewJob = viewModelScope.launch {
            _uiState.update { it.copy(previewBusy = busy, previewError = null) }
            runCatching { request() }
                .onSuccess { response ->
                    // Chapter ids key the list's rows and the selection, so each appears once.
                    val next = response.copy(chapters = response.chapters.distinctBy { it.id })
                    val previous = _uiState.value.preview
                    _previewSelection.value =
                        if (previous == null) defaultSelection(next.chapters)
                        else mergeSelection(previous.chapters, _previewSelection.value, next.chapters)
                    _uiState.update { it.copy(preview = next, previewBusy = null) }
                }
                .onFailure { e ->
                    if (e is CancellationException) throw e
                    _uiState.update { it.copy(previewBusy = null, previewError = e.message ?: "Could not look up the link") }
                }
        }
    }

    fun setPreviewSelection(selected: Set<String>) {
        _previewSelection.value = selected
    }

    /** The preview's "Import N chapters": imports the ticked chapters from the preview and
     *  closes it. A refusal keeps it open, selection intact, with the server's explanation. */
    fun importPreviewSelection() {
        val preview = _uiState.value.preview ?: return
        if (previewJob?.isActive == true) return
        val chapterIds = importChapterIds(preview.chapters, _previewSelection.value)
        previewJob = viewModelScope.launch {
            _uiState.update { it.copy(previewBusy = PreviewBusy.IMPORTING, previewError = null, isUploading = true) }
            try {
                repository.importBookUrl(url = "", previewId = preview.id, chapterIds = chapterIds)
                _uiState.update { it.copy(preview = null, previewBusy = null) }
                _previewSelection.value = emptySet()
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                _uiState.update { it.copy(previewBusy = null, previewError = e.message ?: "Import failed") }
            } finally {
                _uiState.update { it.copy(isUploading = false) }
            }
        }
    }

    /** Closes the preview, dropping a request still in flight. An import already sent may still
     *  go through - the book then just appears in the library. */
    fun dismissPreview() {
        previewJob?.cancel()
        previewJob = null
        _previewSelection.value = emptySet()
        _uiState.update { it.copy(preview = null, previewBusy = null, previewError = null) }
    }

    /** Long-press menu's "Fill locked chapters from an epub", for a book whose source stopped at
     *  a chapter it won't serve. The chapters arrive via the live topic like any check's. */
    fun backfillBookSource(bookId: String, contentResolver: ContentResolver, uri: Uri, cacheDir: File) {
        viewModelScope.launch {
            runCatching { repository.backfillBookSource(bookId, contentResolver, uri, cacheDir) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Long-press menu's "Check for new chapters", for a book imported from a website. Fire-and-
     *  forget like [preprocessBook]: the row's source line follows the fetch via the live topic. */
    fun syncBookSource(bookId: String) {
        viewModelScope.launch {
            runCatching { repository.syncBookSource(bookId) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** "Add another source" (LibraryScreen's AddSourceDialog): the book also tracks [link]. What
     *  it lists that the book doesn't have turns up as entries to review, via the live topic. */
    fun addBookSource(bookId: String, link: String) {
        viewModelScope.launch {
            runCatching { repository.addBookSource(bookId, link.trim()) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** The review dialog's "Apply" (SourceReview.kt): which of the entries a check held back to
     *  import and which to skip. The chapters, and the row's line, follow via the live topic. */
    fun reviewBookSource(bookId: String, importIds: List<String>, skipIds: List<String>) {
        viewModelScope.launch {
            runCatching { repository.reviewBookSource(bookId, importIds, skipIds) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** The top bar's "Check all for new chapters": a check of every book imported from a
     *  website, now. Nothing is imported - what a check finds shows on its book as entries to
     *  review, via the live topic. */
    fun checkAllSources() {
        viewModelScope.launch {
            runCatching { repository.checkSources() }
                .onSuccess { queued -> _uiState.update { it.copy(notice = checkingSourcesMessage(queued)) } }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun dismissNotice() {
        _uiState.update { it.copy(notice = null) }
    }

    /** Long-press menu's "Rename": the title and author the library shows. The row's change
     *  arrives on the live books topic. */
    fun renameBook(id: String, title: String, author: String) {
        viewModelScope.launch {
            runCatching { repository.renameBook(id, title.trim(), author.trim()) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
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
                    runCatching { bookDownloadRemover.remove(id) }
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

    /** Long-press menu's "Make a recap" - queues the "previously on" recap for where the book's
     *  reader is (the reader's own Recap entry, from the library), so it is written and spoken
     *  before the book is opened. Shows on the Jobs screen while it is made; nothing is queued
     *  when the book already has one for that spot. */
    fun makeRecap(bookId: String) {
        viewModelScope.launch {
            runCatching { repository.prepareRecap(bookId, force = false) }
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

    /** Long-press menu's "Mark as read"/"Mark as unread" - moves the book's saved position to its
     *  end or back to its start server-side; the row's change arrives on the live books topic. */
    fun markRead(bookId: String, read: Boolean) {
        viewModelScope.launch {
            runCatching { repository.markRead(bookId, read) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    /** Long-press menu's "Download book" - every chapter, same
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
                    // Front and back matter is left out (see ReaderViewModel.downloadBook); if
                    // the chapter list can't be read, everything is downloaded as before.
                    val matter = runCatching { repository.getBook(bookId) }.getOrNull()
                        ?.chapters.orEmpty().filter { it.matter }.map { it.idx }.toSet()
                    for (idx in fromChapterIdx until book.chapterCount) {
                        if (idx in matter) continue
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
            runCatching { bookDownloadRemover.remove(bookId) }
                .onFailure { e -> _uiState.update { it.copy(error = e.message) } }
        }
    }

    fun dismissError() {
        _uiState.update { it.copy(error = null) }
    }
}

private fun List<DownloadedBook>.toBookListItems(): List<BookListItem> =
    map { book -> BookListItem(book.toOfflineSummary(), book.localCoverPath?.let(::File)) }

/** Best-effort BookSummaryDto for offline display - the position is the backend's as last seen
 *  online; progress/generation/estimate figures are simply unknown without a live backend, so
 *  they're zeroed rather than guessed. LibraryScreen's offline banner (see
 *  [LibraryUiState.offline]) is what actually tells the user why. */
private fun DownloadedBook.toOfflineSummary(): BookSummaryDto = BookSummaryDto(
    id = bookId,
    title = title,
    author = author,
    coverUrl = coverUrl,
    chapterCount = chapterCount,
    posChapterIdx = posChapterIdx,
    posParagraphIdx = posParagraphIdx,
    posSeconds = posSeconds,
    progressPercent = 0.0,
    finished = false,
    estimatedTotalSeconds = 0.0,
    estimateCalibrated = false,
    generatedPercent = 0.0,
)
