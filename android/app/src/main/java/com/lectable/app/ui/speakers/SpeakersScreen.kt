package com.lectable.app.ui.speakers

import androidx.compose.ui.graphics.Color
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.border
import androidx.compose.material.icons.filled.Block
import androidx.compose.material.icons.filled.Groups
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Search
import androidx.compose.foundation.clickable
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
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material.icons.filled.AutoAwesome
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.Merge
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.RecordVoiceOver
import androidx.compose.material.icons.filled.Stop
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Snackbar
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import com.lectable.app.data.remote.dto.CustomVoicePresetDto
import com.lectable.app.data.remote.dto.SpeakerAppearanceDto
import com.lectable.app.data.remote.dto.SpeakerDto
import com.lectable.app.data.remote.dto.VoicePresetDto
import com.lectable.app.ui.components.ConfirmDialog

/**
 * The Android analogue of frontend/src/pages/SpeakersPage.tsx - see [SpeakerViewModel]'s own doc
 * comment for what's deliberately left out of this first pass (per-chapter attribution/direction
 * status/individual buttons, bulk characterize/generate-voice-all).
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SpeakersScreen(
    onBack: () -> Unit,
    onOpenVoices: () -> Unit,
    viewModel: SpeakerViewModel = hiltViewModel(),
) {
    val uiState by viewModel.uiState.collectAsState()
    val snackbarHostState = remember { SnackbarHostState() }
    var pendingDeleteCharacter by remember { mutableStateOf<SpeakerDto?>(null) }
    var pendingMergeTarget by remember { mutableStateOf<Pair<SpeakerDto, String>?>(null) }
    var mergeSheetFor by remember { mutableStateOf<SpeakerDto?>(null) }
    var renameFor by remember { mutableStateOf<SpeakerDto?>(null) }
    var voicePickerFor by remember { mutableStateOf<SpeakerDto?>(null) }
    var pendingDeleteSpeakerData by remember { mutableStateOf(false) }

    LaunchedEffect(uiState.error) {
        uiState.error?.let {
            snackbarHostState.showSnackbar(it)
            viewModel.dismissError()
        }
    }

    // "Someone other than this character" - Narrator plus every other real character's name,
    // shared by the merge sheet and each appearance row's own reassign menu, mirroring
    // frontend's own otherTargets/reassignTargets (the same set for both purposes).
    var rosterFilter by rememberSaveable { mutableStateOf("") }
    val filteredSpeakers = remember(uiState.speakers, rosterFilter) {
        val q = rosterFilter.trim()
        if (q.isEmpty()) uiState.speakers else uiState.speakers.filter { it.name.contains(q, ignoreCase = true) }
    }

    fun otherTargets(characterId: String) =
        listOf("Narrator") + uiState.speakers.filter { it.id.isNotEmpty() && it.id != characterId && !it.invalid }.map { it.name }

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Column {
                        Text("Speakers")
                        Text(uiState.bookTitle, style = MaterialTheme.typography.labelSmall, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    }
                },
                navigationIcon = {
                    IconButton(onClick = onBack) { Icon(Icons.Filled.ArrowBack, contentDescription = "Back") }
                },
                actions = {
                    if (uiState.speakers.isNotEmpty()) {
                        IconButton(onClick = { pendingDeleteSpeakerData = true }) {
                            Icon(Icons.Filled.Delete, contentDescription = "Delete all speaker data")
                        }
                    }
                },
            )
        },
        snackbarHost = { SnackbarHost(snackbarHostState) { Snackbar(it) } },
    ) { padding ->
        LazyColumn(modifier = Modifier.fillMaxSize().padding(padding)) {
            item {
                Column(modifier = Modifier.fillMaxWidth().padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.fillMaxWidth()) {
                        Text("Multi-voice narration", style = MaterialTheme.typography.titleMedium, modifier = Modifier.weight(1f))
                        Switch(checked = uiState.multiVoice, onCheckedChange = viewModel::toggleMultiVoice)
                    }
                    Text(
                        "When on, a character with a voice assigned below narrates in that voice instead of the " +
                            "book's own. Off by default, so attribution and voice assignment can be set up and " +
                            "reviewed at any time without changing anything already generated until you turn this on.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            item {
                Column(modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        Button(onClick = viewModel::preprocess, enabled = !uiState.preprocessing) {
                            Text(if (uiState.preprocessing) "Preprocessing…" else "Preprocess book")
                        }
                        if (uiState.preprocessing) CircularProgressIndicator(modifier = Modifier.size(20.dp))
                        OutlinedButton(onClick = viewModel::provisionUsedVoices) { Text("Provision voices") }
                    }
                    Text(
                        "Runs attribution, characterization, voice provisioning, direction-tagging, and pronunciation for the " +
                            "whole book in one pass, adding any newly-found character to the roster below. " +
                            "\"Provision voices\" only readies the voices this book's lines use: a voice for a character " +
                            "without one, its reference clip, and the emotion variants its lines need.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
            item {
                Text(
                    if (rosterFilter.isBlank() || uiState.speakers.isEmpty()) {
                        "Roster"
                    } else {
                        "Roster (${filteredSpeakers.size} of ${uiState.speakers.size})"
                    },
                    style = MaterialTheme.typography.titleMedium,
                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
                )
            }
            // Roster search - name only, like the web page's; filters just what's shown, while
            // merge/reassign targets still come from the full roster.
            if (uiState.speakers.isNotEmpty()) {
                item {
                    OutlinedTextField(
                        value = rosterFilter,
                        onValueChange = { rosterFilter = it },
                        placeholder = { Text("Search speakers…") },
                        leadingIcon = { Icon(Icons.Filled.Search, contentDescription = null) },
                        trailingIcon = {
                            if (rosterFilter.isNotEmpty()) {
                                IconButton(onClick = { rosterFilter = "" }) {
                                    Icon(Icons.Filled.Close, contentDescription = "Clear search")
                                }
                            }
                        },
                        singleLine = true,
                        modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 4.dp),
                    )
                }
            }
            if (uiState.loading) {
                item { Box(Modifier.fillMaxWidth().padding(24.dp), contentAlignment = Alignment.Center) { CircularProgressIndicator() } }
            } else if (uiState.speakers.isEmpty()) {
                item {
                    Text(
                        "No speakers yet - preprocess the book above to find out who's talking in it.",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
                    )
                }
            } else if (filteredSpeakers.isEmpty()) {
                item {
                    Text(
                        "No matches",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
                    )
                }
            } else {
                items(filteredSpeakers, key = { it.id.ifEmpty { "narrator" } }) { speaker ->
                    SpeakerRow(
                        speaker = speaker,
                        bookVoiceName = uiState.bookVoiceName,
                        expanded = uiState.appearancesFor == speaker.id && speaker.id.isNotEmpty(),
                        appearances = uiState.appearances,
                        appearancesLoading = uiState.appearancesLoading,
                        characterizing = speaker.id in uiState.characterizingIds,
                        generatingVoice = speaker.id in uiState.generatingVoiceIds,
                        deleting = speaker.id in uiState.deletingIds,
                        togglingInvalid = speaker.id in uiState.togglingInvalidIds,
                        onToggleInvalid = { viewModel.setInvalid(speaker.id, !speaker.invalid) },
                        togglingRole = speaker.id in uiState.togglingRoleIds,
                        onToggleRole = { viewModel.setRole(speaker.id, !speaker.role) },
                        reassignTargets = { otherTargets(speaker.id) },
                        playingUrl = uiState.playingUrl,
                        onPlay = { speaker.refAudioUrl?.let(viewModel::playUrl) },
                        onCharacterize = { viewModel.characterize(speaker.id) },
                        onGenerateVoice = { viewModel.generateVoice(speaker.id) },
                        onAssignVoice = { voicePickerFor = speaker },
                        onMerge = { mergeSheetFor = speaker },
                        onRename = { renameFor = speaker },
                        onDeleteRequested = { pendingDeleteCharacter = speaker },
                        onToggleAppearances = { viewModel.toggleAppearances(speaker.id) },
                        onPlayAppearance = viewModel::playUrl,
                        onRegenerateAppearance = viewModel::regenerateAppearance,
                        onReassignAppearance = viewModel::reassignAppearance,
                    )
                }
            }
        }
    }

    pendingDeleteCharacter?.let { speaker ->
        ConfirmDialog(
            title = "Delete \"${speaker.name}\"?",
            text = "Their lines in this book revert to Unknown, and their voice is removed.",
            onConfirm = { viewModel.deleteCharacter(speaker.id) },
            onDismiss = { pendingDeleteCharacter = null },
        )
    }

    pendingMergeTarget?.let { (speaker, targetName) ->
        ConfirmDialog(
            title = "Merge \"${speaker.name}\" into \"$targetName\"?",
            text = "Their lines in this book are reattributed to \"$targetName\", and \"${speaker.name}\"'s own voice is removed.",
            confirmLabel = "Merge",
            onConfirm = { viewModel.mergeCharacter(speaker.id, targetName) },
            onDismiss = { pendingMergeTarget = null },
        )
    }

    renameFor?.let { speaker ->
        RenameCharacterDialog(
            currentName = speaker.name,
            onRename = { name, onResult -> viewModel.renameCharacter(speaker.id, name, onResult) },
            onDismiss = { renameFor = null },
        )
    }

    mergeSheetFor?.let { speaker ->
        MergeCharacterSheet(
            targets = otherTargets(speaker.id),
            onDismiss = { mergeSheetFor = null },
            onSelect = { targetName ->
                mergeSheetFor = null
                pendingMergeTarget = speaker to targetName
            },
        )
    }

    voicePickerFor?.let { speaker ->
        CharacterVoicePickerSheet(
            builtins = uiState.builtinPresets,
            customs = uiState.customPresets,
            currentPresetId = speaker.voicePresetId,
            onDismiss = { voicePickerFor = null },
            onSelect = { presetId ->
                viewModel.setCharacterVoice(speaker.id, presetId)
                voicePickerFor = null
            },
            onManageVoices = {
                voicePickerFor = null
                onOpenVoices()
            },
        )
    }

    if (pendingDeleteSpeakerData) {
        ConfirmDialog(
            title = "Delete all speaker data?",
            text = "This clears every attributed speaker in this book and deletes the character roster " +
                "(names, characterizations, and voice assignments) for its whole series, if it's part of one. " +
                "This cannot be undone.",
            onConfirm = viewModel::deleteSpeakerData,
            onDismiss = { pendingDeleteSpeakerData = false },
        )
    }
}

@Composable
private fun SpeakerRow(
    speaker: SpeakerDto,
    bookVoiceName: String?,
    expanded: Boolean,
    appearances: List<SpeakerAppearanceDto>,
    appearancesLoading: Boolean,
    characterizing: Boolean,
    generatingVoice: Boolean,
    deleting: Boolean,
    togglingInvalid: Boolean,
    onToggleInvalid: () -> Unit,
    togglingRole: Boolean,
    onToggleRole: () -> Unit,
    reassignTargets: () -> List<String>,
    // The relative URL currently playing via SpeakerViewModel.playUrl, or null - compared
    // against this row's/each nested row's own audioUrl to decide Play vs. Stop (see
    // SpeakerUiState.playingUrl's own doc comment).
    playingUrl: String?,
    onPlay: () -> Unit,
    onCharacterize: () -> Unit,
    onGenerateVoice: () -> Unit,
    onAssignVoice: () -> Unit,
    onMerge: () -> Unit,
    onRename: () -> Unit,
    onDeleteRequested: () -> Unit,
    onToggleAppearances: () -> Unit,
    onPlayAppearance: (String) -> Unit,
    onRegenerateAppearance: (chapterIdx: Int, paragraphIdx: Int) -> Unit,
    onReassignAppearance: (chapterIdx: Int, paragraphIdx: Int, speaker: String) -> Unit,
) {
    val isNarrator = speaker.id.isEmpty()
    var showMenu by remember { mutableStateOf(false) }

    Column(
        modifier = Modifier
            .fillMaxWidth()
            .clickable(enabled = !isNarrator, onClick = onToggleAppearances)
            .padding(horizontal = 16.dp, vertical = 10.dp),
    ) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            if (!isNarrator && speaker.refAudioUrl != null) {
                val isPlaying = speaker.refAudioUrl == playingUrl
                IconButton(onClick = onPlay) {
                    if (isPlaying) {
                        Icon(Icons.Filled.Stop, contentDescription = "Stop playing ${speaker.name}'s reference clip")
                    } else {
                        Icon(Icons.Filled.PlayArrow, contentDescription = "Play ${speaker.name}'s reference clip")
                    }
                }
            }
            Column(modifier = Modifier.weight(1f)) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(
                        speaker.name,
                        style = MaterialTheme.typography.bodyLarge,
                        color = if (speaker.invalid) MaterialTheme.colorScheme.onSurfaceVariant else Color.Unspecified,
                    )
                    // A role label ("Guard", "Woman"): several unnamed people of one kind,
                    // not one individual - mirrors the web roster's "role" badge.
                    if (speaker.role) {
                        Text(
                            "role",
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.primary,
                            modifier = Modifier
                                .padding(start = 6.dp)
                                .border(1.dp, MaterialTheme.colorScheme.primary, RoundedCornerShape(4.dp))
                                .padding(horizontal = 4.dp),
                        )
                    }
                    // Not a real speaker (marked invalid): attribution won't assign this name
                    // again - mirrors the web roster's "invalid" badge.
                    if (speaker.invalid) {
                        Text(
                            "invalid",
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.error,
                            modifier = Modifier
                                .padding(start = 6.dp)
                                .border(1.dp, MaterialTheme.colorScheme.error, RoundedCornerShape(4.dp))
                                .padding(horizontal = 4.dp),
                        )
                    }
                }
                Text(
                    "${speaker.readyCount}/${speaker.paragraphCount} generated",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            if (isNarrator) {
                Text(
                    "Book's own voice" + (bookVoiceName?.let { " ($it)" } ?: ""),
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            } else {
                Box {
                    IconButton(onClick = { showMenu = true }) {
                        Icon(Icons.Filled.MoreVert, contentDescription = "Actions for ${speaker.name}")
                    }
                    DropdownMenu(expanded = showMenu, onDismissRequest = { showMenu = false }) {
                        DropdownMenuItem(
                            text = { Text(if (characterizing) "Characterizing…" else "Characterize") },
                            leadingIcon = { Icon(Icons.Filled.AutoAwesome, contentDescription = null) },
                            enabled = !characterizing,
                            onClick = { onCharacterize(); showMenu = false },
                        )
                        if (speaker.voicePresetId == null) {
                            DropdownMenuItem(
                                text = { Text(if (generatingVoice) "Generating…" else "Generate voice") },
                                leadingIcon = { Icon(Icons.Filled.RecordVoiceOver, contentDescription = null) },
                                enabled = !generatingVoice,
                                onClick = { onGenerateVoice(); showMenu = false },
                            )
                        }
                        DropdownMenuItem(
                            text = { Text("Assign voice") },
                            leadingIcon = { Icon(Icons.Filled.RecordVoiceOver, contentDescription = null) },
                            onClick = { onAssignVoice(); showMenu = false },
                        )
                        DropdownMenuItem(
                            text = {
                                Text(
                                    when {
                                        togglingInvalid -> "Updating…"
                                        speaker.invalid -> "Allow again (unmark invalid)"
                                        else -> "Mark invalid (not a real speaker)"
                                    },
                                )
                            },
                            leadingIcon = { Icon(Icons.Filled.Block, contentDescription = null) },
                            enabled = !togglingInvalid,
                            onClick = { onToggleInvalid(); showMenu = false },
                        )
                        DropdownMenuItem(
                            text = {
                                Text(
                                    when {
                                        togglingRole -> "Updating…"
                                        speaker.role -> "Treat as one individual (unmark role)"
                                        else -> "Mark as a role (several unnamed people)"
                                    },
                                )
                            },
                            leadingIcon = { Icon(Icons.Filled.Groups, contentDescription = null) },
                            enabled = !togglingRole,
                            onClick = { onToggleRole(); showMenu = false },
                        )
                        DropdownMenuItem(
                            text = { Text("Rename…") },
                            leadingIcon = { Icon(Icons.Filled.Edit, contentDescription = null) },
                            onClick = { onRename(); showMenu = false },
                        )
                        DropdownMenuItem(
                            text = { Text("Merge into…") },
                            leadingIcon = { Icon(Icons.Filled.Merge, contentDescription = null) },
                            onClick = { onMerge(); showMenu = false },
                        )
                        DropdownMenuItem(
                            text = { Text(if (deleting) "Deleting…" else "Delete") },
                            leadingIcon = { Icon(Icons.Filled.Delete, contentDescription = null) },
                            enabled = !deleting,
                            onClick = { onDeleteRequested(); showMenu = false },
                        )
                    }
                }
            }
        }
        if (!speaker.summary.isNullOrEmpty()) {
            Text(
                speaker.summary,
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 3,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.padding(top = 2.dp),
            )
        }
        if (!speaker.refLine.isNullOrEmpty()) {
            Text(
                "Reference line: “${speaker.refLine}”",
                style = MaterialTheme.typography.labelSmall.copy(fontStyle = FontStyle.Italic),
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                maxLines = 2,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.padding(top = 2.dp),
            )
        }
        if (expanded) {
            Column(modifier = Modifier.padding(top = 8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                when {
                    appearancesLoading -> Text("Loading…", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    appearances.isEmpty() -> Text(
                        "No appearances found.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    else -> appearances.forEach { appearance ->
                        AppearanceRow(
                            appearance = appearance,
                            reassignTargets = reassignTargets(),
                            isPlaying = appearance.audioUrl != null && appearance.audioUrl == playingUrl,
                            onPlay = { appearance.audioUrl?.let(onPlayAppearance) },
                            onRegenerate = { onRegenerateAppearance(appearance.chapterIdx, appearance.paragraphIdx) },
                            onReassign = { target -> onReassignAppearance(appearance.chapterIdx, appearance.paragraphIdx, target) },
                        )
                    }
                }
            }
        }
    }
}

@Composable
private fun AppearanceRow(
    appearance: SpeakerAppearanceDto,
    reassignTargets: List<String>,
    isPlaying: Boolean,
    onPlay: () -> Unit,
    onRegenerate: () -> Unit,
    onReassign: (String) -> Unit,
) {
    var showReassignMenu by remember { mutableStateOf(false) }
    Column(modifier = Modifier.fillMaxWidth().padding(vertical = 4.dp)) {
        Text(
            "${appearance.bookTitle} · ${appearance.chapterTitle}",
            style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        Text(appearance.text, style = MaterialTheme.typography.bodySmall, maxLines = 3, overflow = TextOverflow.Ellipsis)
        Row(verticalAlignment = Alignment.CenterVertically) {
            if (appearance.audioUrl != null) {
                IconButton(onClick = onPlay) {
                    if (isPlaying) {
                        Icon(Icons.Filled.Stop, contentDescription = "Stop playing this line")
                    } else {
                        Icon(Icons.Filled.PlayArrow, contentDescription = "Play this line")
                    }
                }
            }
            IconButton(onClick = onRegenerate) {
                Icon(Icons.Filled.Refresh, contentDescription = if (appearance.audioUrl != null) "Regenerate this line" else "Generate this line")
            }
            Box {
                TextButton(onClick = { showReassignMenu = true }) { Text("Reassign") }
                DropdownMenu(expanded = showReassignMenu, onDismissRequest = { showReassignMenu = false }) {
                    reassignTargets.forEach { name ->
                        DropdownMenuItem(text = { Text(name) }, onClick = { onReassign(name); showReassignMenu = false })
                    }
                }
            }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun MergeCharacterSheet(targets: List<String>, onDismiss: () -> Unit, onSelect: (String) -> Unit) {
    ModalBottomSheet(onDismissRequest = onDismiss) {
        LazyColumn(modifier = Modifier.padding(bottom = 24.dp)) {
            item {
                Text(
                    "Merge into",
                    style = MaterialTheme.typography.titleMedium,
                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
                )
            }
            items(targets, key = { it }) { name ->
                Row(
                    modifier = Modifier
                        .fillMaxWidth()
                        .clickable(onClick = { onSelect(name) })
                        .padding(horizontal = 16.dp, vertical = 12.dp),
                ) {
                    Text(name, style = MaterialTheme.typography.bodyLarge)
                }
            }
        }
    }
}

/** A character's own "Assign voice" picker - every built-in and custom preset, plus a link to
 *  [onManageVoices] (VoicesScreen) for creating a new one first, same hand-off
 *  ReaderScreen.kt's VoicePickerSheet already uses for the book-level narrator voice. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun CharacterVoicePickerSheet(
    builtins: List<VoicePresetDto>,
    customs: List<CustomVoicePresetDto>,
    currentPresetId: String?,
    onDismiss: () -> Unit,
    onSelect: (presetId: String) -> Unit,
    onManageVoices: () -> Unit,
) {
    ModalBottomSheet(onDismissRequest = onDismiss) {
        LazyColumn(modifier = Modifier.padding(bottom = 24.dp)) {
            item {
                Text(
                    "Assign voice",
                    style = MaterialTheme.typography.titleMedium,
                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
                )
            }
            if (customs.isNotEmpty()) {
                item {
                    Text(
                        "Your voices",
                        style = MaterialTheme.typography.labelLarge,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp),
                    )
                }
                items(customs, key = { "custom-${it.id}" }) { preset ->
                    VoicePickerRow(preset.name, preset.instruct, preset.id == currentPresetId) { onSelect(preset.id) }
                }
            }
            item {
                Text(
                    "Built-in voices",
                    style = MaterialTheme.typography.labelLarge,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp),
                )
            }
            items(builtins, key = { "builtin-${it.id}" }) { preset ->
                VoicePickerRow(preset.name, preset.instruct, preset.id == currentPresetId) { onSelect(preset.id) }
            }
            item {
                TextButton(onClick = onManageVoices, modifier = Modifier.padding(horizontal = 12.dp)) {
                    Text("Create a new voice…")
                }
            }
        }
    }
}

@Composable
private fun VoicePickerRow(name: String, subtitle: String, selected: Boolean, onClick: () -> Unit) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clickable(onClick = onClick)
            .padding(horizontal = 16.dp, vertical = 12.dp),
    ) {
        Column(modifier = Modifier.weight(1f)) {
            Text(
                name + if (selected) " (current)" else "",
                style = MaterialTheme.typography.bodyLarge,
                color = if (selected) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurface,
            )
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

/** Renames a character across its series (see SpeakerViewModel.renameCharacter). Stays open on
 *  a refusal - a name another character already has, a group - showing the server's reason. */
