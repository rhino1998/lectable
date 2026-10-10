package com.lectable.app.ui.reader

import androidx.compose.foundation.background
import androidx.compose.foundation.interaction.collectIsDraggedAsState
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material.icons.outlined.Bedtime
import androidx.compose.material.icons.filled.Bedtime
import androidx.compose.material.icons.filled.Download
import androidx.compose.material.icons.filled.DownloadDone
import androidx.compose.material.icons.filled.FormatIndentIncrease
import androidx.compose.material.icons.filled.GraphicEq
import androidx.compose.material.icons.filled.History
import androidx.compose.material.icons.filled.ListAlt
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.Pause
import androidx.compose.material.icons.filled.RecordVoiceOver
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.Spellcheck
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FloatingActionButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LocalContentColor
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Snackbar
import androidx.compose.material3.SnackbarDuration
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.SnackbarResult
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
import com.lectable.app.data.remote.dto.AudioStatuses
import com.lectable.app.data.remote.dto.CharacterNameDto
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ParagraphDto
import com.lectable.app.data.settings.toComposeFontFamily
import com.lectable.app.playback.SleepTimerOption
import com.lectable.app.ui.components.ConfirmDialog
import com.lectable.app.ui.formatDurationLong
import com.lectable.app.ui.formatTime
import kotlin.math.roundToInt
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive

internal const val PLAYBACK_SPEED_MIN = 0.5f
internal const val PLAYBACK_SPEED_MAX = 10f
private const val PLAYBACK_SPEED_INCREMENT = 0.25f
internal val PLAYBACK_SPEED_RANGE = PLAYBACK_SPEED_MIN..PLAYBACK_SPEED_MAX
// Compose Slider's own `steps` counts the stops strictly *between* the two ends, not the ends
// themselves - 0.5 to 10.0 by 0.25 is 39 total stops (0.5, 0.75, ..., 10.0), so 37 lie between.
internal val PLAYBACK_SPEED_STEPS = ((PLAYBACK_SPEED_MAX - PLAYBACK_SPEED_MIN) / PLAYBACK_SPEED_INCREMENT).roundToInt() - 1

/** Rounds a raw drag position to the nearest 0.25 increment - belt-and-suspenders alongside the
 *  Slider's own `steps` snapping (which already restricts the values it hands to onValueChange),
 *  so a value this reaches through any other path (e.g. a restored preference) still lands
 *  exactly on a step rather than carrying it forward unrounded. */
