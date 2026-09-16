package com.lectable.app.data.repository

import android.content.Context
import com.lectable.app.data.download.DownloadStatus
import com.lectable.app.data.download.DownloadedBook
import com.lectable.app.data.download.DownloadedBookDao
import com.lectable.app.data.download.DownloadedChapter
import com.lectable.app.data.download.DownloadedChapterDao
import com.lectable.app.data.download.OfflineParagraph
import com.lectable.app.data.download.PendingPosition
import com.lectable.app.data.download.PendingPositionDao
import com.lectable.app.data.remote.BackendIdentityRepository
import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.dto.AudioStatus
import com.lectable.app.data.remote.dto.BookDetailDto
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ChapterSummaryDto
import com.lectable.app.data.remote.dto.ContentItemDto
import com.lectable.app.data.remote.dto.LookaheadRequestDto
import com.lectable.app.data.remote.dto.ParagraphDto
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.File
import java.security.MessageDigest
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.filterNotNull
import kotlinx.coroutines.flow.flatMapLatest
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.withContext
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json

private const val POLL_INTERVAL_MS = 2_000L

/**
 * presetId+instruct+language, exactly what backend/internal/store/store.go's VoiceID hashes
 * (minus seed, minus the hash itself - see that function) - used here purely for local
 * staleness comparison against a downloaded chapter's voice, not required to match the server's
 * own hash byte for byte.
 */
fun voiceKeyOf(presetId: String, instruct: String, language: String): String =
    "$presetId $instruct $language"

data class DownloadKey(val libraryId: String, val bookId: String, val chapterIdx: Int, val voiceKey: String)

/** One chapter's download state as the UI needs it - see [DownloadRepository.observeChapterStatuses].
 *  [pinned] lets the caller tell a manual/whole-book download apart from the unpinned
 *  ahead-of-playback prefetch, which matters for cancelling the right WorkManager unique work. */
data class ChapterDownloadState(val status: String, val readyParagraphs: Int, val totalParagraphs: Int, val pinned: Boolean)

/**
 * Downloads a chapter's generated audio to the device for offline playback, one paragraph's
 * `.wav` at a time (reusing the same `GET /api/paragraphs/{id}/audio` endpoint streaming
 * playback already uses), plus enough book/chapter metadata (title, author, cover, paragraph
 * text/durations) to browse and open a downloaded book without a live network call at all - see
 * [cachedBook]/[cachedChapter], used by LibraryViewModel/ReaderViewModel as an offline fallback.
 * All of it is scoped by [BackendIdentityRepository]'s current library id, not just book/chapter
 * id: the app can be pointed at different backends over its lifetime (see android/CLAUDE.md's
 * "Server address"), and ids are only unique within one backend's own database.
 * [ParagraphPlayer] consults [localAudioFile] synchronously before falling back to streaming.
 */
