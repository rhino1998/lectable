package com.lectable.app.ui.reader

import androidx.compose.foundation.border
import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Mood
import androidx.compose.material.icons.filled.Bookmark
import androidx.compose.material.icons.filled.BookmarkBorder
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.FormatQuote
import androidx.compose.material.icons.filled.Mood
import androidx.compose.material.icons.filled.Person
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.AssistChip
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.TextButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Rect
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.PathEffect
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.StrokeJoin
import androidx.compose.ui.graphics.drawscope.DrawScope
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.lerp
import androidx.compose.ui.graphics.luminance
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.text.TextLayoutResult
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.text.InlineTextContent
import androidx.compose.foundation.text.appendInlineContent
import androidx.compose.ui.text.Placeholder
import androidx.compose.ui.text.PlaceholderVerticalAlign
import androidx.compose.ui.unit.em
import androidx.compose.ui.unit.dp
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.Row
import androidx.compose.ui.unit.sp
import com.lectable.app.data.remote.dto.AudioStatuses
import com.lectable.app.data.remote.dto.ApiCatalog
import com.lectable.app.data.remote.dto.ParagraphDto
import kotlinx.coroutines.isActive

@Composable
internal fun ParagraphRow(
    segments: List<ParagraphDto>,
    // Index into [segments] of the one currently targeted by playback, or -1 if none of them
    // are (this whole visual block isn't the active one).
    activeSegmentIdx: Int,
    isBookmarked: Boolean,
    playbackSeconds: Double,
    fontSizeSp: Int,
    fontFamily: FontFamily,
    annotationsMode: Boolean,
    onWordTap: (paragraphIdx: Int, startSeconds: Double) -> Unit,
    onRegenerate: () -> Unit,
    onToggleBookmark: () -> Unit,
    // Only ever isQuote segments - see SpeakerPickerTarget's doc comment.
    onOpenSpeakerPicker: (paragraphIndices: List<Int>, currentSpeaker: String) -> Unit,
    onToggleScareQuote: (paragraphIndices: List<Int>, scareQuote: Boolean) -> Unit,
    onSetEmotion: (paragraphIndices: List<Int>, emotion: String) -> Unit,
    // See ParagraphBodyText.
    onActiveLine: (fraction: Float) -> Unit,
    // Set for a paragraph inside a table from the book (see TableBlock in ReaderTable.kt): the
    // table's column shares, for laying a row's cells out in their columns.
    tableColumns: List<Float>? = null,
    // Whether the book's clone model takes direction as text (Fish Drama 3) - then a dialogue
    // line's long-press menu offers "Set direction" (DirectionDialog).
    directed: Boolean = false,
    onSetDirection: (paragraphIdx: Int, direction: String) -> Unit = { _, _ -> },
) {
    val isActive = activeSegmentIdx >= 0
    // A table row of several cells (ParagraphDto.table): each cell's text goes in its own
    // column rather than showing the " | " the row's text joins them with.
    val rowCells = remember(segments, tableColumns) {
        val table = segments.singleOrNull()?.table
        if (tableColumns == null || table == null || table.cells.size < 2) null else cellSlices(segments[0].text, table)
    }
    val soleCellHeader = tableColumns != null && rowCells == null && segments[0].table?.cells?.firstOrNull()?.header == true
    val background = if (isActive) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.surface
    var showMenu by remember { mutableStateOf(false) }
    val clipboardManager = LocalClipboardManager.current
    val isDarkTheme = MaterialTheme.colorScheme.background.luminance() < 0.5f
    val speakerMarkColor = if (isDarkTheme) AnnotationSpeakerColorDark else AnnotationSpeakerColorLight
    val scareQuoteMarkColor = if (isDarkTheme) ScareQuoteMarkColorDark else ScareQuoteMarkColorLight
    // Every distinct non-empty Speaker among this block's segments, "Narrator" included -
    // present whenever attribution has run for this paragraph, not gated on book.multiVoice
    // (it's informational either way, same as frontend's ReaderPage.tsx paragraph-speaker-hint).
    // Shown as a static entry in the long-press menu rather than always inline under the text,
    // since it's secondary information the reader wants on demand, not something that should
    // compete with the narration text itself for every paragraph in a multivoice book.
    val speakers = remember(segments) { segments.mapNotNull { it.speaker }.filter { it.isNotEmpty() }.distinct() }
    // Every distinct emotion among this block's dialogue segments (backend internal/emotions
    // ids) - mirrors frontend ChapterSection's emotion badge. Same "long-press menu, not
    // inline" placement as [speakers].
    val emotions = remember(segments) {
        segments.mapNotNull { it.emotion }.filter { it.isNotEmpty() }.distinct()
    }
    // Every distinct inline Higgs delivery tag among this block's segments (today only pauses),
    // each with its own formatted label and category color - mirrors frontend's
    // formatDirectionTag/directionTagCategory. Same "long-press menu, not inline" placement.
    val directionTags = remember(segments) {
        segments.flatMap { it.directionMarks }.map { it.tag }.distinct()
    }
    // Every resolved pronunciation substitution among this block's segments - same "long-press
    // menu, not inline" placement as directionTags above, mirroring frontend's own reliance on
    // the inline caret's hover title for this (no separate legend/list on the web side either).
    val pronunciationMarks = remember(segments) { segments.flatMap { it.pronunciationMarks } }
    // "Set speaker"/"Mark as scare quote" only make sense when this block actually has quoted
    // dialogue in it - the backend rejects attributing narration to a character (see
    // SpeakerPickerTarget's doc comment) and rejects scare-quoting a non-quoted paragraph the
    // same way, so a pure-narration block just doesn't offer either, same as how
    // [speakers]/[directionTags] above only show when they apply.
    val quoteSegments = remember(segments) { segments.filter { it.isQuote } }
    val hasQuoteSegment = quoteSegments.isNotEmpty()
    // The segment the menu's long press landed on (ParagraphDto.idx), or null when it missed the
    // text's layout.
    var pressedIdx by remember { mutableStateOf<Int?>(null) }
    // What "Set speaker", "Mark/unmark as scare quote" and "Set emotion" act on: the quote that
    // was long-pressed, so a block with several quotes (two speakers, or real dialogue beside a
    // scare quote) can have each set on its own (the web reader's right-click menu is per quote
    // too) - or, when the press landed on narration, every quote in the block.
    val pressedQuote = quoteSegments.firstOrNull { it.idx == pressedIdx }
    val quoteTargets = pressedQuote?.let(::listOf) ?: quoteSegments
    // Shown under each of those items when the block has other quotes the action leaves alone.
    val namedQuote = pressedQuote?.takeIf { quoteSegments.size > 1 }
    val forAll = if (quoteSegments.size > 1 && pressedQuote == null) " for all" else ""
    // The toggle's current value - true only when every target is already marked; toggling sets
    // them all to the opposite (see ReaderViewModel.setScareQuote).
    val allScareQuote = quoteTargets.isNotEmpty() && quoteTargets.all { it.scareQuote }
    // Whether to show the informational "Scare quote" row (any, not all - unlike allScareQuote,
    // which is the toggle's own target) - mirrors frontend's annotationTitle appending "· Scare
    // quote" to the hover tooltip.
    val hasScareQuote = remember(quoteSegments) { quoteSegments.any { it.scareQuote } }
    // "Set emotion" targets - only real dialogue carries an emotion (backend
    // store.Paragraph.EffectiveEmotion), so a scare quote is left out. [currentEmotion] is the
    // first dialogue segment's ("" = neutral), checked in the emotion menu.
    val emotionSegments = quoteTargets.filter { !it.scareQuote }
    val currentEmotion = emotionSegments.firstOrNull()?.emotion.orEmpty()
    var showEmotionMenu by remember { mutableStateOf(false) }
    // "Set direction" acts on one line: the long-pressed one, or the block's only dialogue.
    val directionTarget = if (directed) emotionSegments.singleOrNull() else null
    var showDirectionDialog by remember { mutableStateOf(false) }

    Column(
        modifier = if (tableColumns != null) {
            // The table draws the frame; a row only tints itself while it plays.
            Modifier.fillMaxWidth().background(if (isActive) background else Color.Transparent).padding(horizontal = 8.dp, vertical = 6.dp)
        } else {
            Modifier
                .fillMaxWidth()
                .clip(RoundedCornerShape(8.dp))
                .background(background)
                .padding(12.dp)
        },
    ) {
        Box {
            val bodyActiveSegmentIdx = if (isActive && segments[activeSegmentIdx].audioStatus == AudioStatuses.READY) activeSegmentIdx else -1
            if (rowCells != null && tableColumns != null) {
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    var col = rowCells.first().col
                    rowCells.forEachIndexed { i, cell ->
                        if (cell.col > col) Spacer(Modifier.weight(tableColumns.span(col, cell.col)))
                        Box(Modifier.weight(tableColumns.span(cell.col, cell.col + cell.colSpan))) {
                            ParagraphBodyText(
                                segments = segments,
                                activeSegmentIdx = bodyActiveSegmentIdx,
                                playbackSeconds = playbackSeconds,
                                fontSizeSp = fontSizeSp,
                                fontFamily = fontFamily,
                                annotationsMode = annotationsMode,
                                onWordTap = onWordTap,
                                onLongPress = { paragraphIdx -> pressedIdx = paragraphIdx; showMenu = true },
                                onActiveLine = onActiveLine,
                                cell = CellView(rowCells, i),
                            )
                        }
                        col = cell.col + cell.colSpan
                    }
                }
            } else ParagraphBodyText(
                segments = segments,
                bold = soleCellHeader,
                // The pill background above still lights up for a *targeted* paragraph even before
                // it's generated (see ParagraphPlayer.playParagraph's not-ready branch), so the tap
                // visibly registered - but word-level highlighting needs real elapsed playback time
                // to mean anything, which a paragraph with no audio yet doesn't have.
                activeSegmentIdx = bodyActiveSegmentIdx,
                playbackSeconds = playbackSeconds,
                fontSizeSp = fontSizeSp,
                fontFamily = fontFamily,
                annotationsMode = annotationsMode,
                onWordTap = onWordTap,
                onLongPress = { paragraphIdx -> pressedIdx = paragraphIdx; showMenu = true },
                onActiveLine = onActiveLine,
            )
            DropdownMenu(expanded = showMenu, onDismissRequest = { showMenu = false }) {
                if (speakers.isNotEmpty()) {
                    DropdownMenuItem(
                        text = { Text("Speaker: ${speakers.joinToString(", ")}") },
                        leadingIcon = { AnnotationSwatch(speakerMarkColor) },
                        enabled = false,
                        onClick = {},
                    )
                }
                if (emotions.isNotEmpty()) {
                    DropdownMenuItem(
                        text = { Text("Emotion: ${emotions.joinToString(", ") { formatEmotion(it) }}") },
                        leadingIcon = {
                            // A lone emotion gets its own icon; several share the generic face.
                            Icon(emotions.singleOrNull()?.let(::emotionIcon) ?: Icons.Outlined.Mood, contentDescription = null, tint = emotionColor(isDarkTheme))
                        },
                        enabled = false,
                        onClick = {},
                    )
                }
                val otherDirectionColor = MaterialTheme.colorScheme.onSurfaceVariant
                directionTags.forEach { tag ->
                    DropdownMenuItem(
                        text = { Text(formatDirectionTag(tag)) },
                        leadingIcon = { AnnotationCaretIcon(directionTagColor(tag, isDarkTheme, otherDirectionColor)) },
                        enabled = false,
                        onClick = {},
                    )
                }
                pronunciationMarks.forEach { mark ->
                    DropdownMenuItem(
                        text = { Text(formatPronunciationMark(mark)) },
                        leadingIcon = { PronunciationStrikeIcon(if (isDarkTheme) PronunciationCaretColorDark else PronunciationCaretColorLight) },
                        enabled = false,
                        onClick = {},
                    )
                }
                if (hasScareQuote) {
                    DropdownMenuItem(
                        text = { Text("Scare quote") },
                        leadingIcon = { AnnotationSwatch(scareQuoteMarkColor) },
                        enabled = false,
                        onClick = {},
                    )
                }
                if (speakers.isNotEmpty() || directionTags.isNotEmpty() || pronunciationMarks.isNotEmpty() || hasScareQuote) {
                    HorizontalDivider()
                }
                DropdownMenuItem(
                    text = { Text("Regenerate paragraph") },
                    leadingIcon = { Icon(Icons.Filled.Refresh, contentDescription = null) },
                    onClick = { onRegenerate(); showMenu = false },
                )
                if (hasQuoteSegment) {
                    DropdownMenuItem(
                        text = { QuoteActionLabel("Set speaker$forAll", namedQuote) },
                        leadingIcon = { Icon(Icons.Filled.Person, contentDescription = null) },
                        onClick = {
                            val current = quoteTargets.mapNotNull { it.speaker }.firstOrNull { it.isNotEmpty() } ?: "Narrator"
                            onOpenSpeakerPicker(quoteTargets.map { it.idx }, current)
                            showMenu = false
                        },
                    )
                    DropdownMenuItem(
                        text = {
                            val verb = if (allScareQuote) "Unmark" else "Mark"
                            QuoteActionLabel(if (forAll.isEmpty()) "$verb as scare quote" else "$verb all as scare quotes", namedQuote)
                        },
                        leadingIcon = { Icon(Icons.Filled.FormatQuote, contentDescription = null) },
                        onClick = { onToggleScareQuote(quoteTargets.map { it.idx }, !allScareQuote); showMenu = false },
                    )
                    if (emotionSegments.isNotEmpty()) {
                        DropdownMenuItem(
                            text = { QuoteActionLabel("Set emotion$forAll", namedQuote) },
                            leadingIcon = { Icon(Icons.Filled.Mood, contentDescription = null) },
                            onClick = { showMenu = false; showEmotionMenu = true },
                        )
                    }
                    if (directionTarget != null) {
                        DropdownMenuItem(
                            text = { QuoteActionLabel("Set direction", namedQuote) },
                            leadingIcon = { Icon(Icons.Filled.Edit, contentDescription = null) },
                            onClick = { showMenu = false; showDirectionDialog = true },
                        )
                    }
                }
                DropdownMenuItem(
                    text = { Text(if (isBookmarked) "Remove bookmark" else "Bookmark this paragraph") },
                    leadingIcon = {
                        Icon(
                            if (isBookmarked) Icons.Filled.Bookmark else Icons.Filled.BookmarkBorder,
                            contentDescription = null,
                        )
                    },
                    onClick = { onToggleBookmark(); showMenu = false },
                )
                DropdownMenuItem(
                    text = { Text("Copy paragraph") },
                    leadingIcon = { Icon(Icons.Filled.ContentCopy, contentDescription = null) },
                    onClick = {
                        clipboardManager.setText(AnnotatedString(segments.joinToString(" ") { it.text }))
                        showMenu = false
                    },
                )
            }
            DropdownMenu(expanded = showEmotionMenu, onDismissRequest = { showEmotionMenu = false }) {
                (listOf("") + EMOTION_IDS).forEach { id ->
                    DropdownMenuItem(
                        text = { Text(if (id.isEmpty()) "Neutral" else formatEmotion(id)) },
                        leadingIcon = { Icon(emotionIcon(id), contentDescription = null) },
                        trailingIcon = {
                            if (id == currentEmotion) Icon(Icons.Filled.Check, contentDescription = "Current emotion")
                        },
                        onClick = {
                            if (id != currentEmotion) onSetEmotion(emotionSegments.map { it.idx }, id)
                            showEmotionMenu = false
                        },
                    )
                }
            }
        }
        if (showDirectionDialog && directionTarget != null) {
            DirectionDialog(
                line = directionTarget,
                onDismiss = { showDirectionDialog = false },
                onSave = { direction ->
                    onSetDirection(directionTarget.idx, direction)
                    showDirectionDialog = false
                },
            )
        }
        segments.forEach { segment ->
            if (segment.audioStatus == AudioStatuses.ERROR) {
                Text(
                    text = "Narration failed: ${segment.audioError ?: "unknown error"}",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.error,
                    modifier = Modifier.padding(top = 4.dp),
                )
            }
        }
    }
}

