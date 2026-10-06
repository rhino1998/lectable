package com.lectable.app.ui.reader

import com.lectable.app.data.remote.dto.ParagraphDto
import com.lectable.app.data.remote.dto.TableDto

/** One table cell's span of its paragraph's text, as string indexes. */
internal data class CellSlice(val start: Int, val end: Int, val col: Int, val colSpan: Int, val header: Boolean)

/**
 * [table]'s cells as spans of [text]. The backend counts a cell's offsets in code points (see
 * TableCellDto); strings index by UTF-16 unit, which only differs past an astral character (an
 * emoji in a stat block). A cell that doesn't fit the text is clamped rather than thrown on.
 */
internal fun cellSlices(text: String, table: TableDto): List<CellSlice> {
    val codePoints = text.codePointCount(0, text.length)
    fun index(offset: Int) = text.offsetByCodePoints(0, offset.coerceIn(0, codePoints))
    return table.cells.map { c ->
        val start = index(c.start)
        CellSlice(start, index(c.end).coerceAtLeast(start), c.col, c.colSpan.coerceAtLeast(1), c.header)
    }
}

/** The grid columns a table paragraph's cells cover: first column, and one past the last. */
internal fun TableDto.columnRange(): Pair<Int, Int> {
    val first = cells.firstOrNull()?.col ?: 0
    return first to (cells.maxOfOrNull { it.col + it.colSpan.coerceAtLeast(1) } ?: (first + 1))
}

/** One visual block of a table: a paragraph and its Inline continuations (see groupContent). */
internal typealias TableBlock = List<ParagraphDto>

/** Blocks stacked in the same columns - the paragraphs of one cell, or one row of cells. */
internal data class TableChunk(val col: Int, val end: Int, val blocks: List<TableBlock>)

/** Chunks side by side. [startsRow] is false for a line that continues the table row above it. */
internal data class TableLine(val startsRow: Boolean, val chunks: List<TableChunk>)

/**
 * Lays a table's blocks out in lines, in reading order: a block sits beside the one before it
 * when it starts past its columns, joins it when it covers the same ones (a cell's second
 * paragraph), and otherwise starts a new line - always, for a new table row. Mirrors what the
 * web reader leaves to CSS grid placement (ChapterSection's .chapter-table).
 */
internal fun tableLines(blocks: List<TableBlock>): List<TableLine> {
    val lines = mutableListOf<TableLine>()
    var row = -1
    for (block in blocks) {
        val table = block.first().table ?: continue
        val (col, end) = table.columnRange()
        val line = lines.lastOrNull()
        val last = line?.chunks?.lastOrNull()
        when {
            line == null || last == null || table.row != row ->
                lines.add(TableLine(startsRow = true, chunks = listOf(TableChunk(col, end, listOf(block)))))
            last.col == col && last.end == end ->
                lines[lines.lastIndex] = line.copy(chunks = line.chunks.dropLast(1) + last.copy(blocks = last.blocks + listOf(block)))
            col >= last.end ->
                lines[lines.lastIndex] = line.copy(chunks = line.chunks + TableChunk(col, end, listOf(block)))
            else ->
                lines.add(TableLine(startsRow = false, chunks = listOf(TableChunk(col, end, listOf(block)))))
        }
        row = table.row
    }
    return lines
}

// Bounds on how much wider one column gets than another: a description beside a one-word
// column takes most of the row, but never squeezes it to nothing.
private const val MinColumnChars = 6
private const val MaxColumnChars = 40

/**
 * How the table's width is shared between its columns: by the longest text any one-column cell
 * holds, within bounds. Rows are separate composables, so widths can't be measured across them
 * the way a browser's table layout does - fixed shares keep every row's columns lined up.
 */
internal fun columnWeights(blocks: List<TableBlock>): List<Float> {
    val widest = mutableListOf<Int>()
    for (block in blocks) {
        val table = block.first().table ?: continue
        val text = block.first().text
        val slices = cellSlices(text, table)
        val alone = slices.size == 1
        for (slice in slices) {
            while (widest.size < slice.col + slice.colSpan) widest.add(0)
            if (slice.colSpan != 1) continue
            // A paragraph filling a cell by itself may run on over several segments.
            val chars = if (alone) block.sumOf { it.text.length } else slice.end - slice.start
            widest[slice.col] = maxOf(widest[slice.col], chars)
        }
    }
    return widest.map { it.coerceIn(MinColumnChars, MaxColumnChars).toFloat() }
}

/** The share of the table's width columns [from] until [until] take. */
internal fun List<Float>.span(from: Int, until: Int): Float =
    (from until until).sumOf { (getOrNull(it) ?: MinColumnChars.toFloat()).toDouble() }.toFloat().coerceAtLeast(1f)
