package com.lectable.app.playback

import android.content.Context
import android.content.Intent
import android.net.Uri
import androidx.annotation.OptIn
import androidx.core.content.ContextCompat
import androidx.media3.common.AudioAttributes
import androidx.media3.common.C
import androidx.media3.common.ForwardingPlayer
import androidx.media3.common.MediaItem
import androidx.media3.common.MediaMetadata
import androidx.media3.common.PlaybackParameters
import androidx.media3.common.Player
import androidx.media3.common.util.UnstableApi
import androidx.media3.exoplayer.ExoPlayer
import androidx.media3.session.MediaSession
import com.lectable.app.data.remote.MediaUrlResolver
import com.lectable.app.data.remote.dto.AudioStatuses
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.repository.DownloadRepository
import com.lectable.app.data.settings.PlaybackSettingsRepository
import dagger.hilt.android.qualifiers.ApplicationContext
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.flow.map
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class PlaybackState(
    val chapterIdx: Int = -1,
    val paragraphIdx: Int = -1,
    val isPlaying: Boolean = false,
    val positionSeconds: Double = 0.0,
    val playbackSpeed: Float = 1f,
)

/** The Android analogue of frontend/src/hooks/useSleepTimer.ts's `SleepTimerOption` union. */
sealed class SleepTimerOption {
    object Off : SleepTimerOption()
    data class Countdown(val minutes: Int) : SleepTimerOption()
    object EndOfChapter : SleepTimerOption()
}

data class SleepTimerState(
    val option: SleepTimerOption = SleepTimerOption.Off,
    val remainingSeconds: Int = 0,
)

/**
 * Owns one ExoPlayer instance and advances through a book's paragraphs one
 * `.wav` at a time - the Android analogue of frontend/src/hooks/usePlayback.ts.
 *
 * Like that hook, this is keyed by (chapterIdx, paragraphIdx) pairs looked
 * up in a `Map<Int, ChapterDetailDto>` of currently-loaded chapters, not a
 * flat index, so a chapter the ViewModel loads out of order never shifts
 * anything mid-playback. When the next paragraph's chapter isn't loaded yet
 * (or its audio isn't ready yet), playback parks on [pendingTarget] and
 * [onNeedChapter] fires; call [setChapter] again once that chapter's data
 * (re-)arrives (e.g. from the 1.5s poll while paragraphs are generating) to
 * resume automatically.
 *
 * App-scoped ([Singleton]), not tied to the Reader screen's ViewModel -
 * playback (and the [mediaSession] exposing it to the system) needs to
 * survive leaving/backgrounding the reader, so [PlaybackService] can keep
 * running and ReaderViewModel.onCleared() must NOT release this. A new
 * ReaderViewModel simply overwrites [onNeedChapter]/[onPositionUpdate] with
 * its own callbacks on init, same as before.
 */
