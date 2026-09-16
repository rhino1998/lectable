package com.lectable.app.ui.components

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Slider
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp

private val TrackHeight = 14.dp
private val ThumbRadius = 7.dp

/**
 * A minimal-looking discrete slider - Material3's own default `Slider` draws a thick track with a
 * small stop-indicator dot at *every* step, which reads as busy/cluttered at a fine-grained step
 * count (e.g. quarter-point increments across a wide range); this instead matches
 * frontend/src/components/VoiceEditorForm.tsx's `.speed-slider` as closely as Compose's
 * primitives allow: a hairline track filled solid up to the thumb, with a small plain dot thumb
 * and no ripple/halo (same as a bare `<input type="range">` thumb has none), plus optional
 * [startLabel]/[endLabel] captions below rather than one label per step.
 *
 * Track and thumb are drawn together in one [Canvas] (via the `track` slot; the `thumb` slot
 * itself is left empty) rather than as Material3's usual two independently-positioned composables
 * - both then key off the exact same `size.height / 2f`, which is what guarantees the dot actually
 * sits centered on the line rather than depending on Slider's own thumb/track layout matching up
 * with each other at whatever custom sizes are passed in.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun CleanSlider(
    value: Float,
    onValueChange: (Float) -> Unit,
    valueRange: ClosedFloatingPointRange<Float>,
    steps: Int,
    modifier: Modifier = Modifier,
    startLabel: String? = null,
    endLabel: String? = null,
) {
    val fillColor = MaterialTheme.colorScheme.primary
    val trackColor = MaterialTheme.colorScheme.outlineVariant
    val labelColor = MaterialTheme.colorScheme.onSurfaceVariant
    Column(modifier = modifier) {
        Slider(
            value = value,
            onValueChange = onValueChange,
            valueRange = valueRange,
            steps = steps,
            modifier = Modifier.fillMaxWidth(),
            thumb = {},
            track = { sliderState ->
                val fraction = (sliderState.value - valueRange.start) / (valueRange.endInclusive - valueRange.start)
                Canvas(modifier = Modifier.fillMaxWidth().height(TrackHeight)) {
                    val y = size.height / 2f
                    val strokeWidth = 2.dp.toPx()
                    drawLine(trackColor, Offset(0f, y), Offset(size.width, y), strokeWidth = strokeWidth, cap = StrokeCap.Round)
                    drawLine(
                        fillColor,
                        Offset(0f, y),
                        Offset(size.width * fraction, y),
                        strokeWidth = strokeWidth,
                        cap = StrokeCap.Round,
                    )
                    drawCircle(fillColor, radius = ThumbRadius.toPx(), center = Offset(size.width * fraction, y))
                }
            },
        )
        if (startLabel != null || endLabel != null) {
            Row(modifier = Modifier.fillMaxWidth().padding(top = 2.dp)) {
                Text(startLabel.orEmpty(), style = MaterialTheme.typography.labelSmall, color = labelColor, modifier = Modifier.weight(1f))
                Text(
                    endLabel.orEmpty(),
                    style = MaterialTheme.typography.labelSmall,
                    color = labelColor,
                    textAlign = TextAlign.End,
                    modifier = Modifier.weight(1f),
                )
            }
        }
    }
}
