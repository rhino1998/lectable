import { Outlet, createRootRoute, createRoute, createRouter, Link } from '@tanstack/react-router'
import { LibraryPage } from './pages/LibraryPage'
import { ReaderPage } from './pages/ReaderPage'
import { VoicesPage } from './pages/VoicesPage'
import { SFXPage } from './pages/SFXPage'
import { LLMPage } from './pages/LLMPage'
import { SpeakersPage } from './pages/SpeakersPage'
import { JobsPage } from './pages/JobsPage'
import { ExportPage } from './pages/ExportPage'
import { ThemeToggle } from './components/ThemeToggle'

function RootLayout() {
  return (
    <div className="app-shell-sidebar">
      <nav className="app-sidebar">
        <div className="app-title">
          <img src="/favicon.svg" alt="" width="22" height="22" />
          Lectable
        </div>
        <Link to="/" className="sidebar-tab" activeOptions={{ exact: true }} activeProps={{ className: 'sidebar-tab sidebar-tab-active' }}>
          Library
        </Link>
        <Link to="/voices" className="sidebar-tab" activeProps={{ className: 'sidebar-tab sidebar-tab-active' }}>
          Voices
        </Link>
        <Link to="/sfx" className="sidebar-tab" activeProps={{ className: 'sidebar-tab sidebar-tab-active' }}>
          SFX
        </Link>
        <Link to="/llm" className="sidebar-tab" activeProps={{ className: 'sidebar-tab sidebar-tab-active' }}>
          LLM
        </Link>
        <Link to="/jobs" className="sidebar-tab" activeProps={{ className: 'sidebar-tab sidebar-tab-active' }}>
          Jobs
        </Link>
        <div className="app-sidebar-spacer" />
        <ThemeToggle />
      </nav>
      <main className="app-main">
        <Outlet />
      </main>
    </div>
  )
}

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

const routeTree = rootRoute.addChildren([libraryRoute, voicesRoute, sfxRoute, llmRoute, readerRoute, speakersRoute, exportRoute, jobsRoute])

export const router = createRouter({ routeTree })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
}
