package com.lectable.app.data.download

import androidx.room.Database
import androidx.room.RoomDatabase

// Bump this whenever an entity's columns (or the set of entities) change -
// fallbackToDestructiveMigration() (see di/DownloadModule.kt) only fires on an actual version
// increase; forgetting to bump it here makes Room compare the compiled schema against the
// on-disk one directly in onOpen() instead and crash with "Room cannot verify the data
// integrity" rather than ever reaching the fallback.
@Database(
    entities = [DownloadedChapter::class, DownloadedBook::class, PendingPosition::class],
    version = 5,
    exportSchema = false,
)
abstract class DownloadDatabase : RoomDatabase() {
    abstract fun downloadedChapterDao(): DownloadedChapterDao
    abstract fun downloadedBookDao(): DownloadedBookDao
    abstract fun pendingPositionDao(): PendingPositionDao
}
