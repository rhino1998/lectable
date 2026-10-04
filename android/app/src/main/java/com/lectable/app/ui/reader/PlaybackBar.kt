package com.lectable.app.ui.reader

import androidx.compose.ui.draw.alpha
import androidx.compose.material.icons.outlined.Lightbulb
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.background
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Bookmarks
import androidx.compose.material.icons.filled.FormatListNumbered
import androidx.compose.material.icons.filled.MusicNote
import androidx.compose.material.icons.filled.MusicOff
import androidx.compose.material.icons.filled.Palette
import androidx.compose.material.icons.filled.Pause
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.Lightbulb
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledIconButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LocalContentColor
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.unit.dp
import com.lectable.app.playback.lighting.LightingStatus
import com.lectable.app.ui.components.CleanSlider
import com.lectable.app.ui.formatDurationLong
import com.lectable.app.ui.formatTime
import kotlin.math.roundToInt

@OptIn(ExperimentalFoundationApi::class)
@Composable
internal fun PlaybackBar(
    isPlaying: Boolean,
    playbackSpeed: Float,
    chapterProgress: Float,
    bufferedAheadEnd: Float,
    chapterElapsedSeconds: Double,
    chapterRemainingSeconds: Double,
    chapterTotalSeconds: Double,
    wordsPerMinute: Int?,
    bookRemainingSeconds: Double,
    bookRemainingPercent: Double,
    finished: Boolean,
    onPlayPause: () -> Unit,
    onSpeedClick: () -> Unit,
    onChapterInfoClick: () -> Unit,
    onBookmarksClick: () -> Unit,
    lightingEnabled: Boolean,
    lightingStatus: LightingStatus,
    onToggleLighting: () -> Unit,
    onLightingSettings: () -> Unit,
    musicEnabled: Boolean,
    onToggleMusic: () -> Unit,
    onMusicVolumeClick: () -> Unit,
    annotationsMode: Boolean,
    onToggleAnnotationsMode: () -> Unit,
) {
    Column(modifier = Modifier.fillMaxWidth().background(MaterialTheme.colorScheme.surfaceContainer)) {
        // Progress through the whole chapter (paragraphs completed, plus how far into the
        // current one) - not just the currently-playing clip - mirroring frontend's
        // PlayerBar.tsx chapterProgress, plus a lighter "buffered ahead" segment showing how
        // far playback could run right now without waiting on generation.
        ChapterProgressBar(
            progress = chapterProgress,
            bufferedAheadEnd = bufferedAheadEnd,
            modifier = Modifier.fillMaxWidth(),
        )
        // A Box with explicit start/center/end alignment, not a Row + SpaceBetween - the
        // latter only *looks* centered when the flanking labels are equal width, which
        // "0:20" and "-0:29 · 0:49" generally aren't.
        Box(modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 2.dp)) {
            Text(
                formatTime(chapterElapsedSeconds),
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.align(Alignment.CenterStart),
            )
            if (!finished) {
                Text(
                    buildString {
                        append("${formatDurationLong(bookRemainingSeconds)} · ${bookRemainingPercent.roundToInt()}%")
                        if (wordsPerMinute != null) append(" · $wordsPerMinute wpm")
                    },
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.align(Alignment.Center),
                )
            }
            Text(
                buildString {
                    if (!finished) append("-${formatTime(chapterRemainingSeconds)} · ")
                    append(formatTime(chapterTotalSeconds))
                },
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.align(Alignment.CenterEnd),
            )
        }
        // A Box, not a Row + weight(1f) flanks - that only actually centers Play/Pause when both
        // sides happen to measure the same width, which stopped being true the moment the left
        // and right icon groups became different sizes. Aligning Play/Pause to Alignment.Center
        // directly guarantees it sits at the row's true midpoint regardless of how many icons
        // end up on either side of it.
        Box(
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 16.dp, vertical = 8.dp),
        ) {
            FilledIconButton(onClick = onPlayPause, modifier = Modifier.align(Alignment.Center)) {
                Icon(
                    if (isPlaying) Icons.Filled.Pause else Icons.Filled.PlayArrow,
                    contentDescription = if (isPlaying) "Pause" else "Play",
                )
            }
            // Annotations/Music/Lighting on the left, sized down from IconButton's own 48dp
            // default touch target so all three plus Play/Pause's own footprint leave clear
            // room in the middle.
            Row(
                modifier = Modifier.align(Alignment.CenterStart),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                IconButton(onClick = onToggleAnnotationsMode, modifier = Modifier.size(SecondaryIconButtonSize)) {
                    Icon(
                        Icons.Filled.Palette,
                        contentDescription = if (annotationsMode) "Turn off annotations mode" else "Turn on annotations mode",
                        tint = if (annotationsMode) MaterialTheme.colorScheme.primary else LocalContentColor.current,
                    )
                }
                // Tap toggles book-wide background music (the same setting as VoicePickerSheet's
                // "Background music" switch); long-press opens its volume slider. A plain
                // IconButton has no long-press, hence combinedClickable on a same-sized circle.
                Box(
                    modifier = Modifier
                        .size(SecondaryIconButtonSize)
                        .clip(CircleShape)
                        .combinedClickable(
                            role = Role.Button,
                            onClickLabel = if (musicEnabled) "Turn off background music" else "Turn on background music",
                            onLongClickLabel = "Music volume",
                            onClick = onToggleMusic,
                            onLongClick = onMusicVolumeClick,
                        ),
                    contentAlignment = Alignment.Center,
                ) {
                    Icon(
                        if (musicEnabled) Icons.Filled.MusicNote else Icons.Filled.MusicOff,
                        contentDescription = if (musicEnabled) "Background music on" else "Background music off",
                        tint = if (musicEnabled) MaterialTheme.colorScheme.primary else LocalContentColor.current,
                    )
                }
                // Hue room lighting: tap toggles it (opening the lighting screen instead if no
                // bridge/room is set up yet), long-press opens the lighting screen - same
                // combinedClickable shape as the music button beside it.
                Box(
                    modifier = Modifier
                        .size(SecondaryIconButtonSize)
                        .clip(CircleShape)
                        .combinedClickable(
                            role = Role.Button,
                            onClickLabel = if (lightingEnabled) "Turn off room lighting" else "Turn on room lighting",
                            onLongClickLabel = "Lighting settings",
                            onClick = onToggleLighting,
                            onLongClick = onLightingSettings,
                        ),
                    contentAlignment = Alignment.Center,
                ) {
                    Icon(
                        if (lightingEnabled) Icons.Filled.Lightbulb else Icons.Outlined.Lightbulb,
                        contentDescription = when {
                            !lightingEnabled -> "Room lighting off"
                            lightingStatus == LightingStatus.ERROR -> "Room lighting error"
                            else -> "Room lighting on"
                        },
                        tint = when {
                            !lightingEnabled -> LocalContentColor.current
                            lightingStatus == LightingStatus.ERROR -> MaterialTheme.colorScheme.error
                            else -> MaterialTheme.colorScheme.primary
                        },
                    )
                }
            }
            // Bookmarks/chapter selector/speed selector on the right.
            Row(
                modifier = Modifier.align(Alignment.CenterEnd),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                IconButton(onClick = onBookmarksClick, modifier = Modifier.size(SecondaryIconButtonSize)) {
                    Icon(Icons.Filled.Bookmarks, contentDescription = "Bookmarks")
                }
                IconButton(onClick = onChapterInfoClick, modifier = Modifier.size(SecondaryIconButtonSize)) {
                    Icon(Icons.Filled.FormatListNumbered, contentDescription = "Jump to chapter")
                }
                // Constrained to the same footprint as the other secondary icons in this row
                // (SecondaryIconButtonSize), not TextButton's own wider default minimum size/
                // padding - tight content padding so "1.5×" still fits without clipping.
                Box(modifier = Modifier.size(SecondaryIconButtonSize), contentAlignment = Alignment.Center) {
                    TextButton(
                        onClick = onSpeedClick,
                        contentPadding = PaddingValues(horizontal = 2.dp),
                        modifier = Modifier.fillMaxSize(),
                    ) {
                        Text("${formatSpeed(playbackSpeed)}×", style = MaterialTheme.typography.labelMedium)
                    }
                }
            }
        }
    }
}

