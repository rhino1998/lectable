package com.lectable.app.ui

/** Mirrors frontend/src/components/PlayerBar.tsx's formatTime - m:ss, clamped to non-negative. */
fun formatTime(seconds: Double): String {
    if (!seconds.isFinite() || seconds < 0) return "0:00"
    val m = (seconds / 60).toInt()
    val s = (seconds % 60).toInt()
    return "$m:${s.toString().padStart(2, '0')}"
}

/** Mirrors frontend/src/utils/time.ts's formatDurationLong - e.g. "4h 12m" or "38m". */
fun formatDurationLong(totalSeconds: Double): String {
    if (!totalSeconds.isFinite() || totalSeconds <= 0) return "—"
    val totalMinutes = Math.round(totalSeconds / 60)
    val hours = totalMinutes / 60
    val minutes = totalMinutes % 60
    return when {
        hours == 0L -> "${minutes}m"
        minutes == 0L -> "${hours}h"
        else -> "${hours}h ${minutes}m"
    }
}
