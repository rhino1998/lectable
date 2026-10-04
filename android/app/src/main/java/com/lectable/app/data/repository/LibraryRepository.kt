package com.lectable.app.data.repository

import android.content.ContentResolver
import android.net.Uri
import android.provider.OpenableColumns
import com.lectable.app.data.remote.LectableApi
import com.lectable.app.data.remote.dto.BookDetailDto
import com.lectable.app.data.remote.dto.BulkActions
import com.lectable.app.data.remote.dto.BookSummaryDto
import com.lectable.app.data.remote.dto.BookmarkDto
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.ChapterMusicDto
import com.lectable.app.data.remote.dto.ChapterNotesDto
import com.lectable.app.data.remote.dto.CreateBookmarkRequestDto
import com.lectable.app.data.remote.dto.LookaheadRequestDto
import com.lectable.app.data.remote.dto.PositionDto
import com.lectable.app.data.remote.dto.SearchResultDto
import com.lectable.app.data.remote.dto.SetParagraphDescriptionRequestDto
import com.lectable.app.data.remote.dto.SetParagraphEmotionRequestDto
import com.lectable.app.data.remote.dto.SetParagraphScareQuoteRequestDto
import com.lectable.app.data.remote.dto.SetParagraphSpeakerRequestDto
import com.lectable.app.data.remote.dto.SpeakerDto
import com.lectable.app.data.remote.dto.UpdateBookmarkRequestDto
import com.lectable.app.data.settings.PlaybackSettingsRepository
import javax.inject.Inject
import javax.inject.Singleton
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.MultipartBody
import okhttp3.RequestBody.Companion.asRequestBody
import java.io.File
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import retrofit2.HttpException

