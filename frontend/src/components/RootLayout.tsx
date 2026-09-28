import { Link, Outlet } from '@tanstack/react-router'
import { ThemeToggle } from './ThemeToggle'

// The app shell: sidebar navigation plus the routed page (see routes.tsx).
export function RootLayout() {
  return (
    <div className="app-shell-sidebar">
      <nav className="app-sidebar">
        <div className="app-title">
          <img src="/favicon.svg" alt="" width="22" height="22" />
          Lectable
        </div>
        <Link
          to="/"
          className="sidebar-tab"
          activeOptions={{ exact: true }}
          activeProps={{ className: 'sidebar-tab sidebar-tab-active' }}
        >
          Library
        </Link>
        <Link
          to="/voices"
          className="sidebar-tab"
          activeProps={{ className: 'sidebar-tab sidebar-tab-active' }}
        >
          Voices
        </Link>
        <Link
          to="/sfx"
          className="sidebar-tab"
          activeProps={{ className: 'sidebar-tab sidebar-tab-active' }}
        >
          SFX
        </Link>
        <Link
          to="/llm"
          className="sidebar-tab"
          activeProps={{ className: 'sidebar-tab sidebar-tab-active' }}
        >
          LLM
        </Link>
        <Link
          to="/jobs"
          className="sidebar-tab"
          activeProps={{ className: 'sidebar-tab sidebar-tab-active' }}
        >
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
