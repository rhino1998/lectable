package com.lectable.app.data.remote.dto

// Hand-written helpers alongside the generated ApiTypes.kt (backend/cmd/apigen).

/** BookDetail flattens BookSummary's fields on the wire; this recovers the summary. */
fun BookDetailDto.toSummary() = BookSummaryDto(
    id = id,
    title = title,
    author = author,
    seriesName = seriesName,
    seriesIndex = seriesIndex,
    coverUrl = coverUrl,
    chapterCount = chapterCount,
    posChapterIdx = posChapterIdx,
    posParagraphIdx = posParagraphIdx,
    posSeconds = posSeconds,
    progressPercent = progressPercent,
    finished = finished,
    estimatedTotalSeconds = estimatedTotalSeconds,
    estimateCalibrated = estimateCalibrated,
    generatedPercent = generatedPercent,
    preprocessing = preprocessing,
    musicEnabled = musicEnabled,
)

/** The PUT body for this voice - everything but the server-derived seed. */
fun VoiceSettingsDto.toUpdate() = VoiceSettingsUpdateDto(
    presetId = presetId,
    instruct = instruct,
    language = language,
    cloneModel = cloneModel,
    characterVoiceMode = characterVoiceMode,
    speechDirection = speechDirection,
    musicEnabled = musicEnabled,
)
