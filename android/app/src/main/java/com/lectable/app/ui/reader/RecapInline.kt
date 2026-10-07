package com.lectable.app.ui.reader

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ElevatedCard
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.unit.dp
import com.lectable.app.data.remote.dto.RecapKinds
import com.lectable.app.data.remote.dto.RecapSegmentDto
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive

/**
 * The offer a reader sees on coming back to a book after a while, or on starting one with a
 * book before it in its series - see [shouldOfferRecap]. Sits over the top of the reader until
 * answered or until the book plays. The Android twin of frontend's RecapPanel banner.
 */
@Composable
fun RecapOfferCard(
    state: RecapUiState,
    onPlay: () -> Unit,
    onRead: () -> Unit,
    onDismiss: () -> Unit,
    modifier: Modifier = Modifier,
) {
    val recap = state.recap ?: return
    ElevatedCard(modifier = modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 8.dp)) {
        Column(modifier = Modifier.padding(start = 16.dp, end = 4.dp, top = 4.dp, bottom = 8.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Column(modifier = Modifier.weight(1f)) {
                    Text(
                        if (recap.kind == RecapKinds.SERIES) "Before you start" else "Welcome back",
                        style = MaterialTheme.typography.titleSmall,
                    )
                    Text(
                        when {
                            recap.kind == RecapKinds.SERIES -> "What happened in ${recap.title}"
                            state.lastReadAtOnOpen != null ->
                                "You last listened ${agoLabel(state.lastReadAtOnOpen, state.openedAtMs)}. Previously, in ${recap.title}…"
                            else -> "Previously, in ${recap.title}…"
                        },
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                IconButton(onClick = onDismiss) { Icon(Icons.Filled.Close, contentDescription = "Not now") }
            }
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                if (recap.audioUrl != null) {
                    Button(onClick = onPlay) {
                        Icon(Icons.Filled.PlayArrow, contentDescription = null)
                        Spacer(Modifier.width(4.dp))
                        Text("Play recap")
                    }
                }
                TextButton(onClick = onRead) { Text("Read it") }
            }
        }
    }
}

/**
 * The "previously on" recap of where the reader is, set into the text above the paragraph they
 * were at (ReaderListEntry.RecapItem): its text, with the word being spoken highlighted
 * while its clip plays (driven by the playback bar - see ReaderViewModel.togglePlayPause - and
 * handing back to the book when it ends), writing it (again), and how long away brings the offer. Opened from the offer or
 * the reader's menu. The Android twin of frontend's RecapPanel.
 */
@Composable
fun RecapInline(
    state: RecapUiState,
    // Seconds into the recap's clip, read while it plays.
    positionSeconds: () -> Double,
    onClose: () -> Unit,
    onPrepare: (force: Boolean) -> Unit,
    onSetAfterMinutes: (Int) -> Unit,
) {
    val recap = state.recap
    ElevatedCard(modifier = Modifier.fillMaxWidth()) {
        Column(modifier = Modifier.fillMaxWidth().padding(start = 16.dp, end = 8.dp, top = 4.dp, bottom = 8.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    if (state.current && recap != null) "Previously, in ${recap.title}" else "Previously…",
                    style = MaterialTheme.typography.titleMedium,
                    modifier = Modifier.weight(1f),
                )
                IconButton(onClick = onClose) { Icon(Icons.Filled.Close, contentDescription = "Close the recap") }
            }
            when {
                state.current && recap != null -> {
                    // No controls of its own: the playback bar's play/pause drives its clip.
                    if (recap.audioUrl == null) {
                        Text(
                            if (state.preparing) "Its narration is on the way…" else "Not narrated yet.",
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                    if (recap.audioUrl != null && recap.segments.isNotEmpty()) {
                        RecapSpokenText(recap.segments, state.playing, positionSeconds)
                    } else {
                        recap.text.split("\n\n").forEach { para ->
                            Text(para, style = MaterialTheme.typography.bodyLarge, modifier = Modifier.padding(top = 12.dp, end = 8.dp))
                        }
                    }
                }
                state.preparing -> Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.padding(top = 8.dp)) {
                    CircularProgressIndicator(modifier = Modifier.width(20.dp), strokeWidth = 2.dp)
                    Spacer(Modifier.width(12.dp))
                    Text("Writing the recap…")
                }
                recap?.error != null && recap.chapterIdx == state.pos.first -> Text(
                    "The recap couldn’t be written: ${recap.error}",
                    color = MaterialTheme.colorScheme.error,
                    modifier = Modifier.padding(top = 8.dp),
                )
                else -> Text("Nothing to recap here yet.", color = MaterialTheme.colorScheme.onSurfaceVariant, modifier = Modifier.padding(top = 8.dp))
            }
            Row(
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.SpaceBetween,
                modifier = Modifier.fillMaxWidth().padding(top = 8.dp),
            ) {
                TextButton(onClick = { onPrepare(true) }, enabled = !state.preparing) {
                    Text(if (state.preparing) "Working…" else if (state.current) "Write it again" else "Write one")
                }
                Box {
                    var showOptions by remember { mutableStateOf(false) }
                    val label = RECAP_AFTER_OPTIONS.firstOrNull { it.first == state.afterMinutes }?.second ?: "after a day away"
                    TextButton(onClick = { showOptions = true }) { Text("Offer $label") }
                    DropdownMenu(expanded = showOptions, onDismissRequest = { showOptions = false }) {
                        RECAP_AFTER_OPTIONS.forEach { (minutes, optionLabel) ->
                            DropdownMenuItem(
                                text = { Text(optionLabel.replaceFirstChar { it.uppercase() }) },
                                onClick = { onSetAfterMinutes(minutes); showOptions = false },
                            )
                        }
                    }
                }
            }
        }
    }
}

/** The recap as it is spoken - a paragraph per stretch of the clip - with the word being spoken
 *  highlighted while it plays (see [recapWordTimes]). */
@Composable
private fun RecapSpokenText(segments: List<RecapSegmentDto>, playing: Boolean, positionSeconds: () -> Double) {
    val tokens = remember(segments) { segments.map { tokenizeWords(it.text) } }
    val times = remember(segments) { segments.flatMap(::recapWordTimes) }
    // -1 until the clip has played; where it stopped, after.
    var active by remember(segments) { mutableStateOf(-1) }
    LaunchedEffect(playing, times) {
        while (playing && isActive) {
            active = activeWordIndex(times, positionSeconds())
            delay(80)
        }
    }
    val highlight = SpanStyle(background = MaterialTheme.colorScheme.primary, color = MaterialTheme.colorScheme.onPrimary)
    var base = 0
    segments.forEachIndexed { si, seg ->
        val word = tokens[si].getOrNull(active - base)
        base += tokens[si].size
        Text(
            buildAnnotatedString {
                append(seg.text)
                if (word != null) addStyle(highlight, word.start, word.end)
            },
            style = MaterialTheme.typography.bodyLarge,
            modifier = Modifier.padding(top = 12.dp, end = 8.dp),
        )
    }
}
