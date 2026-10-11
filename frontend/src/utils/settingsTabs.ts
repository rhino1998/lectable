// SettingsPage's tabs, in order; the route's ?tab= names one (routes.tsx).
export const SETTINGS_TABS = ['general', 'sources', 'facts', 'pronunciations'] as const
export type SettingsTab = (typeof SETTINGS_TABS)[number]
