package com.lectable.app.data.remote.dto

import kotlinx.serialization.Serializable

@Serializable
data class PositionDto(
    val chapterIdx: Int,
    val paragraphIdx: Int,
    val seconds: Double,
)

@Serializable
data class SearchResultDto(
    val chapterIdx: Int,
    val chapterTitle: String,
    val paragraphIdx: Int,
    val text: String,
)
