package com.lectable.app.ui.reader

import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Bookmarks
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Download
import androidx.compose.material.icons.filled.DownloadDone
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.EditNote
import androidx.compose.material.icons.filled.FormatQuote
import androidx.compose.material.icons.filled.GraphicEq
import androidx.compose.material.icons.filled.Lightbulb
import androidx.compose.material.icons.filled.Mood
import androidx.compose.material.icons.filled.MusicNote
import androidx.compose.material.icons.filled.RecordVoiceOver
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.SkipNext
import androidx.compose.material.icons.filled.Spellcheck
import androidx.compose.material.icons.filled.TaskAlt
import androidx.compose.material.icons.outlined.Mood
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TextField
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.luminance
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.dp
import com.lectable.app.data.download.DownloadStatus
import com.lectable.app.data.remote.dto.BookmarkDto
import com.lectable.app.data.remote.dto.ChapterSummaryDto
import com.lectable.app.data.remote.dto.CharacterNameDto
import com.lectable.app.data.remote.dto.CustomVoicePresetDto
import com.lectable.app.data.remote.dto.SearchResultDto
import com.lectable.app.data.remote.dto.VoicePresetDto
import com.lectable.app.data.repository.AmbienceDownloadState
import com.lectable.app.data.repository.ChapterDownloadState
import com.lectable.app.playback.SleepTimerOption
import com.lectable.app.playback.SleepTimerState
import com.lectable.app.ui.components.ConfirmDialog
import com.lectable.app.ui.formatTime
import kotlinx.coroutines.delay

@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun VoicePickerSheet(
    onDismiss: () -> Unit,
    fetchPresets: ((List<VoicePresetDto>) -> Unit) -> Unit,
    fetchCustomPresets: ((List<CustomVoicePresetDto>) -> Unit) -> Unit,
    fetchLanguages: ((List<String>) -> Unit) -> Unit,
    initialLanguage: String,
    onSelect: (id: String, name: String, instruct: String, language: String) -> Unit,
    multiVoice: Boolean,
    onMultiVoiceChange: (Boolean) -> Unit,
    musicEnabled: Boolean,
    onMusicEnabledChange: (Boolean) -> Unit,
    onManageVoices: () -> Unit,
    onDeleteBookAudio: () -> Unit,
) {
    var presets by remember { mutableStateOf<List<VoicePresetDto>>(emptyList()) }
    var customPresets by remember { mutableStateOf<List<CustomVoicePresetDto>>(emptyList()) }
    var languages by remember { mutableStateOf(listOf(initialLanguage)) }
    var builtinsLoaded by remember { mutableStateOf(false) }
    // Staged locally, only actually saved (together with whichever preset is tapped below - see
    // onSelect) once the reader picks one - mirrors frontend's VoicePanel, which keeps language
    // as local editable state alongside the preset dropdown behind one shared "Save voice"
    // action, rather than saving on every change.
    var selectedLanguage by remember { mutableStateOf(initialLanguage) }
    var languageMenuOpen by remember { mutableStateOf(false) }
    LaunchedEffect(Unit) {
        fetchPresets { presets = it; builtinsLoaded = true }
        fetchCustomPresets { customPresets = it }
        fetchLanguages { if (it.isNotEmpty()) languages = it }
    }

    ModalBottomSheet(onDismissRequest = onDismiss) {
        LazyColumn(modifier = Modifier.padding(bottom = 24.dp)) {
            item {
                Text(
                    "Narrator voice",
                    style = MaterialTheme.typography.titleMedium,
                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
                )
            }
            item {
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(horizontal = 16.dp, vertical = 4.dp),
                ) {
                    Text("Language", style = MaterialTheme.typography.bodyLarge, modifier = Modifier.weight(1f))
                    Box {
                        TextButton(onClick = { languageMenuOpen = true }) { Text(selectedLanguage) }
                        DropdownMenu(expanded = languageMenuOpen, onDismissRequest = { languageMenuOpen = false }) {
                            languages.forEach { lang ->
                                DropdownMenuItem(
                                    text = { Text(lang) },
                                    onClick = { selectedLanguage = lang; languageMenuOpen = false },
                                )
                            }
                        }
                    }
                }
            }
            item {
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(horizontal = 16.dp, vertical = 8.dp),
                ) {
                    Column(modifier = Modifier.weight(1f)) {
                        Text("Multi-voice narration", style = MaterialTheme.typography.bodyLarge)
                        Text(
                            "Characters with an assigned voice narrate their own dialogue in it, " +
                                "instead of always this book's own voice above.",
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                    Switch(checked = multiVoice, onCheckedChange = onMultiVoiceChange)
                }
            }
            item {
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(horizontal = 16.dp, vertical = 8.dp),
                ) {
                    Column(modifier = Modifier.weight(1f)) {
                        Text("Background music", style = MaterialTheme.typography.bodyLarge)
                        Text(
                            "Mixes a generated mood track in underneath narration, scored " +
                                "chapter by chapter as you read.",
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                    Switch(checked = musicEnabled, onCheckedChange = onMusicEnabledChange)
                }
            }
            if (!builtinsLoaded) {
                item { CircularProgressIndicator(modifier = Modifier.padding(24.dp)) }
            } else {
                if (customPresets.isNotEmpty()) {
                    item { VoiceSectionHeader("Your voices") }
                    items(customPresets, key = { "custom-${it.id}" }) { preset ->
                        VoiceRow(
                            name = preset.name,
                            subtitle = preset.instruct,
                            onClick = { onSelect(preset.id, preset.name, preset.instruct, selectedLanguage) },
                        )
                    }
                }
                item { VoiceSectionHeader("Built-in voices") }
                items(presets, key = { "builtin-${it.id}" }) { preset ->
                    VoiceRow(
                        name = preset.name,
                        subtitle = preset.instruct,
                        onClick = { onSelect(preset.id, preset.name, preset.instruct, selectedLanguage) },
                    )
                }
            }
            item {
                TextButton(onClick = onManageVoices, modifier = Modifier.padding(horizontal = 12.dp)) {
                    Text("Manage custom voices →")
                }
            }
            item {
                var confirmingDelete by remember { mutableStateOf(false) }
                TextButton(
                    onClick = { confirmingDelete = true },
                    modifier = Modifier.padding(horizontal = 12.dp),
                    colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
                ) {
                    Text("Delete generated audio")
                }
                if (confirmingDelete) {
                    ConfirmDialog(
                        title = "Delete generated audio?",
                        text = "Every paragraph in this book will need to be regenerated. This doesn't touch the book's " +
                            "text, chapters, or voice settings.",
                        onConfirm = onDeleteBookAudio,
                        onDismiss = { confirmingDelete = false },
                    )
                }
            }
        }
    }
}

@Composable
private fun VoiceSectionHeader(title: String) {
    Text(
        title,
        style = MaterialTheme.typography.labelLarge,
        color = MaterialTheme.colorScheme.onSurfaceVariant,
        modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp),
    )
}

