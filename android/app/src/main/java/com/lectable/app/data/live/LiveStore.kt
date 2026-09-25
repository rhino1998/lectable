package com.lectable.app.data.live

import com.lectable.app.data.remote.LiveClient
import com.lectable.app.data.remote.LiveTopic
import com.lectable.app.data.remote.LiveTopics
import com.lectable.app.data.remote.LiveResult
import com.lectable.app.data.remote.dto.BookDetailDto
import com.lectable.app.data.remote.dto.BookSummaryDto
import com.lectable.app.data.remote.dto.BookmarkDto
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ChapterMusicDto
import com.lectable.app.data.remote.dto.CustomVoicePresetDto
import com.lectable.app.data.remote.dto.JobsSnapshotDto
import com.lectable.app.data.remote.dto.SpeakerAppearanceDto
import com.lectable.app.data.remote.dto.SpeakerDto
import com.lectable.app.data.remote.dto.VoicePresetsDto
import com.lectable.app.data.remote.dto.VoiceSettingsDto
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.Flow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingCommand
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.combine
import kotlinx.coroutines.flow.distinctUntilChanged
import kotlinx.coroutines.flow.onEach
import kotlinx.coroutines.flow.runningFold
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.flow.transformLatest

// How long a topic's upstream subscription outlives its last reader - long enough to ride out a
// configuration change or quick back-and-forth navigation.
private const val LINGER_MS = 5_000L

// Most non-pinned topic instances kept in memory at once (least recently requested dropped
// first) - one per chapter a reader has ever scrolled through would otherwise grow unbounded.
// Dropping one only forgets its last-known value; anyone still collecting it keeps working.
private const val MAX_CACHED_TOPICS = 64

/**
 * The app's single store of server state: one shared [StateFlow] per topic instance (a book, a
 * chapter, the job queue, ...), fed by [LiveClient]'s pushes. Every screen (and
 * DownloadRepository) reads from here, so each push is decoded once and lands everywhere at
 * the same moment, and a screen opening onto something already held shows it instantly.
 *
 * - **Last-known values survive.** A reconnect, or a topic restarting after its readers went
 *   away, keeps showing the previous value (with `loading`/`error` set) until the fresh snapshot
 *   replaces it, instead of flashing back to empty. A server-address change (see
 *   [LiveResult.reset]) is the exception - that data belongs to another backend.
 * - **Library-wide topics are eager.** While the app is in the foreground ([setForeground],
 *   driven by MainActivity), [books] and the three voice lists stay subscribed whether or not a
 *   screen shows them. Per-book/per-chapter topics and the job queue are live only while
 *   something reads them - each subscription costs the backend a rebuild on every relevant
 *   write, and during generation that's a write every couple of seconds.
 * - **Pushes write through to the offline cache** - see [OfflineCacheWriter]: the book list
 *   refreshes downloaded books' metadata, and any chapter value that passes through here
 *   refreshes that chapter's downloaded copy.
 */
