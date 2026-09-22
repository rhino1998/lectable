package com.lectable.app.data.remote

import com.lectable.app.data.remote.dto.BookDetailDto
import com.lectable.app.data.remote.dto.BookSummaryDto
import com.lectable.app.data.remote.dto.BookmarkDto
import com.lectable.app.data.remote.dto.CancelAllJobsResponseDto
import com.lectable.app.data.remote.dto.ChapterDetailDto
import com.lectable.app.data.remote.dto.CreateBookmarkRequestDto
import com.lectable.app.data.remote.dto.CreateBookmarkResponseDto
import com.lectable.app.data.remote.dto.CharacterizeResponseDto
import com.lectable.app.data.remote.dto.CustomVoicePresetDto
import com.lectable.app.data.remote.dto.CustomVoicePresetInputDto
import com.lectable.app.data.remote.dto.GenerateVoiceResponseDto
import com.lectable.app.data.remote.dto.InstanceDto
import com.lectable.app.data.remote.dto.JobsSnapshotDto
import com.lectable.app.data.remote.dto.LanguagesResponseDto
import com.lectable.app.data.remote.dto.LookaheadRequestDto
import com.lectable.app.data.remote.dto.ChapterMusicDto
import com.lectable.app.data.remote.dto.MergeCharacterRequestDto
import com.lectable.app.data.remote.dto.PausedResponseDto
import com.lectable.app.data.remote.dto.PositionDto
import com.lectable.app.data.remote.dto.QueuedResponseDto
import com.lectable.app.data.remote.dto.RestartWorkerResponseDto
import com.lectable.app.data.remote.dto.SearchResultDto
import com.lectable.app.data.remote.dto.SetCharacterVoiceRequestDto
import com.lectable.app.data.remote.dto.SetParagraphDescriptionRequestDto
import com.lectable.app.data.remote.dto.SetParagraphScareQuoteRequestDto
import com.lectable.app.data.remote.dto.SetParagraphSpeakerRequestDto
import com.lectable.app.data.remote.dto.SpeakerAppearanceDto
import com.lectable.app.data.remote.dto.SpeakerDto
import com.lectable.app.data.remote.dto.TestVoiceDesignRequestDto
import com.lectable.app.data.remote.dto.TestVoiceRequestDto
import com.lectable.app.data.remote.dto.UpdateBookmarkNoteRequestDto
import com.lectable.app.data.remote.dto.VoicePresetsDto
import com.lectable.app.data.remote.dto.VoiceSettingsDto
import okhttp3.MultipartBody
import okhttp3.ResponseBody
import retrofit2.Response
import retrofit2.http.Body
import retrofit2.http.DELETE
import retrofit2.http.GET
import retrofit2.http.Multipart
import retrofit2.http.POST
import retrofit2.http.PUT
import retrofit2.http.Part
import retrofit2.http.Path
import retrofit2.http.Query
import retrofit2.http.Streaming
import retrofit2.http.Url

/**
 * Mirrors frontend/src/api/client.ts one endpoint at a time. Paths are
 * relative; the actual host is injected by [DynamicBaseUrlInterceptor] from
 * whatever the user configured in Settings, since (unlike the Vite dev
 * server) there's no local proxy to hide the backend's origin.
 */
interface LectableApi {

    @GET("api/instance")
    suspend fun getInstance(): InstanceDto

    @GET("api/books")
    suspend fun listBooks(): List<BookSummaryDto>

    @GET("api/books/{id}")
    suspend fun getBook(@Path("id") id: String): BookDetailDto

    @Multipart
    @POST("api/books")
    suspend fun uploadBook(@Part file: MultipartBody.Part): BookSummaryDto

    @DELETE("api/books/{id}")
    suspend fun deleteBook(@Path("id") id: String): Response<Unit>

    /** Enqueues background TTS generation for every chapter in a book at once - the library's own
     *  "Generate audio" action, for a reader who wants a whole book ready to listen to without
     *  opening it chapter by chapter first. Fire-and-forget: 202 {"queued": true} immediately, not
     *  the result - EnqueueChapter is idempotent per chapter, so calling this repeatedly (or
     *  alongside a reader actively reading the book) just re-enqueues whatever isn't already
     *  ready/generating, harmlessly. No book-level "is generating" flag to poll, unlike
     *  [preprocessBook]'s own `preprocessing` - progress shows up the same way any other
     *  generation does, via each chapter's own paragraph statuses. See backend's
     *  handleGenerateBook. */
    @POST("api/books/{id}/generate")
    suspend fun generateBook(@Path("id") id: String): QueuedResponseDto