// UnstableApi: ForwardingPlayer, ExoPlayer.Builder's seek-increment setters.
@OptIn(UnstableApi::class)
@Singleton
class ParagraphPlayer @Inject constructor(
    @ApplicationContext private val context: Context,
    private val mediaUrlResolver: MediaUrlResolver,
    private val playbackSettingsRepository: PlaybackSettingsRepository,
    private val downloadRepository: DownloadRepository,
) {
    private val player = ExoPlayer.Builder(context)
        .setAudioAttributes(
            AudioAttributes.Builder()
                .setUsage(C.USAGE_MEDIA)
                .setContentType(C.AUDIO_CONTENT_TYPE_SPEECH)
                .build(),
            // Ducks/pauses for other apps' audio and vice versa - handled by ExoPlayer
            // via the attributes above rather than manually, standard for a narration player.
            true,
        )
        // Auto-pauses on unplug/disconnect (headphones, Bluetooth) - the same "system audio
        // signal" spirit as respecting play/pause from a headset button.
        .setHandleAudioBecomingNoisy(true)
        // Playback now needs to keep decoding/streaming with the screen off and the app
        // backgrounded (that's the point of this class being a foreground-service-backed
        // singleton) - without a wake lock, network reads for the next paragraph's audio
        // can stall once the device sleeps.
        .setWakeMode(C.WAKE_MODE_NETWORK)
        // Gives the notification/lock-screen media control rewind-30/forward-30 buttons
        // (Media3's default notification picks the icon matching this exact increment) -
        // in-app playback has no equivalent control, just word/paragraph tapping, so this is
        // notification-only.
        .setSeekBackIncrementMs(30_000)
        .setSeekForwardIncrementMs(30_000)
        .build()

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private val chapters = mutableMapOf<Int, ChapterDetailDto>()
    private var pendingTarget: Pair<Int, Int>? = null
    // The (chapterIdx, paragraphIdx) whose audio is actually loaded into [player] right now -
    // distinct from _state's chapterIdx/paragraphIdx, which also gets set to a target that's
    // merely parked/pending (see the audioStatus != READY branch below) so the UI can show
    // where the listener is waiting. Only this one may gate the "already loaded, just seek"
    // fast path - using _state there instead let a park-then-resume (playParagraph() called
    // again for the same target once its audio actually becomes ready) mistake "the UI already
    // says we're on this paragraph" for "the player already has this paragraph's clip loaded"
    // and just seek/resume whatever was previously loaded rather than loading the new clip.
    private var loadedTarget: Pair<Int, Int>? = null
    // The raw (backend-relative) audioUrl of whatever's actually loaded into [player] right now -
    // lets a scare-quote merge group's other members (see ParagraphDto.audioPointerSeconds)
    // recognize "this exact same shared clip is already loaded" purely by comparing DTO fields,
    // without needing to compare resolved Uris (which differ between a streamed URL and a local
    // downloaded file, even when the underlying content is identical). Used only to skip an
    // unnecessary reload/restart when [playParagraph] is called explicitly for a different
    // member of an already-loaded group (e.g. tapping directly on it) - ordinary continuous
    // playback through a group's own internal boundaries never calls playParagraph at all, see
    // [advancePastMergeGroupBoundaries].
    private var loadedAudioUrl: String? = null
    private var coverArtUri: Uri? = null
    // A StateFlow (not a plain var, unlike bookAuthor) specifically so PlaybackWidget can
    // observe it directly for its own book-title display, without needing to reach into
    // ParagraphPlayer's chapters map the way NotificationPlayer does.
    private val _bookTitle = MutableStateFlow<String?>(null)
    val bookTitle: StateFlow<String?> = _bookTitle
    private var bookAuthor: String? = null
    private var bookId: String? = null
    // Set whenever the book's current voice is known/changes (see ReaderViewModel) - a
    // downloaded chapter is only used for playback if it matches this exactly, since changing
    // voice invalidates cached audio the same way it does server-side (see root CLAUDE.md).
    private var voiceKey: String? = null

    var onNeedChapter: ((Int) -> Unit)? = null
    var onPositionUpdate: ((chapterIdx: Int, paragraphIdx: Int, seconds: Double) -> Unit)? = null

    private val _state = MutableStateFlow(PlaybackState())
    val state: StateFlow<PlaybackState> = _state

    private var sleepTimerJob: Job? = null
    private val _sleepTimer = MutableStateFlow(SleepTimerState())
    val sleepTimer: StateFlow<SleepTimerState> = _sleepTimer

    /** Exposed for [PlaybackService] to return from `onGetSession()` - this is what makes
     *  system play/pause (notification, lock screen, headset/Bluetooth buttons) reach
     *  [player]; Media3's session wraps it and forwards transport commands directly. Wrapped
     *  in [NotificationPlayer] both to hide skip-previous/-next (there's no "track" to skip
     *  to, just paragraphs advancing automatically) and to report chapter-relative position/
     *  duration instead of the current paragraph clip's own. Declared after [chapters]/[_state]
     *  deliberately - `MediaSession.Builder(...).build()` synchronously calls
     *  `NotificationPlayer.getDuration()` during construction (to seed its initial metadata),
     *  and Kotlin runs property initializers in declaration order, so building this any
     *  earlier crashed with a NullPointerException on that not-yet-initialized state. */
    val mediaSession: MediaSession = MediaSession.Builder(context, NotificationPlayer(player)).build()

    init {
        player.addListener(object : Player.Listener {
            override fun onPlaybackStateChanged(playbackState: Int) {
                if (playbackState == Player.STATE_ENDED) advanceToNext()
            }

            override fun onMediaItemTransition(mediaItem: MediaItem?, reason: Int) {
                if (reason == Player.MEDIA_ITEM_TRANSITION_REASON_AUTO && mediaItem != null) onQueuedClipStarted(mediaItem)
            }

            override fun onIsPlayingChanged(isPlaying: Boolean) {
                _state.update { it.copy(isPlaying = isPlaying) }
                // Promotes PlaybackService to a foreground service the moment audio actually
                // starts, rather than keeping it running unconditionally from app start - a
                // service that's merely declared/injectable isn't "started" on its own.
                if (isPlaying) {
                    ContextCompat.startForegroundService(context, Intent(context, PlaybackService::class.java))
                }
            }
        })
        scope.launch {
            while (true) {
                delay(1000)
                if (player.isPlaying) {
                    val cur = _state.value
                    val seconds = player.currentPosition / 1000.0
                    _state.update { it.copy(positionSeconds = seconds) }
                    if (cur.chapterIdx >= 0) onPositionUpdate?.invoke(cur.chapterIdx, cur.paragraphIdx, seconds)
                }
            }
        }
        // Tighter interval than the position-report loop above, dedicated to noticing a
        // scare-quote merge group's own internal boundaries (see advancePastMergeGroupBoundaries)
        // - a merge-group member can be as short as a single word, easily missed at a 1s
        // granularity (which would leave the wrong paragraph highlighted/reported as "current"
        // for up to a second at a time while several short members play through).
        scope.launch {
            while (true) {
                delay(120)
                if (player.isPlaying) advancePastMergeGroupBoundaries()
            }
        }
        // Restores whatever speed the user last picked, same DataStore-backed pattern as
        // theme/reading preferences - a brief default-1x flash before this first collection
        // lands is harmless, unlike the server URL's synchronous preload requirement.
        scope.launch {
            playbackSettingsRepository.speed.collect { speed -> applyPlaybackSpeed(speed) }
        }
    }

    /** Call once the book's own detail loads - the notification/lock-screen media control
     *  identifies the book being narrated (title/author/cover), not the current chapter,
     *  same as any audiobook app; none of this lives on [ChapterDetailDto]. [bookId] is kept
     *  separately from the DTOs passed to [setChapter] purely so [playParagraph] can check
     *  [DownloadRepository] for a local copy before resolving a streaming URL. */
    fun setBookInfo(bookId: String, title: String, author: String?, coverUrl: String?) {
        this.bookId = bookId
        _bookTitle.value = title
        bookAuthor = author
        coverArtUri = mediaUrlResolver.resolve(coverUrl)?.let(Uri::parse)
    }

    /** Call whenever the book's current voice is known or changes - see [voiceKey]. */
    fun setVoiceKey(voiceKey: String) {
        this.voiceKey = voiceKey
    }

    /** Call whenever a chapter's data loads or refreshes (e.g. from generation polling). */
    fun setChapter(chapter: ChapterDetailDto) {
        chapters[chapter.idx] = chapter
        val target = pendingTarget
        if (target != null && target.first == chapter.idx) {
            playParagraph(target.first, target.second)
        } else if (loadedTarget != null) {
            queueNextClip()
        }
    }

    fun clearChapters() {
        chapters.clear()
        pendingTarget = null
        loadedTarget = null
        loadedAudioUrl = null
        // A clip queued behind the current one belongs to the data just dropped.
        if (player.mediaItemCount > player.currentMediaItemIndex + 1) {
            player.removeMediaItems(player.currentMediaItemIndex + 1, player.mediaItemCount)
        }
    }

    fun playFrom(chapterIdx: Int, paragraphIdx: Int, startSeconds: Double = 0.0) {
        playParagraph(chapterIdx, paragraphIdx, startSeconds)
    }

    fun togglePlayPause() {
        if (player.isPlaying) player.pause() else player.play()
    }

    fun pause() = player.pause()

    /** Applies immediately, persists across paragraph/chapter transitions (ExoPlayer's
     *  playback speed is a player-level setting, not per-media-item), and saves it via
     *  [PlaybackSettingsRepository] so it's restored on the next app launch too. */
    fun setPlaybackSpeed(speed: Float) {
        applyPlaybackSpeed(speed)
        scope.launch { playbackSettingsRepository.setSpeed(speed) }
    }

    private fun applyPlaybackSpeed(speed: Float) {
        player.playbackParameters = PlaybackParameters(speed)
        _state.update { it.copy(playbackSpeed = speed) }
    }

    /**
     * The Android analogue of frontend/src/hooks/useSleepTimer.ts: either a real-wall-clock
     * countdown (ticks regardless of play/pause, same as e.g. Audible's) that pauses once and
     * resets to [SleepTimerOption.Off], or [SleepTimerOption.EndOfChapter], which instead pauses
     * the moment the chapter changes away from whatever it was when that option was picked.
     * Lives here rather than in ReaderViewModel so it keeps running (and can still pause
     * playback) if the reader locks the screen or backgrounds the app after arming it - the
     * whole point of a sleep timer is falling asleep with the app backgrounded.
     */
    fun setSleepTimer(option: SleepTimerOption) {
        sleepTimerJob?.cancel()
        when (option) {
            is SleepTimerOption.Off -> {
                _sleepTimer.value = SleepTimerState()
            }
            is SleepTimerOption.Countdown -> {
                val totalSeconds = option.minutes * 60
                _sleepTimer.value = SleepTimerState(option, totalSeconds)
                sleepTimerJob = scope.launch {
                    var remaining = totalSeconds
                    while (remaining > 0) {
                        delay(1000)
                        remaining--
                        _sleepTimer.update { it.copy(remainingSeconds = remaining) }
                    }
                    pause()
                    _sleepTimer.value = SleepTimerState()
                }
            }
            is SleepTimerOption.EndOfChapter -> {
                val startChapterIdx = _state.value.chapterIdx
                _sleepTimer.value = SleepTimerState(option, 0)
                sleepTimerJob = scope.launch {
                    _state.map { it.chapterIdx }.first { it != startChapterIdx && it >= 0 }
                    pause()
                    _sleepTimer.value = SleepTimerState()
                }
            }
        }
    }

    /** Raw player position/duration for high-frequency polling (e.g. word-highlight
     *  tracking) - deliberately not throttled like [state]'s positionSeconds. */
    fun positionMs(): Long = player.currentPosition

    fun durationMs(): Long = player.duration.takeIf { it > 0 } ?: 0L

    private fun playParagraph(chapterIdx: Int, paragraphIdx: Int, startSeconds: Double = 0.0) {
        val chapter = chapters[chapterIdx]
        val paragraph = chapter?.paragraphs?.getOrNull(paragraphIdx)
        if (chapter == null || paragraph == null) {
            pendingTarget = chapterIdx to paragraphIdx
            onNeedChapter?.invoke(chapterIdx)
            return
        }
        if (paragraph.audioStatus != AudioStatuses.READY || paragraph.audioUrl == null) {
            // Not generated yet - park here; setChapter() re-attempts once fresh data reports
            // this paragraph ready. [chapters] is this app-scoped singleton's own cache, decoupled
            // from whichever ReaderViewModel is currently alive: a chapter can sit in here stale
            // (e.g. this exact paragraph was regenerated, or simply never refreshed, under an
            // earlier ReaderViewModel instance that has since been cleared) without the *current*
            // one's own loadedChapters map - and therefore its poll loop/socket updates - ever
            // covering it. So this always asks (same as the chapter==null branch above) rather
            // than assuming some poll/socket is already watching this chapterIdx - onNeedChapter's
            // handler does a live refetch and re-pushes the result via setChapter, at which point
            // this chapterIdx joins that ViewModel's own loadedChapters and starts getting the
            // usual poll/socket coverage on its own. Also still moves the reported "current"
            // position here (rather than leaving it wherever playback last actually was) so the UI
            // shows where the listener selected/is waiting, and so the lookahead-on-position-change
            // collector in ReaderViewModel requests generation starting from *this* paragraph
            // instead of only from wherever audio was last genuinely playing - the Android
            // equivalent of an explicit "generate this" request, reusing the same mechanism
            // that already keeps generation running ahead of normal playback.
            pendingTarget = chapterIdx to paragraphIdx
            player.pause()
            _state.update { it.copy(chapterIdx = chapterIdx, paragraphIdx = paragraphIdx) }
            onNeedChapter?.invoke(chapterIdx)
            return
        }
        pendingTarget = null

        // Every "seconds" value tied to a paragraph (this one included) is an absolute position
        // within *that paragraph's own* audioUrl, never paragraph-relative - correct without any
        // special-casing for an ordinary paragraph (audioPointerSeconds 0, so absolute == 0-based)
        // and exactly the right seek target for a scare-quote merge group's own non-anchor member
        // (see ParagraphDto.audioPointerSeconds's own doc comment). startSeconds > 0 means "seek
        // to this specific point" (e.g. a word tap, which already reports an absolute time under
        // that same convention - see WordHighlight.kt's wordStartTimes); otherwise this paragraph's
        // own start offset within its clip is the right default landing point, not literal 0 (the
        // anchor's own start, which would restart the whole group from its very beginning).
        val seekTarget = if (startSeconds > 0) startSeconds else paragraph.audioPointerSeconds

        if (loadedTarget == chapterIdx to paragraphIdx && player.mediaItemCount > 0) {
            // Already the loaded clip (e.g. tapping another word within the paragraph
            // that's already playing) - just seek rather than reloading it from scratch.
            player.seekTo((seekTarget * 1000).toLong())
            player.playWhenReady = true
            return
        }

        if (loadedAudioUrl != null && loadedAudioUrl == paragraph.audioUrl && player.mediaItemCount > 0) {
            // A different member of the same already-loaded scare-quote merge group (see
            // loadedAudioUrl's own doc comment) - never reload/restart the shared clip, just
            // relocate playback to this paragraph's own start offset within it.
            player.seekTo((seekTarget * 1000).toLong())
            player.playWhenReady = true
            loadedTarget = chapterIdx to paragraphIdx
            _state.update { it.copy(chapterIdx = chapterIdx, paragraphIdx = paragraphIdx) }
            return
        }

        // Prefer a downloaded local copy over streaming, if one exists for the book's current
        // voice and still matches this paragraph's current audio - the whole point of offline downloads (see DownloadRepository). ExoPlayer
        // accepts a file:// Uri identically to an http:// one, so this is the only call site
        // that needs to know about local downloads at all.
        val mediaItem = mediaItemFor(chapterIdx, paragraphIdx) ?: return
        player.setMediaItem(mediaItem)
        player.playbackParameters = PlaybackParameters(_state.value.playbackSpeed)
        player.prepare()
        if (seekTarget > 0) player.seekTo((seekTarget * 1000).toLong())
        player.playWhenReady = true
        loadedTarget = chapterIdx to paragraphIdx
        loadedAudioUrl = paragraph.audioUrl
        _state.update { it.copy(chapterIdx = chapterIdx, paragraphIdx = paragraphIdx) }
        queueNextClip()
    }

    /**
     * The player item for one paragraph's clip, or null if it has none to play. [MediaItem.mediaId]
     * records which paragraph and clip it is ("<chapterIdx>:<paragraphIdx>:<audioUrl>") so
     * [onQueuedClipStarted] can tell whether a clip queued ahead of time is still current.
     */
    private fun mediaItemFor(chapterIdx: Int, paragraphIdx: Int): MediaItem? {
        val chapter = chapters[chapterIdx] ?: return null
        val paragraph = chapter.paragraphs.getOrNull(paragraphIdx) ?: return null
        val audioUrl = paragraph.audioUrl ?: return null
        // Prefer a downloaded local copy over streaming, if one exists for the book's current
        // voice and still matches this paragraph's current audio - the whole point of offline downloads (see DownloadRepository). ExoPlayer
        // accepts a file:// Uri identically to an http:// one, so this is the only call site
        // that needs to know about local downloads at all.
        val localFile = bookId?.let { id -> voiceKey?.let { key -> downloadRepository.localAudioFile(id, chapterIdx, key, paragraph) } }
        val uri = localFile?.let(Uri::fromFile) ?: mediaUrlResolver.resolve(audioUrl)?.let(Uri::parse) ?: return null
        return MediaItem.Builder()
            .setUri(uri)
            .setMediaId("$chapterIdx:$paragraphIdx:$audioUrl")
            // Book title/author/cover, not the current chapter or paragraph text - that's
            // what identifies *what's playing* on a lock-screen/notification media control,
            // same as any audiobook app (chapter is more like a "track position" within it).
            .setMediaMetadata(
                MediaMetadata.Builder()
                    .setTitle(_bookTitle.value ?: chapter.title)
                    .setArtist(bookAuthor)
                    .setArtworkUri(coverArtUri)
                    .build(),
            )
            .build()
    }

    /**
     * The paragraph whose clip plays after the loaded one: the first paragraph past the loaded
     * clip's scare-quote merge group (whose members all share one clip), crossing into the next
     * chapter when that's already cached. Null when it isn't ready yet or doesn't start its own
     * clip - [advanceToNext] handles those the old way once the current clip ends.
     */
    private fun nextClipStart(): Pair<Int, Int>? {
        var (chapterIdx, paragraphIdx) = loadedTarget ?: return null
        val url = loadedAudioUrl ?: return null
        while (true) {
            val chapter = chapters[chapterIdx] ?: return null
            paragraphIdx++
            if (paragraphIdx >= chapter.paragraphs.size) {
                chapterIdx++
                paragraphIdx = -1
                continue
            }
            val p = chapter.paragraphs[paragraphIdx]
            if (p.audioStatus != AudioStatuses.READY || p.audioUrl == null) return null
            if (p.audioUrl == url) continue
            return if (p.audioPointerSeconds == 0.0) chapterIdx to paragraphIdx else null
        }
    }

    /**
     * Queues the next paragraph's clip behind the current one, so ExoPlayer opens and buffers it
     * while the current one plays and moves straight on at the boundary. Loading each clip only
     * once the previous one had ended left an audible gap at every paragraph - an Ogg Opus clip
     * takes several round trips to open (headers, then a seek to the end for its duration).
     * Idempotent: leaves an already-correct queue alone.
     */
    private fun queueNextClip() {
        if (player.mediaItemCount == 0) return
        val current = player.currentMediaItemIndex
        val wanted = nextClipStart()?.let { (c, p) -> mediaItemFor(c, p) }
        val queued = if (player.mediaItemCount > current + 1) player.getMediaItemAt(current + 1) else null
        if (queued?.mediaId == wanted?.mediaId) return
        if (queued != null) player.removeMediaItems(current + 1, player.mediaItemCount)
        if (wanted != null) player.addMediaItem(wanted)
    }

    /** The player moved on to a clip [queueNextClip] queued: make it the current paragraph. */
    private fun onQueuedClipStarted(item: MediaItem) {
        val parts = item.mediaId.split(":", limit = 3)
        val chapterIdx = parts.getOrNull(0)?.toIntOrNull() ?: return
        val paragraphIdx = parts.getOrNull(1)?.toIntOrNull() ?: return
        val audioUrl = parts.getOrNull(2)
        player.removeMediaItems(0, player.currentMediaItemIndex)
        val paragraph = chapters[chapterIdx]?.paragraphs?.getOrNull(paragraphIdx)
        if (paragraph == null || paragraph.audioStatus != AudioStatuses.READY || paragraph.audioUrl != audioUrl) {
            // Regenerated/changed since it was queued - load what's there now instead.
            loadedTarget = null
            loadedAudioUrl = null
            playParagraph(chapterIdx, paragraphIdx)
            return
        }
        loadedTarget = chapterIdx to paragraphIdx
        loadedAudioUrl = audioUrl
        _state.update { it.copy(chapterIdx = chapterIdx, paragraphIdx = paragraphIdx) }
        queueNextClip()
    }

    private fun advanceToNext() {
        // Belt-and-suspenders alongside the periodic poll driving
        // advancePastMergeGroupBoundaries() below: resolves the merge group's real last
        // member from the clip's own full duration, rather than assuming paragraphIdx
        // already got walked all the way there before STATE_ENDED fired - covers the
        // narrow race where STATE_ENDED is delivered before one last queued poll tick
        // would have caught the final boundary (e.g. the group's last member is shorter
        // than the poll interval, or the main thread is briefly busy right at clip end).
        if (player.duration != C.TIME_UNSET) advancePastMergeGroupBoundaries(player.duration)
        val cur = _state.value
        if (cur.chapterIdx < 0) return
        val chapter = chapters[cur.chapterIdx] ?: return
        val nextParagraphIdx = cur.paragraphIdx + 1
        if (nextParagraphIdx < chapter.paragraphs.size) {
            playParagraph(cur.chapterIdx, nextParagraphIdx)
        } else {
            playParagraph(cur.chapterIdx + 1, 0)
        }
    }

    /**
     * Advances the logical "current paragraph" through a scare-quote merge group's own shared
     * clip purely by comparing playback position against each next paragraph's own start offset
     * within that same file (audioPointerSeconds) - never touches [player] itself (no seek, no
     * reload, no pause): the underlying recording is one continuous, never-cut file, so there's
     * nothing to actually transition. A group's own real end is a genuinely different audioUrl
     * (or nothing at all, chapter end), handled by the ordinary [Player.STATE_ENDED]/
     * [advanceToNext] path, not this.
     *
     * Loops internally to fully catch up in one call (unlike frontend's usePlayback.ts, which
     * advances at most one paragraph per animation frame and relies on ~60fps polling to still
     * reliably catch a merge member too short to survive between two frames) - this runs far
     * less often (see its own call site's 120ms interval), so a single check per tick could
     * plausibly skip past more than one very short member in a row without ever marking it
     * "current"; looping here removes any dependency on the poll interval being fast enough.
     *
     * [atPositionMs] defaults to the live playback position for the periodic poll's own call
     * site; [advanceToNext] instead passes the clip's full duration, resolving every remaining
     * boundary at once as its own belt-and-suspenders check right as the clip actually ends -
     * see its call site's own comment.
     */
    private fun advancePastMergeGroupBoundaries(atPositionMs: Long = player.currentPosition) {
        while (true) {
            val cur = _state.value
            if (cur.chapterIdx < 0) return
            val chapter = chapters[cur.chapterIdx] ?: return
            val current = chapter.paragraphs.getOrNull(cur.paragraphIdx) ?: return
            if (current.audioStatus != AudioStatuses.READY || current.audioUrl == null) return
            val next = chapter.paragraphs.getOrNull(cur.paragraphIdx + 1) ?: return
            if (next.audioStatus != AudioStatuses.READY || next.audioUrl != current.audioUrl) return
            if (atPositionMs < (next.audioPointerSeconds * 1000).toLong()) return
            loadedTarget = cur.chapterIdx to (cur.paragraphIdx + 1)
            _state.update { it.copy(paragraphIdx = cur.paragraphIdx + 1) }
        }
    }

    /**
     * Hides skip-to-previous/-next from whatever's driven by [Player.getAvailableCommands] (the
     * notification/lock-screen transport controls) - this never uses ExoPlayer's own playlist/
     * queue (each paragraph is one single-item load, see [playParagraph]), so those commands
     * would otherwise show up as buttons that do nothing meaningful. Also reports
     * chapter-relative position/duration instead of ExoPlayer's own (per-paragraph-clip)
     * values via [getCurrentPosition]/[getDuration] - the notification/lock-screen scrubber
     * reads straight from those, and a bar that resets every 5-40 seconds as paragraphs change
     * would be far less useful than one spanning the whole chapter, same reasoning as
     * PlaybackBar's chapterProgress in ReaderScreen.kt (which this duplicates the math of, just
     * in milliseconds). An `inner class` so it can reach [chapters]/[_state] directly.
     */
    private inner class NotificationPlayer(player: Player) : ForwardingPlayer(player) {
        private val hiddenCommands = intArrayOf(
            Player.COMMAND_SEEK_TO_NEXT,
            Player.COMMAND_SEEK_TO_NEXT_MEDIA_ITEM,
            Player.COMMAND_SEEK_TO_PREVIOUS,
            Player.COMMAND_SEEK_TO_PREVIOUS_MEDIA_ITEM,
        )

        override fun getAvailableCommands(): Player.Commands {
            val builder = super.getAvailableCommands().buildUpon()
            hiddenCommands.forEach { builder.remove(it) }
            return builder.build()
        }

        override fun isCommandAvailable(command: Int): Boolean =
            command !in hiddenCommands && super.isCommandAvailable(command)

        override fun getDuration(): Long {
            val chapter = chapters[_state.value.chapterIdx] ?: return super.getDuration()
            val totalSeconds = chapter.paragraphs.sumOf { it.durationSeconds ?: 0.0 }
            return (totalSeconds * 1000).toLong()
        }

        override fun getCurrentPosition(): Long {
            val cur = _state.value
            val chapter = chapters[cur.chapterIdx] ?: return super.getCurrentPosition()
            val priorParagraphs = chapter.paragraphs.take(cur.paragraphIdx.coerceAtLeast(0))
            val elapsedBeforeSeconds = priorParagraphs.sumOf { it.durationSeconds ?: 0.0 }
            // super.getCurrentPosition() is raw/absolute within whatever clip is loaded - correct
            // as-is for an ordinary paragraph, but for a scare-quote merge group member it also
            // includes everything before this paragraph's own audioPointerSeconds within that
            // shared clip, which priorParagraphs' own durationSeconds sum already accounts for -
            // without subtracting it back out here, this member's own span would be double-counted.
            val pointerOffsetMs = ((chapter.paragraphs.getOrNull(cur.paragraphIdx)?.audioPointerSeconds ?: 0.0) * 1000).toLong()
            return (elapsedBeforeSeconds * 1000).toLong() + (super.getCurrentPosition() - pointerOffsetMs).coerceAtLeast(0)
        }
    }
}