// PlaybackBar's own secondary icon buttons (annotations/music/lighting/bookmarks/chapter selector)
// - smaller than IconButton's own 48dp default touch target, see its call sites' own comment.
private val SecondaryIconButtonSize = 40.dp

/**
 * The playback-speed picker - a [ModalBottomSheet] like [ChapterPickerSheet]/[VoicePickerSheet]
 * rather than a small anchored popup, so it pops up over the whole screen the same way those do.
 * See [CleanSlider]'s own doc comment for why the slider itself isn't just a stock Material3
 * `Slider` at [PLAYBACK_SPEED_STEPS]'s 37 steps.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun SpeedPickerSheet(playbackSpeed: Float, onSpeedChange: (Float) -> Unit, onDismiss: () -> Unit) {
    ModalBottomSheet(onDismissRequest = onDismiss) {
        Text(
            "Speed (${formatSpeed(playbackSpeed)}×)",
            style = MaterialTheme.typography.titleMedium,
            modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
        )
        CleanSlider(
            value = playbackSpeed,
            onValueChange = onSpeedChange,
            valueRange = PLAYBACK_SPEED_RANGE,
            steps = PLAYBACK_SPEED_STEPS,
            startLabel = "${formatSpeed(PLAYBACK_SPEED_MIN)}×",
            endLabel = "${formatSpeed(PLAYBACK_SPEED_MAX)}×",
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 16.dp)
                .padding(bottom = 24.dp),
        )
    }
}

/** Background music's own volume slider - opened by long-pressing PlaybackBar's music button.
 *  Device-local (see PlaybackSettingsRepository.musicVolume), relative to narration's full
 *  volume, applied live as it's dragged. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun MusicVolumeSheet(volume: Float, onVolumeChange: (Float) -> Unit, onDismiss: () -> Unit) {
    ModalBottomSheet(onDismissRequest = onDismiss) {
        Text(
            "Music volume (${(volume * 100).roundToInt()}%)",
            style = MaterialTheme.typography.titleMedium,
            modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
        )
        CleanSlider(
            value = volume,
            onValueChange = onVolumeChange,
            valueRange = 0f..1f,
            steps = 19,
            startLabel = "0%",
            endLabel = "100%",
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 16.dp)
                .padding(bottom = 24.dp),
        )
    }
}

private fun formatSpeed(speed: Float): String =
    if (speed == speed.toInt().toFloat()) speed.toInt().toString() else speed.toString()

/** Mirrors frontend/src/components/PlayerBar.tsx's own wordCount. */
internal fun wordCount(text: String): Int {
    val trimmed = text.trim()
    return if (trimmed.isEmpty()) 0 else trimmed.split(Regex("\\s+")).size
}