    /** "Remaining" alongside [generateBook]'s "All": enqueues background TTS generation for
     *  every paragraph from the book's current stored reading position through the end, instead
     *  of the whole book from chapter 0. Same fire-and-forget 202 {"queued": true} shape; no
     *  body needed, position is read server-side from the book itself. See backend's
     *  handleGenerateRemaining. */
    @POST("api/books/{id}/generate-remaining")
    suspend fun generateBookRemaining(@Path("id") id: String): QueuedResponseDto

    /** Kicks off the whole-book meta-task (attribution -> characterization -> voice provisioning
     *  -> direction-tagging) in one call instead of the Speakers page's individual steps.
     *  Fire-and-forget: 202 {"queued": true} immediately, not the result - progress is observed
     *  via [BookSummaryDto.preprocessing]/[BookDetailDto.preprocessing] (LibraryViewModel polls
     *  while any book has it set). 503 if the backend has no SPEAKER_LLM_MODEL_PATH configured,
     *  409 if a run's already in progress for this book - see backend's handlePreprocessBook. */
    @POST("api/books/{id}/preprocess")
    suspend fun preprocessBook(@Path("id") id: String): QueuedResponseDto

    /** Wipes every generated audio file for this book (any voice it's ever been generated
     *  under) without touching text/chapters/attribution/voice settings - see backend's
     *  handleDeleteBookAudio. Every paragraph reports pending again on the next fetch. */
    @DELETE("api/books/{id}/audio")
    suspend fun deleteBookAudio(@Path("id") bookId: String): Response<Unit>

    /** [deleteBookAudio]'s single-chapter counterpart - see backend's handleDeleteChapterAudio. */
    @DELETE("api/books/{id}/chapters/{idx}/audio")
    suspend fun deleteChapterAudio(@Path("id") bookId: String, @Path("idx") idx: Int): Response<Unit>

    @GET("api/books/{id}/chapters/{idx}")
    suspend fun getChapter(@Path("id") bookId: String, @Path("idx") idx: Int): ChapterDetailDto

    @POST("api/books/{id}/chapters/{idx}/generate")
    suspend fun generateChapter(@Path("id") bookId: String, @Path("idx") idx: Int): QueuedResponseDto

    /** Enqueues LLM speaker attribution for one chapter - fire-and-forget, 202 immediately (503
     *  if the backend has no SPEAKER_LLM_MODEL_PATH configured). See backend's
     *  handleAttributeSpeakers - the Speakers page's per-chapter "Attribute" button. */
    @POST("api/books/{id}/chapters/{idx}/attribute-speakers")
    suspend fun attributeSpeakers(@Path("id") bookId: String, @Path("idx") idx: Int): QueuedResponseDto

    /** Enqueues description tagging for one chapter - fire-and-forget, 202 immediately, like
     *  [attributeSpeakers]; the backend job waits on the chapter's scare-quote tagging first. See
     *  backend's handleRetagDescriptions. */
    @POST("api/books/{id}/chapters/{idx}/retag-descriptions")
    suspend fun retagDescriptions(@Path("id") bookId: String, @Path("idx") idx: Int): QueuedResponseDto

    /** Enqueues scare-quote tagging for one chapter - fire-and-forget, 202 immediately. The
     *  backend runs this before a chapter's attribution/description tagging (both wait on it),
     *  queuing it itself if needed, so this is only an explicit re-run. See backend's
     *  handleRetagScareQuotes. */
    @POST("api/books/{id}/chapters/{idx}/retag-scare-quotes")
    suspend fun retagScareQuotes(@Path("id") bookId: String, @Path("idx") idx: Int): QueuedResponseDto

    /** Enqueues speech-direction tagging for one chapter - fire-and-forget, 202 immediately
     *  (503 if unconfigured, 400 if this book's clone model isn't Higgs). See backend's
     *  handleTagDirections. */
    @POST("api/books/{id}/chapters/{idx}/tag-directions")
    suspend fun tagDirections(@Path("id") bookId: String, @Path("idx") idx: Int): QueuedResponseDto

    /** Enqueues pronunciation resolution (ambiguous abbreviations like "Dr." -> "Doctor") for one
     *  chapter - fire-and-forget, 202 immediately (503 if unconfigured). Unlike [tagDirections],
     *  works for every clone model. See backend's handleResolvePronunciation. */
    @POST("api/books/{id}/chapters/{idx}/resolve-pronunciation")
    suspend fun resolvePronunciation(@Path("id") bookId: String, @Path("idx") idx: Int): QueuedResponseDto

