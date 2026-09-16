package com.lectable.app.data.remote.dto

import kotlinx.serialization.Serializable

/**
 * Mirrors backend/internal/httpapi/books.go's bookSummaryDTO and
 * frontend/src/api/types.ts's BookSummary. Keep in sync by hand if the Go
 * DTO changes shape - see backend/CLAUDE.md.
 */
@Serializable
data class BookSummaryDto(
    val id: String,
    val title: String,
    val author: String,
    val coverUrl: String? = null,
    val chapterCount: Int,
    val posChapterIdx: Int,
    val posParagraphIdx: Int,
    val posSeconds: Double,
    val progressPercent: Double,
    val finished: Boolean,
    val estimatedTotalSeconds: Double,
    val estimateCalibrated: Boolean,
    val generatedPercent: Double,
    val seriesName: String? = null,
    val seriesIndex: Double? = null,
    // True while the backend's whole-book preprocess meta-task (attribution -> characterization
    // -> voice provisioning -> direction tagging) is still running for this book - see backend
    // httpapi.isPreprocessing/handlePreprocessBook. LibraryViewModel polls while any book has this
    // set so the library screen's per-book spinner clears on its own once a run finishes, mirroring
    // frontend's useBooks.
    val preprocessing: Boolean = false,
    // Reader-facing background-music toggle - book-wide (unlike ChapterMusicDto.scored, which is
    // chapter-scoped scoring progress), set via VoiceSettingsDto.musicEnabled/updateVoice.
    // Mirrored here too purely as a read convenience, so screens already holding a book don't
    // need a separate voice-settings fetch just to know whether background music is on - see
    // backend store.Book.MusicEnabled's own doc comment. Off by default.
    val musicEnabled: Boolean = false,
)

@Serializable
data class ChapterSummaryDto(
    val idx: Int,
    val title: String,
    val paragraphCount: Int,
    val readyCount: Int,
)

/**
 * The Go/TS DTO flattens BookSummary's fields into BookDetail via
 * interface extension; JSON is flat, so this duplicates those fields
 * rather than nesting.
 */
@Serializable
data class BookDetailDto(
    val id: String,
    val title: String,
    val author: String,
    val coverUrl: String? = null,
    val chapterCount: Int,
    val posChapterIdx: Int,
    val posParagraphIdx: Int,
    val posSeconds: Double,
    val progressPercent: Double,
    val finished: Boolean,
    val estimatedTotalSeconds: Double,
    val estimateCalibrated: Boolean,
    val generatedPercent: Double,
    val seriesName: String? = null,
    val seriesIndex: Double? = null,
    val preprocessing: Boolean = false,
    val musicEnabled: Boolean = false,
    val chapters: List<ChapterSummaryDto>,
) {
    fun toSummary() = BookSummaryDto(
        id = id,
        title = title,
        author = author,
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
        seriesName = seriesName,
        seriesIndex = seriesIndex,
        preprocessing = preprocessing,
        musicEnabled = musicEnabled,
    )
}
