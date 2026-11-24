// SPDX-License-Identifier: Apache-2.0

import { useEffect, useState } from 'react'
import { NativeSelect } from '@/components/ui/native-select'
import { applyTheme, isThemeChoice, readThemeChoice, saveThemeChoice, type ThemeChoice } from '@/lib/theme'

const labels: Record<ThemeChoice, string> = { system: 'System', dark: 'Dark', light: 'Light' }

/** ThemeSwitch picks System, Dark or Light. System follows prefers-color-scheme and tracks a change of it. */
export function ThemeSwitch() {
  const [choice, setChoice] = useState<ThemeChoice>(readThemeChoice)

  useEffect(() => {
    applyTheme(choice)
    if (choice !== 'system' || typeof window.matchMedia !== 'function') return
    const query = window.matchMedia('(prefers-color-scheme: light)')
    const onChange = () => applyTheme('system')
    query.addEventListener('change', onChange)
    return () => query.removeEventListener('change', onChange)
  }, [choice])

  return (
    <NativeSelect
      aria-label="Theme"
      className="h-8 w-auto"
      value={choice}
      onChange={(e) => {
        const next = e.target.value
        if (!isThemeChoice(next)) return
        saveThemeChoice(next)
        setChoice(next)
      }}
    >
      {(Object.keys(labels) as ThemeChoice[]).map((c) => (
        <option key={c} value={c}>
          {labels[c]}
        </option>
      ))}
    </NativeSelect>
  )
}
