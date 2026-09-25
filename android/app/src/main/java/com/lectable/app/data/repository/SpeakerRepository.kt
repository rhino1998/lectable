package com.lectable.app.data.repository

import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.dto.CharacterizeResponseDto
import com.lectable.app.data.remote.dto.GenerateVoiceResponseDto
import com.lectable.app.data.remote.dto.MergeCharacterRequestDto
import com.lectable.app.data.remote.dto.SetCharacterVoiceRequestDto
import com.lectable.app.data.remote.dto.SpeakerAppearanceDto
import javax.inject.Inject
import javax.inject.Singleton

/** Thin wrapper over [LectableApi]'s per-character endpoints - the Android analogue of
 *  frontend/src/pages/SpeakersPage.tsx's own character-management calls. Kept separate from
 *  [LibraryRepository] (which already owns the book-wide [LibraryRepository.listSpeakers]/
 *  [LibraryRepository.setParagraphSpeaker] the reader's own "Set speaker" picker uses) since this
 *  is its own concern - one character at a time, used only by ui/speakers/SpeakersScreen.kt. */
@Singleton
class SpeakerRepository @Inject constructor(
    private val api: LectableApi,
) {
    suspend fun setCharacterVoice(bookId: String, characterId: String, voicePresetId: String) {
        api.setCharacterVoice(bookId, characterId, SetCharacterVoiceRequestDto(voicePresetId))
    }

    suspend fun deleteCharacter(bookId: String, characterId: String) {
        api.deleteCharacter(bookId, characterId)
    }

    suspend fun mergeCharacter(bookId: String, characterId: String, targetName: String) {
        api.mergeCharacter(bookId, characterId, MergeCharacterRequestDto(targetName))
    }

    suspend fun generateVoice(bookId: String, characterId: String): GenerateVoiceResponseDto =
        api.generateCharacterVoice(bookId, characterId)

    suspend fun appearances(bookId: String, characterId: String): List<SpeakerAppearanceDto> =
        api.characterAppearances(bookId, characterId)

    /** [appearances]'s own description-tagging counterpart - see [LectableApi.characterDescriptions]. */
    suspend fun descriptions(bookId: String, characterId: String): List<SpeakerAppearanceDto> =
        api.characterDescriptions(bookId, characterId)

    suspend fun characterize(bookId: String, characterId: String): CharacterizeResponseDto =
        api.characterizeSpeaker(bookId, characterId)

    /** "Delete speaker data" - see [com.lectable.app.data.remote.LectableApi.deleteSpeakerData]. */
    suspend fun deleteSpeakerData(bookId: String) {
        api.deleteBookSpeakerData(bookId)
    }
}
