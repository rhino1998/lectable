package com.lectable.app.ui.reader

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Close
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberModalBottomSheetState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import com.lectable.app.data.remote.LiveResult
import com.lectable.app.data.remote.dto.CastMemberDto
import com.lectable.app.data.remote.dto.ChapterNotesDto
import com.lectable.app.data.remote.dto.ChapterNotesResponseDto
import com.lectable.app.data.remote.dto.SummarySceneDto
import com.lectable.app.ui.components.ConfirmDialog
import kotlinx.coroutines.flow.StateFlow

private val PRONOUN_OPTIONS = listOf("", "he/him", "she/her", "they/them", "it/its")

// Draft rows carry a local key so a scene's present list survives a cast member being renamed
// mid-edit - the Android twin of frontend's ChapterNotesBlock draft.
private data class CastDraft(val key: Int, val name: String, val role: Boolean, val pronouns: String, val refs: String)

private data class SceneDraft(val key: Int, val startIdx: String, val location: String, val time: String, val present: Set<Int>)

private data class NotesDraft(val summary: String, val cast: List<CastDraft>, val scenes: List<SceneDraft>, val nextKey: Int)

private fun ChapterNotesDto.toDraft(): NotesDraft {
    var key = 0
    val cast = cast.map { CastDraft(key++, it.name, it.role, it.pronouns, it.refs.joinToString("; ")) }
    val keyByName = cast.associate { it.name.lowercase() to it.key }
    val scenes = scenes.map { sc ->
        SceneDraft(key++, sc.startIdx.toString(), sc.location, sc.time, sc.present.mapNotNull { keyByName[it.lowercase()] }.toSet())
    }
    return NotesDraft(summary, cast, scenes, key)
}

private fun NotesDraft.toNotes(): ChapterNotesDto {
    val nameByKey = cast.associate { it.key to it.name.trim() }
    return ChapterNotesDto(
        summary = summary,
        cast = cast.map { c ->
            CastMemberDto(c.name, c.role, c.pronouns, c.refs.split(';').map { it.trim() }.filter { it.isNotEmpty() })
        },
        scenes = scenes.map { sc ->
            SummarySceneDto(
                startIdx = sc.startIdx.trim().toIntOrNull() ?: -1,
                location = sc.location,
                time = sc.time,
                present = sc.present.mapNotNull { nameByKey[it] }.filter { it.isNotEmpty() },
            )
        },
        edited = true,
    )
}