@Singleton
class LiveStore @Inject constructor(
    private val liveClient: LiveClient,
    private val offlineCache: OfflineCacheWriter,
) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val foreground = MutableStateFlow(false)
    private val lock = Any()
    private val pinned = mutableMapOf<String, StateFlow<LiveResult<*>>>()
    private val cached = object : LinkedHashMap<String, StateFlow<LiveResult<*>>>(16, 0.75f, true) {
        override fun removeEldestEntry(eldest: MutableMap.MutableEntry<String, StateFlow<LiveResult<*>>>) = size > MAX_CACHED_TOPICS
    }

    /** Called from MainActivity's onStart/onStop - see the class doc's "eager" point. */
    fun setForeground(value: Boolean) {
        foreground.value = value
    }

    fun books(): StateFlow<LiveResult<List<BookSummaryDto>>> =
        topic(LiveTopics.books(), eager = true) { offlineCache.onBooks(it) }

    fun voicePresets(): StateFlow<LiveResult<VoicePresetsDto>> = topic(LiveTopics.voicePresets(), eager = true)

    fun customVoicePresets(): StateFlow<LiveResult<List<CustomVoicePresetDto>>> = topic(LiveTopics.customVoicePresets(), eager = true)

    fun defaultVoice(): StateFlow<LiveResult<VoiceSettingsDto>> = topic(LiveTopics.defaultVoice(), eager = true)

    fun jobs(): StateFlow<LiveResult<JobsSnapshotDto>> = topic(LiveTopics.jobs())

    fun book(bookId: String): StateFlow<LiveResult<BookDetailDto>> = topic(LiveTopics.book(bookId))

    fun voice(bookId: String): StateFlow<LiveResult<VoiceSettingsDto>> = topic(LiveTopics.voice(bookId))

    fun speakers(bookId: String): StateFlow<LiveResult<List<SpeakerDto>>> = topic(LiveTopics.speakers(bookId))

    fun bookmarks(bookId: String): StateFlow<LiveResult<List<BookmarkDto>>> = topic(LiveTopics.bookmarks(bookId))

    fun chapter(bookId: String, chapterIdx: Int): StateFlow<LiveResult<ChapterDetailDto>> =
        topic(LiveTopics.chapter(bookId, chapterIdx)) { offlineCache.onChapter(bookId, it) }

    fun chapterMusic(bookId: String, chapterIdx: Int): StateFlow<LiveResult<ChapterMusicDto>> =
        topic(LiveTopics.chapterMusic(bookId, chapterIdx))

    fun characterAppearances(bookId: String, characterId: String): StateFlow<LiveResult<List<SpeakerAppearanceDto>>> =
        topic(LiveTopics.characterAppearances(bookId, characterId))

    fun characterDescriptions(bookId: String, characterId: String): StateFlow<LiveResult<List<SpeakerAppearanceDto>>> =
        topic(LiveTopics.characterDescriptions(bookId, characterId))

    /** One topic instance's shared flow - [LiveTopics] (generated from the backend's topic table)
     *  fixes its wire name, params, and value type together. */
    @Suppress("UNCHECKED_CAST")
    private fun <T> topic(
        t: LiveTopic<T>,
        eager: Boolean = false,
        onValue: (suspend (T) -> Unit)? = null,
    ): StateFlow<LiveResult<T>> {
        val key = "${t.name}:${t.params.toSortedMap()}"
        synchronized(lock) {
            (pinned[key] ?: cached[key])?.let { return it as StateFlow<LiveResult<T>> }
            val flow = build(t.name, t.params, eager, t.deserializer, onValue)
            if (eager) pinned[key] = flow else cached[key] = flow
            return flow
        }
    }

    private fun <T> build(
        name: String,
        params: Map<String, Any>,
        eager: Boolean,
        deserializer: kotlinx.serialization.DeserializationStrategy<T>,
        onValue: (suspend (T) -> Unit)?,
    ): StateFlow<LiveResult<T>> =
        liveClient.observe(name, params, deserializer)
            .onEach { r -> r.data?.let { d -> onValue?.let { runCatching { it(d) } } } }
            .keepLastKnown()
            .stateIn(scope, if (eager) whileSubscribedOrForeground else whileSubscribed, LiveResult())

    /** Carries the previous value's data across a loading/error emission that has none (a
     *  topic restarting, or a reconnect before its fresh snapshot) - except on [LiveResult.reset]. */
    private fun <T> Flow<LiveResult<T>>.keepLastKnown(): Flow<LiveResult<T>> =
        runningFold(LiveResult<T>()) { prev, next ->
            if (next.data == null && !next.reset && prev.data != null) next.copy(data = prev.data) else next
        }

    private val whileSubscribed: SharingStarted = SharingStarted.WhileSubscribed(stopTimeoutMillis = LINGER_MS)

    private val whileSubscribedOrForeground = object : SharingStarted {
        override fun command(subscriptionCount: StateFlow<Int>): Flow<SharingCommand> =
            combine(subscriptionCount, foreground) { n, fg -> n > 0 || fg }
                .distinctUntilChanged()
                .transformLatest { active ->
                    if (active) {
                        emit(SharingCommand.START)
                    } else {
                        delay(LINGER_MS)
                        emit(SharingCommand.STOP)
                    }
                }
    }
}
