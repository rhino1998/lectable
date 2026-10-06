package com.lectable.app.ui.reader

import com.lectable.app.data.remote.dto.ParagraphDto
import com.lectable.app.data.remote.dto.TableCellDto
import com.lectable.app.data.remote.dto.TableDto
import org.junit.Assert.assertEquals
import org.junit.Test

class TableLayoutTest {
    /** A row whose cells are [text]'s " | "-separated parts, in columns [cols] (default 0, 1, ...). */
    private fun row(idx: Int, tableRow: Int, text: String, cols: List<Pair<Int, Int>>? = null): List<ParagraphDto> {
        var start = 0
        val cells = text.split(" | ").mapIndexed { i, part ->
            val length = part.codePointCount(0, part.length)
            val (col, span) = cols?.get(i) ?: (i to 1)
            TableCellDto(start = start, end = start + length, col = col, colSpan = span, rowSpan = 1).also { start += length + 3 }
        }
        return listOf(ParagraphDto(idx = idx, text = text, table = TableDto(table = 0, row = tableRow, cells = cells)))
    }

    @Test
    fun cellSlicesFollowCodePoints() {
        val p = row(0, 0, "🔥 Fire | 10 mana").single()
        val slices = cellSlices(p.text, p.table!!)
        assertEquals(listOf("🔥 Fire", "10 mana"), slices.map { p.text.substring(it.start, it.end) })
    }

    @Test
    fun rowTokensSkipTheSeparators() {
        val p = row(0, 0, "Summon Water | Water | 10 mana").single()
        val tokens = rowTokens(p.text, cellSlices(p.text, p.table!!))
        assertEquals(listOf("Summon", "Water", "Water", "10", "mana"), tokens.map { it.word })
        assertEquals("mana", p.text.substring(tokens.last().start, tokens.last().end))
    }

    @Test
    fun aCellsParagraphsStackBesideTheCellBeforeThem() {
        val blocks = listOf(
            row(0, 0, "Ring of Healing | Further Description", listOf(0 to 2, 2 to 2)),
            row(1, 1, "Details:", listOf(0 to 2)),
            row(2, 1, "Heals twice per day.", listOf(2 to 2)),
            row(3, 1, "Needs 11 Intelligence.", listOf(2 to 2)),
            row(4, 2, "Rarity: | Magical: | Durability: | Charge:"),
        )
        val lines = tableLines(blocks)
        assertEquals(listOf(true, true, true), lines.map { it.startsRow })
        assertEquals(listOf(1, 2, 1), lines.map { it.chunks.size })
        val details = lines[1].chunks
        assertEquals(listOf(0 to 2, 2 to 4), details.map { it.col to it.end })
        assertEquals(listOf(1, 2), details.map { it.blocks.size })
    }

    @Test
    fun aCellFurtherLeftInTheSameRowStartsALine() {
        val lines = tableLines(listOf(row(0, 0, "Wide", listOf(1 to 2)), row(1, 0, "Back", listOf(0 to 1))))
        assertEquals(listOf(true, false), lines.map { it.startsRow })
    }

    @Test
    fun columnsShareWidthByTheirLongestCell() {
        val weights = columnWeights(
            listOf(
                row(0, 0, "Name | Description"),
                row(1, 1, "Icebolt | Hurls a bolt of ice at a target, freezing it in place for a while"),
            ),
        )
        assertEquals(listOf(7f, 40f), weights)
        assertEquals(47f, weights.span(0, 2))
    }
}
