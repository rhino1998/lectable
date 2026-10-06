package com.lectable.app.data.repository

import android.content.Context
import com.lectable.app.data.download.DownloadStatus
import com.lectable.app.data.download.DownloadedBook
import com.lectable.app.data.download.DownloadedBookDao
import com.lectable.app.data.download.DownloadedChapter
import com.lectable.app.data.download.DownloadedChapterDao
import com.lectable.app.data.download.OfflineParagraph
import com.lectable.app.data.live.LiveStore
import com.lectable.app.data.remote.dto.ChapterSummaryDto
import com.lectable.app.data.remote.BackendIdentityRepository
import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.LiveError
import com.lectable.app.data.remote.LiveResult
import com.lectable.app.data.remote.MediaApi
import com.lectable.app.data.remote.dto.AudioStatus
import com.lectable.app.data.remote.dto.AudioStatuses
import com.lectable.app.data.remote.dto.BookDetailDto
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ContentItemDto
import com.lectable.app.data.remote.dto.ParagraphDto
import io.mockk.coEvery
import io.mockk.coVerify
import io.mockk.every
import io.mockk.mockk
import java.io.File
import java.io.IOException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.async
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.runBlocking
import kotlinx.coroutines.withTimeout
import kotlinx.serialization.json.Json
import okhttp3.ResponseBody
import okhttp3.ResponseBody.Companion.toResponseBody
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

private const val LIB = "lib"
private const val BOOK = "book"
private const val VOICE = "voice-a"

/** [DownloadRepository.downloadChapter] end to end, against in-memory DAOs, a fake media
 *  endpoint and a real directory. */
class DownloadChapterTest {

    @get:Rule
    val tmp = TemporaryFolder()

    private val json = Json { ignoreUnknownKeys = true }

    private class FakeChapterDao : DownloadedChapterDao {
        val rows = MutableStateFlow<List<DownloadedChapter>>(emptyList())
        private fun DownloadedChapter.isAt(libraryId: String, bookId: String, chapterIdx: Int) =
            this.libraryId == libraryId && this.bookId == bookId && this.chapterIdx == chapterIdx

        override fun observeComplete(): Flow<List<DownloadedChapter>> = rows.map { all -> all.filter { it.status == DownloadStatus.COMPLETE } }
        override suspend fun forBook(libraryId: String, bookId: String) = rows.value.filter { it.libraryId == libraryId && it.bookId == bookId }
        override fun observeForBook(libraryId: String, bookId: String): Flow<List<DownloadedChapter>> =
            rows.map { all -> all.filter { it.libraryId == libraryId && it.bookId == bookId } }
        override suspend fun get(libraryId: String, bookId: String, chapterIdx: Int) = rows.value.firstOrNull { it.isAt(libraryId, bookId, chapterIdx) }
        override suspend fun upsert(entity: DownloadedChapter) =
            rows.update { all -> all.filterNot { it.isAt(entity.libraryId, entity.bookId, entity.chapterIdx) } + entity }
        override suspend fun setMusicJson(libraryId: String, bookId: String, chapterIdx: Int, musicJson: String) =
            rows.update { all -> all.map { if (it.isAt(libraryId, bookId, chapterIdx)) it.copy(musicJson = musicJson) else it } }
        override suspend fun delete(libraryId: String, bookId: String, chapterIdx: Int) =
            rows.update { all -> all.filterNot { it.isAt(libraryId, bookId, chapterIdx) } }
        override suspend fun deleteForBook(libraryId: String, bookId: String) =
            rows.update { all -> all.filterNot { it.libraryId == libraryId && it.bookId == bookId } }
    }

    private class FakeBookDao : DownloadedBookDao {
        val rows = MutableStateFlow<List<DownloadedBook>>(emptyList())
        override fun observeAll(): Flow<List<DownloadedBook>> = rows
        override suspend fun get(libraryId: String, bookId: String) = rows.value.firstOrNull { it.libraryId == libraryId && it.bookId == bookId }
        override suspend fun bookIdsForLibrary(libraryId: String) = rows.value.filter { it.libraryId == libraryId }.map { it.bookId }
        override suspend fun upsert(entity: DownloadedBook) =
            rows.update { all -> all.filterNot { it.libraryId == entity.libraryId && it.bookId == entity.bookId } + entity }
        override suspend fun delete(libraryId: String, bookId: String) =
            rows.update { all -> all.filterNot { it.libraryId == libraryId && it.bookId == bookId } }
    }

