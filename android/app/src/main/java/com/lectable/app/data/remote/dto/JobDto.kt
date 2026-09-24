package com.lectable.app.data.remote.dto

import kotlinx.serialization.Serializable

/**
 * One task in the backend's shared job queue (see backend/internal/jobs.Kind/.QueueTask) -
 * voice-clone/design paragraph generation, a one-off design preview, a character's voice
 * provisioning, or a speaker-attribution/characterization run, all sorted through one
 * tier-ordered priority queue. Mirrors frontend/src/api/types.ts's QueueTask.
 *
 * [kind] is a plain String, not an enum, deliberately - backend's jobs.Kind has already grown
 * past what an early version of this DTO's own doc comment listed (voice_design_preview/
 * voice_provision were added later), and an unrecognized enum value would fail to deserialize
 * the whole snapshot outright; [kindLabel] falls back to the raw string for anything this app
 * doesn't have a friendly label for yet, so a newly-added Kind degrades gracefully instead of
 * breaking the whole Jobs screen.
 */
@Serializable
data class QueueTaskDto(
    val id: String,
    val kind: String,
    // Human-readable identifier for a kind that isn't chapter/paragraph-scoped - a character's
    // name for "speaker_characterization"/"voice_provision", a "preview #N" tag for
    // "voice_design_preview" - "" for every other kind, which already has a chapter/paragraph.
    val label: String? = null,
    val bookId: String,
    val bookTitle: String,
    // Meaningless (0/"") for "speaker_characterization"/"voice_provision"/"voice_design_preview",
    // none of which are chapter-scoped - see label above.
    val chapterIdx: Int,
    val chapterTitle: String,
    // Meaningless (0) for every kind that isn't a single generated paragraph.
    val paragraphIdx: Int,
    val tier: String,
    // "" for the two LLM kinds, and for a "voice_design" task presetId alone is "" (a pure
    // custom instruct with no preset backing it) - resolve against the built-in/custom preset
    // lists for a friendly name, falling back to instruct.
    val presetId: String,
    val instruct: String,
    // The emotion variant a clone task's line generates in ("sad", "whisper") - "" for neutral.
    val emotion: String = "",
)

@Serializable
data class JobsSnapshotDto(
    val inFlight: List<QueueTaskDto>,
    val queued: List<QueueTaskDto>,
    // Mirrors backend jobs.Manager.Paused() - see JobsViewModel.togglePause.
    val paused: Boolean = false,
)

@Serializable
data class CancelAllJobsResponseDto(val canceled: Int)

@Serializable
data class PausedResponseDto(val paused: Boolean)

@Serializable
data class RestartWorkerResponseDto(val restarted: Boolean)
