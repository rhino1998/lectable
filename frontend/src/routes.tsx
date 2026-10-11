import { createRootRoute, createRoute, createRouter } from '@tanstack/react-router'
import { LibraryPage } from './pages/LibraryPage'
import { ReaderPage } from './pages/ReaderPage'
import { VoicesPage } from './pages/VoicesPage'
import { SFXPage } from './pages/SFXPage'
import { LLMPage } from './pages/LLMPage'
import { BookSettingsPage } from './pages/BookSettingsPage'
import { BOOK_SETTINGS_TABS, type BookSettingsTab } from './utils/bookSettingsTabs'
import { JobsPage } from './pages/JobsPage'
import { SettingsPage } from './pages/SettingsPage'
import { SETTINGS_TABS, type SettingsTab } from './utils/settingsTabs'
import { ExportPage } from './pages/ExportPage'
import { RootLayout } from './components/RootLayout'

const rootRoute = createRootRoute({ component: RootLayout })

const libraryRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/',
  component: LibraryPage,
})

const voicesRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/voices',
  component: VoicesPage,
})

const sfxRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/sfx',
  component: SFXPage,
})

const llmRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/llm',
  component: LLMPage,
})

const readerRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/books/$bookId',
  component: ReaderPage,
})

const speakersRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/books/$bookId/settings',
  component: BookSettingsPage,
  // ?tab= picks the page's tab; anything else is the first one.
  validateSearch: (search: Record<string, unknown>): { tab?: BookSettingsTab } => ({
    tab: BOOK_SETTINGS_TABS.find((t) => t === search.tab),
  }),
})

const exportRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/books/$bookId/export',
  component: ExportPage,
})

const jobsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/jobs',
  component: JobsPage,
})

const settingsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/settings',
  component: SettingsPage,
  // ?tab= picks the page's tab; anything else is the first one.
  validateSearch: (search: Record<string, unknown>): { tab?: SettingsTab } => ({
    tab: SETTINGS_TABS.find((t) => t === search.tab),
  }),
})

const routeTree = rootRoute.addChildren([
  libraryRoute,
  voicesRoute,
  sfxRoute,
  llmRoute,
  readerRoute,
  speakersRoute,
  exportRoute,
  jobsRoute,
  settingsRoute,
])

export const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