    /** Serves each clip URL's bytes, counting fetches; a URL in [failing] drops the connection. */
    private class FakeMediaApi : MediaApi {
        val clips = mutableMapOf<String, String>()
        val failing = mutableSetOf<String>()
        val fetches = mutableListOf<String>()
        override suspend fun downloadFile(url: String): ResponseBody {
            synchronized(fetches) { fetches += url }
            if (url in failing) throw IOException("connection reset fetching $url")
            return (clips[url] ?: throw IOException("404 $url")).toResponseBody()
        }
    }

    private val dao = FakeChapterDao()
    private val bookDao = FakeBookDao()
    private val media = FakeMediaApi()
    private val live = MutableStateFlow(LiveResult<ChapterDetailDto>())
    private val chapterTopics = java.util.concurrent.ConcurrentHashMap<Int, MutableStateFlow<LiveResult<ChapterDetailDto>>>().apply { put(0, live) }

    /** The book topic: five chapters, none generated ([generated] marks some as fully ready). */
    private val liveBook = MutableStateFlow(LiveResult<BookDetailDto>())
    private fun generated(vararg idx: Int) {
        liveBook.value = LiveResult(
            data = BookDetailDto(
                id = BOOK,
                chapters = (0 until 5).map { ChapterSummaryDto(idx = it, paragraphCount = 4, readyCount = if (it in idx) 4 else 1) },
            ),
            loading = false,
        )
    }
    private val api = mockk<LectableApi>(relaxed = true) {
        coEvery { getBook(BOOK) } returns BookDetailDto(id = BOOK, title = "The Book", author = "A. Writer", chapterCount = 3)
    }

    init {
        generated()
    }

    private val repo by lazy {
        DownloadRepository(
            context = mockk<Context> { every { filesDir } returns tmp.root },
            api = api,
            mediaApi = media,
            dao = dao,
            bookDao = bookDao,
            positionDao = mockk(relaxed = true),
            backendIdentity = mockk<BackendIdentityRepository> {
                every { libraryId } returns MutableStateFlow(LIB)
                coEvery { currentLibraryId() } returns LIB
            },
            json = json,
            liveStore = mockk<LiveStore> {
                every { chapter(BOOK, any()) } answers { chapterTopics.getOrPut(secondArg()) { MutableStateFlow(LiveResult()) } }
                every { book(BOOK) } returns liveBook
            },
            lightingSettings = mockk(relaxed = true),
        )
    }

    /** A paragraph whose clip is version [v]: a new version is new audio at a new URL. */
    private fun para(idx: Int, v: Int = 1, status: AudioStatus = AudioStatuses.READY, text: String = "p$idx"): ParagraphDto {
        val ready = status == AudioStatuses.READY
        val url = "/audio/$idx?v=$v"
        if (ready) media.clips[url] = "clip $idx v$v"
        return ParagraphDto(
            idx = idx, text = text, audioStatus = status, audioUrl = url.takeIf { ready },
            durationSeconds = if (ready) 2.0 else null, contentHash = "c$idx$text", audioHash = if (ready) "a$idx-$v" else "",
        )
    }

    private fun chapter(vararg paragraphs: ParagraphDto, hash: String = "h1") = ChapterDetailDto(
        idx = 0,
        title = "Chapter One",
        paragraphs = paragraphs.toList(),
        content = paragraphs.map { ContentItemDto(kind = "text", paragraphIdx = it.idx) },
        hash = hash,
    )

    private fun serve(chapter: ChapterDetailDto) {
        live.value = LiveResult(data = chapter, loading = false)
    }

    private fun download(voiceKey: String = VOICE, onProgress: (Int, Int) -> Unit = { _, _ -> }) =
        runBlocking { withTimeout(10_000) { repo.downloadChapter(BOOK, 0, voiceKey, pinned = true, onProgress = onProgress) } }

    private fun row(): DownloadedChapter? = dao.rows.value.singleOrNull()

    private fun DownloadedChapter.files() = File(localDir).listFiles().orEmpty().associate { it.name to it.readText() }.toSortedMap()

    private fun DownloadedChapter.paragraphs() = json.decodeFromString<List<OfflineParagraph>>(paragraphsJson)

    private suspend fun awaitRow(what: String, predicate: (DownloadedChapter?) -> Boolean) {
        try {
            withTimeout(5_000) { while (!predicate(row())) delay(10) }
        } catch (e: Exception) {
            fail("timed out waiting for $what; row = ${row()}")
        }
    }