/** Thin wrapper over [LectableApi]'s book/chapter/position/search endpoints - mirrors client.ts. */
@Singleton
class LibraryRepository @Inject constructor(
    private val api: LectableApi,
    private val playbackSettingsRepository: PlaybackSettingsRepository,
) {
    suspend fun listBooks(): List<BookSummaryDto> = api.listBooks()

    suspend fun getBook(id: String): BookDetailDto = api.getBook(id)

    suspend fun uploadBook(file: File, fileName: String): BookSummaryDto {
        val body = file.asRequestBody("application/epub+zip".toMediaType())
        val part = MultipartBody.Part.createFormData("file", fileName, body)
        return api.uploadBook(part)
    }

    /** Copies a SAF `content://` Uri (from the system file picker) into [cacheDir] before uploading it. */
    suspend fun uploadBook(contentResolver: ContentResolver, uri: Uri, cacheDir: File): BookSummaryDto {
        val fileName = contentResolver.displayNameFor(uri)
        val tempFile = File(cacheDir, "upload-${System.currentTimeMillis()}.epub")
        contentResolver.openInputStream(uri)?.use { input ->
            tempFile.outputStream().use { output -> input.copyTo(output) }
        } ?: error("Could not open picked file")
        return try {
            uploadBook(tempFile, fileName)
        } finally {
            tempFile.delete()
        }
    }

    suspend fun deleteBook(id: String) {
        api.deleteBook(id)
    }

    /** [LectableApi.preprocessBook] - see its own doc comment. */
    suspend fun preprocessBook(id: String): Boolean = api.preprocessBook(id).queued > 0

    /** The "used_voices" bulk action (backend httpapi.usedVoicesBulkRun): queues whatever the
     *  voices this book's lines use still lack. Returns how many voices needed something. */
    suspend fun provisionUsedVoices(id: String): Int = api.bulkAction(id, BulkActions.USED_VOICES).queued

    /** [LectableApi.generateBook] - see its own doc comment. */
    suspend fun generateBook(id: String): Boolean = api.generateBook(id).queued > 0

    /** [LectableApi.generateBookRemaining] - see its own doc comment. */
    suspend fun generateBookRemaining(id: String): Boolean = api.generateRemaining(id).queued > 0

    /** Wipes every generated audio file for bookId (any voice) - reclaims disk space or forces
     *  a full regenerate, without touching the book's text/chapters/voice settings. */
    suspend fun deleteBookAudio(bookId: String) {
        api.deleteBookAudio(bookId)
    }

    /** [deleteBookAudio]'s single-chapter counterpart. */
    suspend fun deleteChapterAudio(bookId: String, chapterIdx: Int) {
        api.deleteChapterAudio(bookId, chapterIdx)
    }

    suspend fun getChapter(bookId: String, idx: Int): ChapterDetailDto = api.getChapter(bookId, idx)

    suspend fun generateChapter(bookId: String, idx: Int): Boolean = api.generateChapter(bookId, idx).queued > 0

    /** [LectableApi.attributeSpeakers] - see its own doc comment. */
    suspend fun attributeSpeakers(bookId: String, idx: Int): Boolean = api.attributeSpeakers(bookId, idx).queued > 0

    /** [attributeSpeakers] limited to the dialogue around the chapter's corrected speakers, with
     *  the corrections shown to the model (`?scope=edits`). A chapter with no corrections throws
     *  with a hint instead of the bare 409. */
    suspend fun attributeAroundEdits(bookId: String, idx: Int): Boolean = withServerMessage("Re-attribution failed") {
        try {
            api.attributeSpeakers(bookId, idx, scope = "edits").queued > 0
        } catch (e: HttpException) {
            if (e.code() == 409) {
                throw IllegalStateException("No corrected speakers in this chapter yet - fix a line first (long-press it)", e)
            }
            throw e
        }
    }

    /** Saves the reader's own version of a chapter's notes (backend handlePutChapterNotes),
     *  marked edited so the summary pass keeps it. A refusal (a group name, a scene listing
     *  someone not in the cast) throws with the server's explanation, which the editor shows. */
    suspend fun saveChapterNotes(bookId: String, idx: Int, notes: ChapterNotesDto) =
        withServerMessage("Could not save the notes") { api.putChapterNotes(bookId, idx, notes) }

    /** Re-runs a chapter's summary pass over notes the reader edited, replacing them. */
    suspend fun regenerateChapterNotes(bookId: String, idx: Int): Boolean =
        api.summarizeChapter(bookId, idx, overwriteEdits = "true").queued > 0

    /** Runs [block], turning an HTTP error into an exception carrying the server's own `error`
     *  text (or [fallback] with the status code). */
    private suspend fun <T> withServerMessage(fallback: String, block: suspend () -> T): T =
        try {
            block()
        } catch (e: HttpException) {
            val message = runCatching {
                Json.parseToJsonElement(e.response()?.errorBody()?.string().orEmpty())
                    .jsonObject["error"]?.jsonPrimitive?.content
            }.getOrNull()
            throw IllegalStateException(message ?: "$fallback (HTTP ${e.code()})", e)
        }

    /** [LectableApi.retagDescriptions] - see its own doc comment. */
    suspend fun retagDescriptions(bookId: String, idx: Int): Boolean = api.retagDescriptions(bookId, idx).queued > 0

    /** [LectableApi.retagScareQuotes] - see its own doc comment. */
    suspend fun retagScareQuotes(bookId: String, idx: Int): Boolean = api.retagScareQuotes(bookId, idx).queued > 0

    /** [LectableApi.tagDirections] - see its own doc comment. */
    suspend fun tagDirections(bookId: String, idx: Int): Boolean = api.tagDirections(bookId, idx).queued > 0

    /** [LectableApi.resolvePronunciation] - see its own doc comment. */
    suspend fun resolvePronunciation(bookId: String, idx: Int): Boolean = api.resolvePronunciation(bookId, idx).queued > 0

    suspend fun lookahead(bookId: String, chapterIdx: Int, paragraphIdx: Int, paragraphCount: Int? = null): Boolean =
        api.lookahead(
            bookId,
            LookaheadRequestDto(
                chapterIdx = chapterIdx,
                paragraphIdx = paragraphIdx,
                paragraphCount = paragraphCount ?: 0,
                clientId = playbackSettingsRepository.clientId(),
            ),
        ).queued > 0

    /** Forces one already-generated paragraph to be reset and re-rendered - see
     *  [com.lectable.app.data.remote.LectableApi.regenerateParagraph]. */
    suspend fun regenerateParagraph(bookId: String, chapterIdx: Int, paragraphIdx: Int): Boolean =
        api.regenerateParagraph(bookId, chapterIdx, paragraphIdx).queued > 0

    suspend fun getPosition(bookId: String): PositionDto = api.getPosition(bookId)

    suspend fun updatePosition(bookId: String, position: PositionDto) {
        api.updatePosition(bookId, position)
    }

    suspend fun searchBook(bookId: String, query: String): List<SearchResultDto> = api.searchBook(bookId, query)

    suspend fun listSpeakers(bookId: String): List<SpeakerDto> = api.listSpeakers(bookId)

    /** [LectableApi.setParagraphSpeaker] - reassigns one paragraph's speaker attribution. Leaves
     *  its already-generated audio untouched; a caller wanting the new voice actually narrated
     *  still needs a follow-up regenerate (see LibraryRepository.regenerateParagraph), same two-step
     *  flow as the web Speakers page's separate "Reassign"/"Regenerate" buttons. */
    suspend fun setParagraphSpeaker(bookId: String, chapterIdx: Int, paragraphIdx: Int, speaker: String) {
        api.setParagraphSpeaker(bookId, chapterIdx, paragraphIdx, SetParagraphSpeakerRequestDto(speaker))
    }

    /** [LectableApi.setParagraphDescription] - reassigns one paragraph's description from one
     *  character to another, leaving any other character that same paragraph also describes
     *  untouched. "" and "Narrator" for [to] both mean "describes no one now". */
    suspend fun setParagraphDescription(bookId: String, chapterIdx: Int, paragraphIdx: Int, from: String, to: String) {
        api.setParagraphDescription(bookId, chapterIdx, paragraphIdx, SetParagraphDescriptionRequestDto(from, to))
    }

    /** [LectableApi.setParagraphScareQuote] - marks/unmarks one quoted paragraph as a scare
     *  quote, immediately invalidating its own already-generated audio server-side. */
    suspend fun setParagraphScareQuote(bookId: String, chapterIdx: Int, paragraphIdx: Int, scareQuote: Boolean) {
        api.setParagraphScareQuote(bookId, chapterIdx, paragraphIdx, SetParagraphScareQuoteRequestDto(scareQuote))
    }

    /** [LectableApi.setParagraphEmotion] - overrides one dialogue line's emotion ("" =
     *  neutral); the backend regenerates its audio when the effective emotion changes. Throws
     *  on a non-2xx response so the caller can surface it. */
    suspend fun setParagraphEmotion(bookId: String, chapterIdx: Int, paragraphIdx: Int, emotion: String) {
        api.setParagraphEmotion(bookId, chapterIdx, paragraphIdx, SetParagraphEmotionRequestDto(emotion))
    }

    /** [LectableApi.getChapterMusic] - this chapter's own background-music regions. */
    suspend fun getChapterMusic(bookId: String, chapterIdx: Int): ChapterMusicDto = api.getChapterMusic(bookId, chapterIdx)

    /** [LectableApi.runChapterAmbience] - music scoring plus lighting; see its own doc comment. */
    suspend fun runChapterAmbience(bookId: String, chapterIdx: Int): Boolean = api.runChapterAmbience(bookId, chapterIdx).queued > 0

    /** [LectableApi.regenerateMusicRegion] - see its own doc comment. */
    suspend fun regenerateMusicRegion(regionId: String): Boolean = api.regenerateMusicRegion(regionId).queued > 0

    suspend fun listBookmarks(bookId: String): List<BookmarkDto> = api.listBookmarks(bookId)

    suspend fun createBookmark(bookId: String, chapterIdx: Int, paragraphIdx: Int): String =
        api.createBookmark(bookId, CreateBookmarkRequestDto(chapterIdx, paragraphIdx)).id

    suspend fun updateBookmarkNote(id: String, note: String) {
        api.updateBookmark(id, UpdateBookmarkRequestDto(note))
    }

    suspend fun deleteBookmark(id: String) {
        api.deleteBookmark(id)
    }
}

/** Resolves a content:// Uri (from the system file picker) to a display name, for [LibraryRepository.uploadBook]. */
fun ContentResolver.displayNameFor(uri: Uri): String {
    query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use { cursor ->
        val idx = cursor.getColumnIndex(OpenableColumns.DISPLAY_NAME)
        if (idx >= 0 && cursor.moveToFirst()) return cursor.getString(idx)
    }
    return uri.lastPathSegment ?: "book.epub"
}
