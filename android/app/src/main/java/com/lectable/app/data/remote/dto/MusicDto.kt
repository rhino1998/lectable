package com.lectable.app.data.remote.dto

import kotlinx.serialization.Serializable

/**
 * One chapter tone region for background music, reshaped for playback - see backend
 * store.MusicRegion's own doc comment for the full generation pipeline (a book-wide,
 * LLM-scored/Stable-Audio-rendered mood track mixed in underneath narration) and
 * frontend/src/api/types.ts's own MusicRegion, which this mirrors field-for-field.
 */
@Serializable
data class MusicRegionDto(
    val id: String,
    val startIdx: Int,
    val endIdx: Int,
    val mood: String,
    // The actual Stable Audio generation prompt for this region - fuller than [mood] (a short
    // label for compact display); shown directly under it in the reader's annotations-mode
    // boundary marker - see backend musicRegionDTO.Prompt's own doc comment.
    val prompt: String,
    // "cut" or "continuation" - see backend store.MusicTransition. A "continuation" region was
    // generated with its first chunk seeded from the immediately preceding region's own tail. Only
    // affects generation - [BackgroundMusicPlayer] crossfades every region switch the same way.
    val transition: String,
    val status: AudioStatus,
    val error: String? = null,
    val durationSeconds: Double = 0.0,
    // Set only once [status] is READY - same "no URL until it means something" convention
    // [ParagraphDto.audioUrl] already uses.
    val audioUrl: String? = null,
)

/**
 * `enabled` mirrors [BookSummaryDto.musicEnabled]/[VoiceSettingsDto.musicEnabled] (the book-wide
 * toggle); `scored` mirrors this one chapter's own background-music scoring progress - both
 * duplicated here purely for convenience, same as frontend's ChapterMusic, so a caller holding
 * only this response still knows whether it's worth (re-)triggering scoring for this chapter
 * without also having the book's own chapter-summary list in scope.
 */
@Serializable
data class ChapterMusicDto(
    val enabled: Boolean,
    val scored: Boolean,
    val regions: List<MusicRegionDto> = emptyList(),
)
