package com.lectable.app.ui.speakers

import com.lectable.app.data.remote.dto.CharacterNameDto
import com.lectable.app.data.remote.dto.SpeakerDto
import com.lectable.app.ui.reader.pickerSections
import org.junit.Assert.assertEquals
import org.junit.Test

class SpeakerOrderTest {
    private fun sp(name: String, id: String = name.lowercase()) = SpeakerDto(id = id, name = name)

    @Test
    fun rosterPutsNarratorFirstThenAlphabetical() {
        val sorted = sortedRoster(listOf(sp("zed"), sp("Narrator", id = ""), sp("alice"), sp("Bob")))
        assertEquals(listOf("Narrator", "alice", "Bob", "zed"), sorted.map { it.name })
    }

    @Test
    fun pickerSectionsAreAlphabeticalLocalThenAll() {
        val roster = listOf("Zed", "Narrator", "Unknown", "amy", "Bob", "Cal", "Gone")
            .map { CharacterNameDto(id = it.lowercase(), name = it, invalid = it == "Gone") }
        val (local, all) = pickerSections(roster, chapterNames = setOf("Zed", "Bob", "Ghost"), exclude = null)
        assertEquals(listOf("Bob", "Zed"), local)
        assertEquals(listOf("amy", "Bob", "Cal", "Zed"), all)
        val (local2, all2) = pickerSections(roster, chapterNames = setOf("Zed", "Bob"), exclude = "Bob")
        assertEquals(listOf("Zed"), local2)
        assertEquals(listOf("amy", "Cal", "Zed"), all2)
    }
}
