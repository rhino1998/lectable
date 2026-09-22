package com.lectable.app.ui.jobs

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material.icons.filled.Cancel
import androidx.compose.material.icons.filled.Pause
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.RestartAlt
import androidx.compose.material.icons.filled.StopCircle
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
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
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.hilt.navigation.compose.hiltViewModel
import com.lectable.app.data.remote.dto.QueueTaskDto
import com.lectable.app.ui.components.ConfirmDialog

/** The Android analogue of frontend/src/pages/JobsPage.tsx - a live view of the backend's shared
 *  job queue: what's actually dispatched to a worker slot right now vs. everything still
 *  waiting, all sharing one tier-ordered priority queue (see backend/internal/jobs.Kind). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun JobsScreen(
    onBack: () -> Unit,
    viewModel: JobsViewModel = hiltViewModel(),
) {
    val uiState by viewModel.uiState.collectAsState()
    val snackbarHostState = remember { SnackbarHostState() }
    var confirmingCancelAll by remember { mutableStateOf(false) }
    var confirmingRestart by remember { mutableStateOf(false) }

    LaunchedEffect(uiState.error) {
        uiState.error?.let {
            snackbarHostState.showSnackbar(it)
            viewModel.dismissError()
        }
    }

    val snapshot = uiState.snapshot
    val totalCount = (snapshot?.inFlight?.size ?: 0) + (snapshot?.queued?.size ?: 0)
    val paused = snapshot?.paused ?: false

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("Job queue") },
                navigationIcon = {
                    IconButton(onClick = onBack) { Icon(Icons.Filled.ArrowBack, contentDescription = "Back") }
                },
                actions = {
                    IconButton(
                        onClick = viewModel::togglePause,
                        enabled = snapshot != null && !uiState.togglingPause,
                    ) {
                        Icon(
                            if (paused) Icons.Filled.PlayArrow else Icons.Filled.Pause,
                            contentDescription = if (paused) {
                                "Resume dispatching new jobs"
                            } else {
                                "Pause dispatching new jobs - anything already in flight keeps running to completion"
                            },
                        )
                    }
                    IconButton(onClick = { confirmingCancelAll = true }, enabled = totalCount > 0) {
                        Icon(Icons.Filled.StopCircle, contentDescription = "Cancel every queued and in-flight job")
                    }
                    IconButton(onClick = { confirmingRestart = true }, enabled = !uiState.restarting) {
                        Icon(
                            Icons.Filled.RestartAlt,
                            contentDescription = "Restart ttsworker - recovers a stuck or visibly-leaking worker " +
                                "without waiting for the automatic RSS-threshold watchdog",
                        )
                    }
                },
            )
        },
        snackbarHost = { SnackbarHost(snackbarHostState) { Snackbar(it) } },
    ) { padding ->
        Box(modifier = Modifier.fillMaxSize().padding(padding)) {
            when {
                uiState.loading -> CircularProgressIndicator(modifier = Modifier.align(Alignment.Center))
                snapshot == null -> Text(
                    "Could not load the job queue.",
                    color = MaterialTheme.colorScheme.error,
                    modifier = Modifier.align(Alignment.Center).padding(24.dp),
                )
                else -> LazyColumn(modifier = Modifier.fillMaxSize()) {
                    if (paused) {
                        item {
                            Text(
                                "Paused — no new jobs will be dispatched. Anything already in flight is still running.",
                                style = MaterialTheme.typography.bodyMedium,
                                color = MaterialTheme.colorScheme.error,
                                modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
                            )
                        }
                    }
                    item {
                        Text(
                            "In flight (${snapshot.inFlight.size})",
                            style = MaterialTheme.typography.titleMedium,
                            modifier = Modifier.padding(start = 16.dp, end = 16.dp, top = 16.dp, bottom = 4.dp),
                        )
                    }
                    if (snapshot.inFlight.isEmpty()) {
                        item {
                            Text(
                                "Nothing generating right now.",
                                style = MaterialTheme.typography.bodyMedium,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                modifier = Modifier.padding(start = 16.dp, end = 16.dp, bottom = 8.dp),
                            )
                        }
                    } else {
                        items(snapshot.inFlight, key = { "inflight-${it.id}" }) { task ->
                            JobRow(
                                task = task,
                                voiceName = voiceName(task, uiState.presetNames),
                                canceling = uiState.cancelingId == task.id,
                                onCancel = { viewModel.cancelJob(task) },
                            )
                        }
                    }
                    item {
                        Text(
                            "Queued (${snapshot.queued.size})",
                            style = MaterialTheme.typography.titleMedium,
                            modifier = Modifier.padding(start = 16.dp, end = 16.dp, top = 16.dp, bottom = 4.dp),
                        )
                    }
                    if (snapshot.queued.isEmpty()) {
                        item {
                            Text(
                                "Nothing waiting.",
                                style = MaterialTheme.typography.bodyMedium,
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                modifier = Modifier.padding(start = 16.dp, end = 16.dp, bottom = 16.dp),
                            )
                        }
                    } else {
                        items(snapshot.queued, key = { "queued-${it.id}" }) { task ->
                            JobRow(
                                task = task,
                                voiceName = voiceName(task, uiState.presetNames),
                                canceling = uiState.cancelingId == task.id,
                                onCancel = { viewModel.cancelJob(task) },
                            )
                        }
                    }
                }
            }
        }
    }

    if (confirmingCancelAll) {
        ConfirmDialog(
            title = "Cancel all jobs?",
            text = "All $totalCount queued/in-flight job(s) will be canceled. Anything already generating stops as soon as it notices.",
            confirmLabel = "Cancel all",
            onConfirm = viewModel::cancelAll,
            onDismiss = { confirmingCancelAll = false },
        )
    }

    if (confirmingRestart) {
        ConfirmDialog(
            title = "Restart ttsworker?",
            text = "Any TTS/LLM call in flight will be interrupted until the new worker is healthy again.",
            confirmLabel = "Restart",
            onConfirm = viewModel::restartWorker,
            onDismiss = { confirmingRestart = false },
        )
    }
}

@Composable
private fun JobRow(task: QueueTaskDto, voiceName: String, canceling: Boolean, onCancel: () -> Unit) {
    Row(
        modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(modifier = Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(kindLabel(task.kind), style = MaterialTheme.typography.bodyLarge)
                Text(
                    "  ·  ${task.bookTitle}",
                    style = MaterialTheme.typography.bodyLarge,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    buildString {
                        append(targetLabel(task))
                        if (task.kind == "voice_clone" || task.kind == "voice_design") append(" · ¶${task.paragraphIdx + 1}")
                        append(" · $voiceName")
                    },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 1,
                    overflow = TextOverflow.Ellipsis,
                    modifier = Modifier.weight(1f, fill = false),
                )
                TierBadge(task.tier)
            }
        }
        IconButton(onClick = onCancel, enabled = !canceling) {
            Icon(
                Icons.Filled.Cancel,
                contentDescription = "Cancel this job",
                tint = MaterialTheme.colorScheme.error,
            )
        }
    }
}

@Composable
private fun TierBadge(tier: String) {
    val color = when (tier) {
        "urgent" -> MaterialTheme.colorScheme.error
        "lookahead" -> MaterialTheme.colorScheme.primary
        "normal" -> MaterialTheme.colorScheme.tertiary
        else -> MaterialTheme.colorScheme.onSurfaceVariant
    }
    Text(
        tier,
        style = MaterialTheme.typography.labelSmall,
        color = color,
        modifier = Modifier
            .padding(start = 6.dp)
            .clip(RoundedCornerShape(4.dp))
            .padding(horizontal = 4.dp),
    )
}
