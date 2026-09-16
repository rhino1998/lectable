import { useCallback, useEffect, useState } from 'react'

export type Theme = 'system' | 'light' | 'dark'

const STORAGE_KEY = 'lectable:theme'

function loadStoredTheme(): Theme {
  try {
    const raw = window.localStorage.getItem(STORAGE_KEY)
    return raw === 'light' || raw === 'dark' ? raw : 'system'
  } catch {
    return 'system'
  }
}

// Applies the chosen theme as a data-theme attribute on <html> - index.css
// defines the light palette on bare :root, then redefines it both under
// `@media (prefers-color-scheme: dark)` (guarded so it doesn't fire when
// data-theme="light" is set) and under `:root[data-theme="dark"]`, so this
// attribute is the only thing JS needs to touch. 'system' means "no
// attribute", leaving prefers-color-scheme in charge, same as before this
// hook existed. index.html also applies a stored choice inline before
// first paint, to avoid a flash of the wrong palette.
export function useTheme() {
  const [theme, setThemeState] = useState<Theme>(loadStoredTheme)

  useEffect(() => {
    const root = document.documentElement
    if (theme === 'system') root.removeAttribute('data-theme')
    else root.setAttribute('data-theme', theme)
  }, [theme])

  const setTheme = useCallback((t: Theme) => {
    setThemeState(t)
    try {
      if (t === 'system') window.localStorage.removeItem(STORAGE_KEY)
      else window.localStorage.setItem(STORAGE_KEY, t)
    } catch {
      // Private browsing / blocked storage - the choice just won't persist.
    }
  }, [])

  return { theme, setTheme }
}