    /** This chapter's own background-music tone regions (mood, generation status, clip once
     *  ready) - the reader's own background-music mixer ([BackgroundMusicPlayer]) polls this
     *  while playing a chapter with the book's own [VoiceSettingsDto.musicEnabled] turned on, and
     *  annotations mode's boundary markers poll it too. See backend httpapi
     *  .handleGetChapterMusic. */
    @GET("api/books/{id}/chapters/{idx}/music")
    suspend fun getChapterMusic(@Path("id") bookId: String, @Path("idx") idx: Int): ChapterMusicDto

    /** Triggers background-music tone-region scoring for one chapter - explicit and always
     *  allowed regardless of the book-wide [VoiceSettingsDto.musicEnabled] toggle. Fire-and-
     *  forget, 202 immediately (503 if the backend has no SPEAKER_LLM_MODEL_PATH configured) -
     *  see backend httpapi.handleScoreChapterMusic. */
    @POST("api/books/{id}/chapters/{idx}/score-music")
    suspend fun scoreChapterMusic(@Path("id") bookId: String, @Path("idx") idx: Int): QueuedResponseDto

    /** Generates (or re-generates) one music region's own clip - the reader's own per-region
     *  "Generate"/"Regenerate" action in annotations mode. Always allowed regardless of current
     *  status - see backend httpapi.handleRegenerateMusicRegion. */
    @POST("api/music-regions/{id}/regenerate")
    suspend fun regenerateMusicRegion(@Path("id") regionId: String): QueuedResponseDto

    /** Fetches any relative path's raw bytes for offline caching (see DownloadRepository) - a
     *  paragraph's audioUrl (e.g. `/api/paragraphs/{id}/audio`) or a book's coverUrl alike.
     *  Reuses the same relative-@Url-against-base-URL resolution Retrofit already does for every
     *  other call here, so it goes through the same DynamicBaseUrlInterceptor-equipped
     *  OkHttpClient as everything else. */
    @Streaming
    @GET
    suspend fun downloadFile(@Url url: String): ResponseBody

    @POST("api/books/{id}/lookahead")
    suspend fun lookahead(@Path("id") bookId: String, @Body body: LookaheadRequestDto): QueuedResponseDto

    /** Re-renders one already-generated paragraph from scratch (the reader didn't like how a
     *  specific line came out) - unlike [lookahead], which skips any paragraph already
     *  AudioReady, this unconditionally resets and re-queues it. See backend's
     *  handleRegenerateParagraph. */
    @POST("api/books/{id}/chapters/{idx}/paragraphs/{pidx}/regenerate")
    suspend fun regenerateParagraph(
        @Path("id") bookId: String,
        @Path("idx") chapterIdx: Int,
        @Path("pidx") paragraphIdx: Int,
    ): QueuedResponseDto

    /** Per-book speaker table: Narrator + every attributed character (shared series-wide) -
     *  populates the reader's "Set speaker" picker (see LibraryRepository.listSpeakers). */
    @GET("api/books/{id}/speakers")
    suspend fun listSpeakers(@Path("id") bookId: String): List<SpeakerDto>

    /** Clears this book's own paragraph attribution and deletes the whole series scope's
     *  character roster (identity, voice assignments, auto-created voice presets + cached clips)
     *  - the Speakers screen's own "Delete speaker data" action, for starting over from scratch -
     *  see backend's handleDeleteBookSpeakerData. */
    @DELETE("api/books/{id}/speakers")
    suspend fun deleteSpeakerData(@Path("id") bookId: String): Response<Unit>

    /** Corrects one paragraph's speaker attribution directly - metadata-only, does not touch its
     *  already-cached audio (see LibraryRepository.setParagraphSpeaker). "" and "Narrator" both
     *  mean "no character"; any other name is registered as a real character if it wasn't one
     *  already. 400s if the target paragraph isn't actually quoted dialogue (see backend's
     *  handleSetParagraphSpeaker). */
    @PUT("api/books/{id}/chapters/{idx}/paragraphs/{pidx}/speaker")
    suspend fun setParagraphSpeaker(
        @Path("id") bookId: String,
        @Path("idx") chapterIdx: Int,
        @Path("pidx") paragraphIdx: Int,
        @Body body: SetParagraphSpeakerRequestDto,
    ): Response<Unit>

