package com.lectable.app.ui.navigation

sealed class Routes(val route: String) {
    data object Library : Routes("library")
    data object Settings : Routes("settings")
    data object Voices : Routes("voices")
    data object Jobs : Routes("jobs")
    data object Reader : Routes("reader/{bookId}") {
        fun path(bookId: String) = "reader/$bookId"
    }
    data object Speakers : Routes("speakers/{bookId}") {
        fun path(bookId: String) = "speakers/$bookId"
    }
}