@Singleton
class DownloadRepository @Inject constructor(
    @ApplicationContext private val context: Context,
    private val api: LectableApi,
    private val dao: DownloadedChapterDao,
    private val bookDao: DownloadedBookDao,
    private val positionDao: PendingPositionDao,
    private val backendIdentity: BackendIdentityRepository,
    private val json: Json,
) {
    private val repoScope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    // Mirrors Room's own state via its Flow-returning query - no manual cache invalidation
    // needed, and this keeps ParagraphPlayer's per-paragraph lookup synchronous even though
    // Room's own DAO calls are all suspend. Spans every libraryId (see DownloadedChapterDao
    // .observeComplete's doc) - localAudioFile/isDownloaded key their lookup by the *current*
    // libraryId themselves, so a stale entry from a previously-configured backend just never
    // matches rather than needing its own reactive re-query on every backend switch.
    private val completeByKey: StateFlow<Map<DownloadKey, DownloadedChapter>> =
        dao.observeComplete()
            .map { rows -> rows.associateBy { DownloadKey(it.libraryId, it.bookId, it.chapterIdx, it.voiceKey) } }
            .stateIn(repoScope, SharingStarted.Eagerly, emptyMap())

    fun localAudioFile(bookId: String, chapterIdx: Int, voiceKey: String, paragraphIdx: Int): File? {
        val libraryId = backendIdentity.libraryId.value ?: return null
        val entry = completeByKey.value[DownloadKey(libraryId, bookId, chapterIdx, voiceKey)] ?: return null
        val file = File(entry.localDir, "%05d.wav".format(paragraphIdx))
        return file.takeIf { it.exists() }
    }

    fun isDownloaded(bookId: String, chapterIdx: Int, voiceKey: String): Boolean {
        val libraryId = backendIdentity.libraryId.value ?: return false
        return completeByKey.value.containsKey(DownloadKey(libraryId, bookId, chapterIdx, voiceKey))
    }

    /** Every downloaded-or-downloading chapter index for [bookId] (any voice) on the current
     *  library, mapped to its status and paragraph progress - drives ReaderScreen's per-chapter
     *  download icon (determinate progress ring while downloading, checkmark once complete,
     *  plain download icon otherwise). Doesn't distinguish by voice the way [localAudioFile]
     *  does: the UI just needs "is something there/in flight", not whether it matches the
     *  *current* voice. Re-queries whenever the configured backend itself changes. */
    @OptIn(kotlinx.coroutines.ExperimentalCoroutinesApi::class)
    fun observeChapterStatuses(bookId: String): Flow<Map<Int, ChapterDownloadState>> =
        backendIdentity.libraryId.filterNotNull().flatMapLatest { libraryId ->
            dao.observeForBook(libraryId, bookId).map { rows ->
                rows.associate { it.chapterIdx to ChapterDownloadState(it.status, it.readyParagraphs, it.totalParagraphs, it.pinned) }
            }
        }

    /** Every downloaded book on the current library - the offline fallback for
     *  LibraryViewModel's book list when a live `listBooks()` fails. */
    @OptIn(kotlinx.coroutines.ExperimentalCoroutinesApi::class)
    fun observeDownloadedBooks(): Flow<List<DownloadedBook>> =
        backendIdentity.libraryId.filterNotNull().flatMapLatest { libraryId ->
            bookDao.observeAll().map { books -> books.filter { it.libraryId == libraryId } }
        }

    /** Reconstructs a BookDetailDto from cached metadata, for opening a downloaded book with no
     *  network at all (see ReaderViewModel.loadBook's offline fallback) - `chapters` only
     *  includes chapters that are actually fully downloaded (status == COMPLETE), same as a live
     *  fetch would only ever show what's real; position fields are left at the start since there
     *  is no cached reading-position source offline. */
    suspend fun cachedBook(bookId: String): BookDetailDto? = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        val book = bookDao.get(libraryId, bookId) ?: return@withContext null
        val chapters = dao.forBook(libraryId, bookId)
            .filter { it.status == DownloadStatus.COMPLETE }
            .sortedBy { it.chapterIdx }
            .map { ChapterSummaryDto(idx = it.chapterIdx, title = it.title, paragraphCount = it.totalParagraphs, readyCount = it.totalParagraphs) }
        BookDetailDto(
            id = book.bookId,
            title = book.title,
            author = book.author,
            coverUrl = book.coverUrl,
            chapterCount = book.chapterCount,
            posChapterIdx = 0,
            posParagraphIdx = 0,
            posSeconds = 0.0,
            progressPercent = 0.0,
            finished = false,
            estimatedTotalSeconds = 0.0,
            estimateCalibrated = false,
            generatedPercent = 0.0,
            chapters = chapters,
        )
    }

    /** Reconstructs a ChapterDetailDto from cached paragraph text/durations, for reading a
     *  downloaded chapter with no network at all - null unless it's fully COMPLETE. Every
     *  paragraph reports READY with no audioUrl: ParagraphPlayer resolves playback through
     *  [localAudioFile] instead, which is guaranteed to have every one of these once this chapter
     *  is COMPLETE, so a missing audioUrl here never actually gets used to try streaming. */
    suspend fun cachedChapter(bookId: String, chapterIdx: Int): ChapterDetailDto? = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        val row = dao.get(libraryId, bookId, chapterIdx)?.takeIf { it.status == DownloadStatus.COMPLETE } ?: return@withContext null
        val offlineParagraphs = runCatching { json.decodeFromString<List<OfflineParagraph>>(row.paragraphsJson) }.getOrDefault(emptyList())
        val paragraphs = offlineParagraphs.map { p ->
            ParagraphDto(
                idx = p.idx,
                text = p.text,
                audioStatus = AudioStatus.READY,
                durationSeconds = p.durationSeconds,
                speaker = p.speaker,
                inline = p.inline,
                directionMarks = p.directionMarks,
                pronunciationMarks = p.pronunciationMarks,
                isQuote = p.isQuote,
                describesCharacters = p.describesCharacters,
                scareQuote = p.scareQuote,
                audioPointerSeconds = p.audioPointerSeconds,
            )
        }
        // Falls back to one synthesized text item per paragraph (pre-contentJson rows, or a row
        // that somehow saved an empty list) rather than an empty chapter - contentJson is what
        // preserves image placement offline, but its absence shouldn't lose the text itself.
        val content = runCatching { json.decodeFromString<List<ContentItemDto>>(row.contentJson) }.getOrNull()
            ?.takeIf { it.isNotEmpty() }
            ?: paragraphs.map { ContentItemDto(kind = "text", paragraphIdx = it.idx) }
        ChapterDetailDto(
            idx = chapterIdx,
            title = row.title,
            generating = false,
            paragraphs = paragraphs,
            content = content,
        )
    }

    /** A chapter content image's locally downloaded bytes (see [downloadChapter]), if this book
     *  has any download at all - images live under the book's own dir, not per-chapter/voice
     *  (unlike audio, their bytes don't depend on voice), so this works regardless of which
     *  chapter/voice combination was actually downloaded. [imageUrl] is the same relative
     *  `/api/images/{id}` path a live ContentItemDto carries - only its trailing id is used, so
     *  this matches equally whether the caller's chapter came from the network or was itself
     *  reconstructed by [cachedChapter]. */
    fun localImageFile(bookId: String, imageUrl: String): File? {
        val libraryId = backendIdentity.libraryId.value ?: return null
        val id = imageUrl.substringAfterLast('/').takeIf { it.isNotBlank() } ?: return null
        return File(imagesDir(libraryId, bookId), id).takeIf { it.exists() }
    }

    /** The voiceKey a downloaded chapter was fetched under, if it's fully COMPLETE - lets
     *  ReaderViewModel's offline fallback set [ParagraphPlayer]'s voiceKey to whatever will
     *  actually match [localAudioFile] for this chapter, since there's no live voice endpoint to
     *  ask when offline. */
    suspend fun cachedChapterVoiceKey(bookId: String, chapterIdx: Int): String? = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        dao.get(libraryId, bookId, chapterIdx)?.takeIf { it.status == DownloadStatus.COMPLETE }?.voiceKey
    }

    /** Queues a reading position [ReaderViewModel][com.lectable.app.ui.reader.ReaderViewModel]
     *  couldn't report to the server (see PendingPosition) - called on a failed updatePosition
     *  call, so progress made with no network isn't silently lost. Overwrites any previously
     *  queued position for this book - only the latest matters. */
    suspend fun savePendingPosition(bookId: String, chapterIdx: Int, paragraphIdx: Int, seconds: Double) =
        withContext(Dispatchers.IO) {
            val libraryId = backendIdentity.currentLibraryId()
            positionDao.upsert(PendingPosition(libraryId, bookId, chapterIdx, paragraphIdx, seconds))
        }

    /** A book's queued-but-not-yet-synced position, if any - see [savePendingPosition]. Used
     *  both to push it up once the server's reachable again, and as the resume point for opening
     *  the book while *still* offline (otherwise there'd be nothing distinguishing "never
     *  synced" from "nothing new to sync" once the book's own cached position, if any, is gone). */
    suspend fun pendingPosition(bookId: String): PendingPosition? = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        positionDao.get(libraryId, bookId)
    }

    /** Clears a book's queued position once it's been successfully pushed to the server. */
    suspend fun clearPendingPosition(bookId: String) = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        positionDao.delete(libraryId, bookId)
    }

    /** Fetches and caches a book's title/author/cover so it can be browsed/opened offline (see
     *  [cachedBook]) - called opportunistically from [downloadChapter] the first time any of a
     *  book's chapters is downloaded, not on every chapter (no need to re-fetch book-level
     *  metadata 100 times over one whole-book download). Best-effort: a failure here doesn't
     *  fail the chapter download itself, it just means offline library browsing won't have this
     *  book's details until a later attempt succeeds. */
    suspend fun cacheBookMetadata(bookId: String) = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        val book = api.getBook(bookId)
        var localCoverPath: String? = null
        val coverUrl = book.coverUrl
        if (coverUrl != null) {
            runCatching {
                val dir = bookDir(libraryId, bookId).apply { mkdirs() }
                val file = File(dir, "cover")
                api.downloadFile(coverUrl).use { body -> file.outputStream().use { out -> body.byteStream().copyTo(out) } }
                localCoverPath = file.path
            }
        }
        bookDao.upsert(
            DownloadedBook(
                libraryId = libraryId,
                bookId = bookId,
                title = book.title,
                author = book.author,
                coverUrl = book.coverUrl,
                localCoverPath = localCoverPath,
                chapterCount = book.chapterCount,
            ),
        )
    }

    /**
     * Ensures generation is complete, fetching and saving each paragraph's audio individually as
     * soon as it's ready (rather than waiting for the whole chapter, then fetching one bundle) -
     * this is what lets [onProgress] and the Room row's own readyParagraphs/totalParagraphs
     * report real per-paragraph progress instead of just "waiting on generation" vs "done".
     * [onProgress] is additionally consumed by [com.lectable.app.playback.ChapterDownloadWorker]
     * for WorkManager progress. Also caches this chapter's title/paragraph text (see
     * [cachedChapter]) and, the first time this book is touched, its book-level metadata (see
     * [cacheBookMetadata]) - the point of downloading is being able to read/browse offline, not
     * just play audio blindly.
     */
    suspend fun downloadChapter(
        bookId: String,
        chapterIdx: Int,
        voiceKey: String,
        pinned: Boolean,
        onProgress: (ready: Int, total: Int) -> Unit = { _, _ -> },
    ) = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        val dir = chapterDir(libraryId, bookId, chapterIdx, voiceKey)
        dir.deleteRecursively()
        dir.mkdirs()

        dao.upsert(
            DownloadedChapter(
                libraryId = libraryId,
                bookId = bookId,
                chapterIdx = chapterIdx,
                voiceKey = voiceKey,
                pinned = pinned,
                status = DownloadStatus.DOWNLOADING,
                localDir = dir.path,
            ),
        )

        if (bookDao.get(libraryId, bookId) == null) {
            runCatching { cacheBookMetadata(bookId) }
        }

        // Idempotent per chapter on the backend (see jobs.Manager) - safe to call even if
        // everything's already generated or another caller already triggered it.
        runCatching { api.generateChapter(bookId, chapterIdx) }

        try {
            val fetchedIdx = mutableSetOf<Int>()
            // Paragraphs a regenerate has already been requested for, this attempt - guards
            // against asking again on every 2s poll while it's re-generating.
            val regeneratedIdx = mutableSetOf<Int>()
            var totalBytes = 0L
            var total = 0
            var lastChapter: ChapterDetailDto? = null
            while (true) {
                val chapter = api.getChapter(bookId, chapterIdx)
                lastChapter = chapter
                total = chapter.paragraphs.size
                for (p in chapter.paragraphs) {
                    if (p.idx in fetchedIdx) continue
                    if (p.audioStatus == AudioStatus.ERROR) {
                        // A paragraph that failed generation isn't given up on - request it be
                        // regenerated (same mechanism ReaderViewModel.regenerateParagraph uses)
                        // and keep polling like any other not-yet-ready paragraph, rather than
                        // abandoning the whole chapter's download over one bad paragraph.
                        if (regeneratedIdx.add(p.idx)) {
                            runCatching { api.lookahead(bookId, LookaheadRequestDto(chapterIdx, p.idx)) }
                        }
                        continue
                    }
                    val url = p.audioUrl ?: continue
                    if (p.audioStatus != AudioStatus.READY) continue
                    totalBytes += api.downloadFile(url).use { body ->
                        File(dir, "%05d.wav".format(p.idx)).outputStream().use { out -> body.byteStream().copyTo(out) }
                    }
                    fetchedIdx += p.idx
                    onProgress(fetchedIdx.size, total)
                    dao.upsert(
                        DownloadedChapter(
                            libraryId = libraryId,
                            bookId = bookId,
                            chapterIdx = chapterIdx,
                            voiceKey = voiceKey,
                            pinned = pinned,
                            status = DownloadStatus.DOWNLOADING,
                            readyParagraphs = fetchedIdx.size,
                            totalParagraphs = total,
                            totalBytes = totalBytes,
                            localDir = dir.path,
                        ),
                    )
                }
                if (total > 0 && fetchedIdx.size >= total) break
                // No timeout here, deliberately: a background-tier chapter can sit behind a lot
                // of other generation work on a single-GPU backend (see tts-service/CLAUDE.md)
                // and legitimately take a long time - the ring should just keep reflecting real
                // progress for as long as that takes, not flip back to "not downloaded" because
                // it was slow. WorkManager itself is what actually stops this running if the app
                // process is killed - it isn't left spinning forever unsupervised.
                delay(POLL_INTERVAL_MS)
            }

            // Images don't depend on voice/generation the way paragraph audio does - they're
            // already fully available the moment the chapter's own content is - so this only
            // needs to run once, here, after the paragraph loop above already confirms the
            // chapter is fully generated. Stored under the book's own dir (see imagesDir), not
            // this chapterDir, since the same image would otherwise be duplicated once per
            // downloaded voice for no benefit. Best-effort per image: one failing (network drop
            // mid-download) shouldn't fail the whole chapter download over an illustration.
            val content = lastChapter?.content ?: emptyList()
            val imgDir = imagesDir(libraryId, bookId).apply { mkdirs() }
            for (item in content) {
                if (item.kind != "image") continue
                val url = item.imageUrl ?: continue
                val id = url.substringAfterLast('/').takeIf { it.isNotBlank() } ?: continue
                val file = File(imgDir, id)
                if (file.exists()) continue
                runCatching {
                    api.downloadFile(url).use { body -> file.outputStream().use { out -> body.byteStream().copyTo(out) } }
                }
            }

            val offlineParagraphs = (lastChapter?.paragraphs ?: emptyList()).map { p ->
                OfflineParagraph(
                    idx = p.idx,
                    text = p.text,
                    durationSeconds = p.durationSeconds ?: 0.0,
                    inline = p.inline,
                    speaker = p.speaker,
                    directionMarks = p.directionMarks,
                    pronunciationMarks = p.pronunciationMarks,
                    isQuote = p.isQuote,
                    describesCharacters = p.describesCharacters,
                    scareQuote = p.scareQuote,
                    audioPointerSeconds = p.audioPointerSeconds,
                )
            }
            dao.upsert(
                DownloadedChapter(
                    libraryId = libraryId,
                    bookId = bookId,
                    chapterIdx = chapterIdx,
                    voiceKey = voiceKey,
                    pinned = pinned,
                    status = DownloadStatus.COMPLETE,
                    title = lastChapter?.title ?: "",
                    paragraphsJson = json.encodeToString(offlineParagraphs),
                    contentJson = json.encodeToString(content),
                    readyParagraphs = total,
                    totalParagraphs = total,
                    totalBytes = totalBytes,
                    lastAccessedAt = System.currentTimeMillis(),
                    localDir = dir.path,
                ),
            )
        } catch (e: Exception) {
            // A genuine failure (network drop, disk write error, cancellation) rather than a
            // recoverable per-paragraph generation error (handled above without throwing) - mark
            // it rather than leaving the row stuck at DOWNLOADING forever, then rethrow so
            // ChapterDownloadWorker's own catch still sees and logs/reports it (also correct for
            // CancellationException, which must always propagate, not be swallowed).
            markError(libraryId, bookId, chapterIdx)
            throw e
        }
    }

    private suspend fun markError(libraryId: String, bookId: String, chapterIdx: Int) {
        val existing = dao.get(libraryId, bookId, chapterIdx) ?: return
        dao.upsert(existing.copy(status = DownloadStatus.ERROR))
    }

    /** Deletes a chapter's download unconditionally (unlike [evictIfUnpinned]) - an explicit
     *  user action (long-press to cancel an in-flight download or remove a completed one)
     *  overrides pinning. Callers cancelling an in-flight download are responsible for stopping
     *  the underlying WorkManager work first (see ReaderViewModel.cancelOrDeleteDownload). */
    suspend fun deleteDownload(bookId: String, chapterIdx: Int) = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        val entry = dao.get(libraryId, bookId, chapterIdx) ?: return@withContext
        File(entry.localDir).deleteRecursively()
        dao.delete(libraryId, bookId, chapterIdx)
    }

    /** Deletes an unpinned (auto-prefetched) chapter's local files once playback moves past it -
     *  a no-op if it's pinned (a manual "download this book" action, kept until the user
     *  explicitly removes it) or was never downloaded. See ReaderViewModel's
     *  chapter-advance collector. */
    suspend fun evictIfUnpinned(bookId: String, chapterIdx: Int) = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        val entry = dao.get(libraryId, bookId, chapterIdx) ?: return@withContext
        if (entry.pinned) return@withContext
        File(entry.localDir).deleteRecursively()
        dao.delete(libraryId, bookId, chapterIdx)
    }

    /** Removes every downloaded chapter (and cached metadata) for a book regardless of pinned
     *  state - used when the user deletes the book itself, or explicitly removes its offline
     *  download. */
    suspend fun deleteBookDownloads(bookId: String) = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        dao.forBook(libraryId, bookId).forEach { File(it.localDir).deleteRecursively() }
        imagesDir(libraryId, bookId).deleteRecursively()
        dao.deleteForBook(libraryId, bookId)
        bookDao.get(libraryId, bookId)?.localCoverPath?.let { File(it).delete() }
        bookDao.delete(libraryId, bookId)
    }

    /** Total on-disk bytes across every downloaded book's directory for the current library
     *  (chapter audio, cached images, and cover) - walked directly off the filesystem rather
     *  than summed from DownloadedChapter.totalBytes, which only ever tracked audio, not the
     *  images downloadChapter also saves under imagesDir - so this stays accurate regardless of
     *  what's actually on disk. Backs Settings' offline-storage summary. */
    suspend fun totalDownloadedBytes(): Long = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.libraryId.value ?: return@withContext 0L
        val dir = File(context.filesDir, "downloads/${hash(libraryId)}")
        if (!dir.exists()) 0L else dir.walkTopDown().filter { it.isFile }.sumOf { it.length() }
    }

    /** How many distinct books have at least one download for the current library - Settings'
     *  offline-storage summary again, alongside [totalDownloadedBytes]. */
    suspend fun downloadedBookCount(): Int = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        bookDao.bookIdsForLibrary(libraryId).size
    }

    /** Removes every downloaded book for the current library in one action - Settings' "Delete
     *  all downloads" button, for reclaiming space across the whole library at once rather than
     *  one book at a time. Reuses [deleteBookDownloads] per book rather than a bespoke bulk
     *  query, so it can't drift from what a single-book delete actually cleans up. */
    suspend fun deleteAllDownloads() = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        bookDao.bookIdsForLibrary(libraryId).forEach { bookId -> deleteBookDownloads(bookId) }
    }

    private fun chapterDir(libraryId: String, bookId: String, chapterIdx: Int, voiceKey: String): File =
        File(bookDir(libraryId, bookId), "$chapterIdx/${hash(voiceKey)}")

    // A sibling of the per-(chapter,voice) dirs above, not nested under one of them - image
    // bytes don't depend on voice, so they're shared across every voice/chapter download for
    // this book rather than duplicated per voiceKey. See localImageFile/downloadChapter.
    private fun imagesDir(libraryId: String, bookId: String): File = File(bookDir(libraryId, bookId), "images")

    private fun bookDir(libraryId: String, bookId: String): File =
        File(context.filesDir, "downloads/${hash(libraryId)}/$bookId")

    private fun hash(value: String): String {
        val digest = MessageDigest.getInstance("SHA-256").digest(value.toByteArray())
        return digest.joinToString("") { "%02x".format(it) }.take(16)
    }
}
