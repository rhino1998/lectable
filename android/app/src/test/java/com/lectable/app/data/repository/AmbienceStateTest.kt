package com.lectable.app.data.repository

import com.lectable.app.data.remote.dto.AudioStatus
import com.lectable.app.data.remote.dto.AudioStatuses
import com.lectable.app.data.remote.dto.ChapterMusicDto
import com.lectable.app.data.remote.dto.MusicRegionDto
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class AmbienceStateTest {

    private fun region(id: String, status: AudioStatus) = MusicRegionDto(id = id, status = status)

    private fun music(vararg statuses: AudioStatus, enabled: Boolean = true, scored: Boolean = true, lit: Boolean = true) = ChapterMusicDto(
        enabled = enabled,
        scored = scored,
        lit = lit,
        regions = statuses.mapIndexed { i, s -> region("r$i", s) },
    )

    @Test
    fun `download state counts ready and failed clips`() {
        val state = music(AudioStatuses.READY, AudioStatuses.ERROR, AudioStatuses.PENDING, AudioStatuses.READY).toAmbienceDownloadState()
        assertEquals(AmbienceDownloadState(scored = true, withClips = true, readyClips = 2, failedClips = 1, totalClips = 4), state)
        assertFalse(state.settled)
    }

    @Test
    fun `a chapter is only scored once it is lit too`() {
        assertFalse(music(AudioStatuses.READY, lit = false).toAmbienceDownloadState().scored)
        assertFalse(music(AudioStatuses.READY, scored = false).toAmbienceDownloadState().scored)
        assertFalse(music(AudioStatuses.READY, lit = false).toAmbienceDownloadState().settled)
    }

    @Test
    fun `a failed clip doesn't hold a chapter open`() {
        assertTrue(music(AudioStatuses.READY, AudioStatuses.ERROR).toAmbienceDownloadState().settled)
    }

    @Test
    fun `with music off, cues alone settle a chapter`() {
        val state = music(AudioStatuses.PENDING, AudioStatuses.PENDING, enabled = false).toAmbienceDownloadState()
        assertFalse(state.withClips)
        assertTrue(state.settled)
    }

    @Test
    fun `a scored chapter with no regions is settled`() {
        assertTrue(music().toAmbienceDownloadState().settled)
        assertTrue(music().ambienceSettled(withClips = true))
    }

    @Test
    fun `stored ambience is settled once every clip is in or failed`() {
        assertTrue(music(AudioStatuses.READY, AudioStatuses.ERROR).ambienceSettled(withClips = true))
        assertFalse(music(AudioStatuses.READY, AudioStatuses.PENDING).ambienceSettled(withClips = true))
        assertFalse(music(AudioStatuses.READY, AudioStatuses.GENERATING).ambienceSettled(withClips = true))
    }

    @Test
    fun `clips don't matter when they aren't wanted`() {
        assertTrue(music(AudioStatuses.PENDING).ambienceSettled(withClips = false))
    }

    @Test
    fun `unscored or unlit ambience is never settled`() {
        assertFalse(music(AudioStatuses.READY, scored = false).ambienceSettled(withClips = false))
        assertFalse(music(AudioStatuses.READY, lit = false).ambienceSettled(withClips = true))
    }

    @Test
    fun `voice keys differ when any part of the voice does`() {
        val key = voiceKeyOf("velvet-narrator", "Speak warmly.", "Auto")
        assertEquals(key, voiceKeyOf("velvet-narrator", "Speak warmly.", "Auto"))
        assertNotEquals(key, voiceKeyOf("other", "Speak warmly.", "Auto"))
        assertNotEquals(key, voiceKeyOf("velvet-narrator", "Speak coldly.", "Auto"))
        assertNotEquals(key, voiceKeyOf("velvet-narrator", "Speak warmly.", "English"))
    }
}
