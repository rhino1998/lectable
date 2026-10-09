package com.lectable.app.ui.library

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.selection.toggleable
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Checkbox
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.lectable.app.data.remote.dto.SourceReviewEntryDto

// A book imported from a website only takes entries its reader agreed to (backend
// httpapi/source_review.go): what a check for new chapters finds waits in BookSourceDto.review
// until the reader says import or skip, here. The decisions are the plain functions at the top
// (SourceReviewTest); the dialog below only draws them.

/** Entries to import and to skip, as the review request takes them. */
internal data class ReviewDecision(val import: List<String>, val skip: List<String>)

/** Whether an entry is set to import: the reader's own choice for it in [choices] (id ->
 *  import), else import unless its title reads like an author's notice or it is a chapter that
 *  couldn't be read (`unavailable` - the Internet Archive has no usable copy; importing only
 *  tries again). */
internal fun reviewImports(entry: SourceReviewEntryDto, choices: Map<String, Boolean>): Boolean =
    choices[entry.id] ?: (!entry.suspect && !entry.unavailable)

/** Every entry as either import or skip, each list in reading order - none is left waiting. */
internal fun reviewDecision(entries: List<SourceReviewEntryDto>, choices: Map<String, Boolean>): ReviewDecision {
    val (import, skip) = entries.partition { reviewImports(it, choices) }
    return ReviewDecision(import = import.map { it.id }, skip = skip.map { it.id })
}

/** "1 new entry" / "3 new entries" - entries, not chapters: some of them are notices. */
internal fun newEntries(n: Int): String = "$n new entr${if (n == 1) "y" else "ies"}"

/** The count on a cover's review badge, or null when it wouldn't fit beside the bell on a 48dp
 *  cover (the badge's description still says it). */
internal fun reviewBadgeCount(n: Int): String? = if (n in 1..99) n.toString() else null

/** The snackbar after "Check all for new chapters", for the [queued] books being checked. */
internal fun checkingSourcesMessage(queued: Int): String = when {
    queued <= 0 -> "Nothing to check"
    queued == 1 -> "Checking 1 book for new chapters"
    else -> "Checking $queued books for new chapters"
}

// "Import all"/"Skip all" only earn their row past this many entries.
private const val REVIEW_SHORTCUTS_MIN = 4

/** The long-press menu's "Review N new entries": a tick per entry means import it. */
@Composable
internal fun SourceReviewDialog(
    bookTitle: String,
    entries: List<SourceReviewEntryDto>,
    onApply: (ReviewDecision) -> Unit,
    onDismiss: () -> Unit,
) {
    // Only what the reader changed, so entries a check adds meanwhile get their defaults.
    var choices by remember { mutableStateOf<Map<String, Boolean>>(emptyMap()) }
    val importCount = entries.count { reviewImports(it, choices) }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("New entries in $bookTitle") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text(
                    "Entries wait here because authors post announcements in the chapter list. " +
                        "Ticked ones are imported; the rest are skipped and left out for good.",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                if (entries.size >= REVIEW_SHORTCUTS_MIN) {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Text(
                            "$importCount of ${entries.size} to import",
                            style = MaterialTheme.typography.labelLarge,
                            modifier = Modifier.weight(1f),
                        )
                        TextButton(onClick = { choices = entries.associate { it.id to true } }) { Text("Import all") }
                        TextButton(onClick = { choices = entries.associate { it.id to false } }) { Text("Skip all") }
                    }
                }
                LazyColumn(modifier = Modifier.heightIn(max = 360.dp)) {
                    items(entries, key = { it.id }) { entry ->
                        val import = reviewImports(entry, choices)
                        Row(
                            modifier = Modifier
                                .fillMaxWidth()
                                .toggleable(value = import, role = Role.Checkbox, onValueChange = { choices = choices + (entry.id to it) })
                                .padding(vertical = 8.dp),
                            verticalAlignment = Alignment.CenterVertically,
                            horizontalArrangement = Arrangement.spacedBy(12.dp),
                        ) {
                            Checkbox(checked = import, onCheckedChange = null)
                            Text(
                                entry.title,
                                style = MaterialTheme.typography.bodyMedium,
                                maxLines = 2,
                                overflow = TextOverflow.Ellipsis,
                                modifier = Modifier.weight(1f),
                            )
                            if (entry.suspect) {
                                Text("notice?", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.tertiary)
                            }
                            if (entry.unavailable) {
                                Text("not in the archive", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.error)
                            }
                            Text(
                                if (import) "import" else "skip",
                                style = MaterialTheme.typography.labelSmall,
                                color = if (import) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant,
                            )
                        }
                    }
                }
            }
        },
        confirmButton = { TextButton(onClick = { onApply(reviewDecision(entries, choices)) }) { Text("Apply") } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}
