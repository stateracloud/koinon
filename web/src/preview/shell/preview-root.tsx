// SPDX-License-Identifier: Apache-2.0

import '@/preview/styles/tokens.css'
import '@/preview/styles/layout.css'
import '@/preview/styles/components.css'
import '@/preview/styles/preview.css'
import { Outlet, useRouterState } from '@tanstack/react-router'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { isTheme, PreviewContext, themeStorageKey, type PaletteSelection, type PreviewContextValue, type Theme } from '@/preview/lib/context'
import { usePreviewNavigate } from '@/preview/lib/navigation'
import { orgs } from '@/preview/mock/data'
import { CommandPalette, ShortcutsSheet } from '@/preview/shell/command-palette'
import { goTo } from '@/preview/shell/nav'
import { SpacesDialog } from '@/preview/shell/spaces'

function storedTheme(): Theme {
  try {
    const v = localStorage.getItem(themeStorageKey)
    if (isTheme(v)) return v
  } catch {
    // storage blocked: fall back to dark
  }
  return 'dark'
}

function themeFromQuery(search: string): Theme | null {
  const v = new URLSearchParams(search).get('theme')
  return isTheme(v) ? v : null
}

function isTyping(t: EventTarget | null): boolean {
  if (!(t instanceof HTMLElement)) return false
  return t.isContentEditable || t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.tagName === 'SELECT'
}

/**
 * PreviewRoot wraps every /preview screen: the themed .kp root (data-theme, the tokens and Geist), the context
 * screens read with usePreview(), the portal host for dialogs, the ⌘K palette, the ? sheet, and the global keys
 * (⌘/Ctrl-K, ?, /, and G then O, B, I, P, Y or S). A ?theme=dark|light|high-contrast query sets the theme.
 * It renders without a session or API: everything comes from src/preview/mock.
 */
export function PreviewRoot() {
  const searchStr = useRouterState({ select: (s) => s.location.searchStr })
  const [theme, setThemeState] = useState<Theme>(() => themeFromQuery(searchStr) ?? storedTheme())
  const [portal, setPortal] = useState<HTMLElement | null>(null)
  const [orgId, setOrgId] = useState(orgs[0].id)
  const [paletteOpen, setPaletteOpen] = useState(false)
  const [shortcutsOpen, setShortcutsOpen] = useState(false)
  const [switcherOpen, setSwitcherOpen] = useState(false)
  const [spacesDialogOpen, setSpacesDialogOpen] = useState(false)
  const [selection, setSelection] = useState<PaletteSelection | null>(null)
  const navigate = usePreviewNavigate()

  const setTheme = useCallback((t: Theme) => {
    setThemeState(t)
    try {
      localStorage.setItem(themeStorageKey, t)
    } catch {
      // a per-viewer convenience only
    }
  }, [])

  // A ?theme= query sets the theme, and ?switcher=open opens the space switcher (mockup 1b), on each navigation.
  const [seenSearch, setSeenSearch] = useState<string | null>(null)
  if (seenSearch !== searchStr) {
    setSeenSearch(searchStr)
    const t = themeFromQuery(searchStr)
    if (t) setThemeState(t)
    if (new URLSearchParams(searchStr).get('switcher') === 'open') setSwitcherOpen(true)
  }

  const pendingG = useRef<number | null>(null)
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault()
        setPaletteOpen((o) => !o)
        return
      }
      if (e.metaKey || e.ctrlKey || e.altKey || e.defaultPrevented || isTyping(e.target)) return
      // Leave keys alone while a dialog or menu has focus.
      if (document.activeElement?.closest('[role="dialog"],[role="menu"]')) return
      if (e.key === '?') {
        e.preventDefault()
        setShortcutsOpen(true)
        return
      }
      if (e.key === '/') {
        const f = document.querySelector<HTMLInputElement>('[data-kp-filter]')
        if (f) {
          e.preventDefault()
          f.focus()
        }
        return
      }
      const k = e.key.toLowerCase()
      if (pendingG.current !== null) {
        window.clearTimeout(pendingG.current)
        pendingG.current = null
        if (k === 's') {
          e.preventDefault()
          setSwitcherOpen(true)
          return
        }
        const hit = goTo.find((a) => a.keys?.[1]?.toLowerCase() === k)
        if (hit?.href) {
          e.preventDefault()
          navigate(hit.href)
        }
        return
      }
      if (k === 'g') pendingG.current = window.setTimeout(() => (pendingG.current = null), 1200)
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [navigate])

  const value: PreviewContextValue = useMemo(
    () => ({
      theme,
      setTheme,
      portal,
      org: orgs.find((o) => o.id === orgId) ?? orgs[0],
      setOrgId,
      paletteOpen,
      setPaletteOpen,
      shortcutsOpen,
      setShortcutsOpen,
      switcherOpen,
      setSwitcherOpen,
      spacesDialogOpen,
      setSpacesDialogOpen,
      selection,
      setSelection,
    }),
    [theme, setTheme, portal, orgId, paletteOpen, shortcutsOpen, switcherOpen, spacesDialogOpen, selection],
  )

  return (
    <PreviewContext.Provider value={value}>
      <div className="kp ks-root" data-theme={theme}>
        <Outlet />
        <CommandPalette />
        <ShortcutsSheet />
        <SpacesDialog />
        <div className="kp-portal" ref={setPortal} />
      </div>
    </PreviewContext.Provider>
  )
}
