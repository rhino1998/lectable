package com.lectable.app.data.repository

import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.dto.UpsertLexiconRequestDto
import javax.inject.Inject
import javax.inject.Singleton
import okhttp3.ResponseBody

/** Thin wrapper over [LectableApi]'s pronunciation-lexicon endpoints - the Android analogue of
 *  frontend's LexiconPanel calls. Rules belong to the book's whole series (backend
 *  store.SeriesScope), so any book in a series reads and edits the same list. */
@Singleton
class LexiconRepository @Inject constructor(
    private val api: LectableApi,
) {
    suspend fun upsert(bookId: String, term: String, replacement: String, caseSensitive: Boolean): String =
        api.upsertLexicon(bookId, UpsertLexiconRequestDto(term = term, replacement = replacement, caseSensitive = caseSensitive)).id

    suspend fun delete(bookId: String, entryId: String) {
        api.deleteLexicon(bookId, entryId)
    }

    /** The book's narrator reading the first line the rule matches, with it applied (WAV). */
    suspend fun preview(bookId: String, term: String, replacement: String, caseSensitive: Boolean): ResponseBody =
        api.previewLexicon(bookId, UpsertLexiconRequestDto(term = term, replacement = replacement, caseSensitive = caseSensitive))
}
