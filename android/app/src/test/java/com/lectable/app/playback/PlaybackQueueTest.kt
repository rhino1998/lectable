package com.lectable.app.playback

import com.lectable.app.data.remote.dto.AudioStatus
import com.lectable.app.data.remote.dto.AudioStatuses
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ParagraphDto
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class PlaybackQueueTest {

    private fun ready(idx: Int, url: String = "/audio/$idx", duration: Double = 10.0, pointer: Double = 0.0) =
        ParagraphDto(idx = idx, audioStatus = AudioStatuses.READY, audioUrl = url, durationSeconds = duration, audioPointerSeconds = pointer)

    private fun notReady(idx: Int, status: AudioStatus = AudioStatuses.PENDING) = ParagraphDto(idx = idx, audioStatus = status)

    private fun chapter(idx: Int, vararg paragraphs: ParagraphDto) = ChapterDetailDto(idx = idx, paragraphs = paragraphs.toList())

    private fun chapters(vararg chapters: ChapterDetailDto) = chapters.associateBy { it.idx }

    /** Paragraphs 1-3 are one scare-quote merge group sharing paragraph 1's clip. */
    private val withGroup = chapter(
        0,
        ready(0),
        ready(1, url = "/audio/1", duration = 2.0),
        ready(2, url = "/audio/1", duration = 0.5, pointer = 2.0),
        ready(3, url = "/audio/1", duration = 3.0, pointer = 2.5),
        ready(4),
    )

    // playAction

    @Test
    fun `a chapter that isn't loaded is asked for`() {
        assertEquals(PlayAction.NEED_CHAPTER, playAction(emptyMap(), 2 to 0, null, null, false))
    }

    @Test
    fun `a paragraph past the end of its chapter is asked for`() {
        assertEquals(PlayAction.NEED_CHAPTER, playAction(chapters(chapter(0, ready(0))), 0 to 1, null, null, false))
    }

    @Test
    fun `a paragraph without audio yet parks`() {
        val loaded = chapters(chapter(0, ready(0), notReady(1), notReady(2, AudioStatuses.ERROR)))
        assertEquals(PlayAction.PARK, playAction(loaded, 0 to 1, 0 to 0, "/audio/0", true))
        assertEquals(PlayAction.PARK, playAction(loaded, 0 to 2, 0 to 0, "/audio/0", true))
    }

    @Test
    fun `a ready status without a url parks`() {
        val loaded = chapters(chapter(0, ParagraphDto(idx = 0, audioStatus = AudioStatuses.READY)))
        assertEquals(PlayAction.PARK, playAction(loaded, 0 to 0, null, null, false))
    }

    @Test
    fun `the loaded paragraph is only sought within`() {
        val loaded = chapters(chapter(0, ready(0), ready(1)))
        assertEquals(PlayAction.SEEK, playAction(loaded, 0 to 1, 0 to 1, "/audio/1", true))
    }

    @Test
    fun `a parked target that became ready is loaded, not sought`() {
        // The UI already shows the parked paragraph as current, but the player still holds the
        // clip that was playing before it.
        val loaded = chapters(chapter(0, ready(0), ready(1)))
        assertEquals(PlayAction.LOAD, playAction(loaded, 0 to 1, 0 to 0, "/audio/0", true))
    }

    @Test
    fun `another member of the loaded merge group is sought within the shared clip`() {
        assertEquals(PlayAction.SEEK_IN_GROUP, playAction(chapters(withGroup), 0 to 3, 0 to 1, "/audio/1", true))
    }

    @Test
    fun `a later paragraph on the same shared clip is loaded, not sought`() {
        // Paragraphs 0 and 2 say the same line, so they play the same clip - but 2 isn't part
        // of a merge group with 0, and what's queued behind 0 isn't what follows 2.
        val repeated = chapters(chapter(0, ready(0, url = "/clips/k"), ready(1), ready(2, url = "/clips/k")))
        assertEquals(PlayAction.LOAD, playAction(repeated, 0 to 2, 0 to 0, "/clips/k", true))
    }

    @Test
    fun `nothing is sought once the player has dropped its clip`() {
        val loaded = chapters(withGroup)
        assertEquals(PlayAction.LOAD, playAction(loaded, 0 to 1, 0 to 1, "/audio/1", false))
        assertEquals(PlayAction.LOAD, playAction(loaded, 0 to 3, 0 to 1, "/audio/1", false))
    }

    // seekTargetSeconds

    @Test
    fun `a merge group member starts at its own offset in the shared clip`() {
        assertEquals(2.5, seekTargetSeconds(withGroup.paragraphs[3], 0.0), 0.0)
        assertEquals(0.0, seekTargetSeconds(withGroup.paragraphs[0], 0.0), 0.0)
    }

    @Test
    fun `an explicit position wins over the paragraph's own start`() {
        assertEquals(4.2, seekTargetSeconds(withGroup.paragraphs[3], 4.2), 0.0)
    }

    // clipStartAfter

    @Test
    fun `the next clip is the next paragraph`() {
        assertEquals(0 to 1, clipStartAfter(chapters(chapter(0, ready(0), ready(1))), 0 to 0, "/audio/0"))
    }

    @Test
    fun `the next clip skips the rest of a merge group`() {
        val loaded = chapters(withGroup)
        assertEquals(0 to 4, clipStartAfter(loaded, 0 to 1, "/audio/1"))
        assertEquals(0 to 4, clipStartAfter(loaded, 0 to 2, "/audio/1"))
    }

    @Test
    fun `the next clip crosses into a loaded next chapter`() {
        val loaded = chapters(chapter(0, ready(0)), chapter(1, ready(0, url = "/audio/c1p0")))
        assertEquals(1 to 0, clipStartAfter(loaded, 0 to 0, "/audio/0"))
    }

    @Test
    fun `an empty chapter in between is passed over`() {
        val loaded = chapters(chapter(0, ready(0)), chapter(1), chapter(2, ready(0, url = "/audio/c2p0")))
        assertEquals(2 to 0, clipStartAfter(loaded, 0 to 0, "/audio/0"))
    }

    @Test
    fun `the next clip steps over front and back matter`() {
        val loaded = chapters(
            chapter(0, ready(0)),
            chapter(1, ready(0)).copy(matter = true),
            chapter(2, ready(0)).copy(matter = true),
            chapter(3, ready(0, url = "/audio/story")),
        )
        assertEquals(3 to 0, clipStartAfter(loaded, 0 to 0, "/audio/0"))
        assertEquals(3, nextStoryChapter(loaded, 0))
    }

    @Test
    fun `a matter chapter the reader opened is read through`() {
        val loaded = chapters(chapter(0, ready(0, url = "/a"), ready(1, url = "/b")).copy(matter = true), chapter(1, ready(0, url = "/c")))
        assertEquals(0 to 1, clipStartAfter(loaded, 0 to 0, "/a"))
        assertEquals(1 to 0, clipStartAfter(loaded, 0 to 1, "/b"))
    }

    @Test
    fun `whether an unloaded chapter is matter isn't known yet`() {
        val loaded = chapters(chapter(0, ready(0)), chapter(1, ready(0)).copy(matter = true))
        assertEquals(2, nextStoryChapter(loaded, 0))
        assertNull(clipStartAfter(loaded, 0 to 0, "/audio/0"))
    }

    @Test
    fun `there is no next clip past the last loaded chapter`() {
        assertNull(clipStartAfter(chapters(chapter(0, ready(0))), 0 to 0, "/audio/0"))
    }

    @Test
    fun `there is no next clip while the next paragraph isn't ready`() {
        assertNull(clipStartAfter(chapters(chapter(0, ready(0), notReady(1), ready(2))), 0 to 0, "/audio/0"))
    }

    @Test
    fun `a clip that would start mid-group isn't queued`() {
        // Paragraph 1 points into a clip other than the one playing, at an offset: queueing it
        // would start that clip from its beginning.
        val loaded = chapters(chapter(0, ready(0), ready(1, url = "/audio/other", pointer = 1.5)))
        assertNull(clipStartAfter(loaded, 0 to 0, "/audio/0"))
    }

    // clipsToQueue

    @Test
    fun `nothing is queued without a loaded clip`() {
        val loaded = chapters(chapter(0, ready(0), ready(1)))
        assertEquals(emptyList<Pair<Int, Int>>(), clipsToQueue(loaded, null, null))
        assertEquals(emptyList<Pair<Int, Int>>(), clipsToQueue(loaded, 0 to 0, null))
    }

    @Test
    fun `clips are queued until enough audio is lined up`() {
        val loaded = chapters(chapter(0, *Array(8) { ready(it, duration = 10.0) }))
        // 30 seconds ahead of paragraph 0 is three ten-second clips.
        assertEquals(listOf(0 to 1, 0 to 2, 0 to 3), clipsToQueue(loaded, 0 to 0, "/audio/0"))
    }

    @Test
    fun `short clips are capped by count`() {
        val loaded = chapters(chapter(0, *Array(40) { ready(it, duration = 0.2) }))
        val queued = clipsToQueue(loaded, 0 to 0, "/audio/0")
        assertEquals(MAX_QUEUED_CLIPS, queued.size)
        assertEquals(0 to MAX_QUEUED_CLIPS, queued.last())
    }

    @Test
    fun `clips without a duration still stop at the cap`() {
        val loaded = chapters(chapter(0, *Array(40) { ParagraphDto(idx = it, audioStatus = AudioStatuses.READY, audioUrl = "/audio/$it") }))
        assertEquals(MAX_QUEUED_CLIPS, clipsToQueue(loaded, 0 to 0, "/audio/0").size)
    }

    @Test
    fun `queueing stops at the first paragraph that isn't ready`() {
        val loaded = chapters(chapter(0, ready(0, duration = 1.0), ready(1, duration = 1.0), notReady(2), ready(3)))
        assertEquals(listOf(0 to 1), clipsToQueue(loaded, 0 to 0, "/audio/0"))
    }

    @Test
    fun `a merge group is queued once, as its anchor`() {
        val queued = clipsToQueue(chapters(withGroup), 0 to 0, "/audio/0", aheadSeconds = 100.0)
        assertEquals(listOf(0 to 1, 0 to 4), queued)
    }

    @Test
    fun `the queue runs on into the next chapter`() {
        val loaded = chapters(
            chapter(0, ready(0), ready(1)),
            chapter(1, ready(0, url = "/audio/c1p0"), ready(1, url = "/audio/c1p1")),
        )
        assertEquals(listOf(0 to 1, 1 to 0, 1 to 1), clipsToQueue(loaded, 0 to 0, "/audio/0"))
    }

    // clip media ids

    @Test
    fun `a media id round-trips, colons in the url included`() {
        val url = "http://10.0.0.2:8080/api/paragraphs/p1/audio?v=3"
        assertEquals(QueuedClip(12, 7, url), parseClipMediaId(clipMediaId(12, 7, url)))
        assertEquals(QueuedClip(0, 0, "file:///data/00000.wav"), parseClipMediaId(clipMediaId(0, 0, "file:///data/00000.wav")))
    }

    @Test
    fun `an id that isn't a clip's is rejected`() {
        assertNull(parseClipMediaId(""))
        assertNull(parseClipMediaId("chapter:0:/audio/0"))
        assertNull(parseClipMediaId("3"))
    }

    @Test
    fun `a queued clip is current only while the paragraph still has that audio`() {
        val clip = QueuedClip(0, 1, "/audio/1")
        assertTrue(clip.isCurrentFor(ready(1)))
        assertFalse(clip.isCurrentFor(ready(1, url = "/audio/1?v=2")))
        assertFalse(clip.isCurrentFor(notReady(1)))
        assertFalse(clip.isCurrentFor(null))
    }

    // merge group boundaries

    /** Walks [reachedNextMergeMember] the way ParagraphPlayer does. */
    private fun paragraphAt(chapter: ChapterDetailDto, from: Int, positionMs: Long): Int {
        var idx = from
        while (reachedNextMergeMember(chapter, idx, positionMs)) idx++
        return idx
    }

    @Test
    fun `the current paragraph follows playback through a merge group`() {
        assertEquals(1, paragraphAt(withGroup, 1, 1_999))
        assertEquals(2, paragraphAt(withGroup, 1, 2_000))
        assertEquals(2, paragraphAt(withGroup, 2, 2_499))
        assertEquals(3, paragraphAt(withGroup, 2, 2_500))
    }

    @Test
    fun `several short members are caught up in one go`() {
        assertEquals(3, paragraphAt(withGroup, 1, 5_500))
    }

    @Test
    fun `a merge group never runs on into the next clip`() {
        assertEquals(3, paragraphAt(withGroup, 3, 60_000))
        assertEquals(0, paragraphAt(withGroup, 0, 60_000))
    }

    @Test
    fun `a member that isn't ready ends the walk`() {
        val broken = chapter(0, ready(0, url = "/a"), notReady(1), ready(2, url = "/a", pointer = 1.0))
        assertEquals(0, paragraphAt(broken, 0, 5_000))
        assertFalse(reachedNextMergeMember(broken, 1, 5_000))
        assertFalse(reachedNextMergeMember(broken, 9, 5_000))
    }

    // chapter-relative position

    @Test
    fun `chapter duration sums its paragraphs, unknown ones as zero`() {
        assertEquals(25_500, chapterDurationMs(withGroup))
        assertEquals(10_000, chapterDurationMs(chapter(0, ready(0), notReady(1))))
    }

    @Test
    fun `chapter position adds the paragraphs already played`() {
        assertEquals(4_000, chapterPositionMs(withGroup, 0, 4_000))
        assertEquals(26_000, chapterPositionMs(chapter(0, ready(0), ready(1), ready(2)), 2, 6_000))
    }

    @Test
    fun `chapter position doesn't count a merge member's offset twice`() {
        // Paragraph 3 starts 2.5s into the shared clip; 3s into that clip is 0.5s into it, after
        // paragraphs 0-2 (10 + 2 + 0.5 seconds).
        assertEquals(13_000, chapterPositionMs(withGroup, 3, 3_000))
        // Just after a seek the reported position can still be short of the member's start.
        assertEquals(12_500, chapterPositionMs(withGroup, 3, 1_000))
    }

    @Test
    fun `chapter position before any paragraph is the clip's own`() {
        assertEquals(700, chapterPositionMs(withGroup, -1, 700))
    }

    // speed ramp

    @Test
    fun `a ramp moves half a step of speed per ramp second`() {
        assertEquals(0.1f, speedRampStep(rampSeconds = 1f, tickMs = 200), 1e-6f)
        assertEquals(0.05f, speedRampStep(rampSeconds = 2f, tickMs = 200), 1e-6f)
    }

    @Test
    fun `a ramp steps toward its target and lands on it exactly`() {
        assertEquals(1.1f, rampedSpeed(applied = 1f, target = 2f, step = 0.1f), 1e-6f)
        assertEquals(2.9f, rampedSpeed(applied = 3f, target = 1f, step = 0.1f), 1e-6f)
        assertEquals(2f, rampedSpeed(applied = 1.95f, target = 2f, step = 0.1f), 0f)
        assertEquals(2f, rampedSpeed(applied = 2f, target = 2f, step = 0.1f), 0f)
    }

    @Test
    fun `a ramp from one speed to another takes the configured time`() {
        val step = speedRampStep(rampSeconds = 1f, tickMs = 200)
        var speed = 1f
        var ticks = 0
        while (speed != 2f) {
            speed = rampedSpeed(speed, 2f, step)
            ticks++
        }
        // 1x -> 2x is two half-steps of speed: 2 seconds, ten 200ms ticks (give or take one for
        // float rounding).
        assertTrue("took $ticks ticks", ticks in 9..11)
    }

    @Test
    fun `ramping switched off jumps straight to the target`() {
        val step = speedRampStep(rampSeconds = 0f, tickMs = 200)
        assertEquals(10f, rampedSpeed(applied = 1f, target = 10f, step = step), 0f)
    }
}
