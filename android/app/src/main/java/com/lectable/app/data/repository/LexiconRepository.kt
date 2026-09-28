package com.lectable.app.data.repository

import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.dto.LexiconMatchesDto
import com.lectable.app.data.remote.dto.UpsertLexiconRequestDto
import javax.inject.Inject
import javax.inject.Singleton
import okhttp3.ResponseBody

/** Thin wrapper over [LectableApi]'s pronunciation-lexicon endpoints - the Android analogue of
 *  frontend's LexiconPanel calls. A rule belongs to the book's whole series (backend
 *  store.SeriesScope), or to every book when saved with `global`. */
@Singleton
class LexiconRepository @Inject constructor(
    private val api: LectableApi,
) {
    suspend fun upsert(bookId: String, rule: UpsertLexiconRequestDto): String = api.upsertLexicon(bookId, rule).id

    suspend fun delete(bookId: String, entryId: String) {
        api.deleteLexicon(bookId, entryId)
    }

    /** What a rule (saved or not) matches in this book - paragraphs and distinct words. */
    suspend fun matches(bookId: String, rule: UpsertLexiconRequestDto): LexiconMatchesDto = api.lexiconMatches(bookId, rule)

    /** The book's narrator reading the first line the rule matches, with it applied (WAV). */
    suspend fun preview(bookId: String, rule: UpsertLexiconRequestDto): ResponseBody = api.previewLexicon(bookId, rule)
}