@Composable
private fun RenameCharacterDialog(
    currentName: String,
    onRename: (String, (String?) -> Unit) -> Unit,
    onDismiss: () -> Unit,
) {
    var name by rememberSaveable { mutableStateOf(currentName) }
    var error by remember { mutableStateOf<String?>(null) }
    var saving by remember { mutableStateOf(false) }
    val trimmed = name.trim()
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Rename \"$currentName\"") },
        text = {
            Column {
                OutlinedTextField(
                    value = name,
                    onValueChange = { name = it; error = null },
                    singleLine = true,
                    isError = error != null,
                    label = { Text("Name") },
                    modifier = Modifier.fillMaxWidth(),
                )
                Text(
                    error ?: "Their lines, corrections and voice follow the new name; \"$currentName\" stays on as another name for them.",
                    style = MaterialTheme.typography.bodySmall,
                    color = if (error != null) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 8.dp),
                )
            }
        },
        confirmButton = {
            TextButton(
                enabled = !saving && trimmed.isNotEmpty() && trimmed != currentName,
                onClick = {
                    saving = true
                    onRename(trimmed) { failure ->
                        saving = false
                        if (failure == null) onDismiss() else error = failure
                    }
                },
            ) { Text(if (saving) "Renaming…" else "Rename") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}
