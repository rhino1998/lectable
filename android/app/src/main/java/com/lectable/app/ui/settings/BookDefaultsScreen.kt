package com.lectable.app.ui.settings

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.lectable.app.data.live.LiveStore
import com.lectable.app.data.remote.dto.ApiCatalog
import com.lectable.app.data.remote.dto.CharacterVoiceMode
import com.lectable.app.data.remote.dto.CharacterVoiceModes
import com.lectable.app.data.remote.dto.VoiceSettingsDto
import com.lectable.app.data.repository.VoiceRepository
import dagger.hilt.android.lifecycle.HiltViewModel
import javax.inject.Inject
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class BookDefaultsUiState(
    /** GET /api/voices/default - null until the topic's first snapshot. */
    val defaults: VoiceSettingsDto? = null,
    val saving: Boolean = false,
    val error: String? = null,
)

/** What a newly added book starts with - the Android analogue of the web Settings page
 *  (frontend/src/pages/SettingsPage.tsx). The value is the live "default voice" topic, so a
 *  change from the web shows up here too. */
@HiltViewModel
class BookDefaultsViewModel @Inject constructor(
    private val voiceRepository: VoiceRepository,
    liveStore: LiveStore,
) : ViewModel() {
    private val _uiState = MutableStateFlow(BookDefaultsUiState())
    val uiState: StateFlow<BookDefaultsUiState> = _uiState

    init {
        viewModelScope.launch {
            liveStore.defaultVoice().collect { result ->
                _uiState.update { it.copy(defaults = result.data ?: it.defaults, error = result.error?.message ?: it.error) }
            }
        }
    }

    fun update(next: VoiceSettingsDto) {
        viewModelScope.launch {
            _uiState.update { it.copy(saving = true) }
            runCatching { voiceRepository.updateDefaultVoice(next) }
                .onSuccess { saved -> _uiState.update { it.copy(defaults = saved, error = null) } }
                .onFailure { e -> _uiState.update { it.copy(error = e.message ?: "Could not save defaults") } }
            _uiState.update { it.copy(saving = false) }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun BookDefaultsScreen(
    onBack: () -> Unit,
    viewModel: BookDefaultsViewModel = hiltViewModel(),
) {
    val uiState by viewModel.uiState.collectAsState()
    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Defaults for new books") },
                navigationIcon = {
                    IconButton(onClick = onBack) { Icon(Icons.Filled.ArrowBack, contentDescription = "Back") }
                },
            )
        },
    ) { padding ->
        Column(modifier = Modifier.fillMaxSize().padding(padding).verticalScroll(rememberScrollState())) {
            uiState.error?.let { error ->
                Text(error, color = MaterialTheme.colorScheme.error, modifier = Modifier.padding(16.dp))
            }
            val defaults = uiState.defaults
            if (defaults == null) {
                if (uiState.error == null) CircularProgressIndicator(modifier = Modifier.padding(24.dp))
            } else {
                BookDefaultsSection(defaults = defaults, enabled = !uiState.saving, onChange = viewModel::update)
            }
        }
    }
}

// Kept in step with frontend/src/api/types.ts's CHARACTER_VOICE_MODE_LABELS/_DESCRIPTIONS.
private fun characterVoiceModeLabel(mode: CharacterVoiceMode): String = when (mode) {
    CharacterVoiceModes.NARRATOR -> "Narrator only"
    CharacterVoiceModes.ASSIGNED -> "Custom character voices"
    CharacterVoiceModes.INSTRUCT_UNASSIGNED -> "Style unassigned characters with the narrator's voice"
    CharacterVoiceModes.INSTRUCT_ALL -> "Style all characters with the narrator's voice"
    else -> mode
}

private fun characterVoiceModeDescription(mode: CharacterVoiceMode): String = when (mode) {
    CharacterVoiceModes.NARRATOR -> "Every paragraph narrates in the book's own single voice, regardless of attributed speaker."
    CharacterVoiceModes.ASSIGNED -> "A character with a voice assigned narrates in that voice; an unassigned character " +
        "still narrates in the book's own."
    CharacterVoiceModes.INSTRUCT_UNASSIGNED -> "A character with no voice assigned narrates in the book's narrator voice, " +
        "styled by their own characterization. Only for the BreezeTTS clone model."
    CharacterVoiceModes.INSTRUCT_ALL -> "Every character narrates in the book's narrator voice, styled by their own " +
        "characterization - even one with a voice assigned. Only for the BreezeTTS clone model."
    else -> ""
}

