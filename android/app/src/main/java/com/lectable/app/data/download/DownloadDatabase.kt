package com.lectable.app.data.download

import androidx.room.Database
import androidx.room.RoomDatabase
import androidx.room.migration.Migration
import androidx.sqlite.db.SupportSQLiteDatabase

// Bump this whenever an entity's columns (or the set of entities) change -
// fallbackToDestructiveMigration() (see di/DownloadModule.kt) only fires on an actual version
// increase; forgetting to bump it here makes Room compare the compiled schema against the
// on-disk one directly in onOpen() instead and crash with "Room cannot verify the data
// integrity" rather than ever reaching the fallback.
@Database(
    entities = [DownloadedChapter::class, DownloadedBook::class, PendingPosition::class],
    version = 6,
    exportSchema = false,
)
abstract class DownloadDatabase : RoomDatabase() {
    abstract fun downloadedChapterDao(): DownloadedChapterDao
    abstract fun downloadedBookDao(): DownloadedBookDao
    abstract fun pendingPositionDao(): PendingPositionDao
}

/** 5 -> 6 adds the offline-sync hash columns (and the book's cached server position). A real
 *  migration rather than the destructive fallback: dropping the tables would orphan every
 *  already-downloaded chapter's files, and the new columns all have usable defaults ("" never
 *  matches a server hash, so the first reconcile just re-checks everything). */
val MIGRATION_5_6 = object : Migration(5, 6) {
    override fun migrate(db: SupportSQLiteDatabase) {
        db.execSQL("ALTER TABLE downloaded_chapters ADD COLUMN hash TEXT NOT NULL DEFAULT ''")
        db.execSQL("ALTER TABLE downloaded_books ADD COLUMN hash TEXT NOT NULL DEFAULT ''")
        db.execSQL("ALTER TABLE downloaded_books ADD COLUMN posChapterIdx INTEGER NOT NULL DEFAULT 0")
        db.execSQL("ALTER TABLE downloaded_books ADD COLUMN posParagraphIdx INTEGER NOT NULL DEFAULT 0")
        db.execSQL("ALTER TABLE downloaded_books ADD COLUMN posSeconds REAL NOT NULL DEFAULT 0")
    }
}
