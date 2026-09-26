import { RiComputerLine, RiMoonLine, RiSunLine } from 'react-icons/ri'
import { useTheme, type Theme } from '../hooks/useTheme'

const NEXT: Record<Theme, Theme> = { system: 'light', light: 'dark', dark: 'system' }
const ICON: Record<Theme, typeof RiSunLine> = {
  system: RiComputerLine,
  light: RiSunLine,
  dark: RiMoonLine,
}
const LABEL: Record<Theme, string> = {
  system: 'System theme',
  light: 'Light theme',
  dark: 'Dark theme',
}

// Cycles system -> light -> dark -> system on each click. A three-way
// toggle rather than a plain on/off switch, since "system" (the default,
// prefers-color-scheme-driven) is a real third state worth being able to
// return to, not just a transient starting point.
export function ThemeToggle() {
  const { theme, setTheme } = useTheme()
  const Icon = ICON[theme]

  return (
    <button
      className="theme-toggle"
      onClick={() => setTheme(NEXT[theme])}
      title={`${LABEL[theme]} — click for ${LABEL[NEXT[theme]].toLowerCase()}`}
    >
      <Icon /> {LABEL[theme]}
    </button>
  )
}
