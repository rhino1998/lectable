package com.lectable.app

import androidx.compose.ui.test.junit4.createAndroidComposeRule
import androidx.compose.ui.test.onNodeWithText
import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

/**
 * Smoke test only - there's no backend reachable in CI/instrumentation
 * environments, so this just confirms the app launches into the Library
 * screen's empty state without crashing (Hilt graph wires up, DataStore
 * loads, Compose renders).
 */
@RunWith(AndroidJUnit4::class)
class MainActivitySmokeTest {

    @get:Rule
    val composeTestRule = createAndroidComposeRule<MainActivity>()

    @Test
    fun launchesToLibraryScreen() {
        composeTestRule.onNodeWithText("Lectable").assertExists()
    }
}