/** A long-press menu item acting on quotes: [label], with [quote]'s text beneath it when the
 *  action is for that one quote out of several in the block (see ParagraphRow's namedQuote). */
@Composable
private fun QuoteActionLabel(label: String, quote: ParagraphDto?) {
    if (quote == null) {
        Text(label)
        return
    }
    Column {
        Text(label)
        Text(
            quote.text,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

// Extra room around the active word's tight glyph bounds, so the highlight reads as a
// pill-shaped box rather than a strict text-background rectangle - SpanStyle.background
// alone can't do this (no padding/corner-radius concept), hence drawing it manually below.
private val HighlightHorizontalPadding = 4.dp
private val HighlightTopPadding = 2.dp
private val HighlightBottomPadding = 2.dp
private val HighlightCornerRadius = 6.dp

/** Last token whose char range starts at or before [charIndex] - the tap-target analogue of
 *  activeWordIndex's "last word whose start time has passed" scan. */
private fun tokenIndexForChar(tokens: List<WordToken>, charIndex: Int): Int {
    var idx = -1
    for (i in tokens.indices) {
        if (tokens[i].start <= charIndex) idx = i else break
    }
    return idx
}

// One segment's tokens/word-start-times, plus where its text lands (char offset) inside the
// combined, all-segments-joined string ParagraphBodyText actually renders - see its own doc
// comment for why segments are joined into one flowing Text instead of stacked one-per-segment.
private data class SegmentSpan(
    val paragraphIdx: Int,
    val textOffset: Int,
    val tokens: List<WordToken>,
    val startTimes: List<Double>,
    // The text shown for this segment: all of it, or one table cell's span (see CellView).
    val text: String,
    // Where [text] starts in the segment's own text - what a mark's offset is measured from.
    val sliceStart: Int = 0,
    // For a table cell: every word's start time across the whole row, and where this cell's
    // first word falls among them - the word being spoken is found over the row, so a cell the
    // voice has left doesn't keep its last word lit.
    val rowStartTimes: List<Double>? = null,
    val firstWord: Int = 0,
    // A directed line's point tags, drawn into [text] in annotations mode (see pillInserts) -
    // [tokens] are already shifted past them; mapStart/mapEnd shift any other offset into the
    // segment's own text the same way.
    val inserts: List<PillInsert> = emptyList(),
) {
    fun mapStart(offset: Int): Int = offset + inserts.filter { it.at <= offset }.sumOf { it.text.length }
    fun mapEnd(offset: Int): Int = offset + inserts.filter { it.at < offset }.sumOf { it.text.length }
}

/** One point tag of a directed line ("sighs", LineDirectionDto.marks) drawn into its text before
 *  original offset [at]: [text] is what is inserted, the pill being [pillStart] until [pillEnd]
 *  of it (the rest is the space beside it). Mirrors frontend ParagraphText's DeliveryPill. */
internal data class PillInsert(val at: Int, val text: String, val pillStart: Int, val pillEnd: Int)

/** The pills [seg]'s point tags insert into its text in annotations mode, in text order. */
private fun pillInserts(seg: ParagraphDto, annotationsMode: Boolean): List<PillInsert> {
    if (!annotationsMode) return emptyList()
    return seg.delivery?.marks.orEmpty().map { m ->
        val at = m.offset.coerceIn(0, seg.text.length)
        val label = "\u2009${m.tag}\u2009"
        if (at >= seg.text.length) PillInsert(at, " $label", 1, 1 + label.length) else PillInsert(at, "$label ", 0, label.length)
    }.sortedBy { it.at }
}

/** [text] with [inserts] put in. */
private fun withInserts(text: String, inserts: List<PillInsert>): String {
    if (inserts.isEmpty()) return text
    val b = StringBuilder()
    var pos = 0
    for (ins in inserts) {
        b.append(text, pos, ins.at)
        b.append(ins.text)
        pos = ins.at
    }
    b.append(text, pos, text.length)
    return b.toString()
}

/** One cell of a table row, for rendering just that cell's span of the row's text: every cell
 *  of the row ([slices], see cellSlices) and which one this is. */
internal data class CellView(val slices: List<CellSlice>, val index: Int)

/** A table row's words: only the cells hold any. The " | " between cells is never shown, and
 *  the aligner gives a bar on its own no timing, so counting it would put the word count off. */
internal fun rowTokens(text: String, slices: List<CellSlice>): List<WordToken> =
    slices.flatMap { s ->
        tokenizeWords(text.substring(s.start, s.end)).map { it.copy(start = it.start + s.start, end = it.end + s.start) }
    }

/**
 * Renders one or more paragraphs' text joined into a single flowing block of text - a lone
 * paragraph, or a non-Inline paragraph followed by its Inline continuations (see
 * ParagraphDto.inline), which read as one continuous sentence/paragraph even though each is its
 * own playback/highlight/tap-target unit, exactly like frontend/src/components/ParagraphText.tsx
 * does per-span within frontend/src/pages/ReaderPage.tsx's own grouped `<p>`. Each segment's own
 * text is trimmed at parse time (backend's epub.splitQuoteSegments splits mid-sentence on quote
 * boundaries), so a literal space is reinserted between segments here to avoid e.g. "fire." and
 * the next segment's opening quote running together with no gap.
 *
 * Karaoke-highlights the word at [playbackSeconds] through whichever segment is
 * [activeSegmentIdx] (using real per-word forced-alignment timings from that segment's own
 * `words` when available, falling back to the proportional character-position estimate
 * otherwise - see WordHighlight.kt's wordStartTimes); -1 highlights nothing. Reports which
 * paragraph and seconds offset a tapped word belongs to, so the caller can seek there (works on
 * any segment, not just the active one - tapping a word in a paragraph that isn't playing yet
 * starts it from there).
 */
@Composable
internal fun ParagraphBodyText(
    segments: List<ParagraphDto>,
    activeSegmentIdx: Int,
    playbackSeconds: Double,
    fontSizeSp: Int,
    fontFamily: FontFamily,
    annotationsMode: Boolean,
    onWordTap: (paragraphIdx: Int, seconds: Double) -> Unit,
    // The segment under the press (ParagraphDto.idx), or null before the text is laid out.
    onLongPress: (paragraphIdx: Int?) -> Unit,
    // Where the line holding the active word sits, as a fraction of this text's height -
    // reported each time the word moves to another line, so auto-follow can track the line
    // rather than the whole block when the block is too tall to centre (see centerItem).
    onActiveLine: (fraction: Float) -> Unit,
    // Render only this table cell's span of the (single) segment's text - see CellView.
    cell: CellView? = null,
    bold: Boolean = false,
) {
    val slice = cell?.let { it.slices[it.index] }
    // A dialogue segment's emotion, shown in annotations mode as a small inline icon right at
    // the start of the segment it applies to - per segment, not per block, since a block with
    // several dialogue segments can carry a different emotion on each (mirrors frontend
    // ChapterSection's EmotionIcon). Each icon is one placeholder char in the joined text, so
    // every span's textOffset below already accounts for it.
    val segmentEmotions = remember(segments, annotationsMode) {
        segments.map { seg ->
            seg.emotion?.takeIf { annotationsMode && it.isNotEmpty() && (seg.delivery == null || seg.delivery.styleFromLabel == true) }
        }
    }
    val spans = remember(segments, segmentEmotions, cell, annotationsMode) {
        val list = mutableListOf<SegmentSpan>()
        var offset = 0
        segments.forEachIndexed { i, seg ->
            if (segmentEmotions[i] != null) offset += 1 // the emotion icon's placeholder char
            deliveryStyleText(seg, annotationsMode)?.let { offset += it.length }
            seg.label?.takeIf { it.isNotEmpty() }?.let { offset += it.length + 1 } // the label and its space
            if (cell != null && slice != null) {
                val rowWords = rowTokens(seg.text, cell.slices)
                val rowTimes = wordStartTimes(rowWords, seg.words, seg.text.length, seg.durationSeconds ?: 0.0, seg.audioPointerSeconds)
                val mine = rowWords.indices.filter { rowWords[it].start >= slice.start && rowWords[it].end <= slice.end }
                list.add(
                    SegmentSpan(
                        paragraphIdx = seg.idx,
                        textOffset = offset,
                        tokens = mine.map { rowWords[it].copy(start = rowWords[it].start - slice.start, end = rowWords[it].end - slice.start) },
                        startTimes = mine.map { rowTimes[it] },
                        text = seg.text.substring(slice.start, slice.end),
                        sliceStart = slice.start,
                        rowStartTimes = rowTimes,
                        firstWord = mine.firstOrNull() ?: 0,
                    ),
                )
            } else {
                val tokens = tokenizeWords(seg.text)
                val startTimes = wordStartTimes(tokens, seg.words, seg.text.length, seg.durationSeconds ?: 0.0, seg.audioPointerSeconds)
                val inserts = pillInserts(seg, annotationsMode)
                val base = SegmentSpan(seg.idx, offset, tokens, startTimes, withInserts(seg.text, inserts), inserts = inserts)
                list.add(base.copy(tokens = tokens.map { it.copy(start = base.mapStart(it.start), end = base.mapEnd(it.end)) }))
            }
            offset += list.last().text.length + 1 // +1 for the joining space below
        }
        list
    }
    val deliveryColor = emotionColor(MaterialTheme.colorScheme.background.luminance() < 0.5f)
    val baseText = remember(segments, segmentEmotions, spans, bold, slice, deliveryColor, annotationsMode) {
        buildAnnotatedString {
            segments.forEachIndexed { i, seg ->
                if (i > 0) append(" ")
                segmentEmotions[i]?.let { appendInlineContent(emotionInlineId(it)) }
                // A directed line's style, as a script's parenthetical - dimmer when it is only
                // the emotion label's (mirrors frontend's .delivery-style).
                deliveryStyleText(seg, annotationsMode)?.let { styleText ->
                    val alpha = if (seg.delivery?.styleFromLabel == true) 0.55f else 1f
                    withStyle(SpanStyle(fontStyle = FontStyle.Italic, fontSize = 0.9.em, color = deliveryColor.copy(alpha = alpha))) {
                        append(styleText)
                    }
                }
                // A play's speaker label ("THESEUS."), with any direction on how the line is
                // delivered: shown, never spoken - so outside every span's word offsets.
                seg.label?.takeIf { it.isNotEmpty() }?.let { label ->
                    withStyle(SpanStyle(fontWeight = FontWeight.SemiBold)) { append(label) }
                    append(" ")
                }
                val textStart = length
                if (bold || slice?.header == true) {
                    withStyle(SpanStyle(fontWeight = FontWeight.SemiBold)) { append(spans[i].text) }
                } else {
                    append(spans[i].text)
                }
                if (annotationsMode) addDeliveryStyles(seg, spans[i], textStart, deliveryColor)
            }
        }
    }
    val combinedText = baseText.text

    val activeSpan = spans.getOrNull(activeSegmentIdx)
    val activeLocalIdx = if (activeSpan != null) {
        remember(activeSpan, playbackSeconds) {
            val rowTimes = activeSpan.rowStartTimes
            if (rowTimes == null) {
                activeWordIndex(activeSpan.startTimes, playbackSeconds)
            } else {
                (activeWordIndex(rowTimes, playbackSeconds) - activeSpan.firstWord).takeIf { it in activeSpan.tokens.indices } ?: -1
            }
        }
    } else {
        -1
    }
    // Darkened, not the bare primary color - in a dark color scheme, MaterialTheme.colorScheme
    // .primary is typically a *lighter* tone than .primaryContainer (the active paragraph's own
    // background, see ParagraphRow), so it'd render lighter than the paragraph highlight instead
    // of standing out as the more specific one - same fix, and same 85/15 mix, as frontend's
    // .word-active (`color-mix(in srgb, var(--accent) 85%, black)`).
    val highlightColor = lerp(MaterialTheme.colorScheme.primary, Color.Black, 0.15f)
    val highlightTextColor = MaterialTheme.colorScheme.onPrimary
    // Mirrors frontend's .paragraph-pending (opacity: 0.6) - but scoped to just the segments
    // that aren't ready yet, not the whole visual block, so a multi-segment paragraph (a
    // non-Inline paragraph plus its Inline continuations - see ParagraphDto.inline) shows
    // exactly which of its own parts are still generating instead of dimming an already-narrated
    // part along with them.
    val dimColor = MaterialTheme.colorScheme.onSurfaceVariant
    // Annotations mode's own underline/caret colors - exact hex match to frontend's
    // --annotation-speaker CSS variable (index.css), light or dark
    // set picked off the app's actually-resolved background luminance (not a raw
    // isSystemInDarkTheme() check) so this tracks the same light/dark state as the rest of the
    // UI even when the reader's own Settings screen has overridden it away from the system
    // default - see MainActivity's own darkTheme resolution.
    val isDarkTheme = MaterialTheme.colorScheme.background.luminance() < 0.5f
    val speakerMarkColor = if (isDarkTheme) AnnotationSpeakerColorDark else AnnotationSpeakerColorLight
    val pronunciationCaretColor = if (isDarkTheme) PronunciationCaretColorDark else PronunciationCaretColorLight
    val scareQuoteMarkColor = if (isDarkTheme) ScareQuoteMarkColorDark else ScareQuoteMarkColorLight
    var textLayoutResult by remember { mutableStateOf<TextLayoutResult?>(null) }

    // The active token's char range within combinedText - global (span-offset-adjusted), unlike
    // activeSpan.tokens' own per-segment-local offsets.
    val activeRange = if (activeSpan != null && activeLocalIdx in activeSpan.tokens.indices) {
        val token = activeSpan.tokens[activeLocalIdx]
        (activeSpan.textOffset + token.start) to (activeSpan.textOffset + token.end)
    } else {
        null
    }

    val activeLineFraction = remember(textLayoutResult, activeRange?.first) {
        val layout = textLayoutResult
        val start = activeRange?.first
        if (layout == null || start == null || layout.size.height <= 0) {
            null
        } else {
            val line = layout.getLineForOffset(start.coerceIn(0, layout.layoutInput.text.length))
            (layout.getLineTop(line) + layout.getLineBottom(line)) / 2f / layout.size.height
        }
    }
    val currentOnActiveLine by rememberUpdatedState(onActiveLine)
    LaunchedEffect(activeLineFraction) { activeLineFraction?.let { currentOnActiveLine(it) } }

    val annotated = remember(baseText, activeRange, highlightTextColor, dimColor, segments) {
        buildAnnotatedString {
            append(baseText)
            segments.forEachIndexed { i, segment ->
                if (segment.audioStatus != AudioStatuses.READY) {
                    val span = spans[i]
                    val end = (span.textOffset + span.text.length).coerceAtMost(combinedText.length)
                    addStyle(SpanStyle(color = dimColor), span.textOffset, end)
                }
            }
            activeRange?.let { (start, end) -> addStyle(SpanStyle(color = highlightTextColor), start, end) }
        }
    }

    val bodyStyle = MaterialTheme.typography.bodyLarge
    // lineHeight scales along with the chosen font size rather than staying at bodyLarge's own
    // fixed value, preserving its natural line-height:font-size ratio at any size - otherwise a
    // large reading size would end up visually cramped (theme's line height meant for the
    // theme's own, much smaller, default size).
    val lineHeightRatio = bodyStyle.lineHeight.value / bodyStyle.fontSize.value
    val emotionIconColor = emotionColor(isDarkTheme)
    val inlineContent = remember(segmentEmotions, emotionIconColor) {
        segmentEmotions.filterNotNull().distinct().associate { emotion ->
            emotionInlineId(emotion) to InlineTextContent(
                // A touch wider than tall - the gap stands in for frontend's .emotion-icon
                // margin-right, so the icon doesn't butt up against the opening quote.
                Placeholder(width = 1.2.em, height = 0.9.em, placeholderVerticalAlign = PlaceholderVerticalAlign.TextCenter),
            ) {
                Box(Modifier.fillMaxSize(), contentAlignment = Alignment.CenterStart) {
                    Icon(
                        emotionIcon(emotion),
                        contentDescription = "Emotion: ${formatEmotion(emotion)}",
                        tint = emotionIconColor,
                        modifier = Modifier.fillMaxHeight().aspectRatio(1f),
                    )
                }
            }
        }
    }
    Text(
        text = annotated,
        inlineContent = inlineContent,
        style = bodyStyle.copy(
            fontSize = fontSizeSp.sp,
            lineHeight = (fontSizeSp * lineHeightRatio).sp,
            fontFamily = fontFamily,
        ),
        onTextLayout = { textLayoutResult = it },
        modifier = Modifier
            .fillMaxWidth()
            .pointerInput(combinedText, spans) {
                detectTapGestures(
                    onLongPress = { offset ->
                        val charIndex = textLayoutResult?.getOffsetForPosition(offset)
                        onLongPress(charIndex?.let { c -> spans.lastOrNull { it.textOffset <= c }?.paragraphIdx })
                    },
                    onTap = { offset ->
                        val layout = textLayoutResult ?: return@detectTapGestures
                        val charIndex = layout.getOffsetForPosition(offset)
                        val span = spans.lastOrNull { it.textOffset <= charIndex } ?: return@detectTapGestures
                        val wordIdx = tokenIndexForChar(span.tokens, charIndex - span.textOffset)
                        if (wordIdx >= 0) onWordTap(span.paragraphIdx, span.startTimes.getOrElse(wordIdx) { 0.0 })
                    },
                )
            }
            .drawBehind {
                val layout = textLayoutResult ?: return@drawBehind
                // Drawn unconditionally (as long as there's a layout at all) - deliberately
                // *before* the active-word-highlight block below, which has its own early
                // returns (return@drawBehind) whenever this particular paragraph isn't the one
                // currently playing/highlighted. Those returns used to sit ahead of this block
                // too, which meant annotation marks only ever rendered on the one paragraph
                // actively playing at that instant - i.e. never, for the other ~everything else
                // in the chapter - since this is meant to mark every dialogue
                // segment in the whole chapter regardless of playback state.
                if (annotationsMode) {
                    segments.forEachIndexed { i, segment ->
                        val segSpan = spans[i]
                        // Dialogue gets an underline (annotationKind) - orthogonal to
                        // and independent of whichever delivery-tag carets this same segment
                        // might also carry below, exactly like frontend's own annotation-* class
                        // and .direction-caret compose on the same span without one gating the
                        // other.
                        val underlineColor = when (annotationKind(segment)) {
                            AnnotationKind.SPEAKER -> speakerMarkColor
                            AnnotationKind.NARRATOR -> null
                        }
                        if (underlineColor != null) {
                            val segToken = WordToken(segSpan.text, segSpan.textOffset, segSpan.textOffset + segSpan.text.length)
                            for (bounds in wordLineBounds(layout, segToken)) {
                                drawAnnotationUnderline(bounds, underlineColor)
                            }
                        }
                        if (segment.scareQuote) {
                            val segToken = WordToken(segSpan.text, segSpan.textOffset, segSpan.textOffset + segSpan.text.length)
                            for (bounds in wordLineBounds(layout, segToken)) {
                                drawScareQuoteUnderline(bounds, scareQuoteMarkColor)
                            }
                        }
                        // A mark's offset is into the segment's whole text; a table cell shows
                        // only its own span of it.
                        val shown = segSpan.sliceStart until segSpan.sliceStart + segSpan.text.length
                        // A directed line shows its tags as pills and styled words instead.
                        for (mark in if (segment.delivery != null) emptyList() else segment.directionMarks) {
                            if (mark.offset !in shown) continue
                            val charIndex = (segSpan.textOffset + mark.offset - segSpan.sliceStart)
                                .coerceIn(segSpan.textOffset, segSpan.textOffset + segSpan.text.length)
                            val line = layout.getLineForOffset(charIndex)
                            val x = layout.getHorizontalPosition(charIndex, usePrimaryDirection = true)
                            drawDirectionCaret(x, layout.getLineBottom(line), directionTagColor(mark.tag, isDarkTheme, dimColor))
                        }
                        for (mark in segment.pronunciationMarks) {
                            if (mark.offset !in shown) continue
                            val start = (segSpan.textOffset + segSpan.mapStart(mark.offset) - segSpan.sliceStart)
                                .coerceIn(segSpan.textOffset, segSpan.textOffset + segSpan.text.length)
                            val end = (segSpan.textOffset + segSpan.mapEnd(mark.offset + mark.length) - segSpan.sliceStart)
                                .coerceIn(start, segSpan.textOffset + segSpan.text.length)
                            val token = WordToken(mark.original, start, end)
                            for (bounds in wordLineBounds(layout, token)) {
                                drawPronunciationUnderline(bounds, pronunciationCaretColor)
                            }
                        }
                    }
                }
                val span = activeSpan ?: return@drawBehind
                val localToken = span.tokens.getOrNull(activeLocalIdx) ?: return@drawBehind
                val token = WordToken(localToken.word, span.textOffset + localToken.start, span.textOffset + localToken.end)
                val hPad = HighlightHorizontalPadding.toPx()
                val topPad = HighlightTopPadding.toPx()
                val bottomPad = HighlightBottomPadding.toPx()
                for (bounds in wordLineBounds(layout, token)) {
                    drawRoundRect(
                        color = highlightColor,
                        topLeft = Offset(bounds.left - hPad, bounds.top - topPad),
                        size = Size(bounds.width + hPad * 2, bounds.height + topPad + bottomPad),
                        cornerRadius = CornerRadius(HighlightCornerRadius.toPx()),
                    )
                }
            },
    )
}

// Exact hex match to frontend's --annotation-speaker CSS variable
// (index.css's :root and its dark-mode overrides) - light/dark picked at the call site off the
// app's actually-resolved theme, not a raw system check. --annotation-narrator has no
// counterpart here since narrator segments get no mark at all (see drawAnnotationMark).
private val AnnotationSpeakerColorLight = Color(0xFF2563EB)
private val AnnotationSpeakerColorDark = Color(0xFF60A5FA)

// Exact hex match to frontend's --direction-emotion/-style/-prosody/-sfx CSS variables - one
// per delivery-tag category, for the inline caret marks and the long-press menu's own per-tag
// icon (see directionTagColor). No "other" pair here - see directionTagColor's own doc comment.
internal val DirectionEmotionColorLight = Color(0xFFDC2626)
internal val DirectionStyleColorLight = Color(0xFF0891B2)
internal val DirectionProsodyColorLight = Color(0xFF16A34A)
internal val DirectionSfxColorLight = Color(0xFFDB2777)
internal val DirectionEmotionColorDark = Color(0xFFF87171)
internal val DirectionStyleColorDark = Color(0xFF22D3EE)
internal val DirectionProsodyColorDark = Color(0xFF4ADE80)
internal val DirectionSfxColorDark = Color(0xFFF472B6)

// Exact hex match to frontend's --pronunciation-caret CSS variable, for the inline strikeout
// marks and the long-press menu's own per-mark icon (see PronunciationStrikeIcon). Name kept as
// "Caret" for continuity with the CSS variable/backend naming this still traces back to, even
// though the mark itself is a strikeout now, not a caret.
private val PronunciationCaretColorLight = Color(0xFF0D9488)
private val PronunciationCaretColorDark = Color(0xFF2DD4BF)

// Exact hex match to frontend's --scare-quote-mark CSS variable, for the dashed scare-quote
// underline (see drawScareQuoteUnderline) - its own hue, distinct from every annotation-*/
// direction-*/pronunciation-caret color above, since it marks yet another independent fact
// ("structurally a quote, but not actually spoken aloud") that can compose with any of them.
private val ScareQuoteMarkColorLight = Color(0xFFEA580C)
private val ScareQuoteMarkColorDark = Color(0xFFFB923C)

// Vertical gap between the text baseline and the underline, and the underline's own stroke
// width - sits close under the text (not far below it) so it still reads as "attached" to the
// line even with several annotated lines stacked close together in a busy chapter.
private val AnnotationUnderlineGap = 0.dp
private val AnnotationUnderlineStrokeWidth = 1.dp

/** Draws one line-segment's annotations-mode underline (dialogue - see
 *  annotationKind) - no background color, just the line itself, in whichever of the two
 *  annotation colors the caller resolved. */
private fun DrawScope.drawAnnotationUnderline(bounds: Rect, color: Color) {
    val y = bounds.bottom + AnnotationUnderlineGap.toPx()
    drawLine(
        color = color,
        start = Offset(bounds.left, y),
        end = Offset(bounds.right, y),
        strokeWidth = AnnotationUnderlineStrokeWidth.toPx(),
    )
}

// Sits just below the regular annotation underline (not on top of it) so both remain legible as
// two distinct marks stacked on the same line, mirroring how frontend's CSS composes .annotation-*
// (border-bottom, a box border) and .annotation-scare-quote (text-decoration, a separate line)
// on the same span without either replacing the other.
private val ScareQuoteUnderlineGap = 2.dp
private val ScareQuoteDashLength = 3.dp
private val ScareQuoteDashGap = 2.dp

/** Draws one line-segment's dashed scare-quote underline (see annotationKind's own doc comment)
 *  - independent of, and composable with, whichever annotationKind color the segment already
 *  has, same as frontend's own .annotation-scare-quote class layering on top of .annotation-*. */
internal fun DrawScope.drawScareQuoteUnderline(bounds: Rect, color: Color) {
    val y = bounds.bottom + AnnotationUnderlineGap.toPx() + AnnotationUnderlineStrokeWidth.toPx() + ScareQuoteUnderlineGap.toPx()
    drawLine(
        color = color,
        start = Offset(bounds.left, y),
        end = Offset(bounds.right, y),
        strokeWidth = AnnotationUnderlineStrokeWidth.toPx(),
        pathEffect = PathEffect.dashPathEffect(floatArrayOf(ScareQuoteDashLength.toPx(), ScareQuoteDashGap.toPx())),
    )
}

// Size of one inline delivery-tag caret - mirrors frontend's ParagraphText.tsx DirectionCaret
// (a small up-arrow glyph sitting just below the text at its exact insertion point, pointing up
// at the line it annotates - the standard proofreading-insertion-caret convention).
private val DirectionCaretSize = 7.dp
private val DirectionCaretStrokeWidth = 1.3.dp
private val DirectionCaretGap = (-1).dp

/** Draws one inline delivery-tag caret at [x] - an open "^" (two strokes, not a filled
 *  triangle) sitting just below the line it annotates ([lineBottom]), apex pointing back up at
 *  it, rather than pushing text apart - same "zero-width in the real text flow" idea as
 *  frontend's own .direction-caret-mark. */
private fun DrawScope.drawDirectionCaret(x: Float, lineBottom: Float, color: Color) {
    val size = DirectionCaretSize.toPx()
    val top = lineBottom + DirectionCaretGap.toPx()
    val bottom = top + size
    val strokeWidth = DirectionCaretStrokeWidth.toPx()
    val stroke = Stroke(width = strokeWidth, cap = StrokeCap.Round, join = StrokeJoin.Round)
    val path = Path().apply {
        moveTo(x - size / 2, bottom)
        lineTo(x, top)
        lineTo(x + size / 2, bottom)
    }
    drawPath(path, color = color, style = stroke)
}

// Size/gap for one pronunciation caret - deliberately distinct from DirectionCaretSize/Gap
// above (a hair smaller, sitting right above the word rather than below the line) so the two
// read as related but visually different marks, mirroring frontend's own down-arrow-above-word
// vs. up-arrow-below-line distinction (PronunciationCaret vs. DirectionCaret in
// ParagraphText.tsx).
private val PronunciationStrikeWidth = 1.4.dp

/** Draws an underline at the foot of [bounds] (one line-fragment of the word a pronunciation
 *  fix applies to - see wordLineBounds) - marks "this word is read differently than written"
 *  directly on the word itself. It was a strikeout, which read as the word being deleted. Sits
 *  right under the letters, above annotations mode's own underline and the dashed scare-quote
 *  one, so a word carrying several shows them all. */
internal fun DrawScope.drawPronunciationUnderline(bounds: Rect, color: Color) {
    val y = bounds.bottom - PronunciationStrikeWidth.toPx() / 2f
    drawLine(
        color = color,
        start = Offset(bounds.left, y),
        end = Offset(bounds.right, y),
        strokeWidth = PronunciationStrikeWidth.toPx(),
        cap = StrokeCap.Round,
    )
}

/**
 * One rectangle per visual line the token's char range crosses, rather than
 * `layout.getPathForRange(start, end).getBounds()`'s single axis-aligned box - for a
 * word-broken word (or, in principle, any span crossing a line wrap), that single bounding
 * box would stretch from the first line's fragment all the way to the second's, covering
 * most of both lines' full width instead of just the two line-fragments the word actually
 * occupies.
 */
private fun wordLineBounds(layout: TextLayoutResult, token: WordToken): List<Rect> {
    if (token.end <= token.start) return emptyList()
    val startLine = layout.getLineForOffset(token.start)
    val endLine = layout.getLineForOffset(token.end - 1)
    return (startLine..endLine).mapNotNull { line ->
        // Never call getHorizontalPosition() exactly at a line-wrap boundary offset - Android's
        // text layout resolves that position ambiguously (as the *start* of the next line
        // rather than the end of this one), which is what collapsed a wrapped word's first-line
        // highlight into a sliver at the wrong edge: getHorizontalPosition(lineEnd) was
        // returning a small x near the line's start, and min()/max() then built a box from
        // there to the word's real start instead of from the word's start to the line's end.
        // Any edge that isn't the word's own start/end uses the line's own left/right extent
        // instead, which is unambiguous.
        val left = if (line == startLine) {
            layout.getHorizontalPosition(token.start, usePrimaryDirection = true)
        } else {
            layout.getLineLeft(line)
        }
        val right = if (line == endLine) {
            layout.getHorizontalPosition(token.end, usePrimaryDirection = true)
        } else {
            layout.getLineRight(line)
        }
        if (left >= right) return@mapNotNull null
        Rect(
            left = left,
            top = layout.getLineTop(line),
            right = right,
            bottom = layout.getLineBottom(line),
        )
    }
}

/** The long-press "Set direction" editor, for a book whose clone model takes direction as text:
 *  [line] written back with its direction ("[tired] Fine. [sighs] Go."), starting from its own
 *  direction or its plain text, with the emotion labels' styles as presets. Saving the plain
 *  text, or Clear, removes the direction; the backend refuses an edit that changes the words. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun DirectionDialog(line: ParagraphDto, onDismiss: () -> Unit, onSave: (String) -> Unit) {
    var draft by remember(line.idx) { mutableStateOf(line.direction ?: line.text) }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Direction") },
        text = {
            Column {
                OutlinedTextField(
                    value = draft,
                    onValueChange = { draft = it },
                    minLines = 3,
                    modifier = Modifier.fillMaxWidth(),
                    supportingText = { Text("[a short phrase for the delivery] the line, with [sighs] or <whisper>…</whisper>") },
                )
                FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                    ApiCatalog.EMOTIONS.forEach { e ->
                        AssistChip(
                            onClick = { draft = "[${e.direction}] " + draft.replace(LEADING_STYLE, "") },
                            label = { Text(e.label) },
                            leadingIcon = { Icon(emotionIcon(e.id), contentDescription = null) },
                        )
                    }
                }
            }
        },
        confirmButton = {
            TextButton(onClick = { onSave(if (draft.trim() == line.text.trim()) "" else draft) }) { Text("Save") }
        },
        dismissButton = {
            Row {
                if (line.direction != null) TextButton(onClick = { onSave("") }) { Text("Clear") }
                TextButton(onClick = onDismiss) { Text("Cancel") }
            }
        },
    )
}

/** A style prefix at the start of a direction draft, which a preset replaces. */
private val LEADING_STYLE = Regex("""^\s*\[[^\]]*\]\s*""")

/** [seg]'s style as the parenthetical shown before it in annotations mode ("(tired) "), or null. */
private fun deliveryStyleText(seg: ParagraphDto, annotationsMode: Boolean): String? =
    seg.delivery?.style?.takeIf { annotationsMode && it.isNotEmpty() }?.let { "($it) " }

/** Styles a directed line's pills and spans in the text being built, [span]'s text starting at
 *  [textStart]: pills small, italic and tinted on a faint ground; whispered words faded and
 *  italic, slow ones letter-spaced, emphasized ones underlined (mirrors frontend's
 *  .delivery-pill/.delivery-span-* rules). */
private fun AnnotatedString.Builder.addDeliveryStyles(seg: ParagraphDto, span: SegmentSpan, textStart: Int, color: Color) {
    var shift = 0
    for (ins in span.inserts) {
        val at = textStart + ins.at + shift
        addStyle(
            SpanStyle(fontSize = 0.72.em, fontStyle = FontStyle.Italic, color = color, background = color.copy(alpha = 0.12f)),
            at + ins.pillStart,
            at + ins.pillEnd,
        )
        shift += ins.text.length
    }
    for (sp in seg.delivery?.spans.orEmpty()) {
        val start = textStart + span.mapStart(sp.start)
        val end = textStart + span.mapEnd(sp.end)
        if (end <= start) continue
        val style = when (sp.tag) {
            "whisper" -> SpanStyle(fontStyle = FontStyle.Italic, color = color.copy(alpha = 0.75f))
            "slow" -> SpanStyle(letterSpacing = 0.08.em)
            "emphasis" -> SpanStyle(textDecoration = TextDecoration.Underline, fontWeight = FontWeight.Medium)
            else -> continue
        }
        addStyle(style, start, end)
    }
}