    // A whole-book download starts every chapter at once; each open chapter topic is the whole
    // chapter held in memory, so only the first two still waiting on generation, and ones
    // already generated, may open theirs.
    @Test
    fun `chapters waiting on generation do not all open their chapter topic`() = runBlocking {
        val jobs = (0 until 5).map { idx -> async(Dispatchers.IO) { repo.downloadChapter(BOOK, idx, VOICE, pinned = true) } }
        fun open() = (0 until 5).filter { (chapterTopics[it]?.subscriptionCount?.value ?: 0) > 0 }
        suspend fun awaitOpen(want: List<Int>) = try {
            withTimeout(5_000) { while (open() != want) delay(10) }
        } catch (e: Exception) {
            fail("open chapter topics ${open()}, want $want")
        }
        awaitOpen(listOf(0, 1))
        delay(200)
        assertEquals(listOf(0, 1), open())

        generated(4)
        awaitOpen(listOf(0, 1, 4))
        jobs.forEach { it.cancel() }
    }

    @Test
    fun `a first download fetches every clip and records the chapter`() {
        serve(chapter(para(0), para(1), para(2)))
        val progress = mutableListOf<Pair<Int, Int>>()

        download { ready, total -> progress += ready to total }

        val row = row()!!
        assertEquals(DownloadStatus.COMPLETE, row.status)
        assertEquals(mapOf("00000.audio" to "clip 0 v1", "00001.audio" to "clip 1 v1", "00002.audio" to "clip 2 v1"), row.files())
        assertEquals(3 to 3, row.readyParagraphs to row.totalParagraphs)
        assertEquals("h1", row.hash)
        assertEquals("Chapter One", row.title)
        assertEquals(VOICE, row.voiceKey)
        assertTrue(row.pinned)
        assertEquals(listOf("a0-1", "a1-1", "a2-1"), row.paragraphs().map { it.audioHash })
        assertEquals(listOf("p0", "p1", "p2"), row.paragraphs().map { it.text })
        assertEquals("clip 0 v1".length * 3L, row.totalBytes)
        assertEquals(listOf(1 to 3, 2 to 3, 3 to 3), progress)
        coVerify(exactly = 1) { api.generateChapter(BOOK, 0) }
    }

    @Test
    fun `the first download of a book caches its metadata`() {
        serve(chapter(para(0)))
        download()
        val book = bookDao.rows.value.single()
        assertEquals("The Book", book.title)
        assertEquals("A. Writer", book.author)
        assertEquals(3, book.chapterCount)

        serve(chapter(para(0, v = 2), hash = "h2"))
        download()
        coVerify(exactly = 1) { api.getBook(BOOK) }
    }

    @Test
    fun `a book whose metadata can't be fetched still downloads`() {
        coEvery { api.getBook(BOOK) } throws IOException("offline")
        serve(chapter(para(0)))
        download()
        assertEquals(DownloadStatus.COMPLETE, row()!!.status)
        assertTrue(bookDao.rows.value.isEmpty())
    }

    @Test
    fun `a download waits for paragraphs still generating`() = runBlocking {
        serve(chapter(para(0), para(1, status = AudioStatuses.GENERATING)))
        val job = async(Dispatchers.Default) { repo.downloadChapter(BOOK, 0, VOICE, pinned = false) }

        awaitRow("the first clip") { it?.readyParagraphs == 1 }
        assertEquals(DownloadStatus.DOWNLOADING, row()!!.status)
        assertEquals(2, row()!!.totalParagraphs)
        assertFalse(job.isCompleted)

        serve(chapter(para(0), para(1), hash = "h2"))
        withTimeout(10_000) { job.await() }

        assertEquals(DownloadStatus.COMPLETE, row()!!.status)
        assertEquals("h2", row()!!.hash)
        assertEquals(listOf("/audio/0?v=1", "/audio/1?v=1"), media.fetches)
    }

    @Test
    fun `a failed paragraph is asked for again once, not given up on`() = runBlocking {
        serve(chapter(para(0), para(1, status = AudioStatuses.ERROR)))
        val job = async(Dispatchers.Default) { repo.downloadChapter(BOOK, 0, VOICE, pinned = false) }
        awaitRow("the first clip") { it?.readyParagraphs == 1 }

        // Still failed on the next push, then regenerating, then done.
        serve(chapter(para(0), para(1, status = AudioStatuses.ERROR), hash = "h1b"))
        serve(chapter(para(0), para(1, status = AudioStatuses.GENERATING), hash = "h1c"))
        serve(chapter(para(0), para(1, v = 2), hash = "h2"))
        withTimeout(10_000) { job.await() }

        assertEquals(DownloadStatus.COMPLETE, row()!!.status)
        assertEquals("clip 1 v2", row()!!.files()["00001.audio"])
        coVerify(exactly = 1) { api.lookahead(BOOK, match { it.chapterIdx == 0 && it.paragraphIdx == 1 }) }
    }