/**
 * Mirrors frontend's PlayerBar.tsx `.player-track`: a played-progress fill over a lighter
 * "buffered ahead" segment showing the run of already-generated audio ahead of [progress], so
 * the bar communicates both "how far you've listened" and "how far the narrator has caught up."
 * Material3's `LinearProgressIndicator` has no second/buffered track, hence drawing this
 * directly - two start-aligned boxes rather than an offset segment: painting the (wider)
 * buffered layer first and the (narrower) played fill on top of it gives the same visual result
 * as positioning a separate buffered-only segment, with simpler width math.
 */
@Composable
private fun ChapterProgressBar(progress: Float, bufferedAheadEnd: Float, modifier: Modifier = Modifier) {
    Box(
        modifier = modifier
            .height(4.dp)
            .background(MaterialTheme.colorScheme.surfaceVariant),
    ) {
        Box(
            modifier = Modifier
                .fillMaxHeight()
                .fillMaxWidth(bufferedAheadEnd.coerceIn(0f, 1f))
                .background(MaterialTheme.colorScheme.primary.copy(alpha = 0.35f)),
        )
        Box(
            modifier = Modifier
                .fillMaxHeight()
                .fillMaxWidth(progress.coerceIn(0f, 1f))
                .background(MaterialTheme.colorScheme.primary),
        )
    }
}
