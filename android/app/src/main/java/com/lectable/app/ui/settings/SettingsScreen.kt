package com.lectable.app.ui.settings

import androidx.compose.foundation.layout.Arrangement
import com.lectable.app.playback.lighting.LightingStatus
import com.lectable.app.data.settings.MIN_LIGHT_BRIGHTNESS_CAP
import androidx.compose.material3.Switch
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.FilterChip
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material.icons.filled.BrightnessAuto
import androidx.compose.material.icons.filled.DarkMode
import androidx.compose.material.icons.filled.LightMode
import androidx.compose.material3.AssistChip
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SegmentedButton
import androidx.compose.material3.SegmentedButtonDefaults
import androidx.compose.material3.SingleChoiceSegmentedButtonRow
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import com.lectable.app.data.settings.LOOKAHEAD_PARAGRAPHS_STEP
import com.lectable.app.data.settings.MAX_LOOKAHEAD_PARAGRAPHS
import com.lectable.app.data.settings.MAX_SPEED_RAMP_SECONDS
import com.lectable.app.data.settings.SPEED_RAMP_SECONDS_STEP
import com.lectable.app.data.settings.MIN_LOOKAHEAD_PARAGRAPHS
import com.lectable.app.data.settings.ReaderFontFamily
import com.lectable.app.data.settings.MAX_FONT_SIZE_SP
import com.lectable.app.data.settings.MIN_FONT_SIZE_SP
import com.lectable.app.data.settings.ThemePreference
import com.lectable.app.data.settings.toComposeFontFamily
import com.lectable.app.ui.components.CleanSlider
import com.lectable.app.ui.components.ConfirmDialog
import kotlin.math.roundToInt

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SettingsScreen(
    onBack: () -> Unit,
    onOpenVoices: () -> Unit,
    onOpenJobs: () -> Unit,
    onOpenLighting: () -> Unit,
    viewModel: SettingsViewModel = hiltViewModel(),
) {
    val uiState by viewModel.uiState.collectAsState()
    val theme by viewModel.themePreference.collectAsState()
    val fontSize by viewModel.fontSize.collectAsState()
    val fontFamily by viewModel.fontFamily.collectAsState()
    val lookaheadParagraphs by viewModel.lookaheadParagraphs.collectAsState()
    val speedRampSeconds by viewModel.speedRampSeconds.collectAsState()
    val discoveredServers by viewModel.discoveredServers.collectAsState()

    DisposableEffect(Unit) {
        viewModel.startDiscovery()
        onDispose { viewModel.stopDiscovery() }
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Settings") },
                navigationIcon = {
                    IconButton(onClick = onBack) { Icon(Icons.Filled.ArrowBack, contentDescription = "Back") }
                },
            )
        },
    ) { padding ->
        Column(
            modifier = Modifier
                .fillMaxSize()
                .padding(padding)
                .verticalScroll(rememberScrollState())
                .padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            Text(
                "Lectable's backend runs on your own network (see the root CLAUDE.md) - " +
                    "point this at wherever it's listening, e.g. http://192.168.1.42:8080",
                style = MaterialTheme.typography.bodyMedium,
            )
            OutlinedTextField(
                value = uiState.serverUrl,
                onValueChange = viewModel::onUrlChanged,
                label = { Text("Backend URL") },
                isError = !uiState.isValid,
                supportingText = { if (!uiState.isValid) Text("Enter a valid host, e.g. 192.168.1.42:8080") },
                singleLine = true,
                modifier = Modifier.fillMaxWidth(),
            )
            Button(onClick = viewModel::save) {
                Text(if (uiState.saved) "Saved" else "Save")
            }

            Row(verticalAlignment = Alignment.CenterVertically) {
                Text("Found on this network", style = MaterialTheme.typography.titleMedium)
                if (discoveredServers.isEmpty()) {
                    CircularProgressIndicator(
                        modifier = Modifier
                            .padding(start = 8.dp)
                            .size(16.dp),
                        strokeWidth = 2.dp,
                    )
                }
            }
            if (discoveredServers.isEmpty()) {
                Text(
                    "Searching for a Lectable backend advertising itself via mDNS...",
                    style = MaterialTheme.typography.bodySmall,
                )
            } else {
                LazyRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    items(discoveredServers, key = { it.name }) { server ->
                        AssistChip(
                            onClick = { viewModel.selectDiscoveredServer(server) },
                            label = { Text("${server.name} (${server.host}:${server.port})") },
                        )
                    }
                }
            }

            Text("Theme", style = MaterialTheme.typography.titleMedium)
            SingleChoiceSegmentedButtonRow(modifier = Modifier.fillMaxWidth()) {
                val options = listOf(
                    Triple(ThemePreference.SYSTEM, Icons.Filled.BrightnessAuto, "System"),
                    Triple(ThemePreference.LIGHT, Icons.Filled.LightMode, "Light"),
                    Triple(ThemePreference.DARK, Icons.Filled.DarkMode, "Dark"),
                )
                options.forEachIndexed { index, (preference, icon, label) ->
                    SegmentedButton(
                        selected = theme == preference,
                        onClick = { viewModel.setTheme(preference) },
                        shape = SegmentedButtonDefaults.itemShape(index = index, count = options.size),
                        icon = { Icon(icon, contentDescription = null) },
                    ) {
                        Text(label)
                    }
                }
            }

            Text("Reading font size (${fontSize}sp)", style = MaterialTheme.typography.titleMedium)
            CleanSlider(
                value = fontSize.toFloat(),
                onValueChange = { viewModel.setFontSize(it.roundToInt()) },
                valueRange = MIN_FONT_SIZE_SP.toFloat()..MAX_FONT_SIZE_SP.toFloat(),
                // One stop per whole sp value between the endpoints, matching setFontSize's own
                // whole-number-only contract (ReadingSettingsRepository.setFontSize).
                steps = MAX_FONT_SIZE_SP - MIN_FONT_SIZE_SP - 1,
                startLabel = "${MIN_FONT_SIZE_SP}sp",
                endLabel = "${MAX_FONT_SIZE_SP}sp",
                modifier = Modifier.fillMaxWidth(),
            )

            Text("Reading font", style = MaterialTheme.typography.titleMedium)
            SingleChoiceSegmentedButtonRow(modifier = Modifier.fillMaxWidth()) {
                ReaderFontFamily.entries.forEachIndexed { index, family ->
                    SegmentedButton(
                        selected = fontFamily == family,
                        onClick = { viewModel.setFontFamily(family) },
                        shape = SegmentedButtonDefaults.itemShape(index = index, count = ReaderFontFamily.entries.size),
                    ) {
                        Text(family.label, fontFamily = family.toComposeFontFamily())
                    }
                }
            }

            Text("Generation lookahead ($lookaheadParagraphs paragraphs)", style = MaterialTheme.typography.titleMedium)
            Text(
                "How far ahead of playback the server generates audio. Higher means fewer waits, " +
                    "but more queued work to throw away when you jump around.",
                style = MaterialTheme.typography.bodySmall,
            )
            CleanSlider(
                value = lookaheadParagraphs.toFloat(),
                onValueChange = { viewModel.setLookaheadParagraphs(it.roundToInt()) },
                valueRange = MIN_LOOKAHEAD_PARAGRAPHS.toFloat()..MAX_LOOKAHEAD_PARAGRAPHS.toFloat(),
                steps = (MAX_LOOKAHEAD_PARAGRAPHS - MIN_LOOKAHEAD_PARAGRAPHS) / LOOKAHEAD_PARAGRAPHS_STEP - 1,
                startLabel = "$MIN_LOOKAHEAD_PARAGRAPHS",
                endLabel = "$MAX_LOOKAHEAD_PARAGRAPHS",
                modifier = Modifier.fillMaxWidth(),
            )

            Text(
                if (speedRampSeconds > 0f) "Speed change ramp (${"%.2f".format(speedRampSeconds)}s per 0.5×)" else "Speed change ramp (off)",
                style = MaterialTheme.typography.titleMedium,
            )
            Text(
                "How gradually a new playback speed takes effect while narration is playing. Off applies it at once.",
                style = MaterialTheme.typography.bodySmall,
            )
            CleanSlider(
                value = speedRampSeconds,
                onValueChange = { viewModel.setSpeedRampSeconds((it / SPEED_RAMP_SECONDS_STEP).roundToInt() * SPEED_RAMP_SECONDS_STEP) },
                valueRange = 0f..MAX_SPEED_RAMP_SECONDS,
                steps = (MAX_SPEED_RAMP_SECONDS / SPEED_RAMP_SECONDS_STEP).roundToInt() - 1,
                startLabel = "Off",
                endLabel = "${MAX_SPEED_RAMP_SECONDS.roundToInt()}s",
                modifier = Modifier.fillMaxWidth(),
            )

            Text("Room lighting", style = MaterialTheme.typography.titleMedium)
            Button(onClick = onOpenLighting) {
                Text("Philips Hue lighting")
            }

            Text("Voices", style = MaterialTheme.typography.titleMedium)
            Button(onClick = onOpenVoices) {
                Text("Manage custom voices")
            }

            Text("Job queue", style = MaterialTheme.typography.titleMedium)
            Button(onClick = onOpenJobs) {
                Text("View job queue")
            }

            Text("Downloads", style = MaterialTheme.typography.titleMedium)
            var confirmingDeleteAll by remember { mutableStateOf(false) }
            if (uiState.downloadedBookCount == 0) {
                Text("No books downloaded for offline reading.", style = MaterialTheme.typography.bodyMedium)
            } else {
                Text(
                    "${uiState.downloadedBookCount} book(s), ${formatBytes(uiState.downloadedBytes)} of offline audio.",
                    style = MaterialTheme.typography.bodyMedium,
                )
                Button(onClick = { confirmingDeleteAll = true }, colors = ButtonDefaults.buttonColors(containerColor = MaterialTheme.colorScheme.error)) {
                    Text("Delete all downloads")
                }
            }
            if (confirmingDeleteAll) {
                ConfirmDialog(
                    title = "Delete all downloads?",
                    text = "Every downloaded book's offline audio (${formatBytes(uiState.downloadedBytes)}) will be removed from this " +
                        "device. Nothing is deleted from the server.",
                    onConfirm = viewModel::deleteAllDownloads,
                    onDismiss = { confirmingDeleteAll = false },
                )
            }
        }
    }
}

