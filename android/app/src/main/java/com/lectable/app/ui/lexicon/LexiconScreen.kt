package com.lectable.app.ui.lexicon

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.VolumeUp
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material3.AssistChip
import androidx.compose.material3.Button
import androidx.compose.material3.Checkbox
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Snackbar
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import com.lectable.app.data.remote.dto.LexiconEntryDto
import com.lectable.app.data.remote.dto.UpsertLexiconRequestDto
import com.lectable.app.ui.components.ConfirmDialog

/** The book's series-wide pronunciation lexicon - see [LexiconViewModel]. Reached from the
 *  library's long-press menu and the reader's overflow menu. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun LexiconScreen(
    onBack: () -> Unit,
    viewModel: LexiconViewModel = hiltViewModel(),
) {
    val uiState by viewModel.uiState.collectAsState()
    val snackbarHostState = remember { SnackbarHostState() }
    var term by rememberSaveable { mutableStateOf("") }
    var replacement by rememberSaveable { mutableStateOf("") }
    var caseSensitive by rememberSaveable { mutableStateOf(false) }
    var prefix by rememberSaveable { mutableStateOf(false) }
    var global by rememberSaveable { mutableStateOf(false) }
    fun formRule() = UpsertLexiconRequestDto(term = term, replacement = replacement, caseSensitive = caseSensitive, prefix = prefix, global = global)
    LaunchedEffect(term, prefix, caseSensitive) { viewModel.checkMatches(formRule()) }
    fun loadForm(rule: UpsertLexiconRequestDto) {
        term = rule.term
        replacement = rule.replacement
        caseSensitive = rule.caseSensitive
        prefix = rule.prefix
        global = rule.global
    }
    var pendingDelete by remember { mutableStateOf<LexiconEntryDto?>(null) }
    val replacementFocus = remember { FocusRequester() }

    LaunchedEffect(uiState.error) {
        uiState.error?.let {
            snackbarHostState.showSnackbar(it)
            viewModel.dismissError()
        }
    }

    pendingDelete?.let { entry ->
        ConfirmDialog(
            title = "Remove \"${entry.term}\"?",
            text = "Audio for its ${entry.matches} matching paragraph(s) in this book - and any in the rest of " +
                "the series - will regenerate with the default pronunciation.",
            confirmLabel = "Remove",
            onConfirm = { viewModel.delete(entry); pendingDelete = null },
            onDismiss = { pendingDelete = null },
        )
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Column {
                        Text("Pronunciations")
                        Text(uiState.bookTitle, style = MaterialTheme.typography.labelSmall, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    }
                },
                navigationIcon = {
                    IconButton(onClick = onBack) { Icon(Icons.Filled.ArrowBack, contentDescription = "Back") }
                },
            )
        },
        snackbarHost = { SnackbarHost(snackbarHostState) { Snackbar(it) } },
    ) { padding ->
        LazyColumn(modifier = Modifier.fillMaxSize().padding(padding)) {
            item {
                Column(modifier = Modifier.fillMaxWidth().padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text(
                        "Say a word the way you spell it here - whole words, or with \"Derived words\" any word " +
                            "starting with it - overriding the automatic pronunciation pass. Rules are shared by every book in " +
                            (if (uiState.seriesName.isNotEmpty()) "the \"${uiState.seriesName}\" series" else "this book") +
                            ", or the whole library with \"All books\"; saving one regenerates the audio of every line it matches.",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                    OutlinedTextField(
                        value = term,
                        onValueChange = { term = it },
                        label = { Text("Word") },
                        placeholder = { Text("e.g. Nyx") },
                        singleLine = true,
                        modifier = Modifier.fillMaxWidth(),
                    )
                    OutlinedTextField(
                        value = replacement,
                        onValueChange = { replacement = it },
                        label = { Text("Say it as") },
                        placeholder = { Text("e.g. Nicks") },
                        singleLine = true,
                        modifier = Modifier.fillMaxWidth().focusRequester(replacementFocus),
                    )
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Checkbox(checked = caseSensitive, onCheckedChange = { caseSensitive = it })
                        Text("Match case", style = MaterialTheme.typography.bodyMedium)
                    }
                    // "gaol" also covers gaoler, gaols, gaolbird.
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Checkbox(checked = prefix, onCheckedChange = { prefix = it })
                        Text("Derived words (anything starting with it)", style = MaterialTheme.typography.bodyMedium)
                    }
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Checkbox(checked = global, onCheckedChange = { global = it })
                        Text("All books", style = MaterialTheme.typography.bodyMedium)
                    }
                    uiState.formMatches?.takeIf { term.isNotBlank() }?.let { m ->
                        val distinct = m.words.size + (m.moreWords ?: 0)
                        val tooBroad = prefix && distinct > 5
                        Text(
                            buildString {
                                if (m.paragraphs == 0) {
                                    append("No matches in this book.")
                                } else {
                                    append("Matches ${m.paragraphs} paragraph${if (m.paragraphs == 1) "" else "s"} here: ")
                                    append(m.words.joinToString(", "))
                                    m.moreWords?.takeIf { it > 0 }?.let { append(" and $it more") }
                                    append(".")
                                }
                                if (tooBroad) append(" That's $distinct different words - check \"Derived words\" isn't too broad.")
                                if (global) append(" (Counted in this book only.)")
                            },
                            style = MaterialTheme.typography.bodySmall,
                            color = if (tooBroad) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                    val formReady = term.isNotBlank() && replacement.isNotBlank()
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        OutlinedButton(
                            onClick = { viewModel.preview("form", formRule()) },
                            enabled = formReady && uiState.previewingKey == null,
                        ) {
                            if (uiState.previewingKey == "form") {
                                CircularProgressIndicator(modifier = Modifier.size(16.dp), strokeWidth = 2.dp)
                            } else {
                                Icon(Icons.AutoMirrored.Filled.VolumeUp, contentDescription = null)
                            }
                            Text("  Listen")
                        }
                        Button(
                            onClick = {
                                viewModel.save(formRule()) { loadForm(UpsertLexiconRequestDto()) }
                            },
                            enabled = formReady && !uiState.saving,
                        ) { Text("Save") }
                    }
                }
            }
            if (uiState.suggestions.isNotEmpty()) {
                item {
                    Text(
                        "Names without a rule",
                        style = MaterialTheme.typography.labelMedium,
                        modifier = Modifier.padding(horizontal = 16.dp),
                    )
                    LazyRow(
                        contentPadding = PaddingValues(horizontal = 16.dp, vertical = 4.dp),
                        horizontalArrangement = Arrangement.spacedBy(6.dp),
                    ) {
                        items(uiState.suggestions, key = { it.term }) { sug ->
                            AssistChip(
                                onClick = {
                                    loadForm(UpsertLexiconRequestDto(term = sug.term))
                                    replacementFocus.requestFocus()
                                },
                                label = { Text(if (sug.unusual) "${sug.term} •" else sug.term) },
                            )
                        }
                    }
                }
            }
            item { HorizontalDivider(modifier = Modifier.padding(top = 8.dp)) }
            when {
                uiState.loading -> item {
                    CircularProgressIndicator(modifier = Modifier.padding(24.dp))
                }
                uiState.entries.isEmpty() -> item {
                    Text(
                        "No rules yet.",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(16.dp),
                    )
                }
                else -> items(uiState.entries, key = { it.id }) { entry ->
                    LexiconRow(
                        entry = entry,
                        previewing = uiState.previewingKey == entry.id,
                        previewEnabled = uiState.previewingKey == null,
                        onPreview = { viewModel.preview(entry.id, entry.toRequest()) },
                        onEdit = { loadForm(entry.toRequest()) },
                        onDelete = { pendingDelete = entry },
                    )
                }
            }
        }
    }
}

@Composable
private fun LexiconRow(
    entry: LexiconEntryDto,
    previewing: Boolean,
    previewEnabled: Boolean,
    onPreview: () -> Unit,
    onEdit: () -> Unit,
    onDelete: () -> Unit,
) {
    Row(
        verticalAlignment = Alignment.CenterVertically,
        modifier = Modifier.fillMaxWidth().padding(start = 16.dp, end = 4.dp, top = 4.dp, bottom = 4.dp),
    ) {
        Column(modifier = Modifier.weight(1f)) {
            Text(
                buildString {
                    append(entry.term)
                    if (entry.prefix) append("…")
                    append(" → ${entry.replacement}")
                    if (entry.caseSensitive) append(" (match case)")
                    if (entry.global) append(" (all books)")
                },
                style = MaterialTheme.typography.bodyLarge,
            )
            Text(
                "${entry.matches} matching paragraph${if (entry.matches == 1) "" else "s"} in this book" +
                    if (entry.matchedWords.isNotEmpty()) ": ${entry.matchedWords.joinToString(", ")}" else "",
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
        }
        IconButton(onClick = onPreview, enabled = previewEnabled) {
            if (previewing) {
                CircularProgressIndicator(modifier = Modifier.size(20.dp), strokeWidth = 2.dp)
            } else {
                Icon(Icons.AutoMirrored.Filled.VolumeUp, contentDescription = "Listen")
            }
        }
        IconButton(onClick = onEdit) { Icon(Icons.Filled.Edit, contentDescription = "Edit") }
        IconButton(onClick = onDelete) { Icon(Icons.Filled.Delete, contentDescription = "Remove") }
    }
}
