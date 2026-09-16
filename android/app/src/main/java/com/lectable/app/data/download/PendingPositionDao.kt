package com.lectable.app.data.download

import androidx.room.Dao
import androidx.room.Query
import androidx.room.Upsert

@Dao
interface PendingPositionDao {

    @Query("SELECT * FROM pending_positions WHERE libraryId = :libraryId AND bookId = :bookId")
    suspend fun get(libraryId: String, bookId: String): PendingPosition?

    @Upsert
    suspend fun upsert(entity: PendingPosition)

    @Query("DELETE FROM pending_positions WHERE libraryId = :libraryId AND bookId = :bookId")
    suspend fun delete(libraryId: String, bookId: String)
}
