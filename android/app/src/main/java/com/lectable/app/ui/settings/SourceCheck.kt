package com.lectable.app.ui.settings

// Settings' "Check for new chapters": how often the backend checks books imported from websites
// for new entries by itself (SourceSettingsDto.checkEveryHours, library-wide).

/** The intervals offered, in hours; 0 is never. */
internal val SOURCE_CHECK_HOURS = listOf(0, 6, 12, 24, 48, 168)

internal fun sourceCheckLabel(hours: Int): String = when (hours) {
    0 -> "Never"
    6 -> "Every 6 hours"
    12 -> "Every 12 hours"
    24 -> "Daily"
    48 -> "Every 2 days"
    168 -> "Weekly"
    else -> if (hours == 1) "Every hour" else "Every $hours hours"
}

/** The menu's choices: the offered intervals, plus what is stored when it is none of them (set
 *  from elsewhere), in its place by length - "never" first. */
internal fun sourceCheckChoices(current: Int?): List<Int> =
    if (current == null || current < 0 || current in SOURCE_CHECK_HOURS) SOURCE_CHECK_HOURS
    else (SOURCE_CHECK_HOURS + current).sorted()