    /** [setParagraphSpeaker]'s own counterpart for a *description* line - reassigns which
     *  character a narration paragraph describes, from one name to another, without touching any
     *  other character that same paragraph might also describe (a paragraph can describe more
     *  than one person). "" and "Narrator" for [SetParagraphDescriptionRequestDto.to] both mean
     *  "describes no one now"; any other name is registered as a real character if it wasn't one
     *  already. 400s if the target paragraph is actually quoted dialogue - only narration can
     *  describe someone (see backend's handleSetParagraphDescription). */
    @PUT("api/books/{id}/chapters/{idx}/paragraphs/{pidx}/description")
    suspend fun setParagraphDescription(
        @Path("id") bookId: String,
        @Path("idx") chapterIdx: Int,
        @Path("pidx") paragraphIdx: Int,
        @Body body: SetParagraphDescriptionRequestDto,
    ): Response<Unit>

    /** Marks (or unmarks) one quoted paragraph as a scare quote - a reader's own live correction
     *  to speakerattr.Client.ScareQuoteChapter's automatic judgment. 400s for a non-quoted
     *  paragraph. Unlike [setParagraphSpeaker]/[setParagraphDescription], this immediately
     *  invalidates the paragraph's own already-generated audio server-side (a live correction,
     *  not a passive attribute edit) - see backend's handleSetParagraphScareQuote. */
    @PUT("api/books/{id}/chapters/{idx}/paragraphs/{pidx}/scare-quote")
    suspend fun setParagraphScareQuote(
        @Path("id") bookId: String,
        @Path("idx") chapterIdx: Int,
        @Path("pidx") paragraphIdx: Int,
        @Body body: SetParagraphScareQuoteRequestDto,
    ): Response<Unit>

    /** Assigns (or, with "", clears) a character's own narration voice, overriding whatever
     *  they'd otherwise auto-resolve to - see backend's handleSetCharacterVoice. */
    @PUT("api/books/{id}/characters/{characterId}/voice")
    suspend fun setCharacterVoice(
        @Path("id") bookId: String,
        @Path("characterId") characterId: String,
        @Body body: SetCharacterVoiceRequestDto,
    ): Response<Unit>

    /** Removes one character entirely: this book's own paragraphs attributed to them revert to
     *  Narrator, and their identity/voice assignments/auto-created voice presets are deleted for
     *  their whole series scope - see backend's handleDeleteCharacter. */
    @DELETE("api/books/{id}/characters/{characterId}")
    suspend fun deleteCharacter(@Path("id") bookId: String, @Path("characterId") characterId: String): Response<Unit>

    /** Folds one character into another (or into "Narrator") - this book's own paragraphs
     *  attributed to them are reattributed to targetName instead of blanked, and their own
     *  identity/voice/presets are deleted for their whole series scope - see backend's
     *  handleMergeCharacter. */
    @POST("api/books/{id}/characters/{characterId}/merge")
    suspend fun mergeCharacter(
        @Path("id") bookId: String,
        @Path("characterId") characterId: String,
        @Body body: MergeCharacterRequestDto,
    ): Response<Unit>

    /** Force-provisions a voice for one character right now (characterize + auto-create + assign
     *  a preset) instead of waiting for the lazy path to trigger it at generation time - see
     *  backend's handleGenerateCharacterVoice. 409 if there still isn't enough attributed dialogue
     *  to characterize them from yet. */
    @POST("api/books/{id}/characters/{characterId}/generate-voice")
    suspend fun generateCharacterVoice(
        @Path("id") bookId: String,
        @Path("characterId") characterId: String,
    ): GenerateVoiceResponseDto

    /** Every paragraph this character speaks, across every book in their series - a "where does
     *  this character show up" list, each with its own audioUrl once ready under whichever voice
     *  it currently resolves to - see backend's handleCharacterAppearances. */
    @GET("api/books/{id}/characters/{characterId}/appearances")
    suspend fun characterAppearances(
        @Path("id") bookId: String,
        @Path("characterId") characterId: String,
    ): List<SpeakerAppearanceDto>

    /** [characterAppearances]'s own description-tagging counterpart - every paragraph whose
     *  narration describes this character (SpeakerAppearanceDto reused as-is, see its own doc
     *  comment) - see backend's handleCharacterDescriptions. */
    @GET("api/books/{id}/characters/{characterId}/descriptions")
    suspend fun characterDescriptions(
        @Path("id") bookId: String,
        @Path("characterId") characterId: String,
    ): List<SpeakerAppearanceDto>

    /** Force re-runs LLM voice characterization for one character, even if already
     *  characterized - see backend's handleCharacterizeSpeaker. 503 if the backend has no
     *  SPEAKER_LLM_MODEL_PATH configured. */
    @POST("api/books/{id}/characters/{characterId}/characterize")
    suspend fun characterizeSpeaker(
        @Path("id") bookId: String,
        @Path("characterId") characterId: String,
    ): CharacterizeResponseDto

