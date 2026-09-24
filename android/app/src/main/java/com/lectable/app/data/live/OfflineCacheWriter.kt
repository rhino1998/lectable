package com.lectable.app.data.live

import com.lectable.app.data.download.DownloadStatus
import com.lectable.app.data.download.DownloadedBookDao
import com.lectable.app.data.download.DownloadedChapterDao
import com.lectable.app.data.download.mergedWith
import com.lectable.app.data.remote.BackendIdentityRepository
import com.lectable.app.data.remote.dto.BookSummaryDto
import com.lectable.app.data.remote.dto.ChapterDetailDto
import javax.inject.Inject
import javax.inject.Singleton
import kotlinx.serialization.json.Json

/**
 * Writes live pushes through to the offline download cache (Room - see DownloadRepository), so a
 * downloaded book read offline shows what the backend last said about it rather than whatever
 * was true at download time: a book renamed or re-covered, a chapter re-attributed, re-tagged or
 * re-annotated after it was downloaded. Only ever *updates* rows that already exist - what gets
 * downloaded in the first place is still DownloadRepository's call.
 *
 * Only covers what happens to pass through [LiveStore] while the app is connected - chapters
 * nobody opened, regenerated audio, and anything that changed while the phone was away are
 * OfflineReconciler's job. Audio is left alone here (see [mergedWith]).
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
            val updated = row.copy(
                title = b.title,
                author = b.author,
                coverUrl = b.coverUrl,
                chapterCount = b.chapterCount,
                posChapterIdx = b.posChapterIdx,
                posParagraphIdx = b.posParagraphIdx,
                posSeconds = b.posSeconds,
            )
            if (updated != row) bookDao.upsert(updated)
        }
    }

    /** Refreshes a fully downloaded chapter's cached text/annotations from a live value. */
    suspend fun onChapter(bookId: String, chapter: ChapterDetailDto) {
        val libraryId = backendIdentity.libraryId.value ?: return
        val row = chapterDao.get(libraryId, bookId, chapter.idx) ?: return
        if (row.status != DownloadStatus.COMPLETE) return
        val updated = row.mergedWith(chapter, json)
        if (updated != row) chapterDao.upsert(updated)
    }
}