    @Test
    fun `a merge group's shared clip is fetched once`() {
        val anchor = para(1)
        val member = para(2).copy(audioUrl = anchor.audioUrl, audioPointerSeconds = 1.25, audioHash = "a2-merged")
        serve(chapter(para(0), anchor, member))

        download()

        assertEquals(listOf("/audio/0?v=1", "/audio/1?v=1"), media.fetches)
        val row = row()!!
        assertEquals("clip 1 v1", row.files()["00001.audio"])
        assertEquals("clip 1 v1", row.files()["00002.audio"])
        assertEquals(listOf(0.0, 0.0, 1.25), row.paragraphs().map { it.audioPointerSeconds })
    }

    @Test
    fun `a refresh fetches only the clips that changed`() {
        serve(chapter(para(0), para(1), para(2)))
        download()
        media.fetches.clear()

        serve(chapter(para(0), para(1, v = 2, text = "edited"), para(2), hash = "h2"))
        download()

        assertEquals(listOf("/audio/1?v=2"), media.fetches)
        val row = row()!!
        assertEquals(mapOf("00000.audio" to "clip 0 v1", "00001.audio" to "clip 1 v2", "00002.audio" to "clip 2 v1"), row.files())
        assertEquals("h2", row.hash)
        assertEquals(listOf("a0-1", "a1-2", "a2-1"), row.paragraphs().map { it.audioHash })
        assertEquals("edited", row.paragraphs()[1].text)
    }

    @Test
    fun `a refresh drops clips of paragraphs the chapter lost`() {
        serve(chapter(para(0), para(1), para(2)))
        download()

        serve(chapter(para(0), para(1), hash = "h2"))
        download()

        val row = row()!!
        assertEquals(setOf("00000.audio", "00001.audio"), row.files().keys)
        assertEquals(2, row.totalParagraphs)
    }

    @Test
    fun `the old copy stays whole while a refresh is under way`() = runBlocking {
        serve(chapter(para(0), para(1)))
        download()
        val before = row()!!

        // Paragraph 0 has new audio already; paragraph 1 is being regenerated.
        serve(chapter(para(0, v = 2), para(1, status = AudioStatuses.GENERATING), hash = "h2"))
        val job = async(Dispatchers.Default) { repo.downloadChapter(BOOK, 0, VOICE, pinned = true) }
        withTimeout(5_000) { while ("/audio/0?v=2" !in synchronized(media.fetches) { media.fetches.toList() }) delay(10) }
        withTimeout(5_000) { while (!File(before.localDir, "00000.audio.part").exists()) delay(10) }

        assertEquals(before, row())
        assertEquals("clip 0 v1", File(before.localDir, "00000.audio").readText())
        assertEquals("clip 1 v1", File(before.localDir, "00001.audio").readText())

        serve(chapter(para(0, v = 2), para(1, v = 2), hash = "h3"))
        withTimeout(10_000) { job.await() }
        assertEquals(mapOf("00000.audio" to "clip 0 v2", "00001.audio" to "clip 1 v2"), row()!!.files())
        assertEquals("h3", row()!!.hash)
    }

    @Test
    fun `a refresh that fails leaves the old copy as it was`() {
        serve(chapter(para(0), para(1)))
        download()
        val before = row()!!

        serve(chapter(para(0, v = 2), para(1, v = 2), hash = "h2"))
        media.failing += "/audio/1?v=2"
        try {
            download()
            fail("the refresh should have thrown")
        } catch (e: IOException) {
            // expected
        }

        assertEquals(before, row())
        assertEquals(mapOf("00000.audio" to "clip 0 v1", "00001.audio" to "clip 1 v1"), before.files())
    }

    @Test
    fun `a first download that fails is marked as an error`() {
        serve(chapter(para(0), para(1)))
        media.failing += "/audio/1?v=1"
        try {
            download()
            fail("the download should have thrown")
        } catch (e: IOException) {
            // expected
        }

        val row = row()!!
        assertEquals(DownloadStatus.ERROR, row.status)
        assertTrue("staged clips left behind: ${row.files().keys}", row.files().keys.none { it.endsWith(".part") })
    }

