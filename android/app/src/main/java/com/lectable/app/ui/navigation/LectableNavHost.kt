package com.lectable.app.ui.navigation

import androidx.compose.runtime.Composable
import androidx.navigation.NavHostController
import androidx.navigation.NavType
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.rememberNavController
import androidx.navigation.navArgument
import com.lectable.app.ui.jobs.JobsScreen
import com.lectable.app.ui.library.LibraryScreen
import com.lectable.app.ui.reader.ReaderScreen
import com.lectable.app.ui.settings.SettingsScreen
import com.lectable.app.ui.speakers.SpeakersScreen
import com.lectable.app.ui.voices.VoicesScreen

@Composable
fun LectableNavHost(navController: NavHostController = rememberNavController()) {
    NavHost(navController = navController, startDestination = Routes.Library.route) {
        composable(Routes.Library.route) {
            LibraryScreen(
                onOpenBook = { bookId -> navController.navigate(Routes.Reader.path(bookId)) },
                onOpenSettings = { navController.navigate(Routes.Settings.route) },
                onOpenSpeakers = { bookId -> navController.navigate(Routes.Speakers.path(bookId)) },
            )
        }
        composable(
            route = Routes.Reader.route,
            arguments = listOf(navArgument("bookId") { type = NavType.StringType }),
        ) { backStackEntry ->
            val bookId = checkNotNull(backStackEntry.arguments?.getString("bookId"))
            ReaderScreen(
                onBack = { navController.popBackStack() },
                onOpenVoices = { navController.navigate(Routes.Voices.route) },
                onOpenSpeakers = { navController.navigate(Routes.Speakers.path(bookId)) },
                onOpenJobs = { navController.navigate(Routes.Jobs.route) },
            )
        }
        composable(
            route = Routes.Speakers.route,
            arguments = listOf(navArgument("bookId") { type = NavType.StringType }),
        ) {
            SpeakersScreen(
                onBack = { navController.popBackStack() },
                onOpenVoices = { navController.navigate(Routes.Voices.route) },
            )
        }
        composable(Routes.Settings.route) {
            SettingsScreen(
                onBack = { navController.popBackStack() },
                onOpenVoices = { navController.navigate(Routes.Voices.route) },
                onOpenJobs = { navController.navigate(Routes.Jobs.route) },
            )
        }
        composable(Routes.Voices.route) {
            VoicesScreen(onBack = { navController.popBackStack() })
        }
        composable(Routes.Jobs.route) {
            JobsScreen(onBack = { navController.popBackStack() })
        }
    }
}