/** The clone model and per-book switches a newly added book starts with - the fields of the web
 *  Settings page's "Defaults for new books" (frontend BookSettingsFields). */
@Composable
private fun BookDefaultsSection(
    defaults: VoiceSettingsDto,
    enabled: Boolean,
    onChange: (VoiceSettingsDto) -> Unit,
    modifier: Modifier = Modifier,
) {
    var modeMenuOpen by remember { mutableStateOf(false) }
    var cloneMenuOpen by remember { mutableStateOf(false) }
    val cloneModel = defaults.cloneModel.ifEmpty { ApiCatalog.DEFAULT_CLONE_MODEL }
    Column(modifier = modifier.fillMaxWidth().padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Text(
            "What a newly added book starts with. Each book has its own voice and switches; changing these " +
                "doesn't touch existing books. The default voice is set from Your voices.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )

        Text("Cloning model", style = MaterialTheme.typography.bodyLarge)
        Box {
            OutlinedButton(onClick = { cloneMenuOpen = true }, enabled = enabled) {
                Text(ApiCatalog.CLONE_MODELS.firstOrNull { it.id == cloneModel }?.label ?: cloneModel)
            }
            DropdownMenu(expanded = cloneMenuOpen, onDismissRequest = { cloneMenuOpen = false }) {
                ApiCatalog.CLONE_MODELS.forEach { model ->
                    DropdownMenuItem(
                        text = { Text(model.label) },
                        onClick = {
                            cloneMenuOpen = false
                            if (model.id != cloneModel) onChange(defaults.copy(cloneModel = model.id))
                        },
                    )
                }
            }
        }

        Text("Character voices", style = MaterialTheme.typography.bodyLarge)
        Box {
            OutlinedButton(onClick = { modeMenuOpen = true }, enabled = enabled) {
                Text(characterVoiceModeLabel(defaults.characterVoiceMode))
            }
            DropdownMenu(expanded = modeMenuOpen, onDismissRequest = { modeMenuOpen = false }) {
                CharacterVoiceModes.VALUES.forEach { mode ->
                    DropdownMenuItem(
                        text = { Text(characterVoiceModeLabel(mode)) },
                        onClick = {
                            modeMenuOpen = false
                            if (mode != defaults.characterVoiceMode) onChange(defaults.copy(characterVoiceMode = mode))
                        },
                    )
                }
            }
        }
        Text(
            characterVoiceModeDescription(defaults.characterVoiceMode),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )

        DefaultSwitch(
            title = "Wait for emotion labeling and pronunciation",
            description = "A chapter's audio waits for emotion labeling and pronunciation resolution, so each line is " +
                "voiced right the first time instead of needing a later regenerate.",
            checked = defaults.speechDirection,
            enabled = enabled,
            onCheckedChange = { onChange(defaults.copy(speechDirection = it)) },
        )
        DefaultSwitch(
            title = "Wait for speaker attribution",
            description = "With character voices in use, a chapter is only narrated once its speakers are attributed, " +
                "so every line is generated once, in its speaker's voice.",
            checked = defaults.strictOrder ?: false,
            enabled = enabled,
            onCheckedChange = { onChange(defaults.copy(strictOrder = it)) },
        )
        DefaultSwitch(
            title = "Require the book setting",
            description = "The passes that read the book's setting fail if it can't be derived, instead of running " +
                "without it.",
            checked = defaults.settingRequired ?: false,
            enabled = enabled,
            onCheckedChange = { onChange(defaults.copy(settingRequired = it)) },
        )
        DefaultSwitch(
            title = "Background music",
            description = "Each chapter's background music is generated and mixed in under narration.",
            checked = defaults.musicEnabled,
            enabled = enabled,
            onCheckedChange = { onChange(defaults.copy(musicEnabled = it)) },
        )
    }
}

@Composable
private fun DefaultSwitch(
    title: String,
    description: String,
    checked: Boolean,
    enabled: Boolean,
    onCheckedChange: (Boolean) -> Unit,
) {
    Row(verticalAlignment = Alignment.CenterVertically, modifier = Modifier.fillMaxWidth()) {
        Column(modifier = Modifier.weight(1f).padding(end = 12.dp)) {
            Text(title, style = MaterialTheme.typography.bodyLarge)
            Text(description, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Switch(checked = checked, enabled = enabled, onCheckedChange = onCheckedChange)
    }
}
