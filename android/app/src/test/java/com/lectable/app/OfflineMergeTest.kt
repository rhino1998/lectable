package com.lectable.app

import com.lectable.app.data.download.DownloadStatus
import com.lectable.app.data.download.DownloadedChapter
import com.lectable.app.data.download.OfflineParagraph
import com.lectable.app.data.download.hasAudioOf
import com.lectable.app.data.download.mergedWith
import com.lectable.app.data.remote.dto.AudioStatus
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ContentItemDto
import com.lectable.app.data.remote.dto.ParagraphDto
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class OfflineMergeTest {

    private val json = Json { ignoreUnknownKeys = true }

    private fun live(idx: Int, text: String = "p$idx", audioHash: String = "a$idx", status: AudioStatus = AudioStatus.READY) =
        ParagraphDto(idx = idx, text = text, audioStatus = status, durationSeconds = 2.0, contentHash = "c$text", audioHash = audioHash)

    private fun stored(idx: Int, audioHash: String = "a$idx") =
        OfflineParagraph(idx = idx, text = "p$idx", durationSeconds = 2.0, contentHash = "cp$idx", audioHash = audioHash)

    private fun row(paragraphs: List<OfflineParagraph>, hash: String = "old") = DownloadedChapter(
        libraryId = "lib",
        bookId = "book",
        chapterIdx = 0,
        voiceKey = "v",
        pinned = true,
        status = DownloadStatus.COMPLETE,
        paragraphsJson = json.encodeToString(paragraphs),
        hash = hash,
        localDir = "/tmp/x",
    )

    private fun chapter(vararg paragraphs: ParagraphDto, hash: String = "new") = ChapterDetailDto(
        idx = 0,
        title = "Chapter",
        generating = false,
        paragraphs = paragraphs.toList(),
        content = paragraphs.map { ContentItemDto(kind = "text", paragraphIdx = it.idx) },
        hash = hash,
    )

    private fun DownloadedChapter.paragraphs() = json.decodeFromString<List<OfflineParagraph>>(paragraphsJson)

    @Test
    fun `hasAudioOf compares audio hashes`() {
        assertTrue(stored(0).hasAudioOf(live(0)))
        assertFalse(stored(0).hasAudioOf(live(0, audioHash = "regenerated")))
        assertFalse(stored(0).hasAudioOf(live(0, status = AudioStatus.PENDING)))
    }

    @Test
    fun `hasAudioOf falls back to duration for rows stored before hashes`() {
        val legacy = stored(0, audioHash = "")
        assertTrue(legacy.hasAudioOf(live(0)))
        assertFalse(legacy.hasAudioOf(live(0).copy(durationSeconds = 3.5)))
    }

    @Test
    fun `text-only change is applied and adopts the chapter hash`() {
        val merged = row(listOf(stored(0), stored(1))).mergedWith(chapter(live(0), live(1, text = "edited")), json)
        assertEquals("new", merged.hash)
        assertEquals("edited", merged.paragraphs()[1].text)
        assertEquals("cedited", merged.paragraphs()[1].contentHash)
    }

    @Test
    fun `audio change keeps the old hash and the old audio`() {
        val merged = row(listOf(stored(0), stored(1))).mergedWith(chapter(live(0), live(1, text = "edited", audioHash = "regenerated")), json)
        assertEquals("old", merged.hash)
        // Text still updates (the backend wins) - only the stale clip's record is kept, since the
        // .wav on disk is still the old one until a refresh replaces it.
        assertEquals("edited", merged.paragraphs()[1].text)
        assertEquals("a1", merged.paragraphs()[1].audioHash)
    }

    @Test
    fun `legacy rows adopt server audio hashes when durations match`() {
        val merged = row(listOf(stored(0, audioHash = ""))).mergedWith(chapter(live(0)), json)
        assertEquals("new", merged.hash)
        assertEquals("a0", merged.paragraphs()[0].audioHash)
    }

    @Test
    fun `an added paragraph leaves the chapter out of sync`() {
        val merged = row(listOf(stored(0))).mergedWith(chapter(live(0), live(1)), json)
        assertEquals("old", merged.hash)
    }
}
