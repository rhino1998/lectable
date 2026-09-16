package com.lectable.app.ui.voices

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Slider
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableFloatStateOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.Saver
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import com.lectable.app.data.remote.dto.CustomVoicePresetDto
import com.lectable.app.data.remote.dto.CustomVoicePresetInputDto
import com.lectable.app.ui.components.ConfirmDialog
import kotlin.random.Random

// Kept identical to tts-service's own DEFAULT_REF_TEXT (voices.py) and frontend's VoicesPage.tsx -
// the same phonetically-balanced line, so a custom voice's default reference clip samples
// English phonemes as broadly as the built-in presets do.
private const val DEFAULT_REF_TEXT = "The beige hue on the waters of the loch impressed all, including the French queen, " +
    "before she heard that symphony again, just as young Arthur wanted."

// Matches tts-service's WSOLA range (wsola.py's MIN/MAX_PLAYBACK_RATE), same as VoicesPage.tsx.
private const val SPEED_MIN = 0.5f
private const val SPEED_MAX = 4f

private sealed class VoiceFormTarget {
    data object None : VoiceFormTarget()
    data class New(val derivedFrom: CustomVoicePresetDto? = null) : VoiceFormTarget()
    data class Edit(val preset: CustomVoicePresetDto) : VoiceFormTarget()
}

