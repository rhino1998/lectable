package com.lectable.app

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.material3.Surface
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Modifier
import androidx.core.content.ContextCompat
import com.lectable.app.data.live.LiveStore
import com.lectable.app.data.settings.ThemePreference
import com.lectable.app.data.settings.ThemeSettingsRepository
import com.lectable.app.ui.navigation.LectableNavHost
import com.lectable.app.ui.theme.LectableTheme
import dagger.hilt.android.AndroidEntryPoint
import javax.inject.Inject

@AndroidEntryPoint
class MainActivity : ComponentActivity() {

    @Inject lateinit var themeSettingsRepository: ThemeSettingsRepository
    @Inject lateinit var liveStore: LiveStore

    private val requestNotificationPermission =
        registerForActivityResult(ActivityResultContracts.RequestPermission()) { /* no-op either way - PlaybackService just won't show a visible notification if denied */ }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        // Android 13+ needs this granted for PlaybackService's notification to actually show
        // (see AndroidManifest.xml's POST_NOTIFICATIONS) - without it, background playback
        // still works, just without visible lock-screen/notification controls.
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            requestNotificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
        setContent {
            val themePreference by themeSettingsRepository.theme.collectAsState(initial = ThemePreference.SYSTEM)
            val darkTheme = when (themePreference) {
                ThemePreference.DARK -> true
                ThemePreference.LIGHT -> false
                ThemePreference.SYSTEM -> isSystemInDarkTheme()
            }
            LectableTheme(darkTheme = darkTheme) {
                Surface(modifier = Modifier.fillMaxSize()) {
                    LectableNavHost()
                }
            }
        }
    }

    // The live store keeps library-wide state (the book list, voice lists) subscribed while the
    // app is visible, so screens open onto current data - and lets go in the background, where
    // an open socket would only cost battery. See LiveStore.
    override fun onStart() {
        super.onStart()
        liveStore.setForeground(true)
    }

    override fun onStop() {
        super.onStop()
        liveStore.setForeground(false)
    }
}
