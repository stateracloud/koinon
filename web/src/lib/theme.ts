// SPDX-License-Identifier: Apache-2.0

/** The theme switch (ADR 0126 section 1, point 8): System, Dark or Light. */
export type ThemeChoice = 'system' | 'dark' | 'light'
export type ResolvedTheme = 'dark' | 'light'

export const THEME_STORAGE_KEY = 'koinon-theme'
export const THEME_CHOICES: readonly ThemeChoice[] = ['system', 'dark', 'light']

export function isThemeChoice(v: unknown): v is ThemeChoice {
  return v === 'system' || v === 'dark' || v === 'light'
}

/** readThemeChoice returns the saved choice; storage can throw (private window, blocked data), then it is System. */
export function readThemeChoice(): ThemeChoice {
  try {
    const v = localStorage.getItem(THEME_STORAGE_KEY)
    if (isThemeChoice(v)) return v
  } catch {
    // Storage is unavailable: the choice is not remembered.
  }
  return 'system'
}

/** resolveTheme: System follows prefers-color-scheme. A browser always reports dark or light; with no matchMedia (no preference to read) the theme is dark. */
export function resolveTheme(choice: ThemeChoice): ResolvedTheme {
  if (choice !== 'system') return choice
  const light = typeof window.matchMedia === 'function' && window.matchMedia('(prefers-color-scheme: light)').matches
  return light ? 'light' : 'dark'
}

/** applyTheme sets data-theme on <html> and returns the resolved theme. */
export function applyTheme(choice: ThemeChoice): ResolvedTheme {
  const resolved = resolveTheme(choice)
  document.documentElement.setAttribute('data-theme', resolved)
  return resolved
}

/** saveThemeChoice remembers the choice; a storage error is ignored. */
export function saveThemeChoice(choice: ThemeChoice): void {
  try {
    localStorage.setItem(THEME_STORAGE_KEY, choice)
  } catch {
    // Storage is unavailable: the choice holds for this page only.
  }
}
