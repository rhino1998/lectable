package com.lectable.app.di

import android.content.Context
import androidx.room.Room
import androidx.work.WorkManager
import com.lectable.app.data.download.DownloadDatabase
import com.lectable.app.data.download.DownloadedBookDao
import com.lectable.app.data.download.DownloadedChapterDao
import com.lectable.app.data.download.PendingPositionDao
import dagger.Module
import dagger.Provides
import dagger.hilt.InstallIn
import dagger.hilt.android.qualifiers.ApplicationContext
import dagger.hilt.components.SingletonComponent
import javax.inject.Singleton

@Module
@InstallIn(SingletonComponent::class)
object DownloadModule {

    @Provides
    @Singleton
    fun provideDownloadDatabase(@ApplicationContext context: Context): DownloadDatabase =
        Room.databaseBuilder(context, DownloadDatabase::class.java, "downloads.db")
            // This table is a local cache the app can always rebuild by re-downloading, not a
            // source of truth worth writing real migrations for at this stage (app isn't
            // shipped yet - see android/CLAUDE.md) - a schema change just drops and recreates it.
            .fallbackToDestructiveMigration()
            .build()

    @Provides
    @Singleton
    fun provideDownloadedChapterDao(db: DownloadDatabase): DownloadedChapterDao = db.downloadedChapterDao()

    @Provides
    @Singleton
    fun provideDownloadedBookDao(db: DownloadDatabase): DownloadedBookDao = db.downloadedBookDao()

    @Provides
    @Singleton
    fun providePendingPositionDao(db: DownloadDatabase): PendingPositionDao = db.pendingPositionDao()

    @Provides
    @Singleton
    fun provideWorkManager(@ApplicationContext context: Context): WorkManager = WorkManager.getInstance(context)
}
