package com.lectable.app.ui.reader

import androidx.compose.foundation.border
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.graphics.Brush
import com.lectable.app.data.remote.dto.LightEffects
import com.lectable.app.data.remote.dto.LightingDto
import androidx.compose.material.icons.outlined.Lightbulb
import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.animateScrollBy
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.LazyListState
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Air
import androidx.compose.material.icons.filled.MusicNote
import androidx.compose.material.icons.filled.Pause
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Lightbulb
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import coil.compose.AsyncImage
import com.lectable.app.data.remote.dto.AudioStatuses
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.MusicRegionDto
import com.lectable.app.data.remote.dto.ParagraphDto
import com.lectable.app.playback.PlaybackState

internal sealed class ReaderListEntry {
    data class ChapterHeader(val chapterIdx: Int, val title: String) : ReaderListEntry()
    // segments is one or more paragraphs rendered as a single visual block - a lone non-Inline
    // paragraph is a list of one; a non-Inline paragraph followed by one or more Inline ones
    // (see ParagraphDto.inline) is grouped together, mirroring
    // frontend/src/pages/ReaderPage.tsx's groupContent. Always non-empty.
    data class ParagraphItem(val chapterIdx: Int, val segments: List<ParagraphDto>) : ReaderListEntry()
    // A standalone image between/around paragraphs (backend's contentItemDTO kind "image"),
    // e.g. an illustration or figure embedded in the original epub - imageUrl is still the raw
    // relative path (`/api/images/{id}`); resolved against the current server at render time,
    // same as covers.
    data class ImageItem(val chapterIdx: Int, val key: String, val imageUrl: String) : ReaderListEntry()
    // A scene/section break (backend's contentItemDTO kind "break") - the source epub's own
    // <hr/>, or a text-only marker like "* * *" that used to be read aloud literally (see
    // backend epub.BlockBreak). Carries no data beyond its position, same as ImageItem minus the
    // URL - rendered as a plain divider, mirroring frontend's ChapterSection.tsx.
    data class BreakItem(val chapterIdx: Int, val key: String) : ReaderListEntry()
    // Marks where a chapter's own background-music region starts (annotations mode only - see
    // InfiniteChapterContent's own musicByStart, built only while annotationsMode is on) -
    // mirrors frontend's own ChapterSection.MusicRegionBoundary. Always immediately precedes the
    // [ParagraphItem] whose first segment's idx == region.startIdx, never its own list position.
    data class MusicBoundary(val chapterIdx: Int, val key: String, val region: MusicRegionDto) : ReaderListEntry()
}

/**
 * Walks chapter's content items in original document order (backend's contentItemDTO - text
 * interleaved with images and scene breaks) and turns them into [ReaderListEntry]s: each image
 * becomes its own [ReaderListEntry.ImageItem], each break its own [ReaderListEntry.BreakItem],
 * and each text item's referenced paragraph either joins the visual block currently being built
 * (when [ParagraphDto.inline]) or starts a new one - exactly mirrors
 * frontend/src/pages/ReaderPage.tsx's groupContent. A content item whose paragraphIdx doesn't
 * resolve (shouldn't normally happen) is skipped rather than crashing the reader.
 */
internal fun groupContent(
    chapterIdx: Int,
    chapter: ChapterDetailDto,
    // Keyed by MusicRegionDto.startIdx (regions are contiguous/non-overlapping - see backend
    // store.MusicRegion's own doc comment), so a new text group's first paragraph idx matching a
    // region's startIdx is exactly "insert that region's own boundary marker right before this
    // group" - mirrors frontend's ChapterSection.tsx musicRegionsByStartIdx exactly. Empty
    // (skipping every boundary) while annotations mode is off - see the caller.
    musicRegionsByStartIdx: Map<Int, MusicRegionDto>,
): List<ReaderListEntry> {
    val paragraphsByIdx = chapter.paragraphs.associateBy { it.idx }
    val entries = mutableListOf<ReaderListEntry>()
    var currentGroup: MutableList<ParagraphDto>? = null
    chapter.content.forEachIndexed { i, item ->
        if (item.kind == "image") {
            currentGroup = null
            entries.add(ReaderListEntry.ImageItem(chapterIdx, "img-$chapterIdx-$i", item.imageUrl.orEmpty()))
            return@forEachIndexed
        }
        if (item.kind == "break") {
            currentGroup = null
            entries.add(ReaderListEntry.BreakItem(chapterIdx, "break-$chapterIdx-$i"))
            return@forEachIndexed
        }
        val p = paragraphsByIdx[item.paragraphIdx] ?: return@forEachIndexed
        val group = currentGroup
        if (p.inline && group != null) {
            group.add(p)
        } else {
            val newGroup = mutableListOf(p)
            currentGroup = newGroup
            musicRegionsByStartIdx[p.idx]?.let { region ->
                entries.add(ReaderListEntry.MusicBoundary(chapterIdx, "music-$chapterIdx-${region.id}", region))
            }
            entries.add(ReaderListEntry.ParagraphItem(chapterIdx, newGroup))
        }
    }
    return entries
}

