package com.lectable.app.data.repository

import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.dto.CharacterizeResponseDto
import com.lectable.app.data.remote.dto.GenerateVoiceResponseDto
import com.lectable.app.data.remote.dto.MergeCharacterRequestDto
import com.lectable.app.data.remote.dto.RenameCharacterRequestDto
import com.lectable.app.data.remote.dto.SetCharacterInvalidRequestDto
import com.lectable.app.data.remote.dto.SetCharacterRoleRequestDto
import com.lectable.app.data.remote.dto.SetCharacterVoiceRequestDto
import com.lectable.app.data.remote.dto.SpeakerAppearanceDto
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import retrofit2.HttpException

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

    /** Marks (or unmarks) a character as a role label - several unnamed people of one kind. */
    suspend fun setCharacterRole(bookId: String, characterId: String, role: Boolean) {
        api.setCharacterRole(bookId, characterId, SetCharacterRoleRequestDto(role = role))
    }

    /** Marks (or unmarks) a character as not a real speaker - attribution won't create or
     *  assign that name again; existing lines stay until reassigned. */
    suspend fun setCharacterInvalid(bookId: String, characterId: String, invalid: Boolean) {
        api.setCharacterInvalid(bookId, characterId, SetCharacterInvalidRequestDto(invalid = invalid))
    }

    suspend fun deleteCharacter(bookId: String, characterId: String) {
        api.deleteCharacter(bookId, characterId)
    }

    /** Renames a character across its whole series; the old name stays on as an alias. A
     *  refusal (a clash with another character's name, a group name) throws with the server's
     *  own explanation, which the rename dialog shows as-is. */
    suspend fun renameCharacter(bookId: String, characterId: String, name: String) {
        try {
            api.renameCharacter(bookId, characterId, RenameCharacterRequestDto(name = name))
        } catch (e: HttpException) {
            val message = runCatching {
                Json.parseToJsonElement(e.response()?.errorBody()?.string().orEmpty())
                    .jsonObject["error"]?.jsonPrimitive?.content
            }.getOrNull()
            throw IllegalStateException(message ?: "Could not rename (HTTP ${e.code()})", e)
        }
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
