package com.lectable.app.data.repository

import android.content.Context
import android.net.Uri
import android.os.SystemClock
import android.system.Os
import com.lectable.app.data.download.DownloadStatus
import com.lectable.app.data.download.DownloadedBook
import com.lectable.app.data.download.DownloadedBookDao
import com.lectable.app.data.download.DownloadedChapter
import com.lectable.app.data.download.DownloadedChapterDao
import com.lectable.app.data.download.OfflineParagraph
import com.lectable.app.data.download.STAGED_SUFFIX
import com.lectable.app.data.download.clipFile
import com.lectable.app.data.download.clipFiles
import com.lectable.app.data.download.renameLegacyClips
import com.lectable.app.data.download.commitStagedClips
import com.lectable.app.data.download.discardStagedClips
import com.lectable.app.data.download.downloadDirName
import com.lectable.app.data.download.fetchedCount
import com.lectable.app.data.download.stagedClipFile
import com.lectable.app.data.download.toCachedParagraphs
import com.lectable.app.data.download.hasAudioOf
import com.lectable.app.data.download.PendingPosition
import com.lectable.app.data.download.PendingPositionDao
import com.lectable.app.data.remote.BackendIdentityRepository
import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.MediaApi
import com.lectable.app.data.live.LiveStore
import com.lectable.app.data.remote.dto.AudioStatuses
import com.lectable.app.data.remote.dto.BookDetailDto
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ChapterMusicDto
import com.lectable.app.data.remote.dto.ChapterSummaryDto
import com.lectable.app.data.remote.dto.ContentItemDto
import com.lectable.app.data.remote.dto.LookaheadRequestDto
import com.lectable.app.data.remote.dto.ParagraphDto
import com.lectable.app.data.settings.LightingSettingsRepository
import dagger.hilt.android.qualifiers.ApplicationContext
import java.io.File
import java.io.IOException
import kotlin.coroutines.cancellation.CancellationException
import retrofit2.HttpException
import java.util.concurrent.ConcurrentHashMap
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.filterNotNull
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.flatMapLatest
import kotlinx.coroutines.flow.flowOf
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.sync.Semaphore
import kotlinx.coroutines.sync.withPermit
import kotlinx.coroutines.withContext
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json


/**
 * presetId+instruct+language, exactly what backend/internal/store/voicepresets.go's VoiceID hashes
 * (minus seed, minus the hash itself - see that function) - used here purely for local
 * staleness comparison against a downloaded chapter's voice, not required to match the server's
 * own hash byte for byte.
 */
fun voiceKeyOf(presetId: String, instruct: String, language: String): String =
    "$presetId $instruct $language"

data class DownloadKey(val libraryId: String, val bookId: String, val chapterIdx: Int, val voiceKey: String)

// downloadBookAmbience: how many chapters it asks the backend to work on (and polls) at once,
// how often it polls them, and how long none of them may change before it moves on.
private const val AMBIENCE_WINDOW = 8
private const val AMBIENCE_POLL_MS = 20_000L
private const val AMBIENCE_STALL_MS = 4 * 60_000L
// Chapter downloads holding a live chapter at once - see DownloadRepository.withChapterSlot.
private const val MAX_READY_CHAPTER_DOWNLOADS = 3
private const val MAX_WAITING_CHAPTER_DOWNLOADS = 2

/** One chapter's download state as the UI needs it - see [DownloadRepository.observeChapterStatuses].
 *  [ambience] is null until the chapter has any ambience stored. */
data class ChapterDownloadState(
    val status: String,
    val readyParagraphs: Int,
    val totalParagraphs: Int,
    val ambience: AmbienceDownloadState? = null,
)

/** How much of a downloaded chapter's ambience is on the device (see
 *  [DownloadRepository.downloadBookAmbience]): [scored] once its regions and lighting cues are
 *  all there, then - only [withClips], i.e. while the book's background music is on - one clip
 *  per region. */
data class AmbienceDownloadState(
    val scored: Boolean,
    val withClips: Boolean,
    val readyClips: Int,
    val failedClips: Int,
    val totalClips: Int,
) {
    /** Nothing more to fetch - a region whose music failed to render has no clip to wait for. */
    val settled: Boolean get() = scored && (!withClips || readyClips + failedClips >= totalClips)
}

/** A chapter's stored ambience summarized for the UI - see [AmbienceDownloadState]. */
internal fun ChapterMusicDto.toAmbienceDownloadState(): AmbienceDownloadState = AmbienceDownloadState(
    scored = scored && lit,
    withClips = enabled,
    readyClips = regions.count { it.status == AudioStatuses.READY },
    failedClips = regions.count { it.status == AudioStatuses.ERROR },
    totalClips = regions.size,
)

