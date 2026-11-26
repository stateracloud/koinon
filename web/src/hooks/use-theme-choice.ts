// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react'
import { applyTheme, readThemeChoice, saveThemeChoice, type ThemeChoice } from '@/lib/theme'

/**
 * useThemeChoice holds the theme choice (System, Dark or Light), applies it to <html> and remembers it. System
 * follows prefers-color-scheme and tracks a change of it. The component that calls it must stay mounted.
 */
export function useThemeChoice(): [ThemeChoice, (next: ThemeChoice) => void] {
  const [choice, setChoice] = useState<ThemeChoice>(readThemeChoice)

  useEffect(() => {
    applyTheme(choice)
    if (choice !== 'system' || typeof window.matchMedia !== 'function') return
    const query = window.matchMedia('(prefers-color-scheme: light)')
    const onChange = () => applyTheme('system')
    query.addEventListener('change', onChange)
    return () => query.removeEventListener('change', onChange)
  }, [choice])

  return [
    choice,
    (next) => {
      saveThemeChoice(next)
      setChoice(next)
    },
  ]
}