    @Test
    fun `a chapter the backend can't serve fails the download`() {
        live.value = LiveResult(error = LiveError(LiveError.NETWORK, "unreachable"), loading = false)
        try {
            download()
            fail("the download should have thrown")
        } catch (e: IOException) {
            assertEquals("unreachable", e.message)
        }
        assertEquals(DownloadStatus.ERROR, row()!!.status)
    }

    @Test
    fun `a new voice downloads into its own directory and then replaces the old one`() {
        serve(chapter(para(0), para(1)))
        download()
        val old = row()!!
        media.fetches.clear()

        serve(chapter(para(0, v = 2), para(1, v = 2), hash = "h2"))
        download(voiceKey = "voice-b")

        val row = row()!!
        assertEquals("voice-b", row.voiceKey)
        assertNotEquals(old.localDir, row.localDir)
        assertFalse(File(old.localDir).exists())
        assertEquals(mapOf("00000.audio" to "clip 0 v2", "00001.audio" to "clip 1 v2"), row.files())
        assertEquals(2, media.fetches.size)
    }

    @Test
    fun `a new voice that fails to download leaves the old voice's copy`() {
        serve(chapter(para(0), para(1)))
        download()
        val old = row()!!

        serve(chapter(para(0, v = 2), para(1, v = 2), hash = "h2"))
        media.failing += "/audio/1?v=2"
        try {
            download(voiceKey = "voice-b")
            fail("the download should have thrown")
        } catch (e: IOException) {
            // expected
        }

        assertEquals(old, row())
        assertEquals(mapOf("00000.audio" to "clip 0 v1", "00001.audio" to "clip 1 v1"), old.files())
        assertEquals(listOf(File(old.localDir).name), File(old.localDir).parentFile!!.list()!!.toList())
    }

    @Test
    fun `a downloaded clip is used only while it is still the paragraph's audio`() = runBlocking {
        val chapter = chapter(para(0), para(1))
        serve(chapter)
        download()

        // The lookup reads a flow of the DAO's rows, which lands a moment later.
        withTimeout(5_000) { while (repo.localAudioFile(BOOK, 0, VOICE, chapter.paragraphs[0]) == null) delay(10) }
        val file = repo.localAudioFile(BOOK, 0, VOICE, chapter.paragraphs[1])
        assertNotNull(file)
        assertEquals("clip 1 v1", file!!.readText())

        assertNull(repo.localAudioFile(BOOK, 0, VOICE, para(1, v = 2)))
        assertNull(repo.localAudioFile(BOOK, 0, "voice-b", chapter.paragraphs[1]))
        assertNull(repo.localAudioFile(BOOK, 1, VOICE, chapter.paragraphs[1]))
        assertNull(repo.localAudioFile("other-book", 0, VOICE, chapter.paragraphs[1]))
    }

    @Test
    fun `a refresh of a copy with legacy-named clips reuses them`() {
        serve(chapter(para(0), para(1)))
        download()
        val dir = File(row()!!.localDir)
        // As an older version of the app left it.
        dir.listFiles()!!.forEach { it.renameTo(File(dir, it.name.replace(".audio", ".wav"))) }
        media.fetches.clear()

        serve(chapter(para(0), para(1, v = 2), hash = "h2"))
        download()

        assertEquals(listOf("/audio/1?v=2"), media.fetches)
        assertEquals(mapOf("00000.wav" to "clip 0 v1", "00001.audio" to "clip 1 v2"), row()!!.files())
        assertEquals("clip 0 v1".length * 2L, row()!!.totalBytes)
    }

    @Test
    fun `clips downloaded under the legacy name are renamed at startup`() = runBlocking {
        val dir = tmp.newFolder("downloads", "abc", BOOK, "0", "voice")
        File(dir, "00000.wav").writeText("legacy")

        repo // constructing it starts the rename

        withTimeout(5_000) { while (!File(dir, "00000.audio").exists()) delay(10) }
        assertFalse(File(dir, "00000.wav").exists())
        assertEquals("legacy", File(dir, "00000.audio").readText())
    }

    @Test
    fun `deleting a download removes its files and its row`() = runBlocking {
        serve(chapter(para(0)))
        download()
        val dir = File(row()!!.localDir)

        repo.deleteDownload(BOOK, 0)

        assertFalse(dir.exists())
        assertNull(row())
    }
}
