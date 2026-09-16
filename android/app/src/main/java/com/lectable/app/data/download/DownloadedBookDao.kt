package com.lectable.app.data.download

import androidx.room.Dao
import androidx.room.Query
import androidx.room.Upsert
import kotlinx.coroutines.flow.Flow

@Dao
interface DownloadedBookDao {

    /** Every downloaded book, across every libraryId - see DownloadedChapterDao.observeComplete
     *  for why filtering here isn't worth it; callers (LibraryViewModel's offline fallback)
     *  filter to the current library themselves. */
    @Query("SELECT * FROM downloaded_books")
    fun observeAll(): Flow<List<DownloadedBook>>

    @Query("SELECT * FROM downloaded_books WHERE libraryId = :libraryId AND bookId = :bookId")
    suspend fun get(libraryId: String, bookId: String): DownloadedBook?

    /** Every book with at least one download for this library - see
     *  DownloadRepository.deleteAllDownloads/downloadedBookCount. */
    @Query("SELECT bookId FROM downloaded_books WHERE libraryId = :libraryId")
    suspend fun bookIdsForLibrary(libraryId: String): List<String>

    @Upsert
    suspend fun upsert(entity: DownloadedBook)

    @Query("DELETE FROM downloaded_books WHERE libraryId = :libraryId AND bookId = :bookId")
    suspend fun delete(libraryId: String, bookId: String)
}
