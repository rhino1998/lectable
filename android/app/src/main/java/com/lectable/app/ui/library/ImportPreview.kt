package com.lectable.app.ui.library

import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.itemsIndexed
import androidx.compose.foundation.selection.toggleable
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Close
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import com.lectable.app.data.remote.dto.ImportChapterSources
import com.lectable.app.data.remote.dto.ImportPreviewChapterDto
import com.lectable.app.data.remote.dto.ImportPreviewResponseDto

// "Import from a link" -> "Choose chapters…": the link looked over before anything is imported
// (backend httpapi/source_preview.go) - the work's chapter list to pick from, which can be
// widened with the Internet Archive's copies of delisted chapters and with an epub standing in
// for chapters the site won't serve. The decisions are the plain functions at the top
// (ImportPreviewTest); the dialog below only draws them.

/** Whether a chapter starts ticked: one that can be fetched and doesn't read like an author's
 *  notice (`suspect`). Either can still be ticked by hand, but an import stops at a locked one. */
private fun tickedByDefault(chapter: ImportPreviewChapterDto): Boolean =
    chapter.source != ImportChapterSources.LOCKED && !chapter.suspect

/** What a fresh preview starts with ticked. */
internal fun defaultSelection(chapters: List<ImportPreviewChapterDto>): Set<String> =
    chapters.filter(::tickedByDefault).mapTo(HashSet()) { it.id }

/** The selection carried over to a widened preview's list: what was ticked stays ticked, what
 *  was unticked stays unticked, and chapters that are new to the list start as they would in a
 *  fresh preview - as does one that was locked and now has a source (the attached epub has it). */
internal fun mergeSelection(
    previous: List<ImportPreviewChapterDto>,
    selected: Set<String>,
    next: List<ImportPreviewChapterDto>,
): Set<String> {
    val sourceBefore = previous.associate { it.id to it.source }
    return next.filter { chapter ->
        val unlocked = sourceBefore[chapter.id] == ImportChapterSources.LOCKED && chapter.source != ImportChapterSources.LOCKED
        if (chapter.id !in sourceBefore || unlocked) tickedByDefault(chapter) else chapter.id in selected
    }.mapTo(HashSet()) { it.id }
}

/** The 1-based, inclusive positions the range fields name in a list of [count] chapters, or
 *  null when they name none. An empty field means that end of the list, the two may be given
 *  in either order, and a range running past the list is cut to it. */
internal fun parseRange(from: String, to: String, count: Int): IntRange? {
    if (from.isBlank() && to.isBlank()) return null
    val a = if (from.isBlank()) 1 else from.trim().toIntOrNull() ?: return null
    val b = if (to.isBlank()) count else to.trim().toIntOrNull() ?: return null
    val low = minOf(a, b).coerceAtLeast(1)
    val high = maxOf(a, b).coerceAtMost(count)
    return if (low > high) null else low..high
}

/** Exactly the chapters at [range]'s 1-based positions, whatever was ticked before. */
internal fun selectRange(chapters: List<ImportPreviewChapterDto>, range: IntRange): Set<String> =
    chapters.filterIndexed { index, _ -> index + 1 in range }.mapTo(HashSet()) { it.id }

/** The `chapters` of the import request: the ticked ids in list order, or null when every
 *  chapter is ticked (the backend's "all of them", which leaves nothing marked as left out). */
internal fun importChapterIds(chapters: List<ImportPreviewChapterDto>, selected: Set<String>): List<String>? {
    val ids = chapters.filter { it.id in selected }.map { it.id }
    return if (ids.size == chapters.size) null else ids
}

/** How many previewed chapters come from where, and how many of them (from anywhere) read
 *  like notices. A source this build doesn't know counts toward [total] only. */
internal data class PreviewCounts(
    val total: Int = 0,
    val site: Int = 0,
    val archive: Int = 0,
    val epub: Int = 0,
    val locked: Int = 0,
    val suspect: Int = 0,
)

