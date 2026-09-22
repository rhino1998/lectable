package com.lectable.app.data.live

import com.lectable.app.data.download.DownloadStatus
import com.lectable.app.data.download.DownloadedBookDao
import com.lectable.app.data.download.DownloadedChapterDao
import com.lectable.app.data.download.OfflineParagraph
import com.lectable.app.data.remote.BackendIdentityRepository
import com.lectable.app.data.remote.dto.BookSummaryDto
import com.lectable.app.data.remote.dto.ChapterDetailDto
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json

/**
 * Writes live pushes through to the offline download cache (Room - see DownloadRepository), so a
 * downloaded book read offline shows what the backend last said about it rather than whatever
 * was true at download time: a book renamed or re-covered, a chapter re-attributed, re-tagged or
 * re-annotated after it was downloaded. Only ever *updates* rows that already exist - what gets
 * downloaded in the first place is still DownloadRepository's call.
 *
 * Deliberately leaves audio-derived fields alone: a downloaded paragraph's local .wav is the
 * recording it was downloaded with, so its [OfflineParagraph.durationSeconds]/
 * [OfflineParagraph.audioPointerSeconds] keep describing that file even if the backend has since
 * re-generated the paragraph.
 *
 * Depends on the DAOs directly rather than DownloadRepository, which itself reads through
 * [LiveStore] - that would be a cycle.
 */
@Singleton
class OfflineCacheWriter @Inject constructor(
    private val bookDao: DownloadedBookDao,
    private val chapterDao: DownloadedChapterDao,
    private val backendIdentity: BackendIdentityRepository,
    private val json: Json,
) {
    /** Refreshes cached metadata for every book in [books] that's downloaded. */
    suspend fun onBooks(books: List<BookSummaryDto>) {
        val libraryId = backendIdentity.libraryId.value ?: return
        for (b in books) {
            val row = bookDao.get(libraryId, b.id) ?: continue
            val updated = row.copy(title = b.title, author = b.author, coverUrl = b.coverUrl, chapterCount = b.chapterCount)
            if (updated != row) bookDao.upsert(updated)
        }
    }

    /** Refreshes a fully downloaded chapter's cached text/annotations from a live value. */
    suspend fun onChapter(bookId: String, chapter: ChapterDetailDto) {
        val libraryId = backendIdentity.libraryId.value ?: return
        val row = chapterDao.get(libraryId, bookId, chapter.idx) ?: return
        if (row.status != DownloadStatus.COMPLETE) return
        val stored = runCatching { json.decodeFromString<List<OfflineParagraph>>(row.paragraphsJson) }.getOrDefault(emptyList())
            .associateBy { it.idx }
        val paragraphs = chapter.paragraphs.map { p ->
            val old = stored[p.idx]
            OfflineParagraph(
                idx = p.idx,
                text = p.text,
                durationSeconds = old?.durationSeconds ?: (p.durationSeconds ?: 0.0),
                inline = p.inline,
                speaker = p.speaker,
                directionMarks = p.directionMarks,
                pronunciationMarks = p.pronunciationMarks,
                isQuote = p.isQuote,
                describesCharacters = p.describesCharacters,
                scareQuote = p.scareQuote,
                audioPointerSeconds = old?.audioPointerSeconds ?: p.audioPointerSeconds,
            )
        }
        val updated = row.copy(
            title = chapter.title,
            paragraphsJson = json.encodeToString(paragraphs),
            contentJson = json.encodeToString(chapter.content),
        )
        if (updated != row) chapterDao.upsert(updated)
    }
}
