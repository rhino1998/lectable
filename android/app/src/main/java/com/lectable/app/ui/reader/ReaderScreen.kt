package com.lectable.app.ui.reader

import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.gestures.animateScrollBy
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.interaction.collectIsDraggedAsState
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.LazyListState
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.ArrowBack
import androidx.compose.material.icons.filled.Bedtime
import androidx.compose.material.icons.filled.Bookmark
import androidx.compose.material.icons.filled.BookmarkBorder
import androidx.compose.material.icons.filled.Bookmarks
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.ContentCopy
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Download
import androidx.compose.material.icons.filled.DownloadDone
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.FormatIndentIncrease
import androidx.compose.material.icons.filled.FormatListNumbered
import androidx.compose.material.icons.filled.FormatQuote
import androidx.compose.material.icons.filled.FormatStrikethrough
import androidx.compose.material.icons.filled.GraphicEq
import androidx.compose.material.icons.filled.KeyboardArrowUp
import androidx.compose.material.icons.filled.ListAlt
import androidx.compose.material.icons.filled.Mood
import androidx.compose.material.icons.filled.MoreVert
import androidx.compose.material.icons.filled.MusicNote
import androidx.compose.material.icons.filled.Palette
import androidx.compose.material.icons.filled.Pause
import androidx.compose.material.icons.filled.Person
import androidx.compose.material.icons.filled.PlayArrow
import androidx.compose.material.icons.filled.RecordVoiceOver
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Search
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilledIconButton
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.FloatingActionButton
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LocalContentColor
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalBottomSheet
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Snackbar
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.SnackbarResult
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TextField
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Rect
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.PathEffect
import androidx.compose.ui.graphics.StrokeCap
import androidx.compose.ui.graphics.StrokeJoin
import androidx.compose.ui.graphics.drawscope.DrawScope
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.graphics.lerp
import androidx.compose.ui.graphics.luminance
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.TextLayoutResult
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.hilt.navigation.compose.hiltViewModel
import coil.compose.AsyncImage
import com.lectable.app.data.download.DownloadStatus
import com.lectable.app.data.remote.dto.AudioStatus
import com.lectable.app.data.remote.dto.BookmarkDto
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ChapterSummaryDto
import com.lectable.app.data.remote.dto.CustomVoicePresetDto
import com.lectable.app.data.remote.dto.MusicRegionDto
import com.lectable.app.data.remote.dto.ParagraphDto
import com.lectable.app.data.remote.dto.PronunciationMarkDto
import com.lectable.app.data.remote.dto.SearchResultDto
import com.lectable.app.data.remote.dto.SpeakerDto
import com.lectable.app.data.remote.dto.VoicePresetDto
import com.lectable.app.data.remote.dto.WordTimingDto
import com.lectable.app.data.repository.ChapterDownloadState
import com.lectable.app.data.settings.toComposeFontFamily
import com.lectable.app.playback.PlaybackState
import com.lectable.app.playback.SleepTimerOption
import com.lectable.app.playback.SleepTimerState
import com.lectable.app.ui.components.CleanSlider
import com.lectable.app.ui.components.ConfirmDialog
import com.lectable.app.ui.formatDurationLong
import com.lectable.app.ui.formatTime
import kotlin.math.abs
import kotlin.math.roundToInt
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive

private const val PLAYBACK_SPEED_MIN = 0.5f
private const val PLAYBACK_SPEED_MAX = 5f
private const val PLAYBACK_SPEED_INCREMENT = 0.25f
private val PLAYBACK_SPEED_RANGE = PLAYBACK_SPEED_MIN..PLAYBACK_SPEED_MAX
// Compose Slider's own `steps` counts the stops strictly *between* the two ends, not the ends
// themselves - 0.5 to 5.0 by 0.25 is 19 total stops (0.5, 0.75, ..., 5.0), so 17 lie between.
private val PLAYBACK_SPEED_STEPS = ((PLAYBACK_SPEED_MAX - PLAYBACK_SPEED_MIN) / PLAYBACK_SPEED_INCREMENT).roundToInt() - 1

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
    onOpenJobs: () -> Unit,
    viewModel: ReaderViewModel = hiltViewModel(),
) {
    val uiState by viewModel.uiState.collectAsState()
    var showVoicePicker by remember { mutableStateOf(false) }
    // Which displayed block's dialogue segment(s) "Set speaker" is currently targeting - null
    // when the picker sheet is closed. See SpeakerPickerSheet below.
    var speakerPickerTarget by remember { mutableStateOf<SpeakerPickerTarget?>(null) }
    // SpeakerPickerTarget's own counterpart for the annotations-mode "Describes: X" long-press
    // entry - null when the picker sheet is closed. See DescriptionPickerSheet below.
    var descriptionPickerTarget by remember { mutableStateOf<DescriptionPickerTarget?>(null) }
    var showChapterPicker by remember { mutableStateOf(false) }
    var showSpeedPicker by remember { mutableStateOf(false) }
    var showBookmarksSheet by remember { mutableStateOf(false) }
    var showSearchSheet by remember { mutableStateOf(false) }
    var showSleepTimerSheet by remember { mutableStateOf(false) }
    // Underlines dialogue segments and caret-marks descriptive ones (see annotationKind) -
    // purely a display toggle, not persisted, and deliberately minimal for now: no background
    // color-coding, no legend, no "select a speaker to highlight their lines" - just the marks.
    var annotationsMode by remember { mutableStateOf(false) }
    val sleepTimerState by viewModel.player.sleepTimer.collectAsState()
    val snackbarHostState = remember { SnackbarHostState() }
    val listState = rememberLazyListState()

    // Mirrors frontend's autoFollow (ReaderPage.tsx): keeps the currently-playing
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
                            text = { Text("Speakers") },
                            leadingIcon = { Icon(Icons.Filled.RecordVoiceOver, contentDescription = null) },
                            onClick = { onOpenSpeakers(); showMenu = false },
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
                if (chapterParagraphs[i].audioStatus != AudioStatus.READY) break
                bufferedAheadCount++
            }
            val currentIsReady = chapterParagraphs.getOrNull(currentParagraphIdx)?.audioStatus == AudioStatus.READY
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
                isPlaying = uiState.playback.isPlaying,
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
                onSearchClick = { showSearchSheet = true },
                sleepTimerActive = sleepTimerState.option != SleepTimerOption.Off,
                onSleepTimerClick = { showSleepTimerSheet = true },
                annotationsMode = annotationsMode,
                onToggleAnnotationsMode = { annotationsMode = !annotationsMode },
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
                    onOpenDescriptionPicker = { chapterIdx, paragraphIndices, fromName ->
                        descriptionPickerTarget = DescriptionPickerTarget(chapterIdx, paragraphIndices, fromName)
                    },
                    onSetScareQuote = viewModel::setScareQuote,
                    onRegenerateMusic = viewModel::regenerateMusicRegion,
                    onPreviewMusic = viewModel::previewMusicRegion,
                    resolveImageUrl = viewModel::resolveImageUrl,
                )
            }
        }
    }

    speakerPickerTarget?.let { target ->
        SpeakerPickerSheet(
            onDismiss = { speakerPickerTarget = null },
            fetchSpeakers = viewModel::availableSpeakers,
            currentSpeaker = target.currentSpeaker,
            nearbySpeakers = nearbySpeakersFor(uiState.loadedChapters[target.chapterIdx], target.paragraphIndices),
            onSelect = { name ->
                viewModel.setParagraphSpeaker(target.chapterIdx, target.paragraphIndices, name)
                speakerPickerTarget = null
            },
        )
    }

    descriptionPickerTarget?.let { target ->
        DescriptionPickerSheet(
            onDismiss = { descriptionPickerTarget = null },
            fetchSpeakers = viewModel::availableSpeakers,
            fromName = target.fromName,
            onSelect = { name ->
                viewModel.reassignDescription(target.chapterIdx, target.paragraphIndices, target.fromName, name)
                descriptionPickerTarget = null
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
            onAttributeChapter = viewModel::attributeChapter,
        )
    }

    if (showSpeedPicker) {
        SpeedPickerSheet(
            playbackSpeed = uiState.playback.playbackSpeed,
            onSpeedChange = { viewModel.setPlaybackRate(snapPlaybackSpeed(it)) },
            onDismiss = { showSpeedPicker = false },
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
 *  [paragraphIndices] is one displayed block's own isQuote segment idx's only (see
 *  ReaderListEntry.ParagraphItem/groupContent's Inline grouping) since the backend rejects
 *  attributing narration/description to a character; [currentSpeaker] seeds the picker sheet's
 *  selection so it can show what this block is already attributed to. */
private data class SpeakerPickerTarget(
    val chapterIdx: Int,
    val paragraphIndices: List<Int>,
    val currentSpeaker: String,
)

/** What the long-press "Describes: X" action (see ParagraphRow) is currently targeting -
 *  [paragraphIndices] is this block's own segments whose own describesCharacters already lists
 *  [fromName] (a block can have more than one, and a single description can span more than one
 *  inline segment) - mirrors [SpeakerPickerTarget]'s own paragraphIndices/currentSpeaker shape,
 *  just keyed by describes-name instead of speaker. */
private data class DescriptionPickerTarget(
    val chapterIdx: Int,
    val paragraphIndices: List<Int>,
    val fromName: String,
)

/** Speakers already narrating paragraphs near [paragraphIndices] within the same chapter (the
 *  same scene/conversation), closest occurrence first - surfaced ahead of the rest of the book's
 *  full character roster in [SpeakerPickerSheet], since a misattributed line is usually meant for
 *  whoever else is already talking around it, not some character from a completely different part
 *  of the book. Distance is measured in paragraph index from the *closest* of [paragraphIndices]
 *  (a multi-segment block's own quote segments) to each candidate paragraph, in either direction
 *  (before or after). Excludes [paragraphIndices]'s own paragraphs and Narrator/blank - Narrator
 *  already leads the picker unconditionally, and a paragraph's own current speaker isn't a
 *  "different" one to suggest. */
private fun nearbySpeakersFor(chapter: ChapterDetailDto?, paragraphIndices: List<Int>): List<String> {
    if (chapter == null || paragraphIndices.isEmpty()) return emptyList()
    return chapter.paragraphs
        .asSequence()
        .filterNot { it.idx in paragraphIndices }
        .filter { !it.speaker.isNullOrEmpty() && it.speaker != "Narrator" }
        .sortedBy { p -> paragraphIndices.minOf { target -> abs(p.idx - target) } }
        .map { it.speaker!! }
        .distinct()
        .toList()
}

private sealed class ReaderListEntry {
    data class ChapterHeader(val chapterIdx: Int, val title: String) : ReaderListEntry()
    // segments is one or more paragraphs rendered as a single visual block - a lone non-Inline
    // paragraph is a list of one; a non-Inline paragraph followed by one or more Inline ones
    // (see ParagraphDto.inline) is grouped together, mirroring
    // frontend/src/pages/ReaderPage.tsx's groupContent. Always non-empty.
    data class ParagraphItem(val chapterIdx: Int, val segments: List<ParagraphDto>) : ReaderListEntry()
    // A standalone image between/around paragraphs (backend's contentItemDTO kind "image"),
    // e.g. an illustration or figure embedded in the original epub - imageUrl is still the raw
    // relative path (`/api/images/{id}`); resolved against the current server at render time,
    // same as covers.
    data class ImageItem(val chapterIdx: Int, val key: String, val imageUrl: String) : ReaderListEntry()
    // A scene/section break (backend's contentItemDTO kind "break") - the source epub's own
    // <hr/>, or a text-only marker like "* * *" that used to be read aloud literally (see
    // backend epub.BlockBreak). Carries no data beyond its position, same as ImageItem minus the
    // URL - rendered as a plain divider, mirroring frontend's ChapterSection.tsx.
    data class BreakItem(val chapterIdx: Int, val key: String) : ReaderListEntry()
    // Marks where a chapter's own background-music region starts (annotations mode only - see
    // InfiniteChapterContent's own musicByStart, built only while annotationsMode is on) -
    // mirrors frontend's own ChapterSection.MusicRegionBoundary. Always immediately precedes the
    // [ParagraphItem] whose first segment's idx == region.startIdx, never its own list position.
    data class MusicBoundary(val chapterIdx: Int, val key: String, val region: MusicRegionDto) : ReaderListEntry()
}

/**
 * Walks chapter's content items in original document order (backend's contentItemDTO - text
 * interleaved with images and scene breaks) and turns them into [ReaderListEntry]s: each image
 * becomes its own [ReaderListEntry.ImageItem], each break its own [ReaderListEntry.BreakItem],
 * and each text item's referenced paragraph either joins the visual block currently being built
 * (when [ParagraphDto.inline]) or starts a new one - exactly mirrors
 * frontend/src/pages/ReaderPage.tsx's groupContent. A content item whose paragraphIdx doesn't
 * resolve (shouldn't normally happen) is skipped rather than crashing the reader.
 */
private fun groupContent(
    chapterIdx: Int,
    chapter: ChapterDetailDto,
    // Keyed by MusicRegionDto.startIdx (regions are contiguous/non-overlapping - see backend
    // store.MusicRegion's own doc comment), so a new text group's first paragraph idx matching a
    // region's startIdx is exactly "insert that region's own boundary marker right before this
    // group" - mirrors frontend's ChapterSection.tsx musicRegionsByStartIdx exactly. Empty
    // (skipping every boundary) while annotations mode is off - see the caller.
    musicRegionsByStartIdx: Map<Int, MusicRegionDto>,
): List<ReaderListEntry> {
    val paragraphsByIdx = chapter.paragraphs.associateBy { it.idx }
    val entries = mutableListOf<ReaderListEntry>()
    var currentGroup: MutableList<ParagraphDto>? = null
    chapter.content.forEachIndexed { i, item ->
        if (item.kind == "image") {
            currentGroup = null
            entries.add(ReaderListEntry.ImageItem(chapterIdx, "img-$chapterIdx-$i", item.imageUrl.orEmpty()))
            return@forEachIndexed
        }
        if (item.kind == "break") {
            currentGroup = null
            entries.add(ReaderListEntry.BreakItem(chapterIdx, "break-$chapterIdx-$i"))
            return@forEachIndexed
        }
        val p = paragraphsByIdx[item.paragraphIdx] ?: return@forEachIndexed
        val group = currentGroup
        if (p.inline && group != null) {
            group.add(p)
        } else {
            val newGroup = mutableListOf(p)
            currentGroup = newGroup
            musicRegionsByStartIdx[p.idx]?.let { region ->
                entries.add(ReaderListEntry.MusicBoundary(chapterIdx, "music-$chapterIdx-${region.id}", region))
            }
            entries.add(ReaderListEntry.ParagraphItem(chapterIdx, newGroup))
        }
    }
    return entries
}

/**
 * The infinite-scroll reader - the Android analogue of ReaderPage.tsx's `.scroll-reader`: every
 * chapter in [range] that's actually in [loadedChapters] rendered as one continuous flat list
 * (title header, then its paragraphs, then the next chapter's header, and so on), rather than
 * requiring a chapter to be explicitly picked. Grows the window via [onExpandUp]/[onExpandDown]
 * as the list scrolls near either edge - the Compose analogue of the web version's
 * IntersectionObserver sentinels, but driven off `LazyListState.layoutInfo` directly instead of
 * separate sentinel elements, since Compose has no intersection-observer primitive of its own.
 */
@Composable
private fun InfiniteChapterContent(
    listState: LazyListState,
    loadedChapters: Map<Int, ChapterDetailDto>,
    range: IntRange,
    totalChapters: Int,
    playback: PlaybackState,
    playbackSeconds: Double,
    autoFollow: Boolean,
    bookmarksByKey: Map<Pair<Int, Int>, String>,
    fontSizeSp: Int,
    fontFamily: FontFamily,
    annotationsMode: Boolean,
    chapterMusicRegions: Map<Int, List<MusicRegionDto>>,
    previewingMusicRegionId: String?,
    onParagraphClick: (chapterIdx: Int, paragraphIdx: Int, startSeconds: Double) -> Unit,
    onExpandUp: () -> Unit,
    onExpandDown: () -> Unit,
    onRegenerateParagraph: (chapterIdx: Int, paragraphIdx: Int) -> Unit,
    onToggleBookmark: (chapterIdx: Int, paragraphIdx: Int) -> Unit,
    onOpenSpeakerPicker: (chapterIdx: Int, paragraphIndices: List<Int>, currentSpeaker: String) -> Unit,
    onOpenDescriptionPicker: (chapterIdx: Int, paragraphIndices: List<Int>, fromName: String) -> Unit,
    onSetScareQuote: (chapterIdx: Int, paragraphIndices: List<Int>, scareQuote: Boolean) -> Unit,
    onRegenerateMusic: (chapterIdx: Int, regionId: String) -> Unit,
    onPreviewMusic: (regionId: String, audioUrl: String) -> Unit,
    resolveImageUrl: (String) -> Any?,
) {
    val entries = remember(loadedChapters, range, annotationsMode, chapterMusicRegions) {
        range.mapNotNull { idx -> loadedChapters[idx] }.flatMap { chapter ->
            // Only actually inserted while annotations mode is on - see MusicBoundary's own doc
            // comment; chapterMusicRegions itself may already be populated regardless (the live
            // background-music mix needs it independent of whether the reader ever looks at
            // annotations mode), so the gate belongs here, not further upstream.
            val musicByStart = if (annotationsMode) {
                chapterMusicRegions[chapter.idx].orEmpty().associateBy { it.startIdx }
            } else {
                emptyMap()
            }
            buildList {
                add(ReaderListEntry.ChapterHeader(chapter.idx, chapter.title))
                addAll(groupContent(chapter.idx, chapter, musicByStart))
            }
        }
    }

    // Grows the loaded window as the reader nears either end of what's currently mounted -
    // "near" being within 3 items of an edge, same spirit as the web version's 800px sentinel
    // margin (triggering before the reader actually hits the end, not after).
    LaunchedEffect(listState, range.first, totalChapters) {
        snapshotFlow { listState.layoutInfo }.collect { layoutInfo ->
            val visible = layoutInfo.visibleItemsInfo
            if (visible.isEmpty()) return@collect
            if (visible.first().index <= 2 && range.first > 0) onExpandUp()
            if (visible.last().index >= layoutInfo.totalItemsCount - 3 && range.last < totalChapters - 1) onExpandDown()
        }
    }

    // Keeps the currently-playing paragraph centered as playback advances - see
    // LazyListState.centerItem. Suspends itself the moment the reader scrolls by their own
    // input (autoFollow, tracked by the caller), same as ReaderPage.tsx.
    LaunchedEffect(playback.chapterIdx, playback.paragraphIdx, autoFollow, entries) {
        if (!autoFollow) return@LaunchedEffect
        val itemIndex = entries.indexOfFirst {
            it is ReaderListEntry.ParagraphItem && it.chapterIdx == playback.chapterIdx &&
                it.segments.any { s -> s.idx == playback.paragraphIdx }
        }
        if (itemIndex < 0) return@LaunchedEffect
        listState.centerItem(itemIndex)
    }

    LazyColumn(
        state = listState,
        modifier = Modifier.fillMaxSize(),
        contentPadding = PaddingValues(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        items(
            entries,
            key = { entry ->
                when (entry) {
                    is ReaderListEntry.ChapterHeader -> "header-${entry.chapterIdx}"
                    is ReaderListEntry.ParagraphItem -> "p-${entry.chapterIdx}-${entry.segments.first().idx}"
                    is ReaderListEntry.ImageItem -> entry.key
                    is ReaderListEntry.BreakItem -> entry.key
                    is ReaderListEntry.MusicBoundary -> entry.key
                }
            },
        ) { entry ->
            when (entry) {
                is ReaderListEntry.ChapterHeader -> {
                    Text(
                        entry.title,
                        style = MaterialTheme.typography.headlineSmall,
                        modifier = Modifier.padding(
                            top = if (entry.chapterIdx == range.first) 0.dp else 24.dp,
                            bottom = 8.dp,
                        ),
                    )
                }
                is ReaderListEntry.ParagraphItem -> {
                    val activeSegmentIdx = if (playback.chapterIdx == entry.chapterIdx) {
                        entry.segments.indexOfFirst { it.idx == playback.paragraphIdx }
                    } else {
                        -1
                    }
                    val firstIdx = entry.segments.first().idx
                    ParagraphRow(
                        segments = entry.segments,
                        activeSegmentIdx = activeSegmentIdx,
                        isBookmarked = bookmarksByKey.containsKey(entry.chapterIdx to firstIdx),
                        playbackSeconds = if (activeSegmentIdx >= 0) playbackSeconds else 0.0,
                        fontSizeSp = fontSizeSp,
                        fontFamily = fontFamily,
                        annotationsMode = annotationsMode,
                        onWordTap = { paragraphIdx, startSeconds -> onParagraphClick(entry.chapterIdx, paragraphIdx, startSeconds) },
                        // A reader regenerating "this paragraph" means the whole visual block
                        // they're looking at, inline segments included - mirrors
                        // ReaderPage.tsx's own regenerate button.
                        onRegenerate = { entry.segments.forEach { onRegenerateParagraph(entry.chapterIdx, it.idx) } },
                        onToggleBookmark = { onToggleBookmark(entry.chapterIdx, firstIdx) },
                        // Only this block's own isQuote segment(s) - see SpeakerPickerTarget's doc
                        // comment for why narration/description segments are excluded.
                        onOpenSpeakerPicker = {
                            val quoteSegments = entry.segments.filter { it.isQuote }
                            val current = quoteSegments.mapNotNull { it.speaker }.firstOrNull { it.isNotEmpty() } ?: "Narrator"
                            onOpenSpeakerPicker(entry.chapterIdx, quoteSegments.map { it.idx }, current)
                        },
                        onOpenDescriptionPicker = { fromName, paragraphIndices ->
                            onOpenDescriptionPicker(entry.chapterIdx, paragraphIndices, fromName)
                        },
                        onToggleScareQuote = { paragraphIndices, scareQuote ->
                            onSetScareQuote(entry.chapterIdx, paragraphIndices, scareQuote)
                        },
                    )
                }
                is ReaderListEntry.ImageItem -> {
                    AsyncImage(
                        model = resolveImageUrl(entry.imageUrl),
                        contentDescription = null,
                        contentScale = ContentScale.FillWidth,
                        modifier = Modifier
                            .fillMaxWidth()
                            .clip(RoundedCornerShape(8.dp)),
                    )
                }
                is ReaderListEntry.BreakItem -> {
                    Box(modifier = Modifier.fillMaxWidth(), contentAlignment = Alignment.Center) {
                        HorizontalDivider(modifier = Modifier.width(64.dp))
                    }
                }
                is ReaderListEntry.MusicBoundary -> {
                    MusicRegionBoundaryRow(
                        region = entry.region,
                        previewing = previewingMusicRegionId == entry.region.id,
                        onRegenerate = { onRegenerateMusic(entry.chapterIdx, entry.region.id) },
                        onPreview = { entry.region.audioUrl?.let { onPreviewMusic(entry.region.id, it) } },
                    )
                }
            }
        }
    }
}

/** "m:ss" - mirrors frontend's ChapterSection.tsx formatMusicDuration exactly. */
private fun formatMusicDuration(seconds: Double): String {
    val m = (seconds / 60).toInt()
    val s = (seconds % 60).toInt()
    return "$m:${s.toString().padStart(2, '0')}"
}

/**
 * Marks where a chapter's own background-music region starts - the mood/prompt "score" a reader
 * can otherwise only see indirectly by hearing it, plus whether this region continues smoothly
 * out of the previous one or is a hard cut, and its own generation status. A labeled divider, not
 * a per-paragraph badge, since a region's mood applies to every paragraph until the next
 * boundary - mirrors frontend's ChapterSection.tsx MusicRegionBoundary.
 */
@Composable
private fun MusicRegionBoundaryRow(
    region: MusicRegionDto,
    previewing: Boolean,
    onRegenerate: () -> Unit,
    onPreview: () -> Unit,
) {
    val statusLabel = when (region.status) {
        AudioStatus.PENDING -> "not generated yet"
        AudioStatus.GENERATING -> "generating…"
        AudioStatus.ERROR -> "generation failed"
        AudioStatus.READY -> null
    }
    val durationLabel = if (region.status == AudioStatus.READY && region.durationSeconds > 0) {
        formatMusicDuration(region.durationSeconds)
    } else {
        null
    }
    val busy = region.status == AudioStatus.GENERATING
    Row(
        verticalAlignment = Alignment.CenterVertically,
        modifier = Modifier
            .fillMaxWidth()
            .background(MaterialTheme.colorScheme.surfaceVariant, RoundedCornerShape(8.dp))
            .padding(horizontal = 12.dp, vertical = 6.dp),
    ) {
        Icon(Icons.Default.MusicNote, contentDescription = null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
        Column(modifier = Modifier.weight(1f).padding(start = 8.dp)) {
            Text(
                region.mood.ifBlank { "Untitled region" },
                style = MaterialTheme.typography.labelLarge,
            )
            Text(
                buildString {
                    append(if (region.transition == "continuation") "continues" else "new cue")
                    statusLabel?.let { append(" · $it") }
                    durationLabel?.let { append(" · $it") }
                },
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            if (region.prompt.isNotBlank()) {
                Text(
                    region.prompt,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                )
            }
        }
        if (region.status == AudioStatus.READY && region.audioUrl != null) {
            IconButton(onClick = onPreview) {
                Icon(if (previewing) Icons.Default.Pause else Icons.Default.PlayArrow, contentDescription = "Preview this region's music")
            }
        }
        IconButton(onClick = onRegenerate, enabled = !busy) {
            Icon(
                Icons.Default.Refresh,
                contentDescription = if (region.status == AudioStatus.READY) "Regenerate this region's music" else "Generate this region's music",
            )
        }
    }
}

/**
 * Scrolls so the item at [index] (a LazyColumn item index, not a paragraph idx - callers must
 * account for any header items) ends up centered in the viewport, not just visible at an edge.
 * `BringIntoViewRequester` (the more obvious tool here) only scrolls the minimum amount needed
 * to bring an item fully into view, which is why a naive first attempt at this "didn't actually
 * work" as centering - it would leave the active paragraph sitting at whichever edge it
 * scrolled in from.
 *
 * Only falls back to `animateScrollToItem` (itself animated, not an instant jump) when [index]
 * isn't already laid out - e.g. right after a chapter switch. The common case, advancing one
 * paragraph at a time during normal playback, almost always lands on an already-visible item,
 * so it skips straight to a single smooth `animateScrollBy` nudge instead of an instant
 * `scrollToItem` snap immediately followed by a correcting animation - which is what made an
 * earlier version of this feel like an aggressive jump-then-slide rather than one motion.
 */
private suspend fun LazyListState.centerItem(index: Int) {
    if (layoutInfo.visibleItemsInfo.none { it.index == index }) {
        animateScrollToItem(index)
    }
    val info = layoutInfo.visibleItemsInfo.find { it.index == index } ?: return
    val viewportCenter = (layoutInfo.viewportStartOffset + layoutInfo.viewportEndOffset) / 2
    val itemCenter = info.offset + info.size / 2
    animateScrollBy((itemCenter - viewportCenter).toFloat())
}

@Composable
private fun ParagraphRow(
    segments: List<ParagraphDto>,
    // Index into [segments] of the one currently targeted by playback, or -1 if none of them
    // are (this whole visual block isn't the active one).
    activeSegmentIdx: Int,
    isBookmarked: Boolean,
    playbackSeconds: Double,
    fontSizeSp: Int,
    fontFamily: FontFamily,
    annotationsMode: Boolean,
    onWordTap: (paragraphIdx: Int, startSeconds: Double) -> Unit,
    onRegenerate: () -> Unit,
    onToggleBookmark: () -> Unit,
    onOpenSpeakerPicker: () -> Unit,
    onOpenDescriptionPicker: (fromName: String, paragraphIndices: List<Int>) -> Unit,
    onToggleScareQuote: (paragraphIndices: List<Int>, scareQuote: Boolean) -> Unit,
) {
    val isActive = activeSegmentIdx >= 0
    val background = if (isActive) MaterialTheme.colorScheme.primaryContainer else MaterialTheme.colorScheme.surface
    var showMenu by remember { mutableStateOf(false) }
    val clipboardManager = LocalClipboardManager.current
    val isDarkTheme = MaterialTheme.colorScheme.background.luminance() < 0.5f
    val speakerMarkColor = if (isDarkTheme) AnnotationSpeakerColorDark else AnnotationSpeakerColorLight
    val descriptionMarkColor = if (isDarkTheme) AnnotationDescriptionColorDark else AnnotationDescriptionColorLight
    val scareQuoteMarkColor = if (isDarkTheme) ScareQuoteMarkColorDark else ScareQuoteMarkColorLight
    // Every distinct non-empty Speaker among this block's segments, "Narrator" included -
    // present whenever attribution has run for this paragraph, not gated on book.multiVoice
    // (it's informational either way, same as frontend's ReaderPage.tsx paragraph-speaker-hint).
    // Shown as a static entry in the long-press menu rather than always inline under the text,
    // since it's secondary information the reader wants on demand, not something that should
    // compete with the narration text itself for every paragraph in a multivoice book.
    val speakers = remember(segments) { segments.mapNotNull { it.speaker }.filter { it.isNotEmpty() }.distinct() }
    // Every distinct character this block's narration describes (annotations mode's own
    // "description" kind - see annotationKind), each mapped to which of this block's own
    // segment idx's actually carry it - a block can have more than one inline segment, and more
    // than one character described - so "Reassign" (see onOpenDescriptionPicker below) only ever
    // touches the segment(s) that specific name actually applies to, not the whole block.
    val describesByName = remember(segments) {
        val map = linkedMapOf<String, MutableList<Int>>()
        segments.forEach { seg -> seg.describesCharacters.forEach { name -> map.getOrPut(name) { mutableListOf() }.add(seg.idx) } }
        map
    }
    // Every distinct inline Higgs delivery tag among this block's segments, each with its own
    // formatted label and category color - mirrors frontend's ReaderPage.tsx directionTags/
    // formatDirectionTag/directionTagCategory. Same "long-press menu, not inline" placement.
    val directionTags = remember(segments) {
        segments.flatMap { it.directionMarks }.map { it.tag }.distinct()
    }
    // Every resolved pronunciation substitution among this block's segments - same "long-press
    // menu, not inline" placement as directionTags above, mirroring frontend's own reliance on
    // the inline caret's hover title for this (no separate legend/list on the web side either).
    val pronunciationMarks = remember(segments) { segments.flatMap { it.pronunciationMarks } }
    // "Set speaker"/"Mark as scare quote" only make sense when this block actually has quoted
    // dialogue in it - the backend rejects attributing narration/description to a character (see
    // SpeakerPickerTarget's doc comment) and rejects scare-quoting a non-quoted paragraph the
    // same way, so a pure-narration block just doesn't offer either, same as how
    // [speakers]/[describesByName]/[directionTags] above only show when they apply.
    val quoteSegments = remember(segments) { segments.filter { it.isQuote } }
    val hasQuoteSegment = quoteSegments.isNotEmpty()
    // Toggle target for "Mark/unmark as scare quote" - true only when every one of this block's
    // own quote segments is already marked, mirroring how [speakers]' own "current" value is
    // read off the block as a whole rather than per-segment; toggling always sets every quote
    // segment to the same, opposite value (see ReaderViewModel.setScareQuote).
    val allScareQuote = remember(quoteSegments) { quoteSegments.isNotEmpty() && quoteSegments.all { it.scareQuote } }
    // Whether to show the informational "Scare quote" row (any, not all - unlike allScareQuote,
    // which is the toggle's own target) - mirrors frontend's annotationTitle appending "· Scare
    // quote" to the hover tooltip.
    val hasScareQuote = remember(quoteSegments) { quoteSegments.any { it.scareQuote } }

    Column(
        modifier = Modifier
            .fillMaxWidth()
            .clip(RoundedCornerShape(8.dp))
            .background(background)
            .padding(12.dp),
    ) {
        Box {
            ParagraphBodyText(
                segments = segments,
                // The pill background above still lights up for a *targeted* paragraph even before
                // it's generated (see ParagraphPlayer.playParagraph's not-ready branch), so the tap
                // visibly registered - but word-level highlighting needs real elapsed playback time
                // to mean anything, which a paragraph with no audio yet doesn't have.
                activeSegmentIdx = if (isActive && segments[activeSegmentIdx].audioStatus == AudioStatus.READY) activeSegmentIdx else -1,
                playbackSeconds = playbackSeconds,
                fontSizeSp = fontSizeSp,
                fontFamily = fontFamily,
                annotationsMode = annotationsMode,
                onWordTap = onWordTap,
                onLongPress = { showMenu = true },
            )
            DropdownMenu(expanded = showMenu, onDismissRequest = { showMenu = false }) {
                if (speakers.isNotEmpty()) {
                    DropdownMenuItem(
                        text = { Text("Speaker: ${speakers.joinToString(", ")}") },
                        leadingIcon = { AnnotationSwatch(speakerMarkColor) },
                        enabled = false,
                        onClick = {},
                    )
                }
                describesByName.forEach { (name, idxs) ->
                    DropdownMenuItem(
                        text = { Text("Describes: $name") },
                        leadingIcon = { AnnotationSwatch(descriptionMarkColor) },
                        onClick = { onOpenDescriptionPicker(name, idxs); showMenu = false },
                    )
                }
                val otherDirectionColor = MaterialTheme.colorScheme.onSurfaceVariant
                directionTags.forEach { tag ->
                    DropdownMenuItem(
                        text = { Text(formatDirectionTag(tag)) },
                        leadingIcon = { AnnotationCaretIcon(directionTagColor(tag, isDarkTheme, otherDirectionColor)) },
                        enabled = false,
                        onClick = {},
                    )
                }
                pronunciationMarks.forEach { mark ->
                    DropdownMenuItem(
                        text = { Text(formatPronunciationMark(mark)) },
                        leadingIcon = { PronunciationStrikeIcon(if (isDarkTheme) PronunciationCaretColorDark else PronunciationCaretColorLight) },
                        enabled = false,
                        onClick = {},
                    )
                }
                if (hasScareQuote) {
                    DropdownMenuItem(
                        text = { Text("Scare quote") },
                        leadingIcon = { AnnotationSwatch(scareQuoteMarkColor) },
                        enabled = false,
                        onClick = {},
                    )
                }
                if (speakers.isNotEmpty() || describesByName.isNotEmpty() || directionTags.isNotEmpty() || pronunciationMarks.isNotEmpty() || hasScareQuote) {
                    HorizontalDivider()
                }
                DropdownMenuItem(
                    text = { Text("Regenerate paragraph") },
                    leadingIcon = { Icon(Icons.Filled.Refresh, contentDescription = null) },
                    onClick = { onRegenerate(); showMenu = false },
                )
                if (hasQuoteSegment) {
                    DropdownMenuItem(
                        text = { Text("Set speaker") },
                        leadingIcon = { Icon(Icons.Filled.Person, contentDescription = null) },
                        onClick = { onOpenSpeakerPicker(); showMenu = false },
                    )
                    DropdownMenuItem(
                        text = { Text(if (allScareQuote) "Unmark as scare quote" else "Mark as scare quote") },
                        leadingIcon = { Icon(Icons.Filled.FormatQuote, contentDescription = null) },
                        onClick = { onToggleScareQuote(quoteSegments.map { it.idx }, !allScareQuote); showMenu = false },
                    )
                }
                DropdownMenuItem(
                    text = { Text(if (isBookmarked) "Remove bookmark" else "Bookmark this paragraph") },
                    leadingIcon = {
                        Icon(
                            if (isBookmarked) Icons.Filled.Bookmark else Icons.Filled.BookmarkBorder,
                            contentDescription = null,
                        )
                    },
                    onClick = { onToggleBookmark(); showMenu = false },
                )
                DropdownMenuItem(
                    text = { Text("Copy paragraph") },
                    leadingIcon = { Icon(Icons.Filled.ContentCopy, contentDescription = null) },
                    onClick = {
                        clipboardManager.setText(AnnotatedString(segments.joinToString(" ") { it.text }))
                        showMenu = false
                    },
                )
            }
        }
        segments.forEach { segment ->
            if (segment.audioStatus == AudioStatus.ERROR) {
                Text(
                    text = "Narration failed: ${segment.audioError ?: "unknown error"}",
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.error,
                    modifier = Modifier.padding(top = 4.dp),
                )
            }
        }
    }
}

// Extra room around the active word's tight glyph bounds, so the highlight reads as a
// pill-shaped box rather than a strict text-background rectangle - SpanStyle.background
// alone can't do this (no padding/corner-radius concept), hence drawing it manually below.
private val HighlightHorizontalPadding = 4.dp
private val HighlightTopPadding = 2.dp
private val HighlightBottomPadding = 2.dp
private val HighlightCornerRadius = 6.dp

/** Last token whose char range starts at or before [charIndex] - the tap-target analogue of
 *  activeWordIndex's "last word whose start time has passed" scan. */
private fun tokenIndexForChar(tokens: List<WordToken>, charIndex: Int): Int {
    var idx = -1
    for (i in tokens.indices) {
        if (tokens[i].start <= charIndex) idx = i else break
    }
    return idx
}

// One segment's tokens/word-start-times, plus where its text lands (char offset) inside the
// combined, all-segments-joined string ParagraphBodyText actually renders - see its own doc
// comment for why segments are joined into one flowing Text instead of stacked one-per-segment.
private data class SegmentSpan(
    val paragraphIdx: Int,
    val textOffset: Int,
    val tokens: List<WordToken>,
    val startTimes: List<Double>,
)

/**
 * Renders one or more paragraphs' text joined into a single flowing block of text - a lone
 * paragraph, or a non-Inline paragraph followed by its Inline continuations (see
 * ParagraphDto.inline), which read as one continuous sentence/paragraph even though each is its
 * own playback/highlight/tap-target unit, exactly like frontend/src/components/ParagraphText.tsx
 * does per-span within frontend/src/pages/ReaderPage.tsx's own grouped `<p>`. Each segment's own
 * text is trimmed at parse time (backend's epub.splitQuoteSegments splits mid-sentence on quote
 * boundaries), so a literal space is reinserted between segments here to avoid e.g. "fire." and
 * the next segment's opening quote running together with no gap.
 *
 * Karaoke-highlights the word at [playbackSeconds] through whichever segment is
 * [activeSegmentIdx] (using real per-word forced-alignment timings from that segment's own
 * `words` when available, falling back to the proportional character-position estimate
 * otherwise - see WordHighlight.kt's wordStartTimes); -1 highlights nothing. Reports which
 * paragraph and seconds offset a tapped word belongs to, so the caller can seek there (works on
 * any segment, not just the active one - tapping a word in a paragraph that isn't playing yet
 * starts it from there).
 */
@Composable
private fun ParagraphBodyText(
    segments: List<ParagraphDto>,
    activeSegmentIdx: Int,
    playbackSeconds: Double,
    fontSizeSp: Int,
    fontFamily: FontFamily,
    annotationsMode: Boolean,
    onWordTap: (paragraphIdx: Int, seconds: Double) -> Unit,
    onLongPress: () -> Unit,
) {
    val spans = remember(segments) {
        val list = mutableListOf<SegmentSpan>()
        var offset = 0
        for (seg in segments) {
            val tokens = tokenizeWords(seg.text)
            val startTimes = wordStartTimes(tokens, seg.words, seg.text.length, seg.durationSeconds ?: 0.0, seg.audioPointerSeconds)
            list.add(SegmentSpan(seg.idx, offset, tokens, startTimes))
            offset += seg.text.length + 1 // +1 for the joining space below
        }
        list
    }
    val combinedText = remember(segments) { segments.joinToString(" ") { it.text } }

    val activeSpan = spans.getOrNull(activeSegmentIdx)
    val activeLocalIdx = if (activeSpan != null) {
        remember(activeSpan, playbackSeconds) { activeWordIndex(activeSpan.startTimes, playbackSeconds) }
    } else {
        -1
    }
    // Darkened, not the bare primary color - in a dark color scheme, MaterialTheme.colorScheme
    // .primary is typically a *lighter* tone than .primaryContainer (the active paragraph's own
    // background, see ParagraphRow), so it'd render lighter than the paragraph highlight instead
    // of standing out as the more specific one - same fix, and same 85/15 mix, as frontend's
    // .word-active (`color-mix(in srgb, var(--accent) 85%, black)`).
    val highlightColor = lerp(MaterialTheme.colorScheme.primary, Color.Black, 0.15f)
    val highlightTextColor = MaterialTheme.colorScheme.onPrimary
    // Mirrors frontend's .paragraph-pending (opacity: 0.6) - but scoped to just the segments
    // that aren't ready yet, not the whole visual block, so a multi-segment paragraph (a
    // non-Inline paragraph plus its Inline continuations - see ParagraphDto.inline) shows
    // exactly which of its own parts are still generating instead of dimming an already-narrated
    // part along with them.
    val dimColor = MaterialTheme.colorScheme.onSurfaceVariant
    // Annotations mode's own underline/caret colors - exact hex match to frontend's
    // --annotation-speaker/--annotation-description CSS variables (index.css), light or dark
    // set picked off the app's actually-resolved background luminance (not a raw
    // isSystemInDarkTheme() check) so this tracks the same light/dark state as the rest of the
    // UI even when the reader's own Settings screen has overridden it away from the system
    // default - see MainActivity's own darkTheme resolution.
    val isDarkTheme = MaterialTheme.colorScheme.background.luminance() < 0.5f
    val speakerMarkColor = if (isDarkTheme) AnnotationSpeakerColorDark else AnnotationSpeakerColorLight
    val descriptionMarkColor = if (isDarkTheme) AnnotationDescriptionColorDark else AnnotationDescriptionColorLight
    val pronunciationCaretColor = if (isDarkTheme) PronunciationCaretColorDark else PronunciationCaretColorLight
    val scareQuoteMarkColor = if (isDarkTheme) ScareQuoteMarkColorDark else ScareQuoteMarkColorLight
    var textLayoutResult by remember { mutableStateOf<TextLayoutResult?>(null) }

    // The active token's char range within combinedText - global (span-offset-adjusted), unlike
    // activeSpan.tokens' own per-segment-local offsets.
    val activeRange = if (activeSpan != null && activeLocalIdx in activeSpan.tokens.indices) {
        val token = activeSpan.tokens[activeLocalIdx]
        (activeSpan.textOffset + token.start) to (activeSpan.textOffset + token.end)
    } else {
        null
    }

    val annotated = remember(combinedText, activeRange, highlightTextColor, dimColor, segments) {
        buildAnnotatedString {
            append(combinedText)
            segments.forEachIndexed { i, segment ->
                if (segment.audioStatus != AudioStatus.READY) {
                    val span = spans[i]
                    val end = (span.textOffset + segment.text.length).coerceAtMost(combinedText.length)
                    addStyle(SpanStyle(color = dimColor), span.textOffset, end)
                }
            }
            activeRange?.let { (start, end) -> addStyle(SpanStyle(color = highlightTextColor), start, end) }
        }
    }

    val bodyStyle = MaterialTheme.typography.bodyLarge
    // lineHeight scales along with the chosen font size rather than staying at bodyLarge's own
    // fixed value, preserving its natural line-height:font-size ratio at any size - otherwise a
    // large reading size would end up visually cramped (theme's line height meant for the
    // theme's own, much smaller, default size).
    val lineHeightRatio = bodyStyle.lineHeight.value / bodyStyle.fontSize.value
    Text(
        text = annotated,
        style = bodyStyle.copy(
            fontSize = fontSizeSp.sp,
            lineHeight = (fontSizeSp * lineHeightRatio).sp,
            fontFamily = fontFamily,
        ),
        onTextLayout = { textLayoutResult = it },
        modifier = Modifier
            .fillMaxWidth()
            .pointerInput(combinedText, spans) {
                detectTapGestures(
                    onLongPress = { onLongPress() },
                    onTap = { offset ->
                        val layout = textLayoutResult ?: return@detectTapGestures
                        val charIndex = layout.getOffsetForPosition(offset)
                        val span = spans.lastOrNull { it.textOffset <= charIndex } ?: return@detectTapGestures
                        val wordIdx = tokenIndexForChar(span.tokens, charIndex - span.textOffset)
                        if (wordIdx >= 0) onWordTap(span.paragraphIdx, span.startTimes.getOrElse(wordIdx) { 0.0 })
                    },
                )
            }
            .drawBehind {
                val layout = textLayoutResult ?: return@drawBehind
                // Drawn unconditionally (as long as there's a layout at all) - deliberately
                // *before* the active-word-highlight block below, which has its own early
                // returns (return@drawBehind) whenever this particular paragraph isn't the one
                // currently playing/highlighted. Those returns used to sit ahead of this block
                // too, which meant annotation marks only ever rendered on the one paragraph
                // actively playing at that instant - i.e. never, for the other ~everything else
                // in the chapter - since this is meant to mark every dialogue/description
                // segment in the whole chapter regardless of playback state.
                if (annotationsMode) {
                    segments.forEachIndexed { i, segment ->
                        val segSpan = spans[i]
                        // Dialogue/description get an underline (annotationKind) - orthogonal to
                        // and independent of whichever delivery-tag carets this same segment
                        // might also carry below, exactly like frontend's own annotation-* class
                        // and .direction-caret compose on the same span without one gating the
                        // other.
                        val underlineColor = when (annotationKind(segment)) {
                            AnnotationKind.SPEAKER -> speakerMarkColor
                            AnnotationKind.DESCRIPTION -> descriptionMarkColor
                            AnnotationKind.NARRATOR -> null
                        }
                        if (underlineColor != null) {
                            val segToken = WordToken(segment.text, segSpan.textOffset, segSpan.textOffset + segment.text.length)
                            for (bounds in wordLineBounds(layout, segToken)) {
                                drawAnnotationUnderline(bounds, underlineColor)
                            }
                        }
                        if (segment.scareQuote) {
                            val segToken = WordToken(segment.text, segSpan.textOffset, segSpan.textOffset + segment.text.length)
                            for (bounds in wordLineBounds(layout, segToken)) {
                                drawScareQuoteUnderline(bounds, scareQuoteMarkColor)
                            }
                        }
                        for (mark in segment.directionMarks) {
                            val charIndex = (segSpan.textOffset + mark.offset)
                                .coerceIn(segSpan.textOffset, segSpan.textOffset + segment.text.length)
                            val line = layout.getLineForOffset(charIndex)
                            val x = layout.getHorizontalPosition(charIndex, usePrimaryDirection = true)
                            drawDirectionCaret(x, layout.getLineBottom(line), directionTagColor(mark.tag, isDarkTheme, dimColor))
                        }
                        for (mark in segment.pronunciationMarks) {
                            val start = (segSpan.textOffset + mark.offset)
                                .coerceIn(segSpan.textOffset, segSpan.textOffset + segment.text.length)
                            val end = (start + mark.length).coerceIn(start, segSpan.textOffset + segment.text.length)
                            val token = WordToken(mark.original, start, end)
                            for (bounds in wordLineBounds(layout, token)) {
                                drawPronunciationStrike(bounds, pronunciationCaretColor)
                            }
                        }
                    }
                }
                val span = activeSpan ?: return@drawBehind
                val localToken = span.tokens.getOrNull(activeLocalIdx) ?: return@drawBehind
                val token = WordToken(localToken.word, span.textOffset + localToken.start, span.textOffset + localToken.end)
                val hPad = HighlightHorizontalPadding.toPx()
                val topPad = HighlightTopPadding.toPx()
                val bottomPad = HighlightBottomPadding.toPx()
                for (bounds in wordLineBounds(layout, token)) {
                    drawRoundRect(
                        color = highlightColor,
                        topLeft = Offset(bounds.left - hPad, bounds.top - topPad),
                        size = Size(bounds.width + hPad * 2, bounds.height + topPad + bottomPad),
                        cornerRadius = CornerRadius(HighlightCornerRadius.toPx()),
                    )
                }
            },
    )
}

// Exact hex match to frontend's --annotation-speaker/--annotation-description CSS variables
// (index.css's :root and its dark-mode overrides) - light/dark picked at the call site off the
// app's actually-resolved theme, not a raw system check. --annotation-narrator has no
// counterpart here since narrator segments get no mark at all (see drawAnnotationMark).
private val AnnotationSpeakerColorLight = Color(0xFF2563EB)
private val AnnotationDescriptionColorLight = Color(0xFFD97706)
private val AnnotationSpeakerColorDark = Color(0xFF60A5FA)
private val AnnotationDescriptionColorDark = Color(0xFFFBBF24)

// Exact hex match to frontend's --direction-emotion/-style/-prosody/-sfx CSS variables - one
// per delivery-tag category, for the inline caret marks and the long-press menu's own per-tag
// icon (see directionTagColor). No "other" pair here - see directionTagColor's own doc comment.
private val DirectionEmotionColorLight = Color(0xFFDC2626)
private val DirectionStyleColorLight = Color(0xFF0891B2)
private val DirectionProsodyColorLight = Color(0xFF16A34A)
private val DirectionSfxColorLight = Color(0xFFDB2777)
private val DirectionEmotionColorDark = Color(0xFFF87171)
private val DirectionStyleColorDark = Color(0xFF22D3EE)
private val DirectionProsodyColorDark = Color(0xFF4ADE80)
private val DirectionSfxColorDark = Color(0xFFF472B6)

// Exact hex match to frontend's --pronunciation-caret CSS variable, for the inline strikeout
// marks and the long-press menu's own per-mark icon (see PronunciationStrikeIcon). Name kept as
// "Caret" for continuity with the CSS variable/backend naming this still traces back to, even
// though the mark itself is a strikeout now, not a caret.
private val PronunciationCaretColorLight = Color(0xFF0D9488)
private val PronunciationCaretColorDark = Color(0xFF2DD4BF)

// Exact hex match to frontend's --scare-quote-mark CSS variable, for the dashed scare-quote
// underline (see drawScareQuoteUnderline) - its own hue, distinct from every annotation-*/
// direction-*/pronunciation-caret color above, since it marks yet another independent fact
// ("structurally a quote, but not actually spoken aloud") that can compose with any of them.
private val ScareQuoteMarkColorLight = Color(0xFFEA580C)
private val ScareQuoteMarkColorDark = Color(0xFFFB923C)

// Vertical gap between the text baseline and the underline, and the underline's own stroke
// width - sits close under the text (not far below it) so it still reads as "attached" to the
// line even with several annotated lines stacked close together in a busy chapter.
private val AnnotationUnderlineGap = 0.dp
private val AnnotationUnderlineStrokeWidth = 1.dp

/** Draws one line-segment's annotations-mode underline (dialogue/description - see
 *  annotationKind) - no background color, just the line itself, in whichever of the two
 *  annotation colors the caller resolved. */
private fun DrawScope.drawAnnotationUnderline(bounds: Rect, color: Color) {
    val y = bounds.bottom + AnnotationUnderlineGap.toPx()
    drawLine(
        color = color,
        start = Offset(bounds.left, y),
        end = Offset(bounds.right, y),
        strokeWidth = AnnotationUnderlineStrokeWidth.toPx(),
    )
}

// Sits just below the regular annotation underline (not on top of it) so both remain legible as
// two distinct marks stacked on the same line, mirroring how frontend's CSS composes .annotation-*
// (border-bottom, a box border) and .annotation-scare-quote (text-decoration, a separate line)
// on the same span without either replacing the other.
private val ScareQuoteUnderlineGap = 2.dp
private val ScareQuoteDashLength = 3.dp
private val ScareQuoteDashGap = 2.dp

/** Draws one line-segment's dashed scare-quote underline (see annotationKind's own doc comment)
 *  - independent of, and composable with, whichever annotationKind color the segment already
 *  has, same as frontend's own .annotation-scare-quote class layering on top of .annotation-*. */
private fun DrawScope.drawScareQuoteUnderline(bounds: Rect, color: Color) {
    val y = bounds.bottom + AnnotationUnderlineGap.toPx() + AnnotationUnderlineStrokeWidth.toPx() + ScareQuoteUnderlineGap.toPx()
    drawLine(
        color = color,
        start = Offset(bounds.left, y),
        end = Offset(bounds.right, y),
        strokeWidth = AnnotationUnderlineStrokeWidth.toPx(),
        pathEffect = PathEffect.dashPathEffect(floatArrayOf(ScareQuoteDashLength.toPx(), ScareQuoteDashGap.toPx())),
    )
}

// Size of one inline delivery-tag caret - mirrors frontend's ParagraphText.tsx DirectionCaret
// (a small up-arrow glyph sitting just below the text at its exact insertion point, pointing up
// at the line it annotates - the standard proofreading-insertion-caret convention).
private val DirectionCaretSize = 7.dp
private val DirectionCaretStrokeWidth = 1.3.dp
private val DirectionCaretGap = (-1).dp

/** Draws one inline delivery-tag caret at [x] - an open "^" (two strokes, not a filled
 *  triangle) sitting just below the line it annotates ([lineBottom]), apex pointing back up at
 *  it, rather than pushing text apart - same "zero-width in the real text flow" idea as
 *  frontend's own .direction-caret-mark. */
private fun DrawScope.drawDirectionCaret(x: Float, lineBottom: Float, color: Color) {
    val size = DirectionCaretSize.toPx()
    val top = lineBottom + DirectionCaretGap.toPx()
    val bottom = top + size
    val strokeWidth = DirectionCaretStrokeWidth.toPx()
    val stroke = Stroke(width = strokeWidth, cap = StrokeCap.Round, join = StrokeJoin.Round)
    val path = Path().apply {
        moveTo(x - size / 2, bottom)
        lineTo(x, top)
        lineTo(x + size / 2, bottom)
    }
    drawPath(path, color = color, style = stroke)
}

// Size/gap for one pronunciation caret - deliberately distinct from DirectionCaretSize/Gap
// above (a hair smaller, sitting right above the word rather than below the line) so the two
// read as related but visually different marks, mirroring frontend's own down-arrow-above-word
// vs. up-arrow-below-line distinction (PronunciationCaret vs. DirectionCaret in
// ParagraphText.tsx).
private val PronunciationStrikeWidth = 1.4.dp

/** Draws a strikeout line through the vertical center of [bounds] (one line-fragment of the word
 *  a pronunciation fix applies to - see wordLineBounds) - marks "this word is read differently
 *  than written" directly on the word itself, rather than a caret sitting near it. */
private fun DrawScope.drawPronunciationStrike(bounds: Rect, color: Color) {
    val y = (bounds.top + bounds.bottom) / 2f
    drawLine(
        color = color,
        start = Offset(bounds.left, y),
        end = Offset(bounds.right, y),
        strokeWidth = PronunciationStrikeWidth.toPx(),
        cap = StrokeCap.Round,
    )
}

/**
 * One rectangle per visual line the token's char range crosses, rather than
 * `layout.getPathForRange(start, end).getBounds()`'s single axis-aligned box - for a
 * word-broken word (or, in principle, any span crossing a line wrap), that single bounding
 * box would stretch from the first line's fragment all the way to the second's, covering
 * most of both lines' full width instead of just the two line-fragments the word actually
 * occupies.
 */
private fun wordLineBounds(layout: TextLayoutResult, token: WordToken): List<Rect> {
    if (token.end <= token.start) return emptyList()
    val startLine = layout.getLineForOffset(token.start)
    val endLine = layout.getLineForOffset(token.end - 1)
    return (startLine..endLine).mapNotNull { line ->
        // Never call getHorizontalPosition() exactly at a line-wrap boundary offset - Android's
        // text layout resolves that position ambiguously (as the *start* of the next line
        // rather than the end of this one), which is what collapsed a wrapped word's first-line
        // highlight into a sliver at the wrong edge: getHorizontalPosition(lineEnd) was
        // returning a small x near the line's start, and min()/max() then built a box from
        // there to the word's real start instead of from the word's start to the line's end.
        // Any edge that isn't the word's own start/end uses the line's own left/right extent
        // instead, which is unambiguous.
        val left = if (line == startLine) {
            layout.getHorizontalPosition(token.start, usePrimaryDirection = true)
        } else {
            layout.getLineLeft(line)
        }
        val right = if (line == endLine) {
            layout.getHorizontalPosition(token.end, usePrimaryDirection = true)
        } else {
            layout.getLineRight(line)
        }
        if (left >= right) return@mapNotNull null
        Rect(
            left = left,
            top = layout.getLineTop(line),
            right = right,
            bottom = layout.getLineBottom(line),
        )
    }
}

@Composable
private fun PlaybackBar(
    isPlaying: Boolean,
    playbackSpeed: Float,
    chapterProgress: Float,
    bufferedAheadEnd: Float,
    chapterElapsedSeconds: Double,
    chapterRemainingSeconds: Double,
    chapterTotalSeconds: Double,
    wordsPerMinute: Int?,
    bookRemainingSeconds: Double,
    bookRemainingPercent: Double,
    finished: Boolean,
    onPlayPause: () -> Unit,
    onSpeedClick: () -> Unit,
    onChapterInfoClick: () -> Unit,
    onBookmarksClick: () -> Unit,
    onSearchClick: () -> Unit,
    sleepTimerActive: Boolean,
    onSleepTimerClick: () -> Unit,
    annotationsMode: Boolean,
    onToggleAnnotationsMode: () -> Unit,
) {
    Column(modifier = Modifier.fillMaxWidth().background(MaterialTheme.colorScheme.surfaceContainer)) {
        // Progress through the whole chapter (paragraphs completed, plus how far into the
        // current one) - not just the currently-playing clip - mirroring frontend's
        // PlayerBar.tsx chapterProgress, plus a lighter "buffered ahead" segment showing how
        // far playback could run right now without waiting on generation.
        ChapterProgressBar(
            progress = chapterProgress,
            bufferedAheadEnd = bufferedAheadEnd,
            modifier = Modifier.fillMaxWidth(),
        )
        // A Box with explicit start/center/end alignment, not a Row + SpaceBetween - the
        // latter only *looks* centered when the flanking labels are equal width, which
        // "0:20" and "-0:29 · 0:49" generally aren't.
        Box(modifier = Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 2.dp)) {
            Text(
                formatTime(chapterElapsedSeconds),
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.align(Alignment.CenterStart),
            )
            if (!finished) {
                Text(
                    buildString {
                        append("${formatDurationLong(bookRemainingSeconds)} · ${bookRemainingPercent.roundToInt()}%")
                        if (wordsPerMinute != null) append(" · $wordsPerMinute wpm")
                    },
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.align(Alignment.Center),
                )
            }
            Text(
                buildString {
                    if (!finished) append("-${formatTime(chapterRemainingSeconds)} · ")
                    append(formatTime(chapterTotalSeconds))
                },
                style = MaterialTheme.typography.labelSmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.align(Alignment.CenterEnd),
            )
        }
        // A Box, not a Row + weight(1f) flanks - that only actually centers Play/Pause when both
        // sides happen to measure the same width, which stopped being true the moment the left
        // and right icon groups became different sizes. Aligning Play/Pause to Alignment.Center
        // directly guarantees it sits at the row's true midpoint regardless of how many icons
        // end up on either side of it.
        Box(
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 16.dp, vertical = 8.dp),
        ) {
            FilledIconButton(onClick = onPlayPause, modifier = Modifier.align(Alignment.Center)) {
                Icon(
                    if (isPlaying) Icons.Filled.Pause else Icons.Filled.PlayArrow,
                    contentDescription = if (isPlaying) "Pause" else "Play",
                )
            }
            // Annotations/Sleep/Bookmarks on the left, sized down from IconButton's own 48dp
            // default touch target so all three plus Play/Pause's own footprint leave clear
            // room in the middle.
            Row(
                modifier = Modifier.align(Alignment.CenterStart),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                IconButton(onClick = onToggleAnnotationsMode, modifier = Modifier.size(SecondaryIconButtonSize)) {
                    Icon(
                        Icons.Filled.Palette,
                        contentDescription = if (annotationsMode) "Turn off annotations mode" else "Turn on annotations mode",
                        tint = if (annotationsMode) MaterialTheme.colorScheme.primary else LocalContentColor.current,
                    )
                }
                IconButton(onClick = onSleepTimerClick, modifier = Modifier.size(SecondaryIconButtonSize)) {
                    Icon(
                        Icons.Filled.Bedtime,
                        contentDescription = "Sleep timer",
                        tint = if (sleepTimerActive) MaterialTheme.colorScheme.primary else LocalContentColor.current,
                    )
                }
                IconButton(onClick = onBookmarksClick, modifier = Modifier.size(SecondaryIconButtonSize)) {
                    Icon(Icons.Filled.Bookmarks, contentDescription = "Bookmarks")
                }
            }
            // Search/chapter selector/speed selector on the right.
            Row(
                modifier = Modifier.align(Alignment.CenterEnd),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                IconButton(onClick = onSearchClick, modifier = Modifier.size(SecondaryIconButtonSize)) {
                    Icon(Icons.Filled.Search, contentDescription = "Search this book")
                }
                IconButton(onClick = onChapterInfoClick, modifier = Modifier.size(SecondaryIconButtonSize)) {
                    Icon(Icons.Filled.FormatListNumbered, contentDescription = "Jump to chapter")
                }
                // Constrained to the same footprint as the other secondary icons in this row
                // (SecondaryIconButtonSize), not TextButton's own wider default minimum size/
                // padding - tight content padding so "1.5×" still fits without clipping.
                Box(modifier = Modifier.size(SecondaryIconButtonSize), contentAlignment = Alignment.Center) {
                    TextButton(
                        onClick = onSpeedClick,
                        contentPadding = PaddingValues(horizontal = 2.dp),
                        modifier = Modifier.fillMaxSize(),
                    ) {
                        Text("${formatSpeed(playbackSpeed)}×", style = MaterialTheme.typography.labelMedium)
                    }
                }
            }
        }
    }
}

// PlaybackBar's own secondary icon buttons (annotations/sleep/bookmarks/search/chapter selector)
// - smaller than IconButton's own 48dp default touch target, see its call sites' own comment.
private val SecondaryIconButtonSize = 40.dp

/**
 * The playback-speed picker - a [ModalBottomSheet] like [ChapterPickerSheet]/[VoicePickerSheet]
 * rather than a small anchored popup, so it pops up over the whole screen the same way those do.
 * See [CleanSlider]'s own doc comment for why the slider itself isn't just a stock Material3
 * `Slider` at [PLAYBACK_SPEED_STEPS]'s 17 steps.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun SpeedPickerSheet(playbackSpeed: Float, onSpeedChange: (Float) -> Unit, onDismiss: () -> Unit) {
    ModalBottomSheet(onDismissRequest = onDismiss) {
        Text(
            "Speed (${formatSpeed(playbackSpeed)}×)",
            style = MaterialTheme.typography.titleMedium,
            modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
        )
        CleanSlider(
            value = playbackSpeed,
            onValueChange = onSpeedChange,
            valueRange = PLAYBACK_SPEED_RANGE,
            steps = PLAYBACK_SPEED_STEPS,
            startLabel = "${formatSpeed(PLAYBACK_SPEED_MIN)}×",
            endLabel = "${formatSpeed(PLAYBACK_SPEED_MAX)}×",
            modifier = Modifier
                .fillMaxWidth()
                .padding(horizontal = 16.dp)
                .padding(bottom = 24.dp),
        )
    }
}

private fun formatSpeed(speed: Float): String =
    if (speed == speed.toInt().toFloat()) speed.toInt().toString() else speed.toString()

/** Mirrors frontend/src/components/PlayerBar.tsx's own wordCount. */
private fun wordCount(text: String): Int {
    val trimmed = text.trim()
    return if (trimmed.isEmpty()) 0 else trimmed.split(Regex("\\s+")).size
}

// Turns a raw Higgs inline delivery tag ("<|emotion:anger|>", "<|style:whispering|>") into a
// short human-readable label ("Anger", "Whispering") - mirrors frontend's ReaderPage.tsx
// formatDirectionTag exactly, including its fallback: the exact <|category:value|> syntax
// matters to audio.cpp's tokenizer (see backend speakerattr.validDirectionTags), not to a
// reader, so this is purely a display concern - falls back to the raw tag string unchanged if
// it doesn't match the expected shape, rather than hiding it.
private val DIRECTION_TAG_REGEX = Regex("^<\\|\\w+:(\\w+)\\|>$")

private fun formatDirectionTag(tag: String): String {
    val match = DIRECTION_TAG_REGEX.find(tag) ?: return tag
    val value = match.groupValues[1].replace('_', ' ')
    return value.replaceFirstChar { it.uppercaseChar() }
}

/** Mirrors frontend's PronunciationCaret hover tooltip text exactly. */
private fun formatPronunciationMark(mark: PronunciationMarkDto): String =
    "\"${mark.original}\" read as \"${mark.replacement}\""

// A delivery tag's own category ("emotion", "style", "prosody", "sfx") for per-category caret
// coloring - mirrors frontend's utils/directionTags.ts directionTagCategory exactly.
private enum class DirectionTagCategory { EMOTION, STYLE, PROSODY, SFX, OTHER }

private val DIRECTION_TAG_CATEGORY_REGEX = Regex("^<\\|(\\w+):")

private fun directionTagCategory(tag: String): DirectionTagCategory =
    when (DIRECTION_TAG_CATEGORY_REGEX.find(tag)?.groupValues?.get(1)) {
        "emotion" -> DirectionTagCategory.EMOTION
        "style" -> DirectionTagCategory.STYLE
        "prosody" -> DirectionTagCategory.PROSODY
        "sfx" -> DirectionTagCategory.SFX
        else -> DirectionTagCategory.OTHER
    }

/** [otherColor] backs "other" - a display-only fallback for a tag [directionTagCategory] doesn't
 *  recognize, never something this app's own LLM passes actually produce, so it has no real
 *  brand color of its own (mirrors frontend's own .direction-caret-other, which reuses
 *  --annotation-narrator for the same reason). */
private fun directionTagColor(tag: String, isDarkTheme: Boolean, otherColor: Color): Color =
    when (directionTagCategory(tag)) {
        DirectionTagCategory.EMOTION -> if (isDarkTheme) DirectionEmotionColorDark else DirectionEmotionColorLight
        DirectionTagCategory.STYLE -> if (isDarkTheme) DirectionStyleColorDark else DirectionStyleColorLight
        DirectionTagCategory.PROSODY -> if (isDarkTheme) DirectionProsodyColorDark else DirectionProsodyColorLight
        DirectionTagCategory.SFX -> if (isDarkTheme) DirectionSfxColorDark else DirectionSfxColorLight
        DirectionTagCategory.OTHER -> otherColor
    }

// Both long-press menu leading-icon composables below sit in a box of this same size, centered,
// so a colored-swatch row (speaker/description) and a caret-icon row (delivery tags) line up
// identically regardless of which one a given entry uses - matches DropdownMenuItem's own
// default leading-icon slot width.
private val AnnotationMenuIconSize = 24.dp

/** A small colored square identifying which annotation color a long-press menu entry refers to
 *  (speaker/description) - the menu's own analogue of the inline underline it's describing. */
@Composable
private fun AnnotationSwatch(color: Color) {
    Box(modifier = Modifier.size(AnnotationMenuIconSize), contentAlignment = Alignment.Center) {
        Box(
            modifier = Modifier
                .size(16.dp)
                .clip(RoundedCornerShape(3.dp))
                .background(color),
        )
    }
}

/** The direction-tag counterpart to [AnnotationSwatch] - same footprint, an up-arrow caret icon
 *  (mirroring frontend's RiArrowDropUpLine) tinted per tag category instead of a plain swatch. */
@Composable
private fun AnnotationCaretIcon(color: Color) {
    Box(modifier = Modifier.size(AnnotationMenuIconSize), contentAlignment = Alignment.Center) {
        Icon(Icons.Filled.KeyboardArrowUp, contentDescription = null, tint = color)
    }
}

/** The pronunciation-mark counterpart to [AnnotationCaretIcon] - same footprint, a strikethrough
 *  glyph matching the inline mark's own strikeout shape (see drawPronunciationStrike). */
@Composable
private fun PronunciationStrikeIcon(color: Color) {
    Box(modifier = Modifier.size(AnnotationMenuIconSize), contentAlignment = Alignment.Center) {
        Icon(Icons.Filled.FormatStrikethrough, contentDescription = null, tint = color)
    }
}

// Which of the three broad narration roles a paragraph segment plays in annotations mode -
// mirrors frontend's ReaderPage.tsx annotationKind exactly, computed client-side from
// isQuote/describesCharacters rather than sent by the backend as its own field. isQuote (not
// speaker being set) is what marks a segment as dialogue - an unattributed quote still has
// speaker == "" but is dialogue all the same.
private enum class AnnotationKind { NARRATOR, SPEAKER, DESCRIPTION }

// A scare-quoted segment is structurally still IsQuote, but isn't actually spoken aloud (see
// backend store.Paragraph.ScareQuote) - it colors as plain narrator text, same as frontend's own
// annotationKind (utils/annotations.ts). The dashed scare-quote underline (see
// drawScareQuoteUnderline) is what still marks it as a flagged quote, layered on top of that
// narrator color rather than replacing it with the speaker one.
private fun annotationKind(p: ParagraphDto): AnnotationKind = when {
    p.isQuote && !p.scareQuote -> AnnotationKind.SPEAKER
    p.describesCharacters.isNotEmpty() -> AnnotationKind.DESCRIPTION
    else -> AnnotationKind.NARRATOR
}

/**
 * Mirrors frontend's PlayerBar.tsx `.player-track`: a played-progress fill over a lighter
 * "buffered ahead" segment showing the run of already-generated audio ahead of [progress], so
 * the bar communicates both "how far you've listened" and "how far the narrator has caught up."
 * Material3's `LinearProgressIndicator` has no second/buffered track, hence drawing this
 * directly - two start-aligned boxes rather than an offset segment: painting the (wider)
 * buffered layer first and the (narrower) played fill on top of it gives the same visual result
 * as positioning a separate buffered-only segment, with simpler width math.
 */
@Composable
private fun ChapterProgressBar(progress: Float, bufferedAheadEnd: Float, modifier: Modifier = Modifier) {
    Box(
        modifier = modifier
            .height(4.dp)
            .background(MaterialTheme.colorScheme.surfaceVariant),
    ) {
        Box(
            modifier = Modifier
                .fillMaxHeight()
                .fillMaxWidth(bufferedAheadEnd.coerceIn(0f, 1f))
                .background(MaterialTheme.colorScheme.primary.copy(alpha = 0.35f)),
        )
        Box(
            modifier = Modifier
                .fillMaxHeight()
                .fillMaxWidth(progress.coerceIn(0f, 1f))
                .background(MaterialTheme.colorScheme.primary),
        )
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun VoicePickerSheet(
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
 *  sheet opens rather than cached, since the roster can grow between visits (a name typed here
 *  that isn't attributed to anything yet still isn't offered - unlike the web page, there's no
 *  free-text "type a new name" entry here, only reassignment to an already-known character). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun SpeakerPickerSheet(
    onDismiss: () -> Unit,
    fetchSpeakers: ((List<SpeakerDto>) -> Unit) -> Unit,
    currentSpeaker: String,
    // Closest-occurrence-first - see nearbySpeakersFor. Hoisted ahead of the rest of the roster
    // below, under its own section header, so long as there's actually a "rest" to distinguish it
    // from; with none nearby this just falls back to the flat full-roster list it always was.
    nearbySpeakers: List<String>,
    onSelect: (name: String) -> Unit,
) {
    var speakers by remember { mutableStateOf<List<SpeakerDto>>(emptyList()) }
    var loaded by remember { mutableStateOf(false) }
    LaunchedEffect(Unit) { fetchSpeakers { speakers = it; loaded = true } }

    val nearby = remember(speakers, nearbySpeakers) {
        val roster = speakers.map { it.name }.filter { it.isNotEmpty() && it != "Narrator" }.toSet()
        nearbySpeakers.filter { it in roster }
    }
    val rest = remember(speakers, nearby) {
        speakers.map { it.name }.filter { it.isNotEmpty() && it != "Narrator" && it !in nearby }.distinct()
    }

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
                if (nearby.isNotEmpty()) {
                    item { VoiceSectionHeader("Nearby") }
                    items(nearby, key = { "nearby-$it" }) { name -> SpeakerRow(name, currentSpeaker, onSelect) }
                    item { VoiceSectionHeader("All characters") }
                }
                items(rest, key = { "rest-$it" }) { name -> SpeakerRow(name, currentSpeaker, onSelect) }
            }
        }
    }
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

/** [SpeakerPickerSheet]'s own counterpart for the "Describes: X" long-press entry - moves the
 *  targeted segment(s) off [fromName]'s own describes-list and onto a different character's
 *  instead, mirroring the web Speakers page's `DescriptionRow`/frontend ReaderPage's own
 *  annotations-view "Reassign description" context menu (see ReaderViewModel
 *  .reassignDescription). "Remove" always leads the list (clears the description entirely,
 *  describing no one - same as [SpeakerPickerSheet]'s "Narrator" entry clearing attribution),
 *  followed by every other known character except [fromName] itself. Reuses the same roster
 *  fetch [SpeakerPickerSheet] does - descriptions and dialogue share one character roster. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun DescriptionPickerSheet(
    onDismiss: () -> Unit,
    fetchSpeakers: ((List<SpeakerDto>) -> Unit) -> Unit,
    fromName: String,
    onSelect: (targetName: String) -> Unit,
) {
    var speakers by remember { mutableStateOf<List<SpeakerDto>>(emptyList()) }
    var loaded by remember { mutableStateOf(false) }
    LaunchedEffect(Unit) { fetchSpeakers { speakers = it; loaded = true } }

    val targets = remember(speakers, fromName) {
        speakers.map { it.name }.filter { it.isNotEmpty() && it != "Narrator" && it != fromName }.distinct()
    }

    ModalBottomSheet(onDismissRequest = onDismiss) {
        LazyColumn(modifier = Modifier.padding(bottom = 24.dp)) {
            item {
                Text(
                    "Reassign description of \"$fromName\"",
                    style = MaterialTheme.typography.titleMedium,
                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp),
                )
            }
            if (!loaded) {
                item { CircularProgressIndicator(modifier = Modifier.padding(24.dp)) }
            } else {
                item { DescriptionTargetRow("Remove", onSelect = { onSelect("") }) }
                items(targets, key = { it }) { name -> DescriptionTargetRow(name, onSelect = { onSelect(name) }) }
            }
        }
    }
}

@Composable
private fun DescriptionTargetRow(label: String, onSelect: () -> Unit) {
    Row(
        verticalAlignment = Alignment.CenterVertically,
        modifier = Modifier
            .fillMaxWidth()
            .clickable(onClick = onSelect)
            .padding(horizontal = 16.dp, vertical = 12.dp),
    ) {
        Text(label, style = MaterialTheme.typography.bodyLarge)
    }
}

/** "Jump to chapter" - the Android analogue of frontend's PlayerBar.tsx chapter <select>, opened
 *  by tapping the chapter title/index in the playback bar instead of a persistent dropdown. */
@OptIn(ExperimentalMaterial3Api::class, ExperimentalFoundationApi::class)
@Composable
private fun ChapterPickerSheet(
    chapters: List<ChapterSummaryDto>,
    currentChapterIdx: Int,
    durationFor: (ChapterSummaryDto) -> String,
    downloadStates: Map<Int, ChapterDownloadState>,
    onDismiss: () -> Unit,
    onSelect: (idx: Int) -> Unit,
    onDownload: (idx: Int) -> Unit,
    onLongPressDownload: (idx: Int) -> Unit,
    onGenerateChapter: (idx: Int) -> Unit,
    onAttributeChapter: (idx: Int) -> Unit,
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
                            if (chapter.paragraphCount > 0) {
                                Text(
                                    durationFor(chapter),
                                    style = MaterialTheme.typography.labelSmall,
                                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                                )
                            }
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
                        DropdownMenuItem(
                            text = { Text("Attribute & tag chapter") },
                            leadingIcon = { Icon(Icons.Filled.RecordVoiceOver, contentDescription = null) },
                            onClick = { onAttributeChapter(chapter.idx); showChapterMenu = false },
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

/** The Android analogue of frontend's BookmarksPanel.tsx - reviewing, jumping to, or removing
 *  bookmarked paragraphs (bookmarking itself happens via a paragraph's long-press menu, not here). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun BookmarksSheet(
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
 *  book's paragraphs, jumping playback to whichever result is tapped. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun SearchSheet(
    search: (query: String, onResult: (List<SearchResultDto>) -> Unit) -> Unit,
    onDismiss: () -> Unit,
    onJump: (chapterIdx: Int, paragraphIdx: Int) -> Unit,
) {
    var input by remember { mutableStateOf("") }
    var results by remember { mutableStateOf<List<SearchResultDto>>(emptyList()) }
    var searching by remember { mutableStateOf(false) }
    val showResults = input.trim().length > 1

    // Debounced the same 300ms as BookSearch.tsx, so typing doesn't fire a request per keystroke.
    LaunchedEffect(input) {
        if (!showResults) {
            results = emptyList()
            return@LaunchedEffect
        }
        delay(300)
        searching = true
        search(input) { results = it; searching = false }
    }

    ModalBottomSheet(onDismissRequest = onDismiss) {
        Column(modifier = Modifier.padding(horizontal = 16.dp, vertical = 8.dp)) {
            Text("Search this book", style = MaterialTheme.typography.titleMedium, modifier = Modifier.padding(bottom = 8.dp))
            TextField(
                value = input,
                onValueChange = { input = it },
                placeholder = { Text("Search this book…") },
                singleLine = true,
                modifier = Modifier.fillMaxWidth(),
            )
        }
        if (showResults) {
            when {
                searching -> CircularProgressIndicator(modifier = Modifier.padding(24.dp))
                results.isEmpty() -> Text(
                    "No matches.",
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
private fun SleepTimerSheet(
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