internal fun previewCounts(chapters: List<ImportPreviewChapterDto>): PreviewCounts {
    var site = 0
    var archive = 0
    var epub = 0
    var locked = 0
    for (chapter in chapters) {
        when (chapter.source) {
            ImportChapterSources.SITE -> site++
            ImportChapterSources.ARCHIVE -> archive++
            ImportChapterSources.EPUB -> epub++
            ImportChapterSources.LOCKED -> locked++
            else -> Unit
        }
    }
    return PreviewCounts(
        total = chapters.size,
        site = site,
        archive = archive,
        epub = epub,
        locked = locked,
        suspect = chapters.count { it.suspect },
    )
}

/** "1204 chapters: 900 from Royal Road, 300 from the Internet Archive, 4 locked, 3 look like
 *  notices". */
internal fun previewSummary(counts: PreviewCounts, site: String): String {
    val parts = listOfNotNull(
        "${counts.site} from ${site.ifEmpty { "the site" }}".takeIf { counts.site > 0 },
        "${counts.archive} from the Internet Archive".takeIf { counts.archive > 0 },
        "${counts.epub} from the epub".takeIf { counts.epub > 0 },
        "${counts.locked} locked".takeIf { counts.locked > 0 },
        (if (counts.suspect == 1) "1 looks like a notice" else "${counts.suspect} look like notices").takeIf { counts.suspect > 0 },
    )
    val total = chapterCount(counts.total)
    return if (parts.isEmpty()) total else "$total: ${parts.joinToString(", ")}"
}

internal fun chapterCount(n: Int): String = "$n chapter${if (n == 1) "" else "s"}"

/** A row's label for where its chapter comes from - nothing for the usual case, the site. A
 *  chapter with `replacedBy` is one the site has since put something else in the place of (a
 *  notice, a summary - the next row); this is the archive's copy of it from before. */
internal fun sourceLabel(source: String, replacedBy: String? = null): String? = when {
    !replacedBy.isNullOrEmpty() -> "$source · replaced on the site"
    source == ImportChapterSources.SITE || source.isEmpty() -> null
    else -> source
}

