import { createRootRoute, createRoute, createRouter } from '@tanstack/react-router'
import { LibraryPage } from './pages/LibraryPage'
import { ReaderPage } from './pages/ReaderPage'
import { VoicesPage } from './pages/VoicesPage'
import { SFXPage } from './pages/SFXPage'
import { LLMPage } from './pages/LLMPage'
import { SpeakersPage } from './pages/SpeakersPage'
import { JobsPage } from './pages/JobsPage'
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
  path: '/books/$bookId/speakers',
  component: SpeakersPage,
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

const routeTree = rootRoute.addChildren([
  libraryRoute,
  voicesRoute,
  sfxRoute,
  llmRoute,
  readerRoute,
  speakersRoute,
  exportRoute,
  jobsRoute,
])

export const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
