package com.lectable.app.data.remote.dto

import kotlinx.serialization.Serializable

/** One row of a book's speaker table (Narrator + every attributed character, shared series-wide)
 *  - see backend's speakerRowDTO. Populates both the reader's "Set speaker" picker (see
 *  ReaderScreen.kt's SpeakerPickerSheet, which only needs [name]) and the full roster in
 *  ui/speakers/SpeakersScreen.kt, which uses every field. */
@Serializable
data class SpeakerDto(
    val id: String = "",
    val name: String,
    val voicePresetId: String? = null,
    val summary: String? = null,
    // summary's paired reference passage - what this character's auto-created voice preset's
    // own reference clip is actually rendered from, written to match summary's own pace/energy.
    val refLine: String? = null,
    val paragraphCount: Int = 0,
    val readyCount: Int = 0,
    val refAudioUrl: String? = null,
)

@Serializable
data class SetParagraphSpeakerRequestDto(val speaker: String)

@Serializable
data class SetParagraphDescriptionRequestDto(val from: String, val to: String)

@Serializable
data class SetParagraphScareQuoteRequestDto(val scareQuote: Boolean)

/** One paragraph a character speaks (or, from the descriptions endpoint, one paragraph that
 *  describes them) - see backend's speakerAppearanceDTO. Spans every book in their series, not
 *  just whichever book SpeakersScreen is open on, hence carrying its own [bookId]/[bookTitle]. */
@Serializable
data class SpeakerAppearanceDto(
    val bookId: String,
    val bookTitle: String,
    val chapterIdx: Int,
    val chapterTitle: String,
    val paragraphIdx: Int,
    val text: String,
    // Set only once this specific paragraph's audio is ready under whatever voice it currently
    // resolves to - null means it needs generating first (see SpeakersScreen's own "Generate"
    // fallback, mirroring frontend's AppearanceRow).
    val audioUrl: String? = null,
)

@Serializable
data class MergeCharacterRequestDto(val targetName: String)

@Serializable
data class SetCharacterVoiceRequestDto(val voicePresetId: String)

/** Response from POST .../generate-voice - see backend's handleGenerateCharacterVoice. */
@Serializable
data class GenerateVoiceResponseDto(val voicePresetId: String, val audioUrl: String)

/** Response from POST .../characterize - see backend's handleCharacterizeSpeaker.
 *  [voiceInvalidated] is true when this run changed the characterization enough that this
 *  character's assigned voice preset (if any) was invalidated and will re-render next time it's
 *  actually needed, mirroring frontend's own characterizeNote message. */
@Serializable
data class CharacterizeResponseDto(val summary: String, val voiceInvalidated: Boolean)
