package com.lectable.app.data.download

import androidx.room.Dao
import androidx.room.Query
import androidx.room.Upsert
import kotlinx.coroutines.flow.Flow

@Dao
interface DownloadedChapterDao {

    /** Room-generated observable query, deliberately across every libraryId (not just the
     *  currently-configured backend) - the in-memory lookup [com.lectable.app.data.repository
     *  .DownloadRepository] uses for [ParagraphPlayer]'s synchronous local-file check matches
     *  against the current libraryId itself, so filtering here would just mean re-querying every
     *  time the configured backend changes for no benefit. */
    @Query("SELECT * FROM downloaded_chapters WHERE status = 'complete'")
    fun observeComplete(): Flow<List<DownloadedChapter>>

    @Query("SELECT * FROM downloaded_chapters WHERE libraryId = :libraryId AND bookId = :bookId")
    suspend fun forBook(libraryId: String, bookId: String): List<DownloadedChapter>

    /** Every status (downloading/complete/error), unlike [observeComplete] - lets the UI show a
     *  spinner for a chapter that's actively downloading, not just a done/not-done toggle. */
    @Query("SELECT * FROM downloaded_chapters WHERE libraryId = :libraryId AND bookId = :bookId")
    fun observeForBook(libraryId: String, bookId: String): Flow<List<DownloadedChapter>>

    @Query("SELECT * FROM downloaded_chapters WHERE libraryId = :libraryId AND bookId = :bookId AND chapterIdx = :chapterIdx")
    suspend fun get(libraryId: String, bookId: String, chapterIdx: Int): DownloadedChapter?

    @Upsert
    suspend fun upsert(entity: DownloadedChapter)

    /** Just the one column, so storing ambience can't clobber a concurrent text/audio refresh. */
    @Query("UPDATE downloaded_chapters SET musicJson = :musicJson WHERE libraryId = :libraryId AND bookId = :bookId AND chapterIdx = :chapterIdx")
    suspend fun setMusicJson(libraryId: String, bookId: String, chapterIdx: Int, musicJson: String)

    @Query("DELETE FROM downloaded_chapters WHERE libraryId = :libraryId AND bookId = :bookId AND chapterIdx = :chapterIdx")
    suspend fun delete(libraryId: String, bookId: String, chapterIdx: Int)

    @Query("DELETE FROM downloaded_chapters WHERE libraryId = :libraryId AND bookId = :bookId")
    suspend fun deleteForBook(libraryId: String, bookId: String)
}
