package com.lectable.app.data.remote

import okhttp3.ResponseBody
import retrofit2.http.GET
import retrofit2.http.Streaming
import retrofit2.http.Url

/** Fetches a backend-relative media URL as it appears in a DTO (coverUrl, audioUrl, imageUrl) -
 *  the offline downloader's one call that isn't a fixed route, so it lives outside the generated
 *  [LectableApi]. Goes through the same DynamicBaseUrlInterceptor-equipped OkHttpClient. */
interface MediaApi {
    @Streaming
    @GET
    suspend fun downloadFile(@Url url: String): ResponseBody
}
