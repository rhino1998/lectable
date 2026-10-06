package com.lectable.app.ui.reader

import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.layout.LayoutCoordinates
import androidx.compose.ui.layout.onGloballyPositioned
import androidx.compose.ui.node.Ref
import kotlin.math.abs
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.unit.dp
import com.lectable.app.data.remote.dto.ParagraphDto

/**
 * A table from the book (ParagraphDto.table), as a grid: its blocks laid out in lines
 * (tableLines), every line sharing the same column widths (columnWeights), a rule between table
 * rows. [block] renders one visual block of paragraphs given those column shares - a row of
 * several cells spreads them over its columns itself (ParagraphRow's tableColumns). Mirrors the
 * web reader's .chapter-table.
 */
@Composable
internal fun TableBlockView(
    blocks: List<TableBlock>,
    // Index into [blocks] of the one being read, or -1.
    activeBlock: Int,
    // Where that block's middle sits, as a fraction of the table's height - measured, since rows
    // differ in height - so auto-follow can centre the row rather than the table (see centerItem).
    onActiveRow: (blockIdx: Int, fraction: Float) -> Unit,
    block: @Composable (segments: TableBlock, tableColumns: List<Float>) -> Unit,
) {
    val lines = remember(blocks) { tableLines(blocks) }
    val columns = remember(blocks) { columnWeights(blocks) }
    val blockIndex = remember(blocks) { blocks.withIndex().associate { (i, b) -> b.first().idx to i } }
    val shape = RoundedCornerShape(8.dp)

    val root = remember { Ref<LayoutCoordinates>() }
    val placed = remember(blocks) { mutableMapOf<Int, LayoutCoordinates>() }
    val reported = remember { Ref<Pair<Int, Float>>() }
    val currentActive by rememberUpdatedState(activeBlock)
    val currentOnActiveRow by rememberUpdatedState(onActiveRow)
    val report = {
        val table = root.value?.takeIf { it.isAttached && it.size.height > 0 }
        val row = placed[currentActive]?.takeIf { it.isAttached }
        if (table != null && row != null) {
            val middle = table.localPositionOf(row, Offset.Zero).y + row.size.height / 2f
            val now = currentActive to middle / table.size.height
            val last = reported.value
            if (last == null || last.first != now.first || abs(last.second - now.second) > 0.002f) {
                reported.value = now
                currentOnActiveRow(now.first, now.second)
            }
        }
    }
    // The row changing moves nothing, so no layout callback fires for it.
    LaunchedEffect(activeBlock, blocks) {
        // Playback leaving the table forgets the last row, so coming back to it reports again.
        if (activeBlock < 0) reported.value = null
        report()
    }

    Column(
        modifier = Modifier
            .fillMaxWidth()
            .clip(shape)
            .border(1.dp, MaterialTheme.colorScheme.outlineVariant, shape)
            .onGloballyPositioned { root.value = it; report() },
    ) {
        lines.forEachIndexed { i, line ->
            if (i > 0 && line.startsRow) HorizontalDivider(color = MaterialTheme.colorScheme.outlineVariant)
            Row(modifier = Modifier.fillMaxWidth()) {
                var col = 0
                line.chunks.forEach { chunk ->
                    if (chunk.col > col) Spacer(Modifier.weight(columns.span(col, chunk.col)))
                    Column(Modifier.weight(columns.span(chunk.col, chunk.end))) {
                        chunk.blocks.forEach { b ->
                            val i = blockIndex[b.first().idx] ?: -1
                            Box(Modifier.onGloballyPositioned { placed[i] = it }) { block(b, columns) }
                        }
                    }
                    col = chunk.end
                }
                if (col < columns.size) Spacer(Modifier.weight(columns.span(col, columns.size)))
            }
        }
    }
}
