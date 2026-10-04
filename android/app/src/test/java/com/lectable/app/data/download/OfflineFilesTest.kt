package com.lectable.app.data.download

import com.lectable.app.data.remote.dto.AudioStatuses
import com.lectable.app.data.remote.dto.ParagraphDto
import java.io.File
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

class OfflineFilesTest {

    @get:Rule
    val tmp = TemporaryFolder()

    private fun File.names() = listFiles().orEmpty().map { it.name }.sorted()

    private fun stored(idx: Int, pointer: Double = 0.0, audioHash: String = "a$idx") =
        OfflineParagraph(idx = idx, text = "p$idx", durationSeconds = 2.0, audioPointerSeconds = pointer, audioHash = audioHash)

    private fun live(idx: Int, audioHash: String = "a$idx", status: String = AudioStatuses.READY) =
        ParagraphDto(idx = idx, audioStatus = status, durationSeconds = 2.0, audioHash = audioHash)

    @Test
    fun `clip files are named by zero-padded paragraph index`() {
        val dir = tmp.root
        assertEquals("00007.audio", clipFile(dir, 7).name)
        assertEquals("12345.audio", clipFile(dir, 12345).name)
        assertEquals("00007.audio.part", stagedClipFile(dir, 7).name)
        assertEquals(dir, clipFile(dir, 7).parentFile)
    }

    @Test
    fun `committing replaces old clips with staged ones`() {
        val dir = tmp.root
        clipFile(dir, 0).writeText("old 0")
        clipFile(dir, 1).writeText("old 1")
        stagedClipFile(dir, 1).writeText("new 1")
        stagedClipFile(dir, 2).writeText("new 2")

        commitStagedClips(dir, listOf(0, 1, 2))

        assertEquals(listOf("00000.audio", "00001.audio", "00002.audio"), dir.names())
        assertEquals("old 0", clipFile(dir, 0).readText())
        assertEquals("new 1", clipFile(dir, 1).readText())
        assertEquals("new 2", clipFile(dir, 2).readText())
    }

    @Test
    fun `committing drops clips of paragraphs the chapter no longer has`() {
        val dir = tmp.root
        (0..3).forEach { clipFile(dir, it).writeText("clip $it") }
        File(dir, "notes.txt").writeText("not a clip")

        commitStagedClips(dir, listOf(0, 1))

        assertEquals(listOf("00000.audio", "00001.audio", "notes.txt"), dir.names())
    }

    @Test
    fun `a staged clip for a paragraph that has since gone is dropped too`() {
        val dir = tmp.root
        stagedClipFile(dir, 5).writeText("fetched, then the chapter shrank")

        commitStagedClips(dir, listOf(0))

        assertEquals(emptyList<String>(), dir.names())
    }

    @Test
    fun `discarding staged clips leaves the committed copy alone`() {
        val dir = tmp.root
        clipFile(dir, 0).writeText("old 0")
        stagedClipFile(dir, 0).writeText("new 0")
        stagedClipFile(dir, 1).writeText("new 1")

        discardStagedClips(dir)

        assertEquals(listOf("00000.audio"), dir.names())
        assertEquals("old 0", clipFile(dir, 0).readText())
    }

    @Test
    fun `staged clip helpers tolerate a directory that isn't there`() {
        val missing = File(tmp.root, "missing")
        discardStagedClips(missing)
        commitStagedClips(missing, listOf(0))
        assertFalse(missing.exists())
    }

    @Test
    fun `a clip still under its legacy name is found`() {
        val dir = tmp.root
        File(dir, "00003.wav").writeText("legacy 3")
        assertEquals("00003.wav", clipFile(dir, 3).name)
        assertEquals("legacy 3", clipFile(dir, 3).readText())
        // The current name wins once both exist.
        File(dir, "00003.audio").writeText("current 3")
        assertEquals("current 3", clipFile(dir, 3).readText())
        assertEquals(listOf("00003.audio", "00003.wav"), clipFiles(dir).map { it.name }.sorted())
    }

    @Test
    fun `committing over a legacy-named clip leaves one copy`() {
        val dir = tmp.root
        File(dir, "00000.wav").writeText("legacy 0")
        File(dir, "00001.wav").writeText("legacy 1")
        File(dir, "00002.wav").writeText("legacy 2")
        stagedClipFile(dir, 1).writeText("new 1")

        commitStagedClips(dir, listOf(0, 1))

        assertEquals(listOf("00000.wav", "00001.audio"), dir.names())
        assertEquals("new 1", clipFile(dir, 1).readText())
        assertEquals("legacy 0", clipFile(dir, 0).readText())
    }