@Composable
private fun VoiceRow(name: String, subtitle: String, onClick: () -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clickable(onClick = onClick)
            .padding(horizontal = 16.dp, vertical = 12.dp),
    ) {
        Column {
            Text(name, style = MaterialTheme.typography.bodyLarge)
            Text(
                subtitle,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 2,
                overflow = TextOverflow.Ellipsis,
            )
        }
    }
}

/** The long-press "Set speaker" picker - reassigns one displayed block's dialogue segment(s) to a
 *  different character, mirroring the web Speakers page's per-appearance "Reassign to…" (see
 *  ReaderViewModel.setParagraphSpeaker). "Narrator" always leads the list (clears attribution,
 *  same as an empty speaker server-side) followed by every other known character - mirrors the
 *  web page's own reassignTargets (Narrator + every character name), fetched fresh each time the
 *  the live series roster. "New speaker…" asks for a name and makes it a character of its own
 *  ([onCreate]) - for a line attribution gave to someone it took two people for; the web
 *  reader's context menu has the same entry. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun SpeakerPickerSheet(
    onDismiss: () -> Unit,
    // The live series roster (ReaderUiState.characters) - null until it has arrived.
    characters: List<CharacterNameDto>?,
    currentSpeaker: String,
    // Everyone who speaks in this segment's own chapter - see
    // chapterCharacterNames. Listed first, under "This chapter", ahead of the whole roster.
    chapterNames: Set<String>,
    onSelect: (name: String) -> Unit,
    onCreate: (name: String) -> Unit,
) {
    var naming by remember { mutableStateOf(false) }
    val loaded = characters != null
    val (local, all) = remember(characters, chapterNames) { pickerSections(characters.orEmpty(), chapterNames, exclude = null) }

    ModalBottomSheet(onDismissRequest = onDismiss) {
        LazyColumn(modifier = Modifier.padding(bottom = 24.dp)) {
            item {
                Text(
                    "Set speaker",
                    style = MaterialTheme.typography.titleMedium,
                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
                )
            }
            if (!loaded) {
                item { CircularProgressIndicator(modifier = Modifier.padding(24.dp)) }
            } else {
                item { SpeakerRow("Narrator", currentSpeaker, onSelect) }
                // Attribution's own "can't tell who says this" value - not a roster character,
                // so it's a fixed entry like Narrator.
                item { SpeakerRow("Unknown", currentSpeaker, onSelect) }
                item {
                    Text(
                        "New speaker…",
                        style = MaterialTheme.typography.bodyLarge,
                        color = MaterialTheme.colorScheme.primary,
                        modifier = Modifier
                            .fillMaxWidth()
                            .clickable(onClick = { naming = true })
                            .padding(horizontal = 16.dp, vertical = 12.dp),
                    )
                }
                if (local.isNotEmpty()) {
                    item { VoiceSectionHeader("This chapter") }
                    items(local, key = { "local-$it" }) { name -> SpeakerRow(name, currentSpeaker, onSelect) }
                    item { VoiceSectionHeader("All characters") }
                }
                items(all, key = { "all-$it" }) { name -> SpeakerRow(name, currentSpeaker, onSelect) }
            }
        }
    }
    if (naming) {
        NewSpeakerDialog(onDismiss = { naming = false }, onConfirm = onCreate)
    }
}

/** The name prompt behind the speaker picker's "New speaker…". */
@Composable
private fun NewSpeakerDialog(onDismiss: () -> Unit, onConfirm: (name: String) -> Unit) {
    var name by remember { mutableStateOf("") }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("New speaker") },
        text = {
            OutlinedTextField(
                value = name,
                onValueChange = { name = it },
                label = { Text("Name") },
                singleLine = true,
                keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Words, imeAction = ImeAction.Done),
                keyboardActions = KeyboardActions(onDone = { if (name.isNotBlank()) onConfirm(name.trim()) }),
            )
        },
        confirmButton = {
            TextButton(onClick = { onConfirm(name.trim()) }, enabled = name.isNotBlank()) { Text("Add") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun SpeakerRow(name: String, currentSpeaker: String, onSelect: (String) -> Unit) {
    Row(
        verticalAlignment = Alignment.CenterVertically,
        modifier = Modifier
            .fillMaxWidth()
            .clickable(onClick = { onSelect(name) })
            .padding(horizontal = 16.dp, vertical = 12.dp),
    ) {
        Text(name, style = MaterialTheme.typography.bodyLarge, modifier = Modifier.weight(1f))
        if (name == currentSpeaker) {
            Icon(Icons.Filled.Check, contentDescription = null, tint = MaterialTheme.colorScheme.primary)
        }
    }
}

/** "Jump to chapter" - the Android analogue of frontend's PlayerBar.tsx chapter <select>, opened
 *  by tapping the chapter title/index in the playback bar instead of a persistent dropdown. */
@OptIn(ExperimentalMaterial3Api::class, ExperimentalFoundationApi::class)
@Composable
internal fun ChapterPickerSheet(
    chapters: List<ChapterSummaryDto>,
    currentChapterIdx: Int,
    durationFor: (ChapterSummaryDto) -> String,
    downloadStates: Map<Int, ChapterDownloadState>,
    onDismiss: () -> Unit,
    onSelect: (idx: Int) -> Unit,
    onDownload: (idx: Int) -> Unit,
    onLongPressDownload: (idx: Int) -> Unit,
    onGenerateChapter: (idx: Int) -> Unit,
    onChapterPass: (idx: Int, pass: ChapterPass) -> Unit,
    onEditNotes: (idx: Int) -> Unit,
    onSetMatter: (idx: Int, matter: Boolean) -> Unit,
) {
    val listState = rememberLazyListState(initialFirstVisibleItemIndex = (currentChapterIdx - 3).coerceAtLeast(0))
    ModalBottomSheet(onDismissRequest = onDismiss) {
        Text(
            "Jump to chapter",
            style = MaterialTheme.typography.titleMedium,
            modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
        )
        LazyColumn(state = listState, modifier = Modifier.padding(bottom = 24.dp)) {
            items(chapters, key = { it.idx }) { chapter ->
                val isCurrent = chapter.idx == currentChapterIdx
                val state = downloadStates[chapter.idx]
                // Long-press anywhere on the row (not just the download-status icon below, which
                // keeps its own narrower cancel/delete long-press) for a menu of chapter-scoped
                // actions that don't need the reader to actually be inside the chapter -
                // generating or attributing/tagging it ahead of time, or freeing up this device's
                // own offline copy.
                var showChapterMenu by remember { mutableStateOf(false) }
                Box {
                    Column(
                        modifier = Modifier
                            .fillMaxWidth()
                            .background(if (isCurrent) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.surface)
                            .combinedClickable(
                                onClick = { onSelect(chapter.idx) },
                                onLongClick = { showChapterMenu = true },
                            )
                            .padding(horizontal = 16.dp, vertical = 8.dp),
                    ) {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Text(
                                chapter.title,
                                style = MaterialTheme.typography.bodyLarge,
                                maxLines = 1,
                                overflow = TextOverflow.Ellipsis,
                                modifier = Modifier.weight(1f),
                            )
                            if (chapter.matter) {
                                // Front or back matter: playback steps over it.
                                Text(
                                    "Skipped",
                                    style = MaterialTheme.typography.labelSmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    modifier = Modifier.padding(end = 8.dp),
                                )
                            } else if (chapter.paragraphCount > 0) {
                                Text(
                                    durationFor(chapter),
                                    style = MaterialTheme.typography.labelSmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                            }
                            state?.ambience?.let { AmbienceDownloadBadge(it) }
                            when (state?.status) {
                                DownloadStatus.DOWNLOADING -> {
                                    val ready = state.readyParagraphs
                                    val total = state.totalParagraphs
                                    Box(
                                        contentAlignment = Alignment.Center,
                                        modifier = Modifier
                                            .size(48.dp)
                                            .combinedClickable(
                                                onClick = {},
                                                onLongClick = { onLongPressDownload(chapter.idx) },
                                            ),
                                    ) {
                                        CircularProgressIndicator(
                                            progress = { if (total > 0) ready.toFloat() / total else 0f },
                                            modifier = Modifier.size(20.dp),
                                            strokeWidth = 2.dp,
                                        )
                                    }
                                }
                                DownloadStatus.COMPLETE -> {
                                    Box(
                                        contentAlignment = Alignment.Center,
                                        modifier = Modifier
                                            .size(48.dp)
                                            .combinedClickable(
                                                onClick = {},
                                                onLongClick = { onLongPressDownload(chapter.idx) },
                                            ),
                                    ) {
                                        Icon(
                                            Icons.Filled.DownloadDone,
                                            contentDescription = "Downloaded for offline playback - long-press to remove",
                                            tint = MaterialTheme.colorScheme.primary,
                                        )
                                    }
                                }
                                else -> {
                                    IconButton(onClick = { onDownload(chapter.idx) }) {
                                        Icon(Icons.Filled.Download, contentDescription = "Download this chapter")
                                    }
                                }
                            }
                        }
                    }
                    DropdownMenu(expanded = showChapterMenu, onDismissRequest = { showChapterMenu = false }) {
                        DropdownMenuItem(
                            text = { Text("Generate chapter audio") },
                            leadingIcon = { Icon(Icons.Filled.GraphicEq, contentDescription = null) },
                            onClick = { onGenerateChapter(chapter.idx); showChapterMenu = false },
                        )
                        ChapterPass.entries.forEach { pass ->
                            DropdownMenuItem(
                                text = { Text(pass.label) },
                                leadingIcon = { Icon(pass.icon, contentDescription = null) },
                                onClick = { onChapterPass(chapter.idx, pass); showChapterMenu = false },
                            )
                        }
                        DropdownMenuItem(
                            text = { Text(if (chapter.matter) "Read as part of the story" else "Skip when listening") },
                            leadingIcon = { Icon(Icons.Filled.SkipNext, contentDescription = null) },
                            onClick = { onSetMatter(chapter.idx, !chapter.matter); showChapterMenu = false },
                        )
                        DropdownMenuItem(
                            text = { Text("Edit chapter notes") },
                            leadingIcon = { Icon(Icons.Filled.EditNote, contentDescription = null) },
                            onClick = { onEditNotes(chapter.idx); showChapterMenu = false },
                        )
                        if (state?.status == DownloadStatus.DOWNLOADING || state?.status == DownloadStatus.COMPLETE) {
                            DropdownMenuItem(
                                text = { Text("Delete downloaded data") },
                                leadingIcon = { Icon(Icons.Filled.Delete, contentDescription = null) },
                                onClick = { onLongPressDownload(chapter.idx); showChapterMenu = false },
                            )
                        }
                    }
                }
            }
        }
    }
}

/** A downloaded chapter's offline-ambience state, beside its download icon: a lightbulb when
 *  only the lighting cues are kept (the book's music is off), else a music note - dimmed with
 *  "…" until the chapter is scored, then with a clip count until every region's music is on
 *  the device. */
@Composable
private fun AmbienceDownloadBadge(state: AmbienceDownloadState) {
    val icon = if (state.withClips) Icons.Filled.MusicNote else Icons.Filled.Lightbulb
    val progress = when {
        state.settled -> null
        !state.scored -> "…"
        else -> "${state.readyClips}/${state.totalClips}"
    }
    val description = when {
        !state.settled && !state.scored -> "Ambience not scored yet"
        !state.settled -> "Background music downloading, ${state.readyClips} of ${state.totalClips} regions"
        state.withClips -> "Background music and lighting downloaded"
        else -> "Lighting cues downloaded"
    }
    Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.padding(start = 8.dp)) {
        Icon(
            icon,
            contentDescription = description,
            tint = if (state.settled) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.size(16.dp),
        )
        if (progress != null) {
            Text(progress, style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

/** The Android analogue of frontend's BookmarksPanel.tsx - reviewing, jumping to, or removing
 *  bookmarked paragraphs (bookmarking itself happens via a paragraph's long-press menu, not here). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun BookmarksSheet(
    fetchBookmarks: ((List<BookmarkDto>) -> Unit) -> Unit,
    onDismiss: () -> Unit,
    onJump: (chapterIdx: Int, paragraphIdx: Int) -> Unit,
    onUpdateNote: (id: String, note: String, onSuccess: () -> Unit) -> Unit,
    onDelete: (id: String) -> Unit,
) {
    var bookmarks by remember { mutableStateOf<List<BookmarkDto>>(emptyList()) }
    var loaded by remember { mutableStateOf(false) }
    var editingId by remember { mutableStateOf<String?>(null) }
    var noteDraft by remember { mutableStateOf("") }
    var pendingDelete by remember { mutableStateOf<BookmarkDto?>(null) }
    LaunchedEffect(Unit) { fetchBookmarks { bookmarks = it; loaded = true } }

    ModalBottomSheet(onDismissRequest = onDismiss) {
        Text(
            "Bookmarks",
            style = MaterialTheme.typography.titleMedium,
            modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
        )
        when {
            !loaded -> CircularProgressIndicator(modifier = Modifier.padding(24.dp))
            bookmarks.isEmpty() -> Text(
                "No bookmarks yet - long-press a paragraph to add one.",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(horizontal = 16.dp, vertical = 16.dp),
            )
            else -> LazyColumn(modifier = Modifier.padding(bottom = 24.dp)) {
                items(bookmarks, key = { it.id }) { bookmark ->
                    Column(modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp)) {
                        Row(
                            modifier = Modifier
                                .fillMaxWidth()
                                .clickable { onJump(bookmark.chapterIdx, bookmark.paragraphIdx) },
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            Column(modifier = Modifier.weight(1f)) {
                                Text(
                                    bookmark.chapterTitle,
                                    style = MaterialTheme.typography.labelSmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                                Text(bookmark.text, style = MaterialTheme.typography.bodyMedium, maxLines = 2, overflow = TextOverflow.Ellipsis)
                            }
                            IconButton(onClick = {
                                editingId = bookmark.id
                                noteDraft = bookmark.note
                            }) {
                                Icon(Icons.Filled.Edit, contentDescription = "Edit note")
                            }
                            IconButton(onClick = { pendingDelete = bookmark }) {
                                Icon(Icons.Filled.Delete, contentDescription = "Remove bookmark")
                            }
                        }
                        if (editingId == bookmark.id) {
                            Row(
                                modifier = Modifier.fillMaxWidth().padding(top = 4.dp),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                TextField(
                                    value = noteDraft,
                                    onValueChange = { noteDraft = it },
                                    placeholder = { Text("Add a note…") },
                                    singleLine = true,
                                    modifier = Modifier.weight(1f),
                                )
                                IconButton(onClick = {
                                    val id = bookmark.id
                                    val note = noteDraft
                                    onUpdateNote(id, note) {
                                        bookmarks = bookmarks.map { if (it.id == id) it.copy(note = note) else it }
                                    }
                                    editingId = null
                                }) {
                                    Icon(Icons.Filled.Check, contentDescription = "Save note")
                                }
                            }
                        } else if (bookmark.note.isNotBlank()) {
                            Text(
                                bookmark.note,
                                style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.primary,
                                modifier = Modifier.padding(top = 2.dp),
                            )
                        }
                    }
                }
            }
        }
    }

    pendingDelete?.let { bookmark ->
        ConfirmDialog(
            title = "Remove bookmark?",
            text = "The bookmark on \"${bookmark.chapterTitle}\" will be removed.",
            onConfirm = {
                onDelete(bookmark.id)
                bookmarks = bookmarks.filterNot { it.id == bookmark.id }
            },
            onDismiss = { pendingDelete = null },
        )
    }
}

/** The Android analogue of frontend's BookSearch.tsx - a debounced full-text search across the
 *  book's paragraphs, jumping playback to whichever result is tapped. "By meaning" finds the
 *  paragraphs closest to what was typed instead of the ones containing it, best first. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun SearchSheet(
    search: (query: String, byMeaning: Boolean, onResult: (List<SearchResultDto>) -> Unit) -> Unit,
    onDismiss: () -> Unit,
    onJump: (chapterIdx: Int, paragraphIdx: Int) -> Unit,
) {
    var input by remember { mutableStateOf("") }
    var results by remember { mutableStateOf<List<SearchResultDto>>(emptyList()) }
    var searching by remember { mutableStateOf(false) }
    var byMeaning by remember { mutableStateOf(false) }
    val showResults = input.trim().length > 1

    // Debounced the same 300ms as BookSearch.tsx, so typing doesn't fire a request per keystroke.
    LaunchedEffect(input, byMeaning) {
        if (!showResults) {
            results = emptyList()
            return@LaunchedEffect
        }
        delay(300)
        searching = true
        search(input, byMeaning) { results = it; searching = false }
    }

    ModalBottomSheet(onDismissRequest = onDismiss) {
        Column(modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp)) {
            Text("Search this book", style = MaterialTheme.typography.titleMedium, modifier = Modifier.padding(bottom = 8.dp))
            TextField(
                value = input,
                onValueChange = { input = it },
                placeholder = { Text(if (byMeaning) "Describe what you are looking for…" else "Search this book…") },
                singleLine = true,
                modifier = Modifier.fillMaxWidth(),
            )
            FilterChip(
                selected = byMeaning,
                onClick = { byMeaning = !byMeaning },
                label = { Text("By meaning") },
                modifier = Modifier.padding(top = 4.dp),
            )
        }
        if (showResults) {
            when {
                searching -> CircularProgressIndicator(modifier = Modifier.padding(24.dp))
                results.isEmpty() -> Text(
                    if (byMeaning) "Nothing found. This book may not be preprocessed yet." else "No matches.",
                    style = MaterialTheme.typography.bodyMedium,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 16.dp),
                )
                else -> {
                    val highlightBackground = MaterialTheme.colorScheme.primary
                    val highlightForeground = MaterialTheme.colorScheme.onPrimary
                    LazyColumn(modifier = Modifier.padding(bottom = 24.dp)) {
                        items(results, key = { "${it.chapterIdx}-${it.paragraphIdx}" }) { result ->
                            Column(
                                modifier = Modifier
                                    .fillMaxWidth()
                                    .clickable { onJump(result.chapterIdx, result.paragraphIdx) }
                                    .padding(horizontal = 16.dp, vertical = 12.dp),
                            ) {
                                Text(
                                    result.chapterTitle,
                                    style = MaterialTheme.typography.labelSmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                                Text(
                                    searchSnippet(result.text, input, highlightBackground, highlightForeground),
                                    style = MaterialTheme.typography.bodyMedium,
                                    maxLines = 2,
                                    overflow = TextOverflow.Ellipsis,
                                )
                            }
                        }
                    }
                }
            }
        }
    }
}

// Matches frontend's BookSearch.tsx SNIPPET_RADIUS.
private const val SEARCH_SNIPPET_RADIUS = 60

/** Mirrors frontend's BookSearch.tsx snippet(): centers a window around the first match of
 *  [query] (case-insensitive) so a hit buried deep in a long paragraph isn't hidden past the
 *  2-line clip, with the match itself highlighted instead of just plain-text-truncated. Mirrors
 *  the web version's own `<mark>` styling too - a solid [highlightBackground] pill behind the
 *  match with contrasting [highlightForeground] text, not just a recolored/bolded run of text:
 *  that used to be `color = MaterialTheme.colorScheme.primary` alone, which in this app's dark
 *  theme is a light lavender not far in luminance from the already-light body text around it, so
 *  the "highlight" barely read as one. A solid background reads clearly regardless of how close
 *  the accent and body-text colors happen to sit. Falls back to the raw text (no highlight) if
 *  [query] isn't actually found in it - shouldn't normally happen since these are the backend's
 *  own search hits, but a plain fallback beats a blank snippet. */
private fun searchSnippet(text: String, query: String, highlightBackground: Color, highlightForeground: Color): AnnotatedString {
    val idx = text.indexOf(query, ignoreCase = true)
    if (idx == -1) return AnnotatedString(text)
    val start = (idx - SEARCH_SNIPPET_RADIUS).coerceAtLeast(0)
    val end = (idx + query.length + SEARCH_SNIPPET_RADIUS).coerceAtMost(text.length)
    return buildAnnotatedString {
        if (start > 0) append('…')
        append(text.substring(start, idx))
        withStyle(SpanStyle(fontWeight = FontWeight.Bold, background = highlightBackground, color = highlightForeground)) {
            append(text.substring(idx, idx + query.length))
        }
        append(text.substring(idx + query.length, end))
        if (end < text.length) append('…')
    }
}

/** The Android analogue of frontend's SleepTimerButton.tsx - pick (or cancel) a countdown, or
 *  "end of chapter", after which [ParagraphPlayer] pauses playback on its own. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun SleepTimerSheet(
    state: SleepTimerState,
    onDismiss: () -> Unit,
    onSelect: (SleepTimerOption) -> Unit,
) {
    val durationOptions = listOf(5, 10, 15, 30, 45, 60)
    ModalBottomSheet(onDismissRequest = onDismiss) {
        Text(
            "Sleep timer",
            style = MaterialTheme.typography.titleMedium,
            modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
        )
        if (state.option != SleepTimerOption.Off) {
            Row(
                modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Text(
                    if (state.option == SleepTimerOption.EndOfChapter) {
                        "Stops at the end of this chapter"
                    } else {
                        "Stops in ${formatTime(state.remainingSeconds.toDouble())}"
                    },
                    style = MaterialTheme.typography.bodyMedium,
                    modifier = Modifier.weight(1f),
                )
                TextButton(onClick = { onSelect(SleepTimerOption.Off) }) {
                    Text("Cancel")
                }
            }
        }
        LazyColumn(modifier = Modifier.padding(bottom = 24.dp)) {
            items(durationOptions, key = { it }) { minutes ->
                val selected = (state.option as? SleepTimerOption.Countdown)?.minutes == minutes
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .background(if (selected) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.surface)
                        .clickable { onSelect(SleepTimerOption.Countdown(minutes)) }
                        .padding(horizontal = 16.dp, vertical = 12.dp),
                ) {
                    Text("$minutes minutes", style = MaterialTheme.typography.bodyLarge)
                }
            }
            item {
                val selected = state.option == SleepTimerOption.EndOfChapter
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .background(if (selected) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.surface)
                        .clickable { onSelect(SleepTimerOption.EndOfChapter) }
                        .padding(horizontal = 16.dp, vertical = 12.dp),
                ) {
                    Text("End of chapter", style = MaterialTheme.typography.bodyLarge)
                }
            }
        }
    }
}

/** [ChapterPass]'s menu icon - kept here, next to the only UI that shows it, rather than on the
 *  enum itself (which lives with the ViewModel and stays free of Compose types). */
internal val ChapterPass.icon: ImageVector
    get() = when (this) {
        ChapterPass.SCARE_QUOTES -> Icons.Filled.FormatQuote
        ChapterPass.ATTRIBUTION -> Icons.Filled.RecordVoiceOver
        ChapterPass.AROUND_EDITS -> Icons.Filled.TaskAlt
        ChapterPass.DIRECTIONS -> Icons.Filled.Mood
        ChapterPass.PRONUNCIATION -> Icons.Filled.Spellcheck
        ChapterPass.AMBIENCE -> Icons.Filled.MusicNote
    }