/**
 * One chapter's summary notes (backend summary pass) - what happens, who's in it and what the
 * text calls them, and its scenes - viewable and editable from the chapter picker's long-press
 * "Edit chapter notes". The Android twin of frontend's ChapterNotesBlock: attribution reads the
 * cast (who's present in each scene, what they're called), so fixing a wrong or missing cast
 * member is often worth more than fixing lines one by one. Saved notes are marked edited and kept
 * by later summary runs; "Regenerate" replaces them; a save offers to re-run attribution.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
internal fun ChapterNotesSheet(
    chapterTitle: String,
    notesFlow: StateFlow<LiveResult<ChapterNotesResponseDto>>,
    onDismiss: () -> Unit,
    onSave: (ChapterNotesDto, onResult: (String?) -> Unit) -> Unit,
    onRegenerate: () -> Unit,
    onReattribute: () -> Unit,
) {
    val result by notesFlow.collectAsState()
    val notes = result.data?.notes
    var draft by remember { mutableStateOf<NotesDraft?>(null) }
    var saving by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var saved by remember { mutableStateOf(false) }
    var confirmRegenerate by remember { mutableStateOf(false) }

    ModalBottomSheet(onDismissRequest = onDismiss, sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true)) {
        LazyColumn(modifier = Modifier.padding(horizontal = 16.dp).padding(bottom = 24.dp)) {
            item {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(chapterTitle, style = MaterialTheme.typography.titleMedium, modifier = Modifier.weight(1f))
                    if (notes?.edited == true && draft == null) {
                        Text("edited", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
            }
            val d = draft
            when {
                result.loading && notes == null -> item { CircularProgressIndicator(modifier = Modifier.padding(16.dp)) }
                notes == null -> item {
                    Text(
                        "This chapter has no notes yet - they're written by the summary pass.",
                        modifier = Modifier.padding(vertical = 12.dp),
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                d == null -> {
                    item { NotesView(notes) }
                    item {
                        if (saved) {
                            Text(
                                "Saved. Attribution reads the new cast the next time it runs.",
                                style = MaterialTheme.typography.bodySmall,
                                modifier = Modifier.padding(top = 8.dp),
                            )
                        }
                        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.padding(top = 12.dp)) {
                            Button(onClick = { saved = false; error = null; draft = notes.toDraft() }) { Text("Edit") }
                            if (saved) {
                                TextButton(onClick = { saved = false; onReattribute() }) { Text("Re-run attribution") }
                            }
                            if (notes.edited) {
                                TextButton(onClick = { confirmRegenerate = true }) { Text("Regenerate") }
                            }
                        }
                    }
                }
                else -> {
                    item {
                        NotesEditor(d, onChange = { draft = it })
                        error?.let { Text(it, color = MaterialTheme.colorScheme.error, modifier = Modifier.padding(top = 8.dp)) }
                        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.padding(top = 12.dp)) {
                            Button(
                                enabled = !saving,
                                onClick = {
                                    saving = true
                                    error = null
                                    onSave(d.toNotes()) { message ->
                                        saving = false
                                        if (message == null) {
                                            draft = null
                                            saved = true
                                        } else {
                                            error = message
                                        }
                                    }
                                },
                            ) { Text(if (saving) "Saving…" else "Save notes") }
                            TextButton(enabled = !saving, onClick = { draft = null; error = null }) { Text("Cancel") }
                        }
                    }
                }
            }
        }
    }

    if (confirmRegenerate) {
        ConfirmDialog(
            title = "Regenerate notes?",
            text = "Your edits to this chapter's notes will be replaced by a fresh summary pass.",
            confirmLabel = "Regenerate",
            onConfirm = { confirmRegenerate = false; onRegenerate() },
            onDismiss = { confirmRegenerate = false },
        )
    }
}

@Composable
private fun NotesView(notes: ChapterNotesDto) {
    Column(verticalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.padding(top = 8.dp)) {
        if (notes.summary.isNotBlank()) Text(notes.summary, style = MaterialTheme.typography.bodyMedium)
        if (notes.cast.isNotEmpty()) {
            Text("Cast", style = MaterialTheme.typography.labelLarge)
            notes.cast.forEach { m ->
                val extra = buildList {
                    if (m.role) add("role")
                    if (m.pronouns.isNotEmpty()) add(m.pronouns)
                }.joinToString(", ")
                Text(
                    buildString {
                        append(m.name)
                        if (extra.isNotEmpty()) append(" ($extra)")
                        if (m.refs.isNotEmpty()) append(" — ").append(m.refs.joinToString("; "))
                    },
                    style = MaterialTheme.typography.bodySmall,
                )
            }
        }
        if (notes.scenes.isNotEmpty()) {
            Text("Scenes", style = MaterialTheme.typography.labelLarge)
            notes.scenes.forEach { sc ->
                Text(
                    buildString {
                        append("¶").append(sc.startIdx).append(" ").append(sc.location.ifBlank { "Scene" })
                        if (sc.time.isNotBlank()) append(" · ").append(sc.time)
                        if (sc.present.isNotEmpty()) append(" — ").append(sc.present.joinToString(", "))
                    },
                    style = MaterialTheme.typography.bodySmall,
                )
            }
        }
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun NotesEditor(draft: NotesDraft, onChange: (NotesDraft) -> Unit) {
    fun setCast(key: Int, f: (CastDraft) -> CastDraft) =
        onChange(draft.copy(cast = draft.cast.map { if (it.key == key) f(it) else it }))
    fun setScene(key: Int, f: (SceneDraft) -> SceneDraft) =
        onChange(draft.copy(scenes = draft.scenes.map { if (it.key == key) f(it) else it }))

    Column(verticalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.padding(top = 8.dp)) {
        OutlinedTextField(
            value = draft.summary,
            onValueChange = { onChange(draft.copy(summary = it)) },
            label = { Text("Summary") },
            minLines = 2,
            modifier = Modifier.fillMaxWidth(),
        )

        Text("Cast", style = MaterialTheme.typography.labelLarge, fontWeight = FontWeight.SemiBold)
        draft.cast.forEach { c ->
            Column {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    OutlinedTextField(
                        value = c.name,
                        onValueChange = { v -> setCast(c.key) { it.copy(name = v) } },
                        label = { Text("Name") },
                        singleLine = true,
                        modifier = Modifier.weight(1f),
                    )
                    IconButton(onClick = {
                        onChange(
                            draft.copy(
                                cast = draft.cast.filter { it.key != c.key },
                                scenes = draft.scenes.map { it.copy(present = it.present - c.key) },
                            ),
                        )
                    }) { Icon(Icons.Filled.Close, contentDescription = "Remove from the cast") }
                }
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Checkbox(checked = c.role, onCheckedChange = { v -> setCast(c.key) { it.copy(role = v) } })
                    Text("role", style = MaterialTheme.typography.bodySmall)
                    PronounPicker(c.pronouns, onPick = { v -> setCast(c.key) { it.copy(pronouns = v) } })
                }
                OutlinedTextField(
                    value = c.refs,
                    onValueChange = { v -> setCast(c.key) { it.copy(refs = v) } },
                    label = { Text("What the text calls them (; between)") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
            }
            HorizontalDivider()
        }
        TextButton(onClick = {
            onChange(draft.copy(cast = draft.cast + CastDraft(draft.nextKey, "", false, "", ""), nextKey = draft.nextKey + 1))
        }) {
            Icon(Icons.Filled.Add, contentDescription = null)
            Text("Add cast member")
        }

        Text("Scenes", style = MaterialTheme.typography.labelLarge, fontWeight = FontWeight.SemiBold)
        draft.scenes.forEach { sc ->
            Column {
                Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    OutlinedTextField(
                        value = sc.startIdx,
                        onValueChange = { v -> setScene(sc.key) { it.copy(startIdx = v.filter(Char::isDigit)) } },
                        label = { Text("From ¶") },
                        singleLine = true,
                        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                        modifier = Modifier.width(96.dp),
                    )
                    OutlinedTextField(
                        value = sc.location,
                        onValueChange = { v -> setScene(sc.key) { it.copy(location = v) } },
                        label = { Text("Place") },
                        singleLine = true,
                        modifier = Modifier.weight(1f),
                    )
                    IconButton(onClick = { onChange(draft.copy(scenes = draft.scenes.filter { it.key != sc.key })) }) {
                        Icon(Icons.Filled.Close, contentDescription = "Remove this scene")
                    }
                }
                OutlinedTextField(
                    value = sc.time,
                    onValueChange = { v -> setScene(sc.key) { it.copy(time = v) } },
                    label = { Text("Time") },
                    singleLine = true,
                    modifier = Modifier.fillMaxWidth(),
                )
                FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                    draft.cast.filter { it.name.isNotBlank() }.forEach { c ->
                        val on = c.key in sc.present
                        FilterChip(
                            selected = on,
                            onClick = { setScene(sc.key) { it.copy(present = if (on) it.present - c.key else it.present + c.key) } },
                            label = { Text(c.name) },
                        )
                    }
                }
            }
            HorizontalDivider()
        }
        TextButton(onClick = {
            val start = draft.scenes.lastOrNull()?.startIdx?.toIntOrNull()?.plus(1) ?: 0
            onChange(draft.copy(scenes = draft.scenes + SceneDraft(draft.nextKey, start.toString(), "", "", emptySet()), nextKey = draft.nextKey + 1))
        }) {
            Icon(Icons.Filled.Add, contentDescription = null)
            Text("Add scene")
        }
    }
}

@Composable
private fun PronounPicker(value: String, onPick: (String) -> Unit) {
    var open by remember { mutableStateOf(false) }
    TextButton(onClick = { open = true }) { Text(value.ifEmpty { "pronouns —" }) }
    DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
        PRONOUN_OPTIONS.forEach { p ->
            DropdownMenuItem(text = { Text(p.ifEmpty { "—" }) }, onClick = { onPick(p); open = false })
        }
    }
}
