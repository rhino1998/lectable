// BookSettingsPage's tabs, in order; the route's ?tab= names one
// (routes.tsx).
export const BOOK_SETTINGS_TABS = [
  'general',
  'chapters',
  'characters',
  'facts',
  'pronunciations',
  'jobs',
] as const
export type BookSettingsTab = (typeof BOOK_SETTINGS_TABS)[number]