/** The preview, whenever [LibraryUiState] has one open (being looked up, listed, or failed). */
@Composable
fun ImportPreviewHost(uiState: LibraryUiState, viewModel: LibraryViewModel) {
    if (uiState.preview == null && uiState.previewBusy == null && uiState.previewError == null) return
    val context = LocalContext.current
    val selected by viewModel.previewSelection.collectAsState()
    val pickEpubLauncher = rememberLauncherForActivityResult(ActivityResultContracts.GetContent()) { uri ->
        if (uri != null) viewModel.attachPreviewEpub(context.contentResolver, uri, context.cacheDir)
    }
    ImportPreviewDialog(
        preview = uiState.preview,
        busy = uiState.previewBusy,
        error = uiState.previewError,
        selected = selected,
        onSelectionChange = viewModel::setPreviewSelection,
        onSearchArchive = viewModel::searchPreviewArchive,
        onAttachEpub = { pickEpubLauncher.launch("application/epub+zip") },
        onImport = viewModel::importPreviewSelection,
        onDismiss = viewModel::dismissPreview,
    )
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ImportPreviewDialog(
    preview: ImportPreviewResponseDto?,
    busy: PreviewBusy?,
    error: String?,
    selected: Set<String>,
    onSelectionChange: (Set<String>) -> Unit,
    onSearchArchive: () -> Unit,
    onAttachEpub: () -> Unit,
    onImport: () -> Unit,
    onDismiss: () -> Unit,
) {
    Dialog(onDismissRequest = onDismiss, properties = DialogProperties(usePlatformDefaultWidth = false)) {
        Scaffold(
            modifier = Modifier.fillMaxSize(),
            topBar = {
                TopAppBar(
                    title = {
                        Column {
                            Text(
                                preview?.title?.ifEmpty { null } ?: "Choose chapters",
                                maxLines = 1,
                                overflow = TextOverflow.Ellipsis,
                            )
                            val byline = preview?.let { listOf(it.author, it.site).filter(String::isNotEmpty).joinToString(" · ") }
                            if (!byline.isNullOrEmpty()) {
                                Text(
                                    byline,
                                    style = MaterialTheme.typography.bodySmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    maxLines = 1,
                                    overflow = TextOverflow.Ellipsis,
                                )
                            }
                        }
                    },
                    navigationIcon = {
                        IconButton(onClick = onDismiss) { Icon(Icons.Filled.Close, contentDescription = "Cancel") }
                    },
                )
            },
            bottomBar = {
                if (preview != null) {
                    val count = remember(preview.chapters, selected) { preview.chapters.count { it.id in selected } }
                    Surface(tonalElevation = 3.dp) {
                        Column(modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp)) {
                            if (error != null) {
                                Text(error, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
                            }
                            Row(
                                modifier = Modifier.fillMaxWidth(),
                                horizontalArrangement = Arrangement.spacedBy(8.dp, Alignment.End),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                TextButton(onClick = onDismiss) { Text("Cancel") }
                                Button(
                                    onClick = onImport,
                                    enabled = count > 0 && busy == null && preview.existingBookId.isNullOrEmpty(),
                                ) {
                                    Text(if (busy == PreviewBusy.IMPORTING) "Importing…" else "Import ${chapterCount(count)}")
                                }
                            }
                        }
                    }
                }
            },
        ) { padding ->
            if (preview == null) {
                // Nothing to list: the link is still being looked up, or that failed.
                Column(
                    modifier = Modifier.fillMaxSize().padding(padding).padding(32.dp),
                    verticalArrangement = Arrangement.spacedBy(16.dp, Alignment.CenterVertically),
                    horizontalAlignment = Alignment.CenterHorizontally,
                ) {
                    if (error != null) {
                        Text(error, color = MaterialTheme.colorScheme.error, textAlign = TextAlign.Center)
                        TextButton(onClick = onDismiss) { Text("Close") }
                    } else {
                        CircularProgressIndicator()
                        Text("Looking up the link…")
                    }
                }
            } else {
                PreviewChapterList(
                    preview = preview,
                    busy = busy,
                    selected = selected,
                    onSelectionChange = onSelectionChange,
                    onSearchArchive = onSearchArchive,
                    onAttachEpub = onAttachEpub,
                    modifier = Modifier.fillMaxSize().padding(padding),
                )
            }
        }
    }
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun PreviewChapterList(
    preview: ImportPreviewResponseDto,
    busy: PreviewBusy?,
    selected: Set<String>,
    onSelectionChange: (Set<String>) -> Unit,
    onSearchArchive: () -> Unit,
    onAttachEpub: () -> Unit,
    modifier: Modifier = Modifier,
) {
    val chapters = preview.chapters
    val counts = remember(chapters) { previewCounts(chapters) }
    // One toggle callback for the life of the list, reading the selection as it is when a row
    // is tapped - a fresh lambda per selection change would recompose every visible row.
    val currentSelected by rememberUpdatedState(selected)
    val currentOnSelectionChange by rememberUpdatedState(onSelectionChange)
    val onToggle = remember {
        { id: String ->
            currentOnSelectionChange(if (id in currentSelected) currentSelected - id else currentSelected + id)
        }
    }
    var rangeFrom by rememberSaveable { mutableStateOf("") }
    var rangeTo by rememberSaveable { mutableStateOf("") }
    val range = parseRange(rangeFrom, rangeTo, chapters.size)

    LazyColumn(modifier = modifier) {
        item(key = "summary", contentType = "header") {
            Column(
                modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                Text(previewSummary(counts, preview.site), style = MaterialTheme.typography.bodyMedium)
                if (!preview.existingBookId.isNullOrEmpty()) {
                    Text(
                        "This book is already in your library - check it for new chapters instead of importing it again.",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.error,
                    )
                }
                if (counts.locked > 0) {
                    Text(
                        "Locked chapters are listed by ${preview.site.ifEmpty { "the site" }} but not served; an import stops at the first one it is asked for.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                if (busy == PreviewBusy.SEARCHING_ARCHIVE) {
                    LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                    Text(
                        "Searching the Internet Archive - this can take up to a minute…",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                } else if (preview.canSearchArchive) {
                    OutlinedButton(onClick = onSearchArchive, enabled = busy == null, modifier = Modifier.fillMaxWidth()) {
                        Text("Search the Internet Archive for delisted chapters")
                    }
                    Text(
                        "Slow - up to a minute.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                } else if (preview.archiveSearched && counts.archive == 0) {
                    Text(
                        "The Internet Archive has no delisted chapters of this work.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                if (busy == PreviewBusy.ATTACHING_EPUB) {
                    LinearProgressIndicator(modifier = Modifier.fillMaxWidth())
                    Text(
                        "Reading the epub…",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                } else {
                    OutlinedButton(onClick = onAttachEpub, enabled = busy == null, modifier = Modifier.fillMaxWidth()) {
                        Text(if (preview.epubChapters > 0) "Attach a different epub…" else "Attach an epub…")
                    }
                }
                if (preview.epubChapters > 0) {
                    Text(
                        "Epub attached (${chapterCount(preview.epubChapters)}): " +
                            if (counts.epub > 0) "it fills ${counts.epub} the site won't serve." else "none match a chapter the site won't serve.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        }
        // Stays in view while the list scrolls: with a thousand chapters the helpers are the
        // only practical way to pick.
        stickyHeader(key = "select", contentType = "select") {
            Surface(tonalElevation = 1.dp) {
                Column(modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp)) {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Text(
                            "${selected.size} of ${chapters.size} selected",
                            style = MaterialTheme.typography.labelLarge,
                            modifier = Modifier.weight(1f),
                        )
                        TextButton(onClick = { onSelectionChange(chapters.mapTo(HashSet()) { it.id }) }) { Text("All") }
                        TextButton(onClick = { onSelectionChange(emptySet()) }) { Text("None") }
                    }
                    Row(
                        verticalAlignment = Alignment.CenterVertically,
                        horizontalArrangement = Arrangement.spacedBy(8.dp),
                    ) {
                        RangeField(rangeFrom, { rangeFrom = it }, label = "From", modifier = Modifier.weight(1f))
                        RangeField(rangeTo, { rangeTo = it }, label = "To", modifier = Modifier.weight(1f))
                        TextButton(
                            onClick = { range?.let { onSelectionChange(selectRange(chapters, it)) } },
                            enabled = range != null,
                        ) { Text("Select range") }
                    }
                }
            }
        }
        itemsIndexed(chapters, key = { _, chapter -> chapter.id }, contentType = { _, _ -> "chapter" }) { index, chapter ->
            PreviewChapterRow(
                number = index + 1,
                chapter = chapter,
                selected = chapter.id in selected,
                onToggle = onToggle,
            )
        }
    }
}

/** One end of the range: a position in the list, digits only. */
@Composable
private fun RangeField(value: String, onValueChange: (String) -> Unit, label: String, modifier: Modifier = Modifier) {
    OutlinedTextField(
        value = value,
        onValueChange = { onValueChange(it.filter(Char::isDigit).take(6)) },
        label = { Text(label) },
        singleLine = true,
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
        modifier = modifier,
    )
}

@Composable
private fun PreviewChapterRow(number: Int, chapter: ImportPreviewChapterDto, selected: Boolean, onToggle: (String) -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .toggleable(value = selected, role = Role.Checkbox, onValueChange = { onToggle(chapter.id) })
            .padding(horizontal = 16.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        // The row is the toggle; the box only shows it.
        Checkbox(checked = selected, onCheckedChange = null)
        Text(
            number.toString(),
            style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            textAlign = TextAlign.End,
            modifier = Modifier.widthIn(min = 32.dp),
        )
        Text(
            chapter.title,
            style = MaterialTheme.typography.bodyMedium,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            modifier = Modifier.weight(1f),
        )
        // Reads like an author's notice, not a chapter: unticked unless the reader says so.
        if (chapter.suspect) {
            Text("notice?", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.tertiary)
        }
        sourceLabel(chapter.source, chapter.replacedBy)?.let { label ->
            Text(
                label,
                style = MaterialTheme.typography.labelSmall,
                color = if (chapter.source == ImportChapterSources.LOCKED) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.primary,
            )
        }
    }
}