private fun snapPlaybackSpeed(value: Float): Float =
    (((value - PLAYBACK_SPEED_MIN) / PLAYBACK_SPEED_INCREMENT).roundToInt() * PLAYBACK_SPEED_INCREMENT + PLAYBACK_SPEED_MIN)
        .coerceIn(PLAYBACK_SPEED_MIN, PLAYBACK_SPEED_MAX)

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ReaderScreen(
    onBack: () -> Unit,
    onOpenVoices: () -> Unit,
    onOpenSpeakers: () -> Unit,
    onOpenLexicon: () -> Unit,
    onOpenLighting: () -> Unit,
    onOpenJobs: () -> Unit,
    viewModel: ReaderViewModel = hiltViewModel(),
) {
    val uiState by viewModel.uiState.collectAsState()
    var showVoicePicker by remember { mutableStateOf(false) }
    // Which displayed block's dialogue segment(s) "Set speaker" is currently targeting - null
    // when the picker sheet is closed. See SpeakerPickerSheet below.
    var speakerPickerTarget by remember { mutableStateOf<SpeakerPickerTarget?>(null) }
    var showChapterPicker by remember { mutableStateOf(false) }
    // The chapter whose notes sheet is open (chapter picker long-press "Edit chapter notes").
    var notesChapterIdx by remember { mutableStateOf<Int?>(null) }
    // The "previously on" recap - offered over the reader's top; open, it sits in the text
    // above the paragraph the reader was at (recapAnchor, see RecapInline).
    val recapState by viewModel.recapState.collectAsState()
    var recapAnchor by remember { mutableStateOf<ParagraphPos?>(null) }
    // Opened here (the menu, the offer) or by the view model, when play waits for the recap;
    // closed here or by its clip ending (the book then plays on).
    LaunchedEffect(recapState.open) {
        recapAnchor = if (recapState.open) recapState.pos else null
    }
    var showSpeedPicker by remember { mutableStateOf(false) }
    var showBookmarksSheet by remember { mutableStateOf(false) }
    var showSearchSheet by remember { mutableStateOf(false) }
    var showSleepTimerSheet by remember { mutableStateOf(false) }
    var showMusicVolumeSheet by remember { mutableStateOf(false) }
    // Underlines dialogue segments and caret-marks descriptive ones (see annotationKind) -
    // purely a display toggle, not persisted, and deliberately minimal for now: no background
    // color-coding, no legend, no "select a speaker to highlight their lines" - just the marks.
    // Persisted across visits - see ReaderViewModel.setAnnotationsMode.
    val annotationsMode = uiState.annotationsMode
    val sleepTimerState by viewModel.player.sleepTimer.collectAsState()
    val musicVolume by viewModel.musicVolume.collectAsState()
    val snackbarHostState = remember { SnackbarHostState() }
    val listState = rememberLazyListState()

    // Mirrors frontend's autoFollow (hooks/useAutoFollow.ts): keeps the currently-playing
    // paragraph in view as playback advances, but suspends the moment the user drags
    // the list themselves, so it doesn't fight someone scrolling ahead/back to read.
    // Detected via the list's own drag interaction (not the ambient scroll offset),
    // since programmatic bringIntoView() scrolling shouldn't count as "the user
    // scrolled" - same reasoning as the web version listening for raw wheel/touch
    // input rather than the 'scroll' event.
    var autoFollow by remember { mutableStateOf(true) }
    val isDragged by listState.interactionSource.collectIsDraggedAsState()
    LaunchedEffect(isDragged) {
        if (isDragged) autoFollow = false
    }

    // Shared by the bookmarks and search sheets: jumping to a result should also resume
    // following it, same as tapping a paragraph directly.
    val jumpToParagraph: (chapterIdx: Int, paragraphIdx: Int) -> Unit = { chapterIdx, paragraphIdx ->
        viewModel.jumpToParagraph(chapterIdx, paragraphIdx)
        autoFollow = true
    }

    // High-frequency position polling for word-highlight tracking (see WordHighlight.kt) -
    // deliberately separate from playback.positionSeconds, which is throttled to ~1s for
    // backend position sync and would make per-word highlighting visibly jump. Raw seconds
    // (not a 0..1 ratio) since real per-word alignment timings are absolute seconds into the
    // clip, not proportions of it.
    var playbackSeconds by remember { mutableStateOf(0.0) }
    LaunchedEffect(uiState.playback.chapterIdx, uiState.playback.paragraphIdx) {
        while (isActive) {
            playbackSeconds = viewModel.player.positionMs() / 1000.0
            delay(80)
        }
    }

    // Moved on another device while this reader sat paused - see ReaderViewModel.remotePosition.
    // Restarting (the position cleared by playing, or moved again) dismisses the old snackbar.
    LaunchedEffect(uiState.remotePosition) {
        val (chapterIdx, paragraphIdx) = uiState.remotePosition ?: return@LaunchedEffect
        val title = uiState.book?.chapters?.getOrNull(chapterIdx)?.title ?: "Chapter ${chapterIdx + 1}"
        val result = snackbarHostState.showSnackbar(
            "Continued on another device: $title, paragraph ${paragraphIdx + 1}",
            actionLabel = "Jump there",
            withDismissAction = true,
            duration = SnackbarDuration.Indefinite,
        )
        if (result == SnackbarResult.ActionPerformed) viewModel.jumpToRemotePosition() else viewModel.dismissRemotePosition()
    }

    LaunchedEffect(uiState.error) {
        val message = uiState.error ?: return@LaunchedEffect
        val failedChapterIdx = uiState.failedChapterIdx
        val result = snackbarHostState.showSnackbar(message, actionLabel = if (failedChapterIdx != null) "Retry" else null)
        if (result == SnackbarResult.ActionPerformed && failedChapterIdx != null) {
            viewModel.retryChapter(failedChapterIdx)
        }
        viewModel.dismissError()
    }

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    // Mirrors bottomBar's own activeChapterIdx derivation (uiState.playback
                    // .chapterIdx, falling back to the loaded window's start before playback has
                    // actually targeted anything yet) - not shared code since the two live in
                    // separate Scaffold slot lambdas, but same "wherever playback currently is"
                    // definition of "current chapter" either way.
                    val activeChapterIdx = uiState.playback.chapterIdx.takeIf { it >= 0 } ?: uiState.range.first
                    val activeChapterTitle = uiState.loadedChapters[activeChapterIdx]?.title
                    Column {
                        Text(uiState.book?.title.orEmpty(), maxLines = 1, overflow = TextOverflow.Ellipsis)
                        activeChapterTitle?.let {
                            Text(it, maxLines = 1, overflow = TextOverflow.Ellipsis, style = MaterialTheme.typography.labelSmall)
                        }
                    }
                },
                navigationIcon = {
                    IconButton(onClick = onBack) { Icon(Icons.Filled.ArrowBack, contentDescription = "Back") }
                },
                actions = {
                    // Narrator voice/Speakers/offline-download controls all live behind one
                    // overflow menu here rather than as separate top-bar icons - these are
                    // occasional, book-level settings a reader dips into rather than something
                    // wanted at a glance on every visit, unlike Play/Pause and the other controls
                    // that stay in the bottom PlaybackBar.
                    var showMenu by remember { mutableStateOf(false) }
                    var confirmingDeleteLocal by remember { mutableStateOf(false) }
                    IconButton(onClick = { showMenu = true }) {
                        Icon(Icons.Filled.MoreVert, contentDescription = "Book options")
                    }
                    DropdownMenu(expanded = showMenu, onDismissRequest = { showMenu = false }) {
                        DropdownMenuItem(
                            text = { Text("Narrator voice") },
                            leadingIcon = { Icon(Icons.Filled.GraphicEq, contentDescription = null) },
                            onClick = { showVoicePicker = true; showMenu = false },
                        )
                        DropdownMenuItem(
                            text = { Text("Sleep timer") },
                            leadingIcon = {
                                Icon(
                                    Icons.Filled.Bedtime,
                                    contentDescription = null,
                                    tint = if (sleepTimerState.option != SleepTimerOption.Off) MaterialTheme.colorScheme.primary else LocalContentColor.current,
                                )
                            },
                            trailingIcon = {
                                when (sleepTimerState.option) {
                                    SleepTimerOption.Off -> {}
                                    SleepTimerOption.EndOfChapter -> Text("End of ch.", style = MaterialTheme.typography.labelMedium)
                                    else -> Text(formatTime(sleepTimerState.remainingSeconds.toDouble()), style = MaterialTheme.typography.labelMedium)
                                }
                            },
                            onClick = { showSleepTimerSheet = true; showMenu = false },
                        )
                        DropdownMenuItem(
                            text = { Text("Recap") },
                            leadingIcon = { Icon(Icons.Filled.History, contentDescription = null) },
                            onClick = { viewModel.setRecapOpen(true, autoPlay = true); showMenu = false },
                        )
                        DropdownMenuItem(
                            text = { Text("Search") },
                            leadingIcon = { Icon(Icons.Filled.Search, contentDescription = null) },
                            onClick = { showSearchSheet = true; showMenu = false },
                        )
                        DropdownMenuItem(
                            text = { Text("Speakers") },
                            leadingIcon = { Icon(Icons.Filled.RecordVoiceOver, contentDescription = null) },
                            onClick = { onOpenSpeakers(); showMenu = false },
                        )
                        DropdownMenuItem(
                            text = { Text("Pronunciations") },
                            leadingIcon = { Icon(Icons.Filled.Spellcheck, contentDescription = null) },
                            onClick = { onOpenLexicon(); showMenu = false },
                        )
                        DropdownMenuItem(
                            text = { Text("Job queue") },
                            leadingIcon = { Icon(Icons.Filled.ListAlt, contentDescription = null) },
                            onClick = { onOpenJobs(); showMenu = false },
                        )
                        val downloadProgress = uiState.bookDownloadProgress
                        when {
                            downloadProgress != null -> {
                                val (done, total) = downloadProgress
                                DropdownMenuItem(
                                    text = { Text("Downloading… ${if (total > 0) done * 100 / total else 0}%") },
                                    leadingIcon = {
                                        CircularProgressIndicator(
                                            progress = { if (total > 0) done.toFloat() / total else 0f },
                                            modifier = Modifier.size(20.dp),
                                            strokeWidth = 2.dp,
                                        )
                                    },
                                    enabled = false,
                                    onClick = {},
                                )
                            }
                            // Something's downloaded (or partially so) for this book already -
                            // offers to remove the local copy instead of re-downloading, which a
                            // reader has no real reason to do on top of an existing download.
                            uiState.downloadStates.isNotEmpty() -> {
                                DropdownMenuItem(
                                    text = { Text("Remove downloaded copy") },
                                    leadingIcon = { Icon(Icons.Filled.DownloadDone, contentDescription = null) },
                                    onClick = { confirmingDeleteLocal = true; showMenu = false },
                                )
                            }
                            else -> {
                                DropdownMenuItem(
                                    text = { Text("Download for offline") },
                                    leadingIcon = { Icon(Icons.Filled.Download, contentDescription = null) },
                                    onClick = { viewModel.downloadBook(); showMenu = false },
                                )
                            }
                        }
                    }
                    if (confirmingDeleteLocal) {
                        ConfirmDialog(
                            title = "Remove downloaded copy?",
                            text = "This book's offline audio will be removed from this device. It stays in your " +
                                "library and can be downloaded again anytime.",
                            confirmLabel = "Remove",
                            onConfirm = viewModel::deleteLocalCopy,
                            onDismiss = { confirmingDeleteLocal = false },
                        )
                    }
                },
            )
        },
        floatingActionButton = {
            if (!autoFollow) {
                FloatingActionButton(onClick = { autoFollow = true }) {
                    Icon(Icons.Filled.FormatIndentIncrease, contentDescription = "Scroll to the paragraph currently playing")
                }
            }
        },
        bottomBar = {
            // The chapter/progress figures below all describe wherever playback currently is,
            // not "whatever's on screen" - with multiple chapters loaded at once (infinite
            // scroll), those can differ, same as the web frontend's PlayerBar always describing
            // `playback.chapterIdx`/`paragraphIdx` rather than scroll position.
            val activeChapterIdx = uiState.playback.chapterIdx.takeIf { it >= 0 } ?: uiState.range.first
            val activeChapter = uiState.loadedChapters[activeChapterIdx]
            val chapterParagraphs = activeChapter?.paragraphs ?: emptyList()
            val currentParagraphIdx = uiState.playback.paragraphIdx.takeIf { uiState.playback.chapterIdx == activeChapterIdx } ?: 0
            val currentParagraphDuration = chapterParagraphs.getOrNull(currentParagraphIdx)?.durationSeconds ?: 0.0
            // playbackSeconds itself is raw/absolute within whatever clip is loaded (needed as-is
            // for word-highlight comparison against ParagraphBodyText's own equally-absolute word
            // start times - see WordHighlight.kt's wordStartTimes) - for a scare-quote merge
            // group's own non-anchor member (ParagraphDto.audioPointerSeconds > 0), that's well
            // past this one paragraph's own short span within the shared clip, so every "how far
            // into just this paragraph" figure below needs it rebased back down to 0-based first,
            // mirroring frontend's usePlayback.ts's own exposedCurrentTime.
            val currentParagraphSeconds = (playbackSeconds - (chapterParagraphs.getOrNull(currentParagraphIdx)?.audioPointerSeconds ?: 0.0))
                .coerceAtLeast(0.0)

            // Mirrors frontend's PlayerBar.tsx: progress/elapsed/remaining across the whole
            // chapter (paragraphs completed, plus how far into the current one), not just the
            // currently-playing clip - so the bar reflects "how much of this chapter is left."
            val currentParagraphRatio = if (currentParagraphDuration > 0) {
                (currentParagraphSeconds / currentParagraphDuration).coerceIn(0.0, 1.0)
            } else {
                0.0
            }
            val chapterElapsedSeconds = chapterParagraphs.take(currentParagraphIdx).sumOf { it.durationSeconds ?: 0.0 } +
                currentParagraphSeconds
            val chapterTotalSeconds = chapterParagraphs.sumOf { it.durationSeconds ?: 0.0 }
            val chapterRemainingSeconds = (chapterTotalSeconds - chapterElapsedSeconds).coerceAtLeast(0.0)

            // Words per minute the reader is actually hearing right now: this chapter's own
            // narrated pace (words already generated, over their audio's own duration - ready
            // paragraphs only, the same set chapterTotalSeconds already sums), scaled by the
            // current playback speed since a speed change is applied to the clip directly
            // rather than baked into durationSeconds - mirrors frontend's PlayerBar.tsx
            // wordsPerMinute exactly. null until at least one paragraph in this chapter has
            // generated audio to measure a pace from.
            val chapterWordCount = chapterParagraphs.sumOf { if (it.durationSeconds != null) wordCount(it.text) else 0 }
            val wordsPerMinute = if (chapterTotalSeconds > 0) {
                ((chapterWordCount / (chapterTotalSeconds / 60.0)) * uiState.playback.playbackSpeed).roundToInt()
            } else {
                null
            }
            val chapterProgress = if (chapterParagraphs.isNotEmpty()) {
                ((currentParagraphIdx + currentParagraphRatio) / chapterParagraphs.size).coerceIn(0.0, 1.0).toFloat()
            } else {
                0f
            }

            // How much further playback could run right now without waiting on generation - the
            // run of already-ready paragraphs immediately ahead of the current one, plus
            // whatever's left of the current paragraph's own clip (it's already playing, so it's
            // available too - otherwise the remainder between playbackSeconds and the end of the
            // current clip would be neither "played" nor "buffered"). Stops at the first
            // non-ready paragraph, since that's where playback would actually stall even if a
            // later one happens to be ready already (lookahead can finish out of order) - mirrors
            // frontend's PlayerBar.tsx bufferedAheadPercent exactly.
            var bufferedAheadCount = 0
            for (i in currentParagraphIdx + 1 until chapterParagraphs.size) {
                if (chapterParagraphs[i].audioStatus != AudioStatuses.READY) break
                bufferedAheadCount++
            }
            val currentIsReady = chapterParagraphs.getOrNull(currentParagraphIdx)?.audioStatus == AudioStatuses.READY
            val bufferedAheadUnits = bufferedAheadCount + if (currentIsReady) (1 - currentParagraphRatio) else 0.0
            val bufferedAheadEnd = if (chapterParagraphs.isNotEmpty()) {
                (chapterProgress + (bufferedAheadUnits / chapterParagraphs.size)).coerceIn(0.0, 1.0).toFloat()
            } else {
                0f
            }

            // Live book-wide progress, recomputed from the book's chapter/paragraph counts plus
            // the current playback position (same spirit as ReaderPage.tsx's liveProgressPercent),
            // rather than the possibly-stale progressPercent from whenever the book was last loaded.
            val bookChapters = uiState.book?.chapters ?: emptyList()
            var totalParagraphs = 0
            var completedParagraphs = 0
            bookChapters.forEach { c ->
                totalParagraphs += c.paragraphCount
                when {
                    c.idx < activeChapterIdx -> completedParagraphs += c.paragraphCount
                    c.idx == activeChapterIdx -> completedParagraphs += currentParagraphIdx.coerceAtMost(c.paragraphCount)
                }
            }
            val liveProgressPercent = if (totalParagraphs > 0) {
                (completedParagraphs.toDouble() / totalParagraphs * 100).coerceAtMost(100.0)
            } else {
                0.0
            }
            val bookRemainingSeconds = ((uiState.book?.estimatedTotalSeconds ?: 0.0) * (1 - liveProgressPercent / 100)).coerceAtLeast(0.0)
            val finished = uiState.book?.finished ?: false

            // Every clock figure shown in the bar - elapsed/remaining/total for the chapter, and
            // remaining for the book - is scaled by the current playback speed so it reads as
            // real wall-clock listening time at that speed, not the raw narrated (1x) duration
            // the paragraphs' own durationSeconds actually sum to - same reasoning as
            // wordsPerMinute above, which already does this (by multiplying instead of dividing,
            // since it's a rate rather than a duration). Applying the same divisor to elapsed,
            // remaining, and total together keeps them mutually consistent (elapsed + remaining
            // still equals total at any speed) rather than only adjusting one of the three.
            // chapterProgress/bufferedAheadEnd are unaffected - both are unitless ratios, not
            // absolute seconds, so speed cancels out of them already.
            val speed = uiState.playback.playbackSpeed
            PlaybackBar(
                // While the recap is open with a clip, play/pause is the recap's.
                isPlaying = if (recapState.engaged) recapState.playing else uiState.playback.isPlaying,
                playbackSpeed = speed,
                chapterProgress = chapterProgress,
                bufferedAheadEnd = bufferedAheadEnd,
                chapterElapsedSeconds = chapterElapsedSeconds / speed,
                chapterRemainingSeconds = chapterRemainingSeconds / speed,
                chapterTotalSeconds = chapterTotalSeconds / speed,
                wordsPerMinute = wordsPerMinute,
                bookRemainingSeconds = bookRemainingSeconds / speed,
                bookRemainingPercent = (100 - liveProgressPercent).coerceIn(0.0, 100.0),
                finished = finished,
                onPlayPause = viewModel::togglePlayPause,
                onSpeedClick = { showSpeedPicker = true },
                onChapterInfoClick = { showChapterPicker = true },
                onBookmarksClick = { showBookmarksSheet = true },
                lightingEnabled = uiState.lightingEnabled,
                lightingStatus = uiState.lightingStatus,
                onToggleLighting = { if (!viewModel.toggleLighting()) onOpenLighting() },
                onLightingSettings = onOpenLighting,
                musicEnabled = uiState.musicEnabled,
                onToggleMusic = { viewModel.setMusicEnabled(!uiState.musicEnabled) },
                onMusicVolumeClick = { showMusicVolumeSheet = true },
                annotationsMode = annotationsMode,
                onToggleAnnotationsMode = { viewModel.setAnnotationsMode(!annotationsMode) },
            )
        },
        snackbarHost = { SnackbarHost(snackbarHostState) { Snackbar(it) } },
    ) { padding ->
        Box(modifier = Modifier.fillMaxSize().padding(padding)) {
            if (uiState.loading || uiState.loadedChapters.isEmpty()) {
                CircularProgressIndicator(modifier = Modifier.align(Alignment.Center))
            } else {
                InfiniteChapterContent(
                    listState = listState,
                    loadedChapters = uiState.loadedChapters,
                    range = uiState.range,
                    totalChapters = uiState.book?.chapterCount ?: 0,
                    playback = uiState.playback,
                    playbackSeconds = playbackSeconds,
                    autoFollow = autoFollow,
                    bookmarksByKey = uiState.bookmarksByKey,
                    fontSizeSp = uiState.fontSizeSp,
                    fontFamily = uiState.fontFamily.toComposeFontFamily(),
                    annotationsMode = annotationsMode,
                    chapterMusicRegions = uiState.chapterMusic.mapValues { it.value.regions },
                    previewingMusicRegionId = uiState.previewingMusicRegionId,
                    onParagraphClick = viewModel::playParagraph,
                    onExpandUp = viewModel::expandUp,
                    onExpandDown = viewModel::expandDown,
                    onRegenerateParagraph = viewModel::regenerateParagraph,
                    onToggleBookmark = viewModel::toggleBookmark,
                    onOpenSpeakerPicker = { chapterIdx, paragraphIndices, currentSpeaker ->
                        speakerPickerTarget = SpeakerPickerTarget(chapterIdx, paragraphIndices, currentSpeaker)
                    },
                    onSetScareQuote = viewModel::setScareQuote,
                    onSetEmotion = viewModel::setEmotion,
                    onRegenerateMusic = viewModel::regenerateMusicRegion,
                    onPreviewMusic = viewModel::previewMusicRegion,
                    resolveImageUrl = viewModel::resolveImageUrl,
                    recapAnchor = recapAnchor,
                    recapContent = {
                        RecapInline(
                            state = recapState,
                            positionSeconds = viewModel::recapPositionSeconds,
                            onClose = viewModel::declineRecap,
                            onPrepare = viewModel::prepareRecap,
                            onSetAfterMinutes = viewModel::setRecapAfterMinutes,
                        )
                    },
                )
            }
            if (recapState.offer && recapAnchor == null) {
                RecapOfferCard(
                    state = recapState,
                    onPlay = { viewModel.setRecapOpen(true, autoPlay = true) },
                    onRead = { viewModel.setRecapOpen(true) },
                    onDismiss = viewModel::dismissRecap,
                    modifier = Modifier.align(Alignment.TopCenter),
                )
            }
        }
    }

    speakerPickerTarget?.let { target ->
        SpeakerPickerSheet(
            onDismiss = { speakerPickerTarget = null },
            characters = uiState.characters,
            currentSpeaker = target.currentSpeaker,
            chapterNames = chapterCharacterNames(uiState.loadedChapters[target.chapterIdx]),
            onSelect = { name ->
                viewModel.setParagraphSpeaker(target.chapterIdx, target.paragraphIndices, name)
                speakerPickerTarget = null
            },
            onCreate = { name ->
                viewModel.setParagraphSpeaker(target.chapterIdx, target.paragraphIndices, name, isNew = true)
                speakerPickerTarget = null
            },
        )
    }

    if (showVoicePicker) {
        VoicePickerSheet(
            onDismiss = { showVoicePicker = false },
            fetchPresets = viewModel::availablePresets,
            fetchCustomPresets = viewModel::availableCustomPresets,
            fetchLanguages = viewModel::availableLanguages,
            initialLanguage = uiState.voiceLanguage,
            onSelect = { id, name, instruct, language ->
                viewModel.setVoicePreset(id, name, instruct, language)
                showVoicePicker = false
            },
            multiVoice = uiState.multiVoice,
            onMultiVoiceChange = viewModel::setMultiVoice,
            musicEnabled = uiState.musicEnabled,
            onMusicEnabledChange = viewModel::setMusicEnabled,
            onManageVoices = {
                showVoicePicker = false
                onOpenVoices()
            },
            onDeleteBookAudio = viewModel::deleteBookAudio,
        )
    }

    if (showChapterPicker) {
        val bookChapters = uiState.book?.chapters ?: emptyList()
        // No backend field carries a chapter's duration (see chapterSummaryDTO in
        // backend/internal/httpapi/books.go - just paragraphCount/readyCount), so it's derived
        // here: the real sum of paragraph durations for a chapter that's already loaded (has
        // full paragraph data), or an estimate for one that isn't - the book's own calibrated
        // pace (estimatedTotalSeconds / total paragraph count, the same seconds-per-paragraph
        // basis the backend's own book-level estimate uses) applied to that chapter's
        // paragraphCount.
        val totalBookParagraphs = bookChapters.sumOf { it.paragraphCount }
        val avgSecondsPerParagraph = if (totalBookParagraphs > 0) {
            (uiState.book?.estimatedTotalSeconds ?: 0.0) / totalBookParagraphs
        } else {
            0.0
        }
        val activeChapterIdxForPicker = uiState.playback.chapterIdx.takeIf { it >= 0 } ?: uiState.range.first
        ChapterPickerSheet(
            chapters = bookChapters,
            currentChapterIdx = activeChapterIdxForPicker,
            durationFor = { chapter ->
                val loaded = uiState.loadedChapters[chapter.idx]
                if (loaded != null) {
                    formatDurationLong(loaded.paragraphs.sumOf { it.durationSeconds ?: 0.0 })
                } else {
                    formatDurationLong(chapter.paragraphCount * avgSecondsPerParagraph)
                }
            },
            downloadStates = uiState.downloadStates,
            onDismiss = { showChapterPicker = false },
            onSelect = { idx ->
                viewModel.goToChapter(idx)
                showChapterPicker = false
            },
            onDownload = viewModel::downloadChapter,
            onLongPressDownload = viewModel::cancelOrDeleteDownload,
            onGenerateChapter = viewModel::generateChapter,
            onChapterPass = viewModel::runChapterPass,
            onEditNotes = { idx ->
                notesChapterIdx = idx
                showChapterPicker = false
            },
        )
    }

    notesChapterIdx?.let { idx ->
        ChapterNotesSheet(
            chapterTitle = uiState.book?.chapters?.firstOrNull { it.idx == idx }?.title ?: "Chapter ${idx + 1}",
            notesFlow = remember(idx) { viewModel.chapterNotes(idx) },
            onDismiss = { notesChapterIdx = null },
            onSave = { notes, onResult -> viewModel.saveChapterNotes(idx, notes, onResult) },
            onRegenerate = { viewModel.regenerateChapterNotes(idx) },
            onReattribute = { viewModel.runChapterPass(idx, ChapterPass.ATTRIBUTION) },
        )
    }

    if (showSpeedPicker) {
        SpeedPickerSheet(
            playbackSpeed = uiState.playback.playbackSpeed,
            onSpeedChange = { viewModel.setPlaybackRate(snapPlaybackSpeed(it)) },
            onDismiss = { showSpeedPicker = false },
        )
    }

    if (showMusicVolumeSheet) {
        MusicVolumeSheet(
            volume = musicVolume,
            onVolumeChange = viewModel::setMusicVolume,
            onDismiss = { showMusicVolumeSheet = false },
        )
    }

    if (showBookmarksSheet) {
        BookmarksSheet(
            fetchBookmarks = viewModel::availableBookmarks,
            onDismiss = { showBookmarksSheet = false },
            onJump = { chapterIdx, paragraphIdx ->
                jumpToParagraph(chapterIdx, paragraphIdx)
                showBookmarksSheet = false
            },
            onUpdateNote = viewModel::updateBookmarkNote,
            onDelete = viewModel::deleteBookmark,
        )
    }

    if (showSearchSheet) {
        SearchSheet(
            search = viewModel::searchBook,
            onDismiss = { showSearchSheet = false },
            onJump = { chapterIdx, paragraphIdx ->
                jumpToParagraph(chapterIdx, paragraphIdx)
                showSearchSheet = false
            },
        )
    }

    if (showSleepTimerSheet) {
        SleepTimerSheet(
            state = sleepTimerState,
            onDismiss = { showSleepTimerSheet = false },
            onSelect = { option ->
                viewModel.setSleepTimer(option)
                showSleepTimerSheet = false
            },
        )
    }
}