/** The Android analogue of frontend/src/pages/VoicesPage.tsx's custom-voice management - list,
 *  create, edit, delete, and audition your own reusable narrator voices (as opposed to
 *  ReaderScreen's VoicePickerSheet, which only ever *picks* one of these for a specific book). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun VoicesScreen(
    onBack: () -> Unit,
    viewModel: VoicesViewModel = hiltViewModel(),
) {
    val uiState by viewModel.uiState.collectAsState()
    var target by rememberSaveable(stateSaver = VoiceFormTargetSaver) { mutableStateOf<VoiceFormTarget>(VoiceFormTarget.None) }
    var pendingDelete by remember { mutableStateOf<CustomVoicePresetDto?>(null) }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Your voices") },
                navigationIcon = {
                    IconButton(onClick = onBack) { Icon(Icons.Filled.ArrowBack, contentDescription = "Back") }
                },
                actions = {
                    if (target == VoiceFormTarget.None) {
                        IconButton(onClick = { target = VoiceFormTarget.New() }) {
                            Icon(Icons.Filled.Add, contentDescription = "New voice")
                        }
                    }
                },
            )
        },
    ) { padding ->
        Box(modifier = Modifier.fillMaxSize().padding(padding), contentAlignment = Alignment.Center) {
            when (val t = target) {
                VoiceFormTarget.None -> when {
                    uiState.loading -> CircularProgressIndicator()
                    uiState.error != null -> Text(
                        "Could not load your voices: ${uiState.error}",
                        color = MaterialTheme.colorScheme.error,
                        modifier = Modifier.padding(24.dp),
                    )
                    uiState.presets.isEmpty() -> Text(
                        "No custom voices yet. Tap + to create one.",
                        style = MaterialTheme.typography.bodyMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(24.dp),
                    )
                    else -> LazyColumn(modifier = Modifier.fillMaxSize()) {
                        items(uiState.presets, key = { it.id }) { preset ->
                            VoiceListRow(
                                preset = preset,
                                isDefault = preset.id == uiState.defaultPresetId,
                                onPlay = { viewModel.playReferenceClip(preset) },
                                onEdit = { target = VoiceFormTarget.Edit(preset) },
                                onDerive = { target = VoiceFormTarget.New(derivedFrom = preset) },
                                onSetDefault = { viewModel.setDefaultVoice(preset) },
                                onDeleteRequested = { pendingDelete = preset },
                            )
                        }
                    }
                }
                is VoiceFormTarget.New -> VoiceForm(
                    editing = null,
                    prefillFrom = t.derivedFrom,
                    onSave = { input, onError -> viewModel.createPreset(input, { target = VoiceFormTarget.None }, onError) },
                    onCancel = { target = VoiceFormTarget.None },
                    onTest = viewModel::testVoice,
                )
                is VoiceFormTarget.Edit -> VoiceForm(
                    editing = t.preset,
                    prefillFrom = null,
                    onSave = { input, onError -> viewModel.updatePreset(t.preset.id, input, { target = VoiceFormTarget.None }, onError) },
                    onCancel = { target = VoiceFormTarget.None },
                    onTest = viewModel::testVoice,
                )
            }
        }
    }

    pendingDelete?.let { preset ->
        ConfirmDialog(
            title = "Delete voice?",
            text = "\"${preset.name}\" will be removed. Any book currently narrated with it keeps its " +
                "already-generated audio, but can't use this voice for anything new.",
            onConfirm = { viewModel.deletePreset(preset.id) },
            onDismiss = { pendingDelete = null },
        )
    }
}

// rememberSaveable needs a Saver since VoiceFormTarget.Edit/New(derivedFrom=...) can carry a
// non-Parcelable DTO - only a plain, source-less New and None survive a config change; landing
// back on an empty "new voice" (or None) after a rotation mid-edit/mid-derive is an acceptable
// trade for not having to make every voice DTO Parcelable just for this.
private val VoiceFormTargetSaver = Saver<VoiceFormTarget, String>(
    save = { if (it is VoiceFormTarget.New) "new" else "none" },
    restore = { if (it == "new") VoiceFormTarget.New() else VoiceFormTarget.None },
)

@Composable
private fun VoiceListRow(
    preset: CustomVoicePresetDto,
    isDefault: Boolean,
    onPlay: () -> Unit,
    onEdit: () -> Unit,
    onDerive: () -> Unit,
    onSetDefault: () -> Unit,
    onDeleteRequested: () -> Unit,
) {
    Column(modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp)) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            IconButton(onClick = onPlay) {
                Icon(Icons.Filled.PlayArrow, contentDescription = "Play reference clip")
            }
            Column(modifier = Modifier.weight(1f)) {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(preset.name, style = MaterialTheme.typography.bodyLarge)
                    if (isDefault) {
                        Text(
                            "  ·  Default",
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.primary,
                        )
                    }
                }
                Text(
                    preset.instruct,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 2,
                )
                if (preset.refError != null) {
                    Text(
                        "Reference clip failed to render: ${preset.refError}",
                        style = MaterialTheme.typography.labelSmall,
                        color = MaterialTheme.colorScheme.error,
                    )
                }
            }
            IconButton(onClick = onDeleteRequested) {
                Icon(Icons.Filled.Delete, contentDescription = "Delete voice")
            }
        }
        Row {
            TextButton(onClick = onEdit) { Text("Edit") }
            TextButton(onClick = onDerive) { Text("Derive a new voice") }
            if (!isDefault) {
                TextButton(onClick = onSetDefault) { Text("Set as default") }
            }
        }
    }
}

@Composable
private fun VoiceForm(
    editing: CustomVoicePresetDto?,
    prefillFrom: CustomVoicePresetDto?,
    onSave: (CustomVoicePresetInputDto, onError: (String) -> Unit) -> Unit,
    onCancel: () -> Unit,
    onTest: (presetId: String?, instruct: String, text: String, seed: Int?, onError: (String) -> Unit, onDone: () -> Unit) -> Unit,
) {
    // editing takes precedence, then a "derive from" source, then a blank default - editing and
    // prefillFrom are never both non-null in practice (see VoicesScreen's call sites) but this
    // ordering makes the fallback chain unambiguous either way.
    var name by rememberSaveable { mutableStateOf(editing?.name ?: prefillFrom?.let { "${it.name} (derived)" } ?: "") }
    var instruct by rememberSaveable { mutableStateOf(editing?.instruct ?: prefillFrom?.instruct ?: "") }
    var refText by rememberSaveable { mutableStateOf(editing?.refText ?: prefillFrom?.refText ?: DEFAULT_REF_TEXT) }
    var speed by rememberSaveable { mutableFloatStateOf((editing?.speedMultiplier ?: prefillFrom?.speedMultiplier ?: 1.0).toFloat()) }
    var testText by rememberSaveable { mutableStateOf(DEFAULT_REF_TEXT) }
    var saveError by rememberSaveable { mutableStateOf<String?>(null) }
    var testError by rememberSaveable { mutableStateOf<String?>(null) }
    var testing by rememberSaveable { mutableStateOf(false) }
    // Pinned once up front for a brand-new voice (not left for the server to pick at save time)
    // so a design preview and the eventual Create call are guaranteed to render the same clip -
    // see backend voicerefs.DesignConfigHash. When deriving from an existing voice, reuses its
    // exact seed instead - unchanged instruct/refText/seed/speed together are what the backend
    // recognizes as "this is the same rendered clip" (voicerefs' derivation-source lookup), so
    // keeping the seed is what lets a speed-only derivation skip a full re-render. An existing
    // preset's own seed is never surfaced for *editing* here, so update requests simply omit it
    // (nil on update = unchanged).
    val newSeed = rememberSaveable { prefillFrom?.seed ?: Random.nextInt(0, Int.MAX_VALUE) }

    Column(
        modifier = Modifier.fillMaxSize().padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        OutlinedTextField(
            value = name,
            onValueChange = { name = it },
            label = { Text("Name") },
            placeholder = { Text("e.g. Grandpa Joe") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = instruct,
            onValueChange = { instruct = it },
            label = { Text("Voice instruction") },
            placeholder = { Text("Describe the voice: pace, tone, accent…") },
            minLines = 2,
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = refText,
            onValueChange = { refText = it },
            label = { Text("Reference line") },
            minLines = 2,
            modifier = Modifier.fillMaxWidth(),
        )
        Column {
            Text("Speed (${"%.2f".format(speed)}×)", style = MaterialTheme.typography.labelLarge)
            Slider(value = speed, onValueChange = { speed = it }, valueRange = SPEED_MIN..SPEED_MAX)
        }
        if (saveError != null) {
            Text(saveError!!, color = MaterialTheme.colorScheme.error)
        }
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Button(
                onClick = {
                    saveError = null
                    val input = CustomVoicePresetInputDto(
                        name = name,
                        instruct = instruct,
                        refText = refText,
                        seed = if (editing == null) newSeed else null,
                        speedMultiplier = speed.toDouble(),
                    )
                    onSave(input) { message -> saveError = message }
                },
                enabled = name.isNotBlank() && instruct.isNotBlank() && refText.isNotBlank(),
            ) {
                Text(if (editing != null) "Save changes" else "Create voice")
            }
            TextButton(onClick = onCancel) { Text("Cancel") }
        }

        Text("Test this voice", style = MaterialTheme.typography.titleMedium)
        Text(
            if (editing != null) {
                "Uses the saved voice."
            } else {
                "Previews the instruction above directly - creating the voice right after, " +
                    "unchanged, reuses this exact clip instead of rendering again."
            },
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
        OutlinedTextField(
            value = testText,
            onValueChange = { testText = it },
            label = { Text("Text to say") },
            minLines = 2,
            modifier = Modifier.fillMaxWidth(),
        )
        if (testError != null) {
            Text(testError!!, color = MaterialTheme.colorScheme.error)
        }
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Button(
                onClick = {
                    testError = null
                    testing = true
                    onTest(
                        editing?.id,
                        instruct,
                        testText,
                        if (editing == null) newSeed else null,
                        { message -> testError = message },
                        { testing = false },
                    )
                },
                enabled = testText.isNotBlank() && instruct.isNotBlank() && refText.isNotBlank() && !testing,
            ) {
                Text(if (testing) "Synthesizing…" else "Play test")
            }
        }
    }
}