    @GET("api/books/{id}/voice")
    suspend fun getVoice(@Path("id") bookId: String): VoiceSettingsDto

    @PUT("api/books/{id}/voice")
    suspend fun updateVoice(@Path("id") bookId: String, @Body settings: VoiceSettingsDto): VoiceSettingsDto

    @GET("api/books/{id}/position")
    suspend fun getPosition(@Path("id") bookId: String): PositionDto

    @PUT("api/books/{id}/position")
    suspend fun updatePosition(@Path("id") bookId: String, @Body position: PositionDto): Response<Unit>

    @GET("api/books/{id}/search")
    suspend fun searchBook(@Path("id") bookId: String, @Query("q") query: String): List<SearchResultDto>

    @GET("api/books/{id}/bookmarks")
    suspend fun listBookmarks(@Path("id") bookId: String): List<BookmarkDto>

    @POST("api/books/{id}/bookmarks")
    suspend fun createBookmark(@Path("id") bookId: String, @Body body: CreateBookmarkRequestDto): CreateBookmarkResponseDto

    @PUT("api/bookmarks/{id}")
    suspend fun updateBookmarkNote(@Path("id") id: String, @Body body: UpdateBookmarkNoteRequestDto): Response<Unit>

    @DELETE("api/bookmarks/{id}")
    suspend fun deleteBookmark(@Path("id") id: String): Response<Unit>

    @GET("api/voices/presets")
    suspend fun voicePresets(): VoicePresetsDto

    @GET("api/voices/languages")
    suspend fun voiceLanguages(): LanguagesResponseDto

    @GET("api/voices/default")
    suspend fun getDefaultVoice(): VoiceSettingsDto

    @PUT("api/voices/default")
    suspend fun updateDefaultVoice(@Body settings: VoiceSettingsDto): VoiceSettingsDto

    @GET("api/voices/custom-presets")
    suspend fun customVoicePresets(): List<CustomVoicePresetDto>

    @POST("api/voices/custom-presets")
    suspend fun createCustomVoicePreset(@Body input: CustomVoicePresetInputDto): CustomVoicePresetDto

    @PUT("api/voices/custom-presets/{id}")
    suspend fun updateCustomVoicePreset(
        @Path("id") id: String,
        @Body input: CustomVoicePresetInputDto,
    ): CustomVoicePresetDto

    @DELETE("api/voices/custom-presets/{id}")
    suspend fun deleteCustomVoicePreset(@Path("id") id: String): Response<Unit>

    @Streaming
    @POST("api/voices/custom-presets/{id}/test")
    suspend fun testCustomVoicePreset(@Path("id") id: String, @Body body: TestVoiceRequestDto): ResponseBody

    @Streaming
    @POST("api/voices/presets/{id}/test")
    suspend fun testPreset(@Path("id") id: String, @Body body: TestVoiceRequestDto): ResponseBody

    /** Previews an instruct string directly via VoiceDesign, before it's saved as a preset at
     *  all - the only test call that doesn't need a preset id. */
    @Streaming
    @POST("api/voices/design-test")
    suspend fun testVoiceDesign(@Body body: TestVoiceDesignRequestDto): ResponseBody

    /** One-off snapshot of the job queue - screens watch it live via LiveClient's "jobs"
     *  topic instead; this is for point-in-time checks (see ReaderViewModel.onChapterMusic). */
    @GET("api/jobs")
    suspend fun jobsSnapshot(): JobsSnapshotDto

    @DELETE("api/jobs/{id}")
    suspend fun cancelJob(@Path("id") id: String): Response<Unit>

    @DELETE("api/jobs")
    suspend fun cancelAllJobs(): CancelAllJobsResponseDto

    /** Stops the queue from dispatching any *new* task - see backend httpapi.handlePauseJobs.
     *  Not a cancel: whatever's already in flight keeps running to completion. */
    @POST("api/jobs/pause")
    suspend fun pauseJobs(): PausedResponseDto

    /** Undoes [pauseJobs] - see backend httpapi.handleResumeJobs. */
    @POST("api/jobs/resume")
    suspend fun resumeJobs(): PausedResponseDto

    /** Forces an immediate ttsworker restart, the same kind of restart the backend's own
     *  watchdog does automatically on an RSS breach/crash - see backend httpapi
     *  .handleRestartWorker. Blocks until the new worker is confirmed healthy, so this can take
     *  a few seconds. */
    @POST("api/jobs/restart-worker")
    suspend fun restartWorker(): RestartWorkerResponseDto
}
