package com.lectable.app.ui.library

import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.background
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.AutoAwesome
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Download
import androidx.compose.material.icons.filled.DownloadDone
import androidx.compose.material.icons.filled.Headphones
import androidx.compose.material.icons.filled.MenuBook
import androidx.compose.material.icons.filled.RecordVoiceOver
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.ExtendedFloatingActionButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Snackbar
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import coil.compose.AsyncImage
import com.lectable.app.R
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import com.lectable.app.data.remote.dto.BookSummaryDto
import com.lectable.app.ui.components.ConfirmDialog
import com.lectable.app.ui.formatDurationLong
import kotlin.math.roundToInt

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun LibraryScreen(
    onOpenBook: (String) -> Unit,
    onOpenSettings: () -> Unit,
    onOpenSpeakers: (String) -> Unit,
    viewModel: LibraryViewModel = hiltViewModel(),
) {
    val uiState by viewModel.uiState.collectAsState()
    val snackbarHostState = remember { SnackbarHostState() }
    val context = LocalContext.current
    var pendingDelete by remember { mutableStateOf<BookSummaryDto?>(null) }
    var pendingDeleteLocal by remember { mutableStateOf<BookSummaryDto?>(null) }
    val libraryEntries = remember(uiState.books) { groupBySeries(uiState.books) }

    val pickEpubLauncher = rememberLauncherForActivityResult(ActivityResultContracts.GetContent()) { uri ->
        if (uri != null) viewModel.uploadBook(context.contentResolver, uri, context.cacheDir)
    }

    LaunchedEffect(uiState.error) {
        uiState.error?.let {
            snackbarHostState.showSnackbar(it)
            viewModel.dismissError()
        }
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text(stringResource(R.string.app_name)) },
                actions = {
                    IconButton(onClick = onOpenSettings) {
                        Icon(Icons.Filled.Settings, contentDescription = "Settings")
                    }
                },
            )
        },
        floatingActionButton = {
            ExtendedFloatingActionButton(
                text = { Text(if (uiState.isUploading) "Uploading…" else "Add book") },
                icon = { Icon(Icons.Filled.Add, contentDescription = null) },
                onClick = { pickEpubLauncher.launch("application/epub+zip") },
            )
        },
        snackbarHost = { SnackbarHost(snackbarHostState) { Snackbar(it) } },
    ) { padding ->
        PullToRefreshBox(
            isRefreshing = uiState.isLoading,
            onRefresh = viewModel::refresh,
            modifier = Modifier.fillMaxSize().padding(padding),
        ) {
            // A single scrollable LazyColumn throughout (rather than swapping in a bare
            // Box/Text for the empty/loading states) so PullToRefreshBox's nested-scroll
            // connection always has something to receive the drag from - a non-scrolling
            // child wouldn't dispatch the scroll events the gesture relies on.
            LazyColumn(modifier = Modifier.fillMaxSize()) {
                when {
                    uiState.isLoading && uiState.books.isEmpty() -> {
                        // No spinner here - PullToRefreshBox's own indicator (isRefreshing =
                        // uiState.isLoading, above) already shows one; a second one stacked on
                        // top of it was showing two at once during the very first load. Still
                        // one real item, not zero, so PullToRefreshBox's nested-scroll
                        // connection always has something to receive the drag from - see this
                        // LazyColumn's own doc comment above.
                        item { Box(modifier = Modifier.fillParentMaxSize()) }
                    }
                    uiState.books.isEmpty() -> {
                        item {
                            Box(modifier = Modifier.fillParentMaxSize(), contentAlignment = Alignment.Center) {
                                Text(
                                    "No books yet - tap \"Add book\" to upload an epub.",
                                    modifier = Modifier.padding(32.dp),
                                )
                            }
                        }
                    }
                    else -> {
                        if (uiState.offline) {
                            item {
                                Text(
                                    "Offline - showing downloaded books only",
                                    style = MaterialTheme.typography.labelMedium,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
                                )
                            }
                        }
                        items(
                            libraryEntries,
                            key = { entry ->
                                when (entry) {
                                    is LibraryListEntry.SeriesHeader -> "series:${entry.seriesName}"
                                    is LibraryListEntry.Book -> entry.item.summary.id
                                }
                            },
                        ) { entry ->
                            when (entry) {
                                is LibraryListEntry.SeriesHeader -> {
                                    Text(
                                        entry.seriesName,
                                        style = MaterialTheme.typography.titleSmall,
                                        modifier = Modifier.padding(start = 16.dp, end = 16.dp, top = 12.dp, bottom = 4.dp),
                                    )
                                }
                                is LibraryListEntry.Book -> {
                                    BookRow(
                                        item = entry.item,
                                        showSeriesName = entry.showSeriesName,
                                        isDownloaded = entry.item.summary.id in uiState.downloadedBookIds,
                                        onClick = { onOpenBook(entry.item.summary.id) },
                                        onPreprocess = { viewModel.preprocessBook(entry.item.summary.id) },
                                        onOpenSpeakers = { onOpenSpeakers(entry.item.summary.id) },
                                        onGenerate = { viewModel.generateBook(entry.item.summary.id) },
                                        onGenerateRemaining = { viewModel.generateBookRemaining(entry.item.summary.id) },
                                        onDownload = { viewModel.downloadBook(entry.item.summary.id) },
                                        onDownloadRemaining = { viewModel.downloadBookRemaining(entry.item.summary.id) },
                                        onDeleteLocal = { pendingDeleteLocal = entry.item.summary },
                                        onDelete = { pendingDelete = entry.item.summary },
                                    )
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    pendingDelete?.let { book ->
        ConfirmDialog(
            title = "Delete book?",
            text = "\"${book.title}\" and all its downloaded audio will be removed. This can't be undone.",
            onConfirm = { viewModel.deleteBook(book.id) },
            onDismiss = { pendingDelete = null },
        )
    }

    pendingDeleteLocal?.let { book ->
        ConfirmDialog(
            title = "Remove downloaded copy?",
            text = "\"${book.title}\"'s offline audio will be removed from this device. It stays in your " +
                "library and can be downloaded again anytime.",
            confirmLabel = "Remove",
            onConfirm = { viewModel.deleteLocalCopy(book.id) },
            onDismiss = { pendingDeleteLocal = null },
        )
    }
}

// One library list entry: either a titled section header for a series with 2+ books present
// (books inside it show only their own index, not the repeated series name), or an ordinary
// book row - either standalone or the sole present book of its series, which still shows its
// own series name/index inline since there's no section heading naming it. Mirrors
// frontend/src/pages/LibraryPage.tsx's LibraryItem/groupBySeries exactly, just flattened into
// one list (header + rows) instead of a grid + separate <section>s, since this screen is
// already a plain vertical list rather than a grid.
private sealed class LibraryListEntry {
    data class SeriesHeader(val seriesName: String) : LibraryListEntry()
    data class Book(val item: BookListItem, val showSeriesName: Boolean) : LibraryListEntry()
}

private fun groupBySeries(items: List<BookListItem>): List<LibraryListEntry> {
    val entries = mutableListOf<LibraryListEntry>()
    val seenSeries = mutableSetOf<String>()
    for (item in items) {
        val seriesName = item.summary.seriesName
        val seriesMates = if (!seriesName.isNullOrEmpty()) items.filter { it.summary.seriesName == seriesName } else emptyList()
        if (!seriesName.isNullOrEmpty() && seriesMates.size > 1) {
            if (!seenSeries.add(seriesName)) continue
            entries.add(LibraryListEntry.SeriesHeader(seriesName))
            seriesMates.sortedBy { it.summary.seriesIndex ?: 0.0 }
                .forEach { entries.add(LibraryListEntry.Book(it, showSeriesName = false)) }
            continue
        }
        entries.add(LibraryListEntry.Book(item, showSeriesName = true))
    }
    return entries
}

private fun formatSeriesIndex(value: Double): String =
    if (value == value.toLong().toDouble()) value.toLong().toString() else value.toString()

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun BookRow(
    item: BookListItem,
    showSeriesName: Boolean,
    isDownloaded: Boolean,
    onClick: () -> Unit,
    onPreprocess: () -> Unit,
    onOpenSpeakers: () -> Unit,
    onGenerate: () -> Unit,
    onGenerateRemaining: () -> Unit,
    onDownload: () -> Unit,
    onDownloadRemaining: () -> Unit,
    onDeleteLocal: () -> Unit,
    onDelete: () -> Unit,
) {
    val book = item.summary
    var showMenu by remember { mutableStateOf(false) }
    var showGenerateMenu by remember { mutableStateOf(false) }
    var showDownloadMenu by remember { mutableStateOf(false) }
    Box {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .combinedClickable(onClick = onClick, onLongClick = { showMenu = true })
                .padding(horizontal = 16.dp, vertical = 12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Box(
                modifier = Modifier
                    .size(width = 48.dp, height = 64.dp)
                    .clip(RoundedCornerShape(4.dp))
                    .background(MaterialTheme.colorScheme.surfaceVariant),
                contentAlignment = Alignment.Center,
            ) {
                if (item.cover != null) {
                    AsyncImage(model = item.cover, contentDescription = null, modifier = Modifier.fillMaxSize())
                } else {
                    Icon(Icons.Filled.MenuBook, contentDescription = null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
                }
                // Mirrors frontend's book-cover-processing overlay - a dimmed scrim + spinner
                // over the cover while the backend's whole-book preprocess meta-task (attribution
                // -> characterization -> voice provisioning -> direction tagging) is still
                // running, cleared automatically by LibraryViewModel's own poll once it finishes.
                if (book.preprocessing) {
                    Box(
                        modifier = Modifier
                            .matchParentSize()
                            .background(Color.Black.copy(alpha = 0.45f)),
                        contentAlignment = Alignment.Center,
                    ) {
                        CircularProgressIndicator(
                            modifier = Modifier.size(20.dp),
                            strokeWidth = 2.dp,
                            color = Color.White,
                        )
                    }
                }
            }

            Column(
                modifier = Modifier.weight(1f).padding(horizontal = 12.dp),
                verticalArrangement = Arrangement.spacedBy(4.dp),
            ) {
                Text(book.title, style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Text(
                    book.author,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
                if (!book.seriesName.isNullOrEmpty() && (showSeriesName || book.seriesIndex != null)) {
                    Text(
                        buildString {
                            if (showSeriesName) append(book.seriesName)
                            book.seriesIndex?.let {
                                if (showSeriesName) append(' ')
                                append('#').append(formatSeriesIndex(it))
                            }
                        },
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        maxLines = 1,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
                LinearProgressIndicator(
                    progress = { (book.progressPercent / 100.0).toFloat().coerceIn(0f, 1f) },
                    modifier = Modifier.fillMaxWidth(),
                    color = if (book.finished) Color(0xFF4CAF50) else MaterialTheme.colorScheme.primary,
                )
                Text(
                    bookProgressLabel(book),
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
        DropdownMenu(expanded = showMenu, onDismissRequest = { showMenu = false }) {
            DropdownMenuItem(
                text = { Text(if (book.preprocessing) "Preprocessing…" else "Preprocess") },
                leadingIcon = { Icon(Icons.Filled.AutoAwesome, contentDescription = null) },
                enabled = !book.preprocessing,
                onClick = { onPreprocess(); showMenu = false },
            )
            DropdownMenuItem(
                text = { Text("Speakers") },
                leadingIcon = { Icon(Icons.Filled.RecordVoiceOver, contentDescription = null) },
                onClick = { onOpenSpeakers(); showMenu = false },
            )
            // "Generate audio" opens a nested All/Remaining submenu instead of generating
            // directly - mirrors the web frontend's own popover on the library page's
            // "Generate audio" button (LibraryPage.tsx). "All" is onGenerate (whole book, same
            // as before this submenu existed); "Remaining" is onGenerateRemaining (from the
            // book's current stored reading position through the end).
            Box {
                DropdownMenuItem(
                    text = { Text("Generate audio") },
                    leadingIcon = { Icon(Icons.Filled.Headphones, contentDescription = null) },
                    onClick = { showGenerateMenu = true },
                )
                DropdownMenu(expanded = showGenerateMenu, onDismissRequest = { showGenerateMenu = false }) {
                    DropdownMenuItem(
                        text = { Text("All") },
                        onClick = { onGenerate(); showGenerateMenu = false; showMenu = false },
                    )
                    DropdownMenuItem(
                        text = { Text("Remaining") },
                        onClick = { onGenerateRemaining(); showGenerateMenu = false; showMenu = false },
                    )
                }
            }
            // Same nested All/Remaining shape as "Generate audio" above. "All" is onDownload
            // (whole book, unchanged); "Remaining" is onDownloadRemaining (from the book's
            // current stored reading position's chapter through the end - chapter-grained, see
            // LibraryViewModel.downloadBookRemaining's own doc comment for why not paragraph-
            // grained the way generate-remaining is).
            Box {
                DropdownMenuItem(
                    text = { Text("Download book") },
                    leadingIcon = { Icon(Icons.Filled.Download, contentDescription = null) },
                    onClick = { showDownloadMenu = true },
                )
                DropdownMenu(expanded = showDownloadMenu, onDismissRequest = { showDownloadMenu = false }) {
                    DropdownMenuItem(
                        text = { Text("All") },
                        onClick = { onDownload(); showDownloadMenu = false; showMenu = false },
                    )
                    DropdownMenuItem(
                        text = { Text("Remaining") },
                        onClick = { onDownloadRemaining(); showDownloadMenu = false; showMenu = false },
                    )
                }
            }
            if (isDownloaded) {
                DropdownMenuItem(
                    text = { Text("Remove downloaded copy") },
                    leadingIcon = { Icon(Icons.Filled.DownloadDone, contentDescription = null) },
                    onClick = { onDeleteLocal(); showMenu = false },
                )
            }
            DropdownMenuItem(
                text = { Text("Delete book") },
                leadingIcon = { Icon(Icons.Filled.Delete, contentDescription = null) },
                onClick = { onDelete(); showMenu = false },
            )
        }
    }
}

/** Adapted from frontend/src/pages/LibraryPage.tsx's BookCard progress label - always shows
 *  the bare percentage rather than swapping in "Not started"/"Finished" text at the extremes. */
private fun bookProgressLabel(book: BookSummaryDto): String {
    val percent = book.progressPercent.roundToInt()
    val generatedPercent = book.generatedPercent.roundToInt()
    val remainingSeconds = (book.estimatedTotalSeconds * (1 - book.progressPercent / 100)).coerceAtLeast(0.0)
    return buildString {
        append("$percent% read")
        append(" · $generatedPercent% generated")
        append(" · ${formatDurationLong(remainingSeconds)}")
        if (!book.estimateCalibrated) append(" (est.)")
    }
}
