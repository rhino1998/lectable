package com.lectable.app.ui.library

import com.lectable.app.data.remote.dto.ImportChapterSources
import com.lectable.app.data.remote.dto.ImportPreviewChapterDto
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class ImportPreviewTest {
    private fun ch(id: String, source: String = ImportChapterSources.SITE) =
        ImportPreviewChapterDto(id = id, title = "Chapter $id", source = source)

    @Test
    fun defaultSelectionLeavesLockedOut() {
        val chapters = listOf(ch("1"), ch("2", ImportChapterSources.ARCHIVE), ch("3", ImportChapterSources.EPUB), ch("4", ImportChapterSources.LOCKED))
        assertEquals(setOf("1", "2", "3"), defaultSelection(chapters))
        assertEquals(emptySet<String>(), defaultSelection(emptyList()))
    }

    // Widening with the archive puts delisted chapters among the site's: the reader's own
    // ticks stand, the new chapters start ticked.
    @Test
    fun mergeKeepsChoicesAndSelectsNewChapters() {
        val before = listOf(ch("3"), ch("4"), ch("5", ImportChapterSources.LOCKED))
        val after = listOf(
            ch("1", ImportChapterSources.ARCHIVE),
            ch("2", ImportChapterSources.ARCHIVE),
            ch("3"),
            ch("4"),
            ch("5", ImportChapterSources.LOCKED),
            ch("6", ImportChapterSources.LOCKED),
        )
        // "4" was unticked by hand, "5" is locked and was never ticked, "6" is new but locked.
        assertEquals(setOf("1", "2", "3"), mergeSelection(before, setOf("3"), after))
        // A locked chapter ticked by hand stays ticked.
        assertEquals(setOf("1", "2", "5"), mergeSelection(before, setOf("5"), after))
    }

    @Test
    fun mergeSelectsChapterTheEpubUnlocked() {
        val before = listOf(ch("1"), ch("2", ImportChapterSources.LOCKED), ch("3", ImportChapterSources.LOCKED))
        val after = listOf(ch("1"), ch("2", ImportChapterSources.EPUB), ch("3", ImportChapterSources.LOCKED))
        assertEquals(setOf("2"), mergeSelection(before, emptySet(), after))
    }

    @Test
    fun mergeDropsChaptersNoLongerListed() {
        val before = listOf(ch("1"), ch("2"))
        assertEquals(setOf("2"), mergeSelection(before, setOf("1", "2"), listOf(ch("2"))))
    }

    @Test
    fun parseRangeReadsPositions() {
        assertEquals(3..7, parseRange("3", "7", 10))
        assertEquals(3..7, parseRange("7", "3", 10))
        assertEquals(4..4, parseRange(" 4 ", "4", 10))
        // An empty end is that end of the list.
        assertEquals(1..5, parseRange("", "5", 10))
        assertEquals(5..10, parseRange("5", "", 10))
        // Cut to the list.
        assertEquals(8..10, parseRange("8", "500", 10))
        assertEquals(1..2, parseRange("0", "2", 10))
    }

    @Test
    fun parseRangeNullWhenItNamesNothing() {
        assertNull(parseRange("", "", 10))
        assertNull(parseRange("x", "5", 10))
        assertNull(parseRange("2", "1.5", 10))
        assertNull(parseRange("11", "20", 10))
        assertNull(parseRange("1", "5", 0))
    }

    @Test
    fun selectRangeReplacesTheSelection() {
        val chapters = listOf(ch("a"), ch("b", ImportChapterSources.LOCKED), ch("c"), ch("d"))
        assertEquals(setOf("b", "c"), selectRange(chapters, 2..3))
        assertEquals(setOf("a", "b", "c", "d"), selectRange(chapters, 1..4))
    }

    @Test
    fun importChapterIdsNullForEverything() {
        val chapters = listOf(ch("a"), ch("b"), ch("c"))
        assertNull(importChapterIds(chapters, setOf("a", "b", "c")))
        // In list order, whatever order they were ticked in, and ignoring ids not listed.
        assertEquals(listOf("a", "c"), importChapterIds(chapters, linkedSetOf("c", "gone", "a")))
        assertEquals(emptyList<String>(), importChapterIds(chapters, emptySet()))
    }

    @Test
    fun countsBySource() {
        val chapters = listOf(
            ch("1"),
            ch("2"),
            ch("3", ImportChapterSources.ARCHIVE),
            ch("4", ImportChapterSources.EPUB),
            ch("5", ImportChapterSources.LOCKED),
            ch("6", "something-new"),
        )
        assertEquals(PreviewCounts(total = 6, site = 2, archive = 1, epub = 1, locked = 1), previewCounts(chapters))
    }

    @Test
    fun summaryNamesEachSourcePresent() {
        assertEquals(
            "1204 chapters: 900 from Royal Road, 300 from the Internet Archive, 4 locked",
            previewSummary(PreviewCounts(total = 1204, site = 900, archive = 300, locked = 4), "Royal Road"),
        )
        assertEquals(
            "1 chapter: 1 from the epub",
            previewSummary(PreviewCounts(total = 1, epub = 1), "Royal Road"),
        )
        assertEquals("3 chapters: 3 from the site", previewSummary(PreviewCounts(total = 3, site = 3), ""))
        assertEquals("0 chapters", previewSummary(PreviewCounts(), "Royal Road"))
    }
}