/** Philips Hue room lighting driven by the book's ambience pass - pair a bridge, pick a room, cap
 *  brightness. The lights themselves are driven by HueLightingController, which follows playback
 *  whether or not this screen is open. */
@Composable
internal fun HueLightingSection(viewModel: SettingsViewModel) {
    val uiState by viewModel.uiState.collectAsState()
    val settings by viewModel.lightingSettings.collectAsState()
    val lighting by viewModel.lightingState.collectAsState()

    Text("Room lighting (Philips Hue)", style = MaterialTheme.typography.titleMedium)
    Text(
        "Colors and effects come from each chapter's ambience pass. The phone talks to your Hue " +
            "bridge directly over the local network.",
        style = MaterialTheme.typography.bodySmall,
    )
    Row(verticalAlignment = Alignment.CenterVertically) {
        Switch(
            checked = settings.enabled,
            enabled = settings.configured,
            onCheckedChange = viewModel::setLightingEnabled,
        )
        Spacer(Modifier.width(12.dp))
        val status = when (lighting.status) {
            LightingStatus.OFF -> "Off"
            LightingStatus.UNCONFIGURED -> "Pick a bridge and room below"
            LightingStatus.CONNECTING -> "Connecting..."
            LightingStatus.ACTIVE -> "Lighting the room" + (lighting.cue?.description?.let { " - $it" } ?: "")
            LightingStatus.ERROR -> "Error: ${lighting.message ?: "unknown"}"
        }
        Text(status, style = MaterialTheme.typography.bodyMedium)
    }

    if (!settings.paired) {
        OutlinedTextField(
            value = uiState.hueBridgeIp,
            onValueChange = viewModel::onHueBridgeIpChanged,
            label = { Text("Bridge IP address") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            OutlinedButton(onClick = viewModel::discoverHueBridges, enabled = uiState.hueBusy == null) {
                Text(if (uiState.hueBusy == HueBusy.DISCOVERING) "Searching..." else "Find bridge")
            }
            Button(
                onClick = viewModel::pairHueBridge,
                enabled = uiState.hueBusy == null && uiState.hueBridgeIp.isNotBlank(),
            ) {
                Text(if (uiState.hueBusy == HueBusy.PAIRING) "Press the bridge's link button..." else "Pair")
            }
        }
        if (uiState.hueFound.size > 1) {
            LazyRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                items(uiState.hueFound, key = { it.id }) { bridge ->
                    AssistChip(onClick = { viewModel.onHueBridgeIpChanged(bridge.ip) }, label = { Text(bridge.ip) })
                }
            }
        }
    } else {
        Text("Room", style = MaterialTheme.typography.bodyMedium)
        val groups = uiState.hueGroups
        if (groups == null) {
            Text("Loading rooms...", style = MaterialTheme.typography.bodySmall)
        } else {
            LazyRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                items(groups, key = { it.id }) { group ->
                    FilterChip(
                        selected = settings.groupId == group.id,
                        onClick = { viewModel.setHueGroup(group.id) },
                        label = { Text("${group.name} (${group.lights.size})") },
                    )
                }
            }
        }
        Text("Brightness cap (${(settings.brightnessCap * 100).roundToInt()}%)", style = MaterialTheme.typography.bodyMedium)
        CleanSlider(
            value = settings.brightnessCap,
            onValueChange = viewModel::setLightBrightnessCap,
            valueRange = MIN_LIGHT_BRIGHTNESS_CAP..1f,
            steps = 17,
            startLabel = "${(MIN_LIGHT_BRIGHTNESS_CAP * 100).roundToInt()}%",
            endLabel = "100%",
            modifier = Modifier.fillMaxWidth(),
        )
        Row(verticalAlignment = Alignment.CenterVertically) {
            Switch(checked = settings.restoreOnStop, onCheckedChange = viewModel::setRestoreLightsOnStop)
            Spacer(Modifier.width(12.dp))
            Text("Restore the lights when lighting turns off", style = MaterialTheme.typography.bodyMedium)
        }
        OutlinedButton(onClick = viewModel::forgetHueBridge) {
            Text("Forget bridge ${settings.bridgeIp}")
        }
    }
    uiState.hueError?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
}

/** e.g. "128 MB" / "1.4 GB" - no existing formatter in this codebase to reuse (chapter download
 *  progress is reported as ready/total paragraph counts, not bytes). */
private fun formatBytes(bytes: Long): String {
    if (bytes < 1024) return "$bytes B"
    val units = listOf("KB", "MB", "GB", "TB")
    var value = bytes / 1024.0
    var unitIdx = 0
    while (value >= 1024 && unitIdx < units.lastIndex) {
        value /= 1024
        unitIdx++
    }
    return "%.1f %s".format(value, units[unitIdx])
}