    @Test
    fun `legacy clips are renamed throughout the downloads tree`() {
        val chapterA = tmp.newFolder("lib", "book", "0", "voice")
        val chapterB = tmp.newFolder("lib", "book", "1", "voice")
        val music = tmp.newFolder("lib", "book", "music")
        File(chapterA, "00000.wav").writeText("a0")
        File(chapterA, "00001.wav").writeText("a1")
        File(chapterA, "00001.wav.part").writeText("staged, left alone")
        File(chapterB, "00000.wav").writeText("stale duplicate")
        File(chapterB, "00000.audio").writeText("b0")
        File(music, "theme.wav").writeText("not a paragraph clip")
        File(music, "region-1").writeText("music")

        assertEquals(2, renameLegacyClips(tmp.root))

        assertEquals(listOf("00000.audio", "00001.audio", "00001.wav.part"), chapterA.names())
        assertEquals("a1", File(chapterA, "00001.audio").readText())
        assertEquals(listOf("00000.audio"), chapterB.names())
        assertEquals("b0", File(chapterB, "00000.audio").readText())
        assertEquals(listOf("region-1", "theme.wav"), music.names())
        // Nothing left to do the second time, or where there are no downloads at all.
        assertEquals(0, renameLegacyClips(tmp.root))
        assertEquals(0, renameLegacyClips(File(tmp.root, "missing")))
    }

    @Test
    fun `a download directory name is short, stable and path-safe`() {
        val voiceKey = "velvet-narrator Speak warmly / slowly. Auto"
        val name = downloadDirName(voiceKey)
        assertEquals(16, name.length)
        assertTrue(name.all { it in "0123456789abcdef" })
        assertEquals(name, downloadDirName(voiceKey))
        assertNotEquals(name, downloadDirName("$voiceKey "))
    }

    @Test
    fun `fetched count only counts clips that are still the paragraph's audio`() {
        val paragraphs = listOf(live(0), live(1, audioHash = "regenerated"), live(2), live(3, status = AudioStatuses.PENDING))
        val fetched = mapOf(0 to stored(0), 1 to stored(1), 3 to stored(3))
        assertEquals(1, fetchedCount(paragraphs, fetched))
        assertEquals(0, fetchedCount(paragraphs, emptyMap()))
    }

    @Test
    fun `cached paragraphs are ready and point at their own clip`() {
        val paragraphs = listOf(stored(0), stored(1)).toCachedParagraphs { "file:///d/$it.wav" }
        assertEquals(listOf("file:///d/0.wav", "file:///d/1.wav"), paragraphs.map { it.audioUrl })
        assertTrue(paragraphs.all { it.audioStatus == AudioStatuses.READY })
        assertEquals(listOf("p0", "p1"), paragraphs.map { it.text })
        assertEquals(listOf(2.0, 2.0), paragraphs.map { it.durationSeconds })
    }

    @Test
    fun `cached merge group members share their anchor's clip`() {
        val paragraphs = listOf(stored(0), stored(1), stored(2, pointer = 1.5), stored(3, pointer = 2.25), stored(4))
            .toCachedParagraphs { "file:///d/$it.wav" }
        assertEquals(
            listOf("file:///d/0.wav", "file:///d/1.wav", "file:///d/1.wav", "file:///d/1.wav", "file:///d/4.wav"),
            paragraphs.map { it.audioUrl },
        )
        assertEquals(listOf(0.0, 0.0, 1.5, 2.25, 0.0), paragraphs.map { it.audioPointerSeconds })
    }

    @Test
    fun `cached paragraphs keep what the reader shows`() {
        val p = OfflineParagraph(
            idx = 0, text = "\"Go,\" she said.", durationSeconds = 1.0, speaker = "Ada", isQuote = true,
            scareQuote = true, emotion = "angry", inline = true, describesCharacters = listOf("Bo"),
        )
        val dto = listOf(p).toCachedParagraphs { "u" }.single()
        assertEquals("Ada", dto.speaker)
        assertEquals("angry", dto.emotion)
        assertTrue(dto.isQuote && dto.scareQuote && dto.inline)
        assertEquals(listOf("Bo"), dto.describesCharacters)
    }
}
