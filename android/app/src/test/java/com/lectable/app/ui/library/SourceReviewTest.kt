package com.lectable.app.ui.library

import com.lectable.app.data.remote.dto.SourceReviewEntryDto
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class SourceReviewTest {
    private fun entry(id: String, suspect: Boolean = false) = SourceReviewEntryDto(id = id, title = "Entry $id", suspect = suspect)

    private val entries = listOf(entry("1"), entry("2", suspect = true), entry("3"), entry("4", suspect = true))

    @Test
    fun importsByDefaultUnlessSuspect() {
        assertTrue(reviewImports(entry("1"), emptyMap()))
        assertFalse(reviewImports(entry("2", suspect = true), emptyMap()))
        // A chapter that couldn't be read starts as skip: importing only tries it again.
        assertFalse(reviewImports(SourceReviewEntryDto(id = "9", title = "Chapter 9", unavailable = true), emptyMap()))
        assertEquals(ReviewDecision(import = listOf("1", "3"), skip = listOf("2", "4")), reviewDecision(entries, emptyMap()))
    }

    // The reader's own choice wins either way; a choice for an entry no longer listed is ignored.
    @Test
    fun choicesOverrideDefaults() {
        val choices = mapOf("1" to false, "2" to true, "gone" to true)
        assertEquals(ReviewDecision(import = listOf("2", "3"), skip = listOf("1", "4")), reviewDecision(entries, choices))
    }

    @Test
    fun everyEntryIsDecided() {
        val all = entries.associate { it.id to true }
        assertEquals(ReviewDecision(import = listOf("1", "2", "3", "4"), skip = emptyList()), reviewDecision(entries, all))
        val none = entries.associate { it.id to false }
        assertEquals(ReviewDecision(import = emptyList(), skip = listOf("1", "2", "3", "4")), reviewDecision(entries, none))
        assertEquals(ReviewDecision(import = emptyList(), skip = emptyList()), reviewDecision(emptyList(), all))
    }

    @Test
    fun newEntriesPluralizes() {
        assertEquals("1 new entry", newEntries(1))
        assertEquals("3 new entries", newEntries(3))
    }

    @Test
    fun badgeCountOnlyWhenItFits() {
        assertEquals("1", reviewBadgeCount(1))
        assertEquals("99", reviewBadgeCount(99))
        assertEquals(null, reviewBadgeCount(100))
        assertEquals(null, reviewBadgeCount(0))
    }

    @Test
    fun checkingMessageCountsBooks() {
        assertEquals("Nothing to check", checkingSourcesMessage(0))
        assertEquals("Checking 1 book for new chapters", checkingSourcesMessage(1))
        assertEquals("Checking 12 books for new chapters", checkingSourcesMessage(12))
    }

    @Test
    fun lockedEntriesAreShownNotDecided() {
        val entries = listOf(
            entry("1"),
            SourceReviewEntryDto(id = "2", title = "Chapter 51", source = "Patreon", locked = true),
            SourceReviewEntryDto(id = "3", title = "Chapter 52", source = "Patreon", locked = true),
        )
        assertEquals(listOf("1"), openEntries(entries).map { it.id })
        // Applying a review of what is shown leaves the locked ones waiting, not skipped.
        val decision = reviewDecision(openEntries(entries), mapOf("1" to false))
        assertEquals(listOf("1"), decision.skip)
        assertEquals("2 more at Patreon, locked", lockedEntriesLine(entries))
        assertEquals(null, lockedEntriesLine(listOf(entry("1"))))
        assertEquals(null, lockedEntriesLine(null))
    }
}
