package com.lectable.app.data.repository

import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.dto.CustomVoicePresetDto
import com.lectable.app.data.remote.dto.CustomVoicePresetInputDto
import com.lectable.app.data.remote.dto.TestVoiceDesignRequestDto
import com.lectable.app.data.remote.dto.TestVoiceRequestDto
import com.lectable.app.data.remote.dto.VoicePresetsDto
import com.lectable.app.data.remote.dto.VoiceSettingsDto
import javax.inject.Inject
import javax.inject.Singleton
import okhttp3.ResponseBody

/** Thin wrapper over [LectableApi]'s voice endpoints - mirrors client.ts. */
@Singleton
class VoiceRepository @Inject constructor(
    private val api: LectableApi,
) {
    suspend fun getVoice(bookId: String): VoiceSettingsDto = api.getVoice(bookId)

    suspend fun updateVoice(bookId: String, settings: VoiceSettingsDto): VoiceSettingsDto =
        api.updateVoice(bookId, settings)

    suspend fun presets(): VoicePresetsDto = api.voicePresets()

    suspend fun languages(): List<String> = api.voiceLanguages().languages

    suspend fun getDefaultVoice(): VoiceSettingsDto = api.getDefaultVoice()

    suspend fun updateDefaultVoice(settings: VoiceSettingsDto): VoiceSettingsDto = api.updateDefaultVoice(settings)

    suspend fun customPresets(): List<CustomVoicePresetDto> = api.customVoicePresets()

    suspend fun createCustomPreset(input: CustomVoicePresetInputDto): CustomVoicePresetDto =
        api.createCustomVoicePreset(input)

    suspend fun updateCustomPreset(id: String, input: CustomVoicePresetInputDto): CustomVoicePresetDto =
        api.updateCustomVoicePreset(id, input)

    suspend fun deleteCustomPreset(id: String) {
        api.deleteCustomVoicePreset(id)
    }

    suspend fun testCustomPreset(id: String, text: String): ResponseBody =
        api.testCustomVoicePreset(id, TestVoiceRequestDto(text))

    suspend fun testPreset(id: String, text: String): ResponseBody = api.testPreset(id, TestVoiceRequestDto(text))

    suspend fun testVoiceDesign(instruct: String, text: String, seed: Int? = null): ResponseBody =
        api.testVoiceDesign(TestVoiceDesignRequestDto(instruct, text, seed))
}
