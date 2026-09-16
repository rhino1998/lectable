package com.lectable.app.data.remote.dto

import kotlinx.serialization.Serializable

@Serializable
data class BookmarkDto(
    val id: String,
    val chapterIdx: Int,
    val chapterTitle: String,
    val paragraphIdx: Int,
    val text: String,
    val note: String,
    val createdAt: Long,
)

@Serializable
data class CreateBookmarkRequestDto(
    val chapterIdx: Int,
    val paragraphIdx: Int,
    val note: String = "",
)

@Serializable
data class CreateBookmarkResponseDto(val id: String)

@Serializable
data class UpdateBookmarkNoteRequestDto(val note: String)