/** One row of the infinite-scroll reader's flat item list - either a chapter's title header or
 *  one of its paragraphs, tagged with which chapter it belongs to since several chapters can be
 *  mounted (and rendered as one continuous flow) at once. */
/** What the long-press "Set speaker" action (see ParagraphRow) is currently targeting -
 *  [paragraphIndices] is the long-pressed quote, or every isQuote segment of the displayed block
 *  when the press landed on narration (see ReaderListEntry.ParagraphItem/groupContent's Inline
 *  grouping) - isQuote segments only, since the backend rejects
 *  attributing narration to a character; [currentSpeaker] seeds the picker sheet's
 *  selection so it can show what this block is already attributed to. */
internal data class SpeakerPickerTarget(
    val chapterIdx: Int,
    val paragraphIndices: List<Int>,
    val currentSpeaker: String,
)

/** Everyone who speaks anywhere in [chapter] - the reassign picker's "This
 *  chapter" section, since a misattributed line usually belongs to someone
 *  already in the scene. Narrator/blank excluded (Narrator always leads the speaker picker).
 *  Mirrors frontend's ParagraphContextMenu chapterNames. */
internal fun chapterCharacterNames(chapter: ChapterDetailDto?): Set<String> {
    if (chapter == null) return emptySet()
    val out = mutableSetOf<String>()
    for (p in chapter.paragraphs) {
        p.speaker?.takeIf { it.isNotEmpty() && it != "Narrator" }?.let(out::add)
    }
    return out
}

/** A reassign picker's two sections: the roster's names in [chapterNames] (local), then the whole
 *  roster (all) - both alphabetical, Narrator/Unknown/blank (fixed picker entries), invalid
 *  characters and [exclude] left out. */
internal fun pickerSections(roster: List<CharacterNameDto>, chapterNames: Set<String>, exclude: String?): Pair<List<String>, List<String>> {
    val all = roster.asSequence()
        .filterNot { it.invalid }
        .map { it.name }
        .filter { it.isNotEmpty() && it != "Narrator" && it != "Unknown" && it != exclude }
        .distinct()
        .sortedWith(String.CASE_INSENSITIVE_ORDER)
        .toList()
    return all.filter { it in chapterNames } to all
}
