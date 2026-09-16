package com.lectable.app.data.remote.dto

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

/** Curated built-in preset, proxied as-is from tts-service - see tts-service/voices.py. */
@Serializable
data class VoicePresetDto(
    val id: String,
    val name: String,
    val instruct: String,
    val seed: Int,
    @SerialName("ref_text") val refText: String,
    @SerialName("speed_multiplier") val speedMultiplier: Double,
    val audioUrl: String,
)

@Serializable
data class VoicePresetsDto(
    val default: String,
    val presets: List<VoicePresetDto>,
)

// backend's store.CharacterVoiceMode values this DTO's characterVoiceMode field actually sends/
// receives - Android's own UI only ever toggles between the first two (a plain on/off switch,
// not the web's full four-way dropdown - see android/CLAUDE.md's own scaffold-limitations
// entry), but a book whose characterVoiceMode was set to one of the other two via the web UI
// still round-trips it correctly (VoiceSettingsDto.multiVoice below just reads it as "on").
const val CHARACTER_VOICE_MODE_NARRATOR = "narrator"
const val CHARACTER_VOICE_MODE_ASSIGNED = "assigned"

@Serializable
data class VoiceSettingsDto(
    val presetId: String,
    val instruct: String,
    val language: String,
    val seed: Int? = null,
    // Which of backend's four store.CharacterVoiceMode values this book uses for per-character
    // narration voices (see internal/narration.Resolver) - CHARACTER_VOICE_MODE_NARRATOR by
    // default, matching backend's own store.DefaultCharacterVoiceMode. This DTO is a full
    // replace on PUT (see backend's handleUpdateVoice), so every call site that builds one must
    // carry this field's own already-fetched value forward (via .copy(), not a fresh literal) -
    // an earlier version of this DTO had a `multiVoice: Boolean` field instead, which doesn't
    // exist on the wire at all any more; any call that built a bare VoiceSettingsDto(...) with
    // that field silently reset this book's real characterVoiceMode back to "narrator" server-
    // side on every such PUT (see ReaderViewModel.setVoicePreset's own fix for this exact bug).
    // Character roster/voice assignment lives in ui/speakers/SpeakersScreen.kt, which has its
    // own copy of this same on/off toggle (SpeakerViewModel.toggleMultiVoice).
    val characterVoiceMode: String = CHARACTER_VOICE_MODE_NARRATOR,
    // Opts this book into waiting for speech-direction tagging before generating its audio - see
    // backend's voiceSettingsDTO.SpeechDirection. Off by default; no Android UI toggles this yet
    // (tagging itself IS reachable, via the reader's own chapter-picker long-press menu - see
    // ReaderViewModel.attributeChapter) - carried here purely so a PUT from this app doesn't
    // silently clobber it if the web UI already turned it on for this book.
    val speechDirection: Boolean = false,
    // Reader-facing background-music toggle - book-wide (unlike a chapter's own scoring
    // progress, see ChapterMusicDto.scored), since scoring/generating a whole book's worth of
    // chapters at once isn't something turning this on should eagerly trigger - see backend
    // store.Book.MusicEnabled's own doc comment. Off by default. See BackgroundMusicPlayer.
    val musicEnabled: Boolean = false,
)

@Serializable
data class LanguagesResponseDto(val languages: List<String>)

/** User-created, reusable narrator voice - distinct from [VoicePresetDto]; see types.ts. */
@Serializable
data class CustomVoicePresetDto(
    val id: String,
    val name: String,
    val instruct: String,
    val refText: String,
    val seed: Int,
    val speedMultiplier: Double,
    val cloneModel: String,
    val createdAt: Long,
    val audioUrl: String,
    val refError: String? = null,
)

@Serializable
data class CustomVoicePresetInputDto(
    val name: String,
    val instruct: String,
    val refText: String,
    val seed: Int? = null,
    val speedMultiplier: Double? = null,
)

@Serializable
data class TestVoiceRequestDto(val text: String)

/** Previews an instruct directly (no preset saved yet) - see httpapi.handleTestVoiceDesign.
 *  Seed is pinned by the caller (not left for tts-service to pick randomly) only when it wants
 *  a just-created preset's save to reuse this exact rendered clip instead of paying for a
 *  second render - see backend's voicerefs.DesignConfigHash. */
@Serializable
data class TestVoiceDesignRequestDto(val instruct: String, val text: String, val seed: Int? = null)