/**
 * The infinite-scroll reader - the Android analogue of ReaderPage.tsx's `.scroll-reader`: every
 * chapter in [range] that's actually in [loadedChapters] rendered as one continuous flat list
 * (title header, then its paragraphs, then the next chapter's header, and so on), rather than
 * requiring a chapter to be explicitly picked. Grows the window via [onExpandUp]/[onExpandDown]
 * as the list scrolls near either edge - the Compose analogue of the web version's
 * IntersectionObserver sentinels, but driven off `LazyListState.layoutInfo` directly instead of
 * separate sentinel elements, since Compose has no intersection-observer primitive of its own.
 */
@Composable
internal fun InfiniteChapterContent(
    listState: LazyListState,
    loadedChapters: Map<Int, ChapterDetailDto>,
    range: IntRange,
    totalChapters: Int,
    playback: PlaybackState,
    playbackSeconds: Double,
    autoFollow: Boolean,
    bookmarksByKey: Map<Pair<Int, Int>, String>,
    fontSizeSp: Int,
    fontFamily: FontFamily,
    annotationsMode: Boolean,
    chapterMusicRegions: Map<Int, List<MusicRegionDto>>,
    previewingMusicRegionId: String?,
    onParagraphClick: (chapterIdx: Int, paragraphIdx: Int, startSeconds: Double) -> Unit,
    onExpandUp: () -> Unit,
    onExpandDown: () -> Unit,
    onRegenerateParagraph: (chapterIdx: Int, paragraphIdx: Int) -> Unit,
    onToggleBookmark: (chapterIdx: Int, paragraphIdx: Int) -> Unit,
    onOpenSpeakerPicker: (chapterIdx: Int, paragraphIndices: List<Int>, currentSpeaker: String) -> Unit,
    onOpenDescriptionPicker: (chapterIdx: Int, paragraphIndices: List<Int>, fromName: String) -> Unit,
    onSetScareQuote: (chapterIdx: Int, paragraphIndices: List<Int>, scareQuote: Boolean) -> Unit,
    onSetEmotion: (chapterIdx: Int, paragraphIndices: List<Int>, emotion: String) -> Unit,
    onRegenerateMusic: (chapterIdx: Int, regionId: String) -> Unit,
    onPreviewMusic: (regionId: String, audioUrl: String) -> Unit,
    resolveImageUrl: (String) -> Any?,
) {
    val entries = remember(loadedChapters, range, annotationsMode, chapterMusicRegions) {
        range.mapNotNull { idx -> loadedChapters[idx] }.flatMap { chapter ->
            // Only actually inserted while annotations mode is on - see MusicBoundary's own doc
            // comment; chapterMusicRegions itself may already be populated regardless (the live
            // background-music mix needs it independent of whether the reader ever looks at
            // annotations mode), so the gate belongs here, not further upstream.
            val musicByStart = if (annotationsMode) {
                chapterMusicRegions[chapter.idx].orEmpty().associateBy { it.startIdx }
            } else {
                emptyMap()
            }
            buildList {
                add(ReaderListEntry.ChapterHeader(chapter.idx, chapter.title))
                addAll(groupContent(chapter.idx, chapter, musicByStart))
            }
        }
    }

    // Grows the loaded window as the reader nears either end of what's currently mounted -
    // "near" being within 3 items of an edge, same spirit as the web version's 800px sentinel
    // margin (triggering before the reader actually hits the end, not after).
    LaunchedEffect(listState, range.first, totalChapters) {
        snapshotFlow { listState.layoutInfo }.collect { layoutInfo ->
            val visible = layoutInfo.visibleItemsInfo
            if (visible.isEmpty()) return@collect
            if (visible.first().index <= 2 && range.first > 0) onExpandUp()
            if (visible.last().index >= layoutInfo.totalItemsCount - 3 && range.last < totalChapters - 1) onExpandDown()
        }
    }

    // Keeps the currently-playing paragraph centered as playback advances - see
    // LazyListState.centerItem. Suspends itself the moment the reader scrolls by their own
    // input (autoFollow, tracked by the caller), same as the web reader's useAutoFollow.
    LaunchedEffect(playback.chapterIdx, playback.paragraphIdx, autoFollow, entries) {
        if (!autoFollow) return@LaunchedEffect
        val itemIndex = entries.indexOfFirst {
            it is ReaderListEntry.ParagraphItem && it.chapterIdx == playback.chapterIdx &&
                it.segments.any { s -> s.idx == playback.paragraphIdx }
        }
        if (itemIndex < 0) return@LaunchedEffect
        listState.centerItem(itemIndex)
    }

    LazyColumn(
        state = listState,
        modifier = Modifier.fillMaxSize(),
        contentPadding = PaddingValues(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        items(
            entries,
            key = { entry ->
                when (entry) {
                    is ReaderListEntry.ChapterHeader -> "header-${entry.chapterIdx}"
                    is ReaderListEntry.ParagraphItem -> "p-${entry.chapterIdx}-${entry.segments.first().idx}"
                    is ReaderListEntry.ImageItem -> entry.key
                    is ReaderListEntry.BreakItem -> entry.key
                    is ReaderListEntry.MusicBoundary -> entry.key
                }
            },
        ) { entry ->
            when (entry) {
                is ReaderListEntry.ChapterHeader -> {
                    Text(
                        entry.title,
                        style = MaterialTheme.typography.headlineSmall,
                        modifier = Modifier.padding(
                            top = if (entry.chapterIdx == range.first) 0.dp else 24.dp,
                            bottom = 8.dp,
                        ),
                    )
                }
                is ReaderListEntry.ParagraphItem -> {
                    val activeSegmentIdx = if (playback.chapterIdx == entry.chapterIdx) {
                        entry.segments.indexOfFirst { it.idx == playback.paragraphIdx }
                    } else {
                        -1
                    }
                    val firstIdx = entry.segments.first().idx
                    ParagraphRow(
                        segments = entry.segments,
                        activeSegmentIdx = activeSegmentIdx,
                        isBookmarked = bookmarksByKey.containsKey(entry.chapterIdx to firstIdx),
                        playbackSeconds = if (activeSegmentIdx >= 0) playbackSeconds else 0.0,
                        fontSizeSp = fontSizeSp,
                        fontFamily = fontFamily,
                        annotationsMode = annotationsMode,
                        onWordTap = { paragraphIdx, startSeconds -> onParagraphClick(entry.chapterIdx, paragraphIdx, startSeconds) },
                        // A reader regenerating "this paragraph" means the whole visual block
                        // they're looking at, inline segments included - mirrors
                        // ReaderPage.tsx's own regenerate button.
                        onRegenerate = { entry.segments.forEach { onRegenerateParagraph(entry.chapterIdx, it.idx) } },
                        onToggleBookmark = { onToggleBookmark(entry.chapterIdx, firstIdx) },
                        // Only this block's own isQuote segment(s) - see SpeakerPickerTarget's doc
                        // comment for why narration/description segments are excluded.
                        onOpenSpeakerPicker = {
                            val quoteSegments = entry.segments.filter { it.isQuote }
                            val current = quoteSegments.mapNotNull { it.speaker }.firstOrNull { it.isNotEmpty() } ?: "Narrator"
                            onOpenSpeakerPicker(entry.chapterIdx, quoteSegments.map { it.idx }, current)
                        },
                        onOpenDescriptionPicker = { fromName, paragraphIndices ->
                            onOpenDescriptionPicker(entry.chapterIdx, paragraphIndices, fromName)
                        },
                        onToggleScareQuote = { paragraphIndices, scareQuote ->
                            onSetScareQuote(entry.chapterIdx, paragraphIndices, scareQuote)
                        },
                        onSetEmotion = { paragraphIndices, emotion ->
                            onSetEmotion(entry.chapterIdx, paragraphIndices, emotion)
                        },
                    )
                }
                is ReaderListEntry.ImageItem -> {
                    AsyncImage(
                        model = resolveImageUrl(entry.imageUrl),
                        contentDescription = null,
                        contentScale = ContentScale.FillWidth,
                        modifier = Modifier
                            .fillMaxWidth()
                            .clip(RoundedCornerShape(8.dp)),
                    )
                }
                is ReaderListEntry.BreakItem -> {
                    Box(modifier = Modifier.fillMaxWidth(), contentAlignment = Alignment.Center) {
                        HorizontalDivider(modifier = Modifier.width(64.dp))
                    }
                }
                is ReaderListEntry.MusicBoundary -> {
                    MusicRegionBoundaryRow(
                        region = entry.region,
                        previewing = previewingMusicRegionId == entry.region.id,
                        onRegenerate = { onRegenerateMusic(entry.chapterIdx, entry.region.id) },
                        onPreview = { entry.region.audioUrl?.let { onPreviewMusic(entry.region.id, it) } },
                    )
                }
            }
        }
    }
}

/** "m:ss" - mirrors frontend's ChapterSection.tsx formatMusicDuration exactly. */
private fun formatMusicDuration(seconds: Double): String {
    val m = (seconds / 60).toInt()
    val s = (seconds % 60).toInt()
    return "$m:${s.toString().padStart(2, '0')}"
}

/**
 * Marks where a chapter's own background-music region starts - the mood/prompt "score" a reader
 * can otherwise only see indirectly by hearing it, plus whether this region continues smoothly
 * out of the previous one or is a hard cut, and its own generation status. A labeled divider, not
 * a per-paragraph badge, since a region's mood applies to every paragraph until the next
 * boundary - mirrors frontend's ChapterSection.tsx MusicRegionBoundary.
 */
@Composable
private fun MusicRegionBoundaryRow(
    region: MusicRegionDto,
    previewing: Boolean,
    onRegenerate: () -> Unit,
    onPreview: () -> Unit,
) {
    val statusLabel = when (region.status) {
        AudioStatuses.PENDING -> "not generated yet"
        AudioStatuses.GENERATING -> "generating…"
        AudioStatuses.ERROR -> "generation failed"
        else -> null
    }
    val durationLabel = if (region.status == AudioStatuses.READY && region.durationSeconds > 0) {
        formatMusicDuration(region.durationSeconds)
    } else {
        null
    }
    val busy = region.status == AudioStatuses.GENERATING
    Row(
        verticalAlignment = Alignment.CenterVertically,
        modifier = Modifier
            .fillMaxWidth()
            .background(MaterialTheme.colorScheme.surfaceVariant, RoundedCornerShape(8.dp))
            .padding(horizontal = 12.dp, vertical = 6.dp),
    ) {
        Icon(Icons.Default.MusicNote, contentDescription = null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
        Column(modifier = Modifier.weight(1f).padding(start = 8.dp)) {
            Text(
                region.mood.ifBlank { "Untitled region" },
                style = MaterialTheme.typography.labelLarge,
            )
            Text(
                buildString {
                    append(if (region.transition == "continuation") "continues" else "new cue")
                    statusLabel?.let { append(" · $it") }
                    durationLabel?.let { append(" · $it") }
                },
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            if (region.prompt.isNotBlank()) {
                Text(
                    region.prompt,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            // The ambient soundscape layered under this region's music, on its own line so it
            // doesn't read as a continuation of the music prompt.
            region.ambience?.takeIf { it.isNotBlank() }?.let { ambience ->
                Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.padding(top = 2.dp)) {
                    Icon(
                        Icons.Outlined.Air,
                        contentDescription = "Ambience",
                        tint = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.size(14.dp),
                    )
                    Text(
                        ambience,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 2,
                        overflow = TextOverflow.Ellipsis,
                        modifier = Modifier.padding(start = 4.dp),
                    )
                }
            }
            // The region's room-lighting cue (the ambience pass's lighting sub-pass) - mirrors
            // frontend ChapterSection's LightingSwatch, minus the animation.
            region.lighting?.let { lighting ->
                Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.padding(top = 2.dp)) {
                    Icon(
                        Icons.Outlined.Lightbulb,
                        contentDescription = "Lighting",
                        tint = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.size(14.dp),
                    )
                    LightingSwatch(lighting, Modifier.padding(start = 4.dp))
                    Text(
                        listOfNotNull(
                            lighting.description?.takeIf { it.isNotBlank() },
                            lightEffectLabel(lighting.effect),
                            "${lighting.brightness}%",
                        ).joinToString(" · "),
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 2,
                        overflow = TextOverflow.Ellipsis,
                        modifier = Modifier.padding(start = 4.dp),
                    )
                }
            }
        }
        if (region.status == AudioStatuses.READY && region.audioUrl != null) {
            IconButton(onClick = onPreview) {
                Icon(if (previewing) Icons.Default.Pause else Icons.Default.PlayArrow, contentDescription = "Preview this region's music")
            }
        }
        IconButton(onClick = onRegenerate, enabled = !busy) {
            Icon(
                Icons.Default.Refresh,
                contentDescription = if (region.status == AudioStatuses.READY) "Regenerate this region's music" else "Generate this region's music",
            )
        }
    }
}

/** A lighting cue's color (and accent, as the second half), dimmed by its brightness. */
@Composable
private fun LightingSwatch(lighting: LightingDto, modifier: Modifier = Modifier) {
    fun parse(hex: String?): Color? = hex?.takeIf { it.isNotBlank() }?.let {
        runCatching { Color(android.graphics.Color.parseColor(it)) }.getOrNull()
    }
    val base = parse(lighting.color) ?: return
    val accent = parse(lighting.accent) ?: base
    Box(
        modifier
            .size(12.dp)
            .alpha(0.35f + 0.65f * (lighting.brightness / 100f).coerceIn(0f, 1f))
            .clip(CircleShape)
            .background(Brush.linearGradient(0.5f to base, 0.5f to accent))
            .border(1.dp, MaterialTheme.colorScheme.outlineVariant, CircleShape),
    )
}

/** How each lighting effect reads in the UI - frontend's LIGHT_EFFECT_LABELS. */
private fun lightEffectLabel(effect: String): String = when (effect) {
    LightEffects.FLICKER -> "flickering"
    LightEffects.PULSE -> "pulsing"
    LightEffects.LIGHTNING -> "lightning"
    LightEffects.SHIMMER -> "shimmering"
    else -> "steady"
}

/**
 * Scrolls so the item at [index] (a LazyColumn item index, not a paragraph idx - callers must
 * account for any header items) ends up centered in the viewport, not just visible at an edge.
 * `BringIntoViewRequester` (the more obvious tool here) only scrolls the minimum amount needed
 * to bring an item fully into view, which is why a naive first attempt at this "didn't actually
 * work" as centering - it would leave the active paragraph sitting at whichever edge it
 * scrolled in from.
 *
 * Only falls back to `animateScrollToItem` (itself animated, not an instant jump) when [index]
 * isn't already laid out - e.g. right after a chapter switch. The common case, advancing one
 * paragraph at a time during normal playback, almost always lands on an already-visible item,
 * so it skips straight to a single smooth `animateScrollBy` nudge instead of an instant
 * `scrollToItem` snap immediately followed by a correcting animation - which is what made an
 * earlier version of this feel like an aggressive jump-then-slide rather than one motion.
 */
private suspend fun LazyListState.centerItem(index: Int) {
    if (layoutInfo.visibleItemsInfo.none { it.index == index }) {
        animateScrollToItem(index)
    }
    val info = layoutInfo.visibleItemsInfo.find { it.index == index } ?: return
    val viewportCenter = (layoutInfo.viewportStartOffset + layoutInfo.viewportEndOffset) / 2
    val itemCenter = info.offset + info.size / 2
    animateScrollBy((itemCenter - viewportCenter).toFloat())
}