/** Whether this stored ambience is everything there is to have: scored and lit, and - when
 *  clips are wanted ([withClips]) - every region either has its clip or failed to render. */
internal fun ChapterMusicDto.ambienceSettled(withClips: Boolean): Boolean {
    if (!scored || !lit) return false
    return !withClips || regions.all { it.status == AudioStatuses.READY || it.status == AudioStatuses.ERROR }
}

/**
 * Downloads a chapter's generated audio to the device for offline playback, one paragraph's
 * clip at a time (reusing the same `GET /api/paragraphs/{id}/audio` endpoint streaming
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
    private val mediaApi: MediaApi,
    private val dao: DownloadedChapterDao,
    private val bookDao: DownloadedBookDao,
    private val positionDao: PendingPositionDao,
    private val backendIdentity: BackendIdentityRepository,
    private val json: Json,
    private val liveStore: LiveStore,
    private val lightingSettings: LightingSettingsRepository,
) {
    private val repoScope = CoroutineScope(SupervisorJob() + Dispatchers.Default)

    // Every downloadChapter call currently running, as (bookId, chapterIdx) - see withChapterSlot.
    private val activeDownloads = MutableStateFlow<Set<Pair<String, Int>>>(emptySet())
    private val readyChapterSlots = Semaphore(MAX_READY_CHAPTER_DOWNLOADS)

    init {
        // Clips downloaded before they were given a format-neutral name - see renameLegacyClips.
        repoScope.launch(Dispatchers.IO) { renameLegacyClips(File(context.filesDir, "downloads")) }
    }

    // Mirrors Room's own state via its Flow-returning query - no manual cache invalidation
    // needed, and this keeps ParagraphPlayer's per-paragraph lookup synchronous even though
    // Room's own DAO calls are all suspend. Spans every libraryId (see DownloadedChapterDao
    // .observeComplete's doc) - localAudioFile keys its lookup by the *current*
    // libraryId themselves, so a stale entry from a previously-configured backend just never
    // matches rather than needing its own reactive re-query on every backend switch.
    private val completeByKey: StateFlow<Map<DownloadKey, DownloadedChapter>> =
        dao.observeComplete()
            .map { rows -> rows.associateBy { DownloadKey(it.libraryId, it.bookId, it.chapterIdx, it.voiceKey) } }
            .stateIn(repoScope, SharingStarted.Eagerly, emptyMap())

    // Each COMPLETE row's decoded paragraphsJson, keyed by row and re-decoded only when that
    // row's JSON actually changes (compared by reference - a new row from Room is a new string).
    private val storedParagraphs = ConcurrentHashMap<DownloadKey, Pair<String, Map<Int, OfflineParagraph>>>()

    /** The downloaded clip for [paragraph], or null if there isn't one or it's stale. A live
     *  [paragraph] is only matched to its local file while its audio is still what was
     *  downloaded (see [hasAudioOf] - by audioHash): the file is keyed by the book's voice only,
     *  but a paragraph's audio also depends on its speaker (and scare-quote merge group), so a
     *  re-attribution after download regenerates it server-side while the local copy keeps the
     *  old speaker until OfflineReconciler refreshes it. (A [cachedChapter] paragraph never needs
     *  this - its audioUrl already is its local file.) */
    fun localAudioFile(bookId: String, chapterIdx: Int, voiceKey: String, paragraph: ParagraphDto): File? {
        val libraryId = backendIdentity.libraryId.value ?: return null
        val key = DownloadKey(libraryId, bookId, chapterIdx, voiceKey)
        val entry = completeByKey.value[key] ?: return null
        val stored = storedParagraphs(key, entry)[paragraph.idx] ?: return null
        if (!stored.hasAudioOf(paragraph)) return null
        return clipFile(File(entry.localDir), paragraph.idx).takeIf { it.exists() }
    }

    private fun storedParagraphs(key: DownloadKey, entry: DownloadedChapter): Map<Int, OfflineParagraph> {
        storedParagraphs[key]?.let { (raw, decoded) -> if (raw === entry.paragraphsJson) return decoded }
        val decoded = runCatching { json.decodeFromString<List<OfflineParagraph>>(entry.paragraphsJson) }.getOrDefault(emptyList())
            .associateBy { it.idx }
        storedParagraphs[key] = entry.paragraphsJson to decoded
        return decoded
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
                rows.associate { it.chapterIdx to ChapterDownloadState(it.status, it.readyParagraphs, it.totalParagraphs, ambienceState(it)) }
            }
        }

    // Each row's decoded ambience summary, re-decoded only when its musicJson changes - this
    // flow re-emits every row on every paragraph of a download in progress.
    private val ambienceStates = ConcurrentHashMap<DownloadKey, Pair<String, AmbienceDownloadState?>>()

    private fun ambienceState(row: DownloadedChapter): AmbienceDownloadState? {
        if (row.status != DownloadStatus.COMPLETE) return null
        val key = DownloadKey(row.libraryId, row.bookId, row.chapterIdx, "")
        ambienceStates[key]?.let { (raw, state) -> if (raw == row.musicJson) return state }
        val state = decodeMusic(row.musicJson)?.toAmbienceDownloadState()
        ambienceStates[key] = row.musicJson to state
        return state
    }

    /** Every downloaded book on the current library - what LibraryViewModel's book list shows
     *  until the live one arrives, or instead of it when the backend can't be reached. Empty
     *  (rather than silent) while the library id isn't known: nothing can have been downloaded
     *  from a backend that has never been reached. */
    @OptIn(kotlinx.coroutines.ExperimentalCoroutinesApi::class)
    fun observeDownloadedBooks(): Flow<List<DownloadedBook>> =
        backendIdentity.libraryId.flatMapLatest { libraryId ->
            if (libraryId == null) {
                flowOf(emptyList())
            } else {
                bookDao.observeAll().map { books -> books.filter { it.libraryId == libraryId } }
            }
        }

    /** Reconstructs a BookDetailDto from cached metadata, for opening a downloaded book before
     *  the backend has answered, or with no network at all (see ReaderViewModel.loadBook) -
     *  `chapters` only includes chapters that are actually fully downloaded (status ==
     *  COMPLETE), same as a live fetch would only ever show what's real; the position is the
     *  backend's as last seen online (see DownloadedBook.posChapterIdx). Like every cached read
     *  here, never waits on the network: null when the library id isn't known yet. */
    suspend fun cachedBook(bookId: String): BookDetailDto? = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.libraryId.value ?: return@withContext null
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
            posChapterIdx = book.posChapterIdx,
            posParagraphIdx = book.posParagraphIdx,
            posSeconds = book.posSeconds,
            progressPercent = 0.0,
            finished = false,
            estimatedTotalSeconds = 0.0,
            estimateCalibrated = false,
            generatedPercent = 0.0,
            chapters = chapters,
        )
    }

    /** Reconstructs a ChapterDetailDto from cached paragraph text/durations, for reading a
     *  downloaded chapter before the backend has answered, or with no network at all - null
     *  unless it's fully COMPLETE. Every paragraph reports READY with its downloaded clip's
     *  `file://` URI as its audioUrl (MediaUrlResolver passes those through), so ParagraphPlayer
     *  plays it like any other. The members of a scare-quote merge group share their anchor's
     *  file - each one's own is a duplicate of the same whole clip (see
     *  OfflineParagraph.audioPointerSeconds), and the player tells a group by its shared URL. */
    suspend fun cachedChapter(bookId: String, chapterIdx: Int): ChapterDetailDto? = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.libraryId.value ?: return@withContext null
        val row = dao.get(libraryId, bookId, chapterIdx)?.takeIf { it.status == DownloadStatus.COMPLETE } ?: return@withContext null
        val offlineParagraphs = runCatching { json.decodeFromString<List<OfflineParagraph>>(row.paragraphsJson) }.getOrDefault(emptyList())
        val dir = File(row.localDir)
        val paragraphs = offlineParagraphs.toCachedParagraphs { anchorIdx -> Uri.fromFile(clipFile(dir, anchorIdx)).toString() }
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

    /** A downloaded chapter's stored ambience (see [downloadBookAmbience]), for the reader's
     *  background music and room lighting before the backend has answered, or with no network at
     *  all - null if none was ever stored. A region whose clip is on disk is READY with that
     *  clip's `file://` URI as its audioUrl; any other has no audio (its lighting cue still
     *  applies). `enabled` is the book's music toggle as last seen online. */
    suspend fun cachedChapterMusic(bookId: String, chapterIdx: Int): ChapterMusicDto? = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.libraryId.value ?: return@withContext null
        val row = dao.get(libraryId, bookId, chapterIdx)?.takeIf { it.status == DownloadStatus.COMPLETE } ?: return@withContext null
        val music = decodeMusic(row.musicJson) ?: return@withContext null
        val dir = musicDir(libraryId, bookId)
        music.copy(
            regions = music.regions.map { r ->
                val file = File(dir, r.id)
                if (r.status == AudioStatuses.READY && file.exists()) {
                    r.copy(audioUrl = Uri.fromFile(file).toString())
                } else {
                    r.copy(status = AudioStatuses.PENDING, audioUrl = null)
                }
            },
        )
    }

    private fun decodeMusic(raw: String): ChapterMusicDto? =
        raw.takeIf { it.isNotEmpty() }?.let { runCatching { json.decodeFromString<ChapterMusicDto>(it) }.getOrNull() }

    /**
     * Brings every downloaded chapter of [bookId] up to having its ambience offline too: the
     * music regions and their lighting cues, plus each region's clip while the book's background
     * music is on. Does nothing unless the listener uses ambience at all (the book's music
     * toggle, or Hue lighting on this device). Unlike narration, ambience usually doesn't exist
     * yet for chapters nobody has listened to - regions are scored and their music rendered as a
     * reader reaches them - so this asks the backend for whatever is missing (the ambience pass,
     * then the whole-chapter music run) and polls until it lands, [AMBIENCE_WINDOW] chapters at
     * a time in reading order. All progress is in the rows themselves, so a run that's stopped
     * just picks up where it left off.
     *
     * Returns whether every downloaded chapter is settled. Chapters whose ambience doesn't move
     * for [AMBIENCE_STALL_MS] (backend busy with something else, queue paused) are skipped for
     * this run, as are ones the backend refuses - the caller retries later. A chapter already
     * settled isn't looked at again, so a region re-rendered afterwards is only picked up when
     * its chapter is next downloaded.
     */
    suspend fun downloadBookAmbience(bookId: String): Boolean = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        val music = api.getVoice(bookId).musicEnabled
        if (!music && !lightingSettings.settings.first().enabled) return@withContext true
        val askedScoring = mutableSetOf<Int>()
        val askedMusic = mutableSetOf<Int>()
        val skipped = mutableSetOf<Int>()
        var lastChangeAt = SystemClock.elapsedRealtime()
        while (true) {
            val open = dao.forBook(libraryId, bookId)
                .filter { it.status == DownloadStatus.COMPLETE && it.chapterIdx !in skipped && !ambienceSettled(it, music) }
                .sortedBy { it.chapterIdx }
                .take(AMBIENCE_WINDOW)
            if (open.isEmpty()) return@withContext skipped.isEmpty()
            var changed = false
            for (row in open) {
                val idx = row.chapterIdx
                val dto = api.getChapterMusic(bookId, idx)
                if (storeAmbience(libraryId, bookId, idx, dto, withClips = music)) changed = true
                try {
                    if (!dto.scored || !dto.lit) {
                        if (askedScoring.add(idx)) {
                            api.runChapterAmbience(bookId, idx)
                            changed = true
                        }
                    } else if (music && dto.regions.any { it.status != AudioStatuses.READY && it.status != AudioStatuses.ERROR }) {
                        if (askedMusic.add(idx)) {
                            api.generateChapterMusic(bookId, idx)
                            changed = true
                        }
                    }
                } catch (e: HttpException) {
                    // No LLM configured, narration regenerating, chapter gone - not something
                    // waiting will fix.
                    skipped += idx
                }
            }
            val now = SystemClock.elapsedRealtime()
            if (changed) {
                lastChangeAt = now
            } else if (now - lastChangeAt > AMBIENCE_STALL_MS) {
                open.mapTo(skipped) { it.chapterIdx }
                lastChangeAt = now
                continue
            }
            delay(AMBIENCE_POLL_MS)
        }
        @Suppress("UNREACHABLE_CODE")
        false
    }

    /** Whether [row]'s stored ambience is everything there is to have - see
     *  [ChapterMusicDto.ambienceSettled]; false while none is stored. */
    private fun ambienceSettled(row: DownloadedChapter, music: Boolean): Boolean =
        decodeMusic(row.musicJson)?.ambienceSettled(withClips = music) ?: false

    /** Stores [dto] as a downloaded chapter's ambience, fetching (when [withClips]) the clip of
     *  every READY region that isn't on disk yet or was re-rendered since, and dropping clips of
     *  regions the chapter no longer has. A clip the backend is re-rendering, or one that fails
     *  to download, keeps its old local copy. Returns whether anything stored changed. */
    private suspend fun storeAmbience(libraryId: String, bookId: String, chapterIdx: Int, dto: ChapterMusicDto, withClips: Boolean): Boolean {
        val row = dao.get(libraryId, bookId, chapterIdx)?.takeIf { it.status == DownloadStatus.COMPLETE } ?: return false
        val stored = decodeMusic(row.musicJson)?.regions.orEmpty().associateBy { it.id }
        val dir = musicDir(libraryId, bookId).apply { mkdirs() }
        val regions = dto.regions.map { r ->
            val file = File(dir, r.id)
            val local = stored[r.id]?.takeIf { it.status == AudioStatuses.READY && file.exists() }
            val url = r.audioUrl
            val ready = r.status == AudioStatuses.READY
            when {
                ready && local != null && local.durationSeconds == r.durationSeconds -> r
                ready && url != null && withClips && fetchClip(url, file) -> r
                local != null -> r.copy(status = AudioStatuses.READY, audioUrl = local.audioUrl, durationSeconds = local.durationSeconds, error = null)
                r.status == AudioStatuses.ERROR -> r
                else -> r.copy(status = AudioStatuses.PENDING, audioUrl = null)
            }
        }
        val liveIds = dto.regions.mapTo(mutableSetOf()) { it.id }
        stored.keys.filter { it !in liveIds }.forEach { File(dir, it).delete() }
        val encoded = json.encodeToString(dto.copy(regions = regions))
        if (encoded == row.musicJson) return false
        dao.setMusicJson(libraryId, bookId, chapterIdx, encoded)
        return true
    }

    /** Downloads [url] to [target] via a staged file, so a clip being played is never half
     *  written. False if it couldn't be fetched. */
    private suspend fun fetchClip(url: String, target: File): Boolean {
        val part = File(target.path + STAGED_SUFFIX)
        return try {
            mediaApi.downloadFile(url).use { body -> part.outputStream().use { out -> body.byteStream().copyTo(out) } }
            part.renameTo(target)
        } catch (e: CancellationException) {
            part.delete()
            throw e
        } catch (e: Exception) {
            part.delete()
            false
        }
    }

    /** The voiceKey a downloaded chapter was fetched under, if it's fully COMPLETE - lets
     *  ReaderViewModel's offline fallback set [ParagraphPlayer]'s voiceKey to whatever will
     *  actually match [localAudioFile] for this chapter, since there's no live voice endpoint to
     *  ask when offline. */
    suspend fun cachedChapterVoiceKey(bookId: String, chapterIdx: Int): String? = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.libraryId.value ?: return@withContext null
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
        val libraryId = backendIdentity.libraryId.value ?: return@withContext null
        positionDao.get(libraryId, bookId)
    }

    /** Clears a book's queued position once it's been successfully pushed to the server. */
    suspend fun clearPendingPosition(bookId: String) = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        positionDao.delete(libraryId, bookId)
    }

    /** Fetches and caches a book's title/author/cover so it can be browsed/opened offline (see
     *  [cachedBook]) - called opportunistically from [downloadChapter] the first time any of a
     *  book's chapters is downloaded (and by OfflineReconciler when the book changed), not on every chapter (no need to re-fetch book-level
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
                mediaApi.downloadFile(coverUrl).use { body -> file.outputStream().use { out -> body.byteStream().copyTo(out) } }
                localCoverPath = file.path
            }
        }
        // Keeps an existing row's sync state (hash) - OfflineReconciler re-runs this to pick up
        // a changed cover, and records the new hash itself once everything else matches too.
        val base = bookDao.get(libraryId, bookId) ?: DownloadedBook(
            libraryId = libraryId,
            bookId = bookId,
            title = book.title,
            author = book.author,
            coverUrl = book.coverUrl,
            localCoverPath = null,
            chapterCount = book.chapterCount,
        )
        bookDao.upsert(
            base.copy(
                title = book.title,
                author = book.author,
                coverUrl = book.coverUrl,
                // A failed cover fetch keeps whatever was there; a cover gone server-side goes.
                localCoverPath = if (coverUrl == null) null else localCoverPath ?: base.localCoverPath,
                chapterCount = book.chapterCount,
                posChapterIdx = book.posChapterIdx,
                posParagraphIdx = book.posParagraphIdx,
                posSeconds = book.posSeconds,
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
     *
     * Doubles as the refresh path for a chapter that's already COMPLETE (OfflineReconciler found
     * its hash changed server-side, or the user downloaded it again): under the same voice, every
     * paragraph whose local clip still matches the backend's audioHash is kept rather than
     * re-fetched; under a new voice everything is fetched into a fresh directory. Either way the
     * existing copy stays untouched, and playable offline, until the new one is complete - new
     * clips are staged as `.part` files and only swapped in (and the row replaced) at the end,
     * and a failed refresh just discards them.
     */
    suspend fun downloadChapter(
        bookId: String,
        chapterIdx: Int,
        voiceKey: String,
        pinned: Boolean,
        onProgress: (ready: Int, total: Int) -> Unit = { _, _ -> },
    ) = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        val existing = dao.get(libraryId, bookId, chapterIdx)?.takeIf { it.status == DownloadStatus.COMPLETE }
        val reusable = existing?.takeIf { it.voiceKey == voiceKey }
        val dir = reusable?.let { File(it.localDir) } ?: chapterDir(libraryId, bookId, chapterIdx, voiceKey)
        if (reusable == null) dir.deleteRecursively()
        dir.mkdirs()
        discardStagedClips(dir)

        if (existing == null) {
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
        }

        if (bookDao.get(libraryId, bookId) == null) {
            runCatching { cacheBookMetadata(bookId) }
        }

        // Idempotent per chapter on the backend (see jobs.Manager) - safe to call even if
        // everything's already generated or another caller already triggered it.
        runCatching { api.generateChapter(bookId, chapterIdx) }

        try {
            withChapterSlot(bookId, chapterIdx) {
                // The audio each local file (committed or staged) holds, per paragraph - seeded from
                // the existing copy's stored paragraphs on a same-voice refresh, then from the live
                // paragraph each fetch came from. A paragraph whose audio changes again mid-download
                // (re-attributed, re-merged) gets fetched again.
                val fetched = mutableMapOf<Int, OfflineParagraph>()
                if (reusable != null) {
                    runCatching { json.decodeFromString<List<OfflineParagraph>>(reusable.paragraphsJson) }.getOrDefault(emptyList())
                        .filter { clipFile(dir, it.idx).exists() }
                        .associateByTo(fetched) { it.idx }
                }
                // Paragraphs a regenerate has already been requested for, this attempt - guards
                // against asking again on every live update while it's re-generating.
                val regeneratedIdx = mutableSetOf<Int>()
                // The local file already holding each clip fetched this attempt, by audioUrl. A
                // scare-quote merge group's members all point at their anchor's one clip, so without
                // this every member downloaded the same file again.
                val clipFiles = mutableMapOf<String, File>()
                var fetchedBytes = 0L
                var lastChapter: ChapterDetailDto? = null
                // The chapter's live topic (see LiveStore) delivers a fresh value every time one of
                // its paragraphs changes; each is scanned for newly-ready audio until everything's
                // fetched. An error only arrives before any value did (backend unreachable, chapter
                // gone) - thrown so the worker fails the same way a failed fetch always has.
                liveStore.chapter(bookId, chapterIdx).first { result ->
                    result.error?.let { e -> if (result.data == null) throw IOException(e.message) }
                    val chapter = result.data ?: return@first false
                    lastChapter = chapter
                    val total = chapter.paragraphs.size
                    for (p in chapter.paragraphs) {
                        val f = fetched[p.idx]
                        if (f != null) {
                            if (f.hasAudioOf(p)) continue
                            fetched.remove(p.idx)
                        }
                        if (p.audioStatus == AudioStatuses.ERROR) {
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
                        if (p.audioStatus != AudioStatuses.READY) continue
                        val target = stagedClipFile(dir, p.idx)
                        val sameClip = clipFiles[url]?.takeIf { it.exists() }
                        if (sameClip != null) {
                            reuseClip(sameClip, target)
                        } else {
                            fetchedBytes += mediaApi.downloadFile(url).use { body ->
                                target.outputStream().use { out -> body.byteStream().copyTo(out) }
                            }
                        }
                        clipFiles[url] = target
                        fetched[p.idx] = OfflineParagraph(
                            idx = p.idx,
                            text = p.text,
                            durationSeconds = p.durationSeconds ?: 0.0,
                            audioPointerSeconds = p.audioPointerSeconds,
                            audioHash = p.audioHash,
                        )
                        val ready = fetchedCount(chapter.paragraphs, fetched)
                        onProgress(ready, total)
                        // A refresh leaves the COMPLETE row alone until the swap below - it's what
                        // keeps the old copy playable offline meanwhile.
                        if (existing == null) {
                            dao.upsert(
                                DownloadedChapter(
                                    libraryId = libraryId,
                                    bookId = bookId,
                                    chapterIdx = chapterIdx,
                                    voiceKey = voiceKey,
                                    pinned = pinned,
                                    status = DownloadStatus.DOWNLOADING,
                                    readyParagraphs = ready,
                                    totalParagraphs = total,
                                    totalBytes = fetchedBytes,
                                    localDir = dir.path,
                                ),
                            )
                        }
                    }
                    // No timeout here, deliberately: a background-tier chapter can sit behind a lot
                    // of other generation work on a single-GPU backend and legitimately take a long
                    // time - the ring should just keep reflecting real progress for as long as that
                    // takes, not flip back to "not downloaded" because it was slow. WorkManager
                    // itself is what actually stops this running if the app process is killed.
                    total > 0 && fetchedCount(chapter.paragraphs, fetched) == total
                }
                val chapter = lastChapter ?: throw IOException("chapter $chapterIdx never loaded")

                // Images don't depend on voice/generation the way paragraph audio does - they're
                // already fully available the moment the chapter's own content is - so this only
                // needs to run once, here, after the paragraph loop above already confirms the
                // chapter is fully generated. Stored under the book's own dir (see imagesDir), not
                // this chapterDir, since the same image would otherwise be duplicated once per
                // downloaded voice for no benefit. Best-effort per image: one failing (network drop
                // mid-download) shouldn't fail the whole chapter download over an illustration.
                val imgDir = imagesDir(libraryId, bookId).apply { mkdirs() }
                for (item in chapter.content) {
                    if (item.kind != "image") continue
                    val url = item.imageUrl ?: continue
                    val id = url.substringAfterLast('/').takeIf { it.isNotBlank() } ?: continue
                    val file = File(imgDir, id)
                    if (file.exists()) continue
                    runCatching {
                        mediaApi.downloadFile(url).use { body -> file.outputStream().use { out -> body.byteStream().copyTo(out) } }
                    }
                }

                // The swap: staged clips replace their committed counterparts, and files for
                // paragraphs the chapter no longer has are dropped.
                commitStagedClips(dir, chapter.paragraphs.map { it.idx })

                val offlineParagraphs = chapter.paragraphs.map { p ->
                    val f = fetched.getValue(p.idx)
                    OfflineParagraph(
                        idx = p.idx,
                        text = p.text,
                        durationSeconds = f.durationSeconds,
                        inline = p.inline,
                        speaker = p.speaker,
                        label = p.label,
                        table = p.table,
                        directionMarks = p.directionMarks,
                        pronunciationMarks = p.pronunciationMarks,
                        isQuote = p.isQuote,
                        scareQuote = p.scareQuote,
                        emotion = p.emotion,
                        audioPointerSeconds = f.audioPointerSeconds,
                        contentHash = p.contentHash,
                        audioHash = p.audioHash,
                    )
                }
                val total = chapter.paragraphs.size
                dao.upsert(
                    DownloadedChapter(
                        libraryId = libraryId,
                        bookId = bookId,
                        chapterIdx = chapterIdx,
                        voiceKey = voiceKey,
                        pinned = pinned,
                        status = DownloadStatus.COMPLETE,
                        title = chapter.title,
                        paragraphsJson = json.encodeToString(offlineParagraphs),
                        contentJson = json.encodeToString(chapter.content),
                        readyParagraphs = total,
                        totalParagraphs = total,
                        totalBytes = clipFiles(dir).sumOf { it.length() },
                        lastAccessedAt = System.currentTimeMillis(),
                        hash = chapter.hash,
                        // Ambience isn't part of this download (see downloadBookAmbience) - a
                        // refresh keeps whatever is stored, re-read since it may have moved.
                        musicJson = dao.get(libraryId, bookId, chapterIdx)?.musicJson.orEmpty(),
                        localDir = dir.path,
                    ),
                )
                // A voice change downloaded into a fresh directory - the old voice's copy is only
                // dropped now that the new one has replaced it.
                if (existing != null && existing.localDir != dir.path) File(existing.localDir).deleteRecursively()
            }
        } catch (e: Exception) {
            // A genuine failure (network drop, disk write error, cancellation) rather than a
            // recoverable per-paragraph generation error (handled above without throwing) - mark
            // a first download as failed rather than leaving the row stuck at DOWNLOADING
            // forever; a refresh instead just drops what it staged, leaving the existing copy
            // exactly as it was (the next reconcile tries again). Then rethrow so
            // ChapterDownloadWorker's own catch still sees and logs/reports it (also correct for
            // CancellationException, which must always propagate, not be swallowed).
            discardStagedClips(dir)
            if (existing == null) {
                markError(libraryId, bookId, chapterIdx)
            } else if (existing.localDir != dir.path) {
                dir.deleteRecursively()
            }
            throw e
        }
    }

    /**
     * Runs [block] - the part of [downloadChapter] that holds the chapter's live topic - once
     * this chapter has a turn. A whole-book download starts every chapter's worker at once, and
     * each open chapter is its full text and word timings held in memory until its audio is
     * generated, which behind a long queue is hours: 139 of them ran the app out of memory at
     * launch. So a chapter waits here, watching only the book's one shared topic, until either
     * its audio is fully generated (then at most [MAX_READY_CHAPTER_DOWNLOADS] fetch at a time)
     * or it is among the book's first [MAX_WAITING_CHAPTER_DOWNLOADS] downloads still waiting on
     * generation (those stay open, so the next chapters to generate show paragraph progress and
     * get their failed paragraphs re-requested).
     */
    private suspend fun <T> withChapterSlot(bookId: String, chapterIdx: Int, block: suspend () -> T): T {
        val key = bookId to chapterIdx
        activeDownloads.update { it + key }
        try {
            val generated = combine(liveStore.book(bookId), activeDownloads) { book, active ->
                // Unreachable backend or unknown chapter: let the chapter topic report it.
                val chapters = book.data?.chapters ?: return@combine if (book.error != null) false else null
                val unready = chapters.filter { it.paragraphCount == 0 || it.readyCount < it.paragraphCount }.map { it.idx }.toSet()
                if (chapters.any { it.idx == chapterIdx } && chapterIdx !in unready) return@combine true
                val ahead = active.count { it.first == bookId && it.second < chapterIdx && it.second in unready }
                if (ahead < MAX_WAITING_CHAPTER_DOWNLOADS) false else null
            }.filterNotNull().first()
            return if (generated) readyChapterSlots.withPermit { block() } else block()
        } finally {
            activeDownloads.update { it - key }
        }
    }

    /** Gives [target] the same contents as [source] - a hard link (no extra space; both stay
     *  valid when one is later renamed or deleted), falling back to a copy. */
    private fun reuseClip(source: File, target: File) {
        target.delete()
        runCatching { Os.link(source.path, target.path) }
            .onFailure { source.copyTo(target, overwrite = true) }
    }

    private suspend fun markError(libraryId: String, bookId: String, chapterIdx: Int) {
        val existing = dao.get(libraryId, bookId, chapterIdx) ?: return
        dao.upsert(existing.copy(status = DownloadStatus.ERROR))
    }

    /** Deletes a chapter's download - an explicit user action (long-press to cancel an
     *  in-flight download or remove a completed one). Callers cancelling an in-flight download are responsible for stopping
     *  the underlying WorkManager work first (see ReaderViewModel.cancelOrDeleteDownload). */
    suspend fun deleteDownload(bookId: String, chapterIdx: Int) = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        val entry = dao.get(libraryId, bookId, chapterIdx) ?: return@withContext
        File(entry.localDir).deleteRecursively()
        val music = musicDir(libraryId, bookId)
        decodeMusic(entry.musicJson)?.regions?.forEach { File(music, it.id).delete() }
        dao.delete(libraryId, bookId, chapterIdx)
    }

    /** Removes every downloaded chapter (and cached metadata) for a book regardless of pinned
     *  state - used when the user deletes the book itself, or explicitly removes its offline
     *  download. */
    suspend fun deleteBookDownloads(bookId: String) = withContext(Dispatchers.IO) {
        val libraryId = backendIdentity.currentLibraryId()
        dao.forBook(libraryId, bookId).forEach { File(it.localDir).deleteRecursively() }
        imagesDir(libraryId, bookId).deleteRecursively()
        musicDir(libraryId, bookId).deleteRecursively()
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
        val dir = File(context.filesDir, "downloads/${downloadDirName(libraryId)}")
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
        File(bookDir(libraryId, bookId), "$chapterIdx/${downloadDirName(voiceKey)}")

    // A sibling of the per-(chapter,voice) dirs above, not nested under one of them - image
    // bytes don't depend on voice, so they're shared across every voice/chapter download for
    // this book rather than duplicated per voiceKey. See localImageFile/downloadChapter.
    private fun imagesDir(libraryId: String, bookId: String): File = File(bookDir(libraryId, bookId), "images")

    // Music clips, by region id - like images, not voice-dependent. See storeAmbience.
    private fun musicDir(libraryId: String, bookId: String): File = File(bookDir(libraryId, bookId), "music")

    private fun bookDir(libraryId: String, bookId: String): File =
        File(context.filesDir, "downloads/${downloadDirName(libraryId)}/$bookId")
}
