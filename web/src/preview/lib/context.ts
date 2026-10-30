// SPDX-License-Identifier: Apache-2.0

import { createContext, useContext, useEffect, useEffectEvent } from 'react'
import { orgs, type Org } from '@/preview/mock/data'

/** Theme is one of the design system's three value sets for the same token names. */
export type Theme = 'dark' | 'light' | 'high-contrast'

export const themes: { id: Theme; label: string }[] = [
  { id: 'dark', label: 'Dark' },
  { id: 'light', label: 'Light' },
  { id: 'high-contrast', label: 'High contrast' },
]

export function isTheme(v: unknown): v is Theme {
  return v === 'dark' || v === 'light' || v === 'high-contrast'
}

/** PaletteAction is one row of the command palette. Give it an href to navigate, or onSelect to act. */
export type PaletteAction = {
  label: string
  /** Quiet text after the label, such as a run ID or a cost. */
  hint?: string
  /** Key caps shown on the right, such as ['G', 'I'] or ['A']. */
  keys?: string[]
  href?: string
  onSelect?: () => void
}

/** PaletteSelection is what a page has selected; its actions come first in the palette. */
export type PaletteSelection = {
  /** Changes whenever the selection changes; the palette re-reads the actions then. */
  key: string
  /** The badge in the palette's input row, such as the task title. */
  title: string
  /** The group heading, such as "For the selected task". */
  heading: string
  actions: PaletteAction[]
}

export type PreviewContextValue = {
  theme: Theme
  setTheme: (t: Theme) => void
  /** The element dialogs, drawers and menus portal into: inside the themed root, so tokens resolve. */
  portal: HTMLElement | null
  org: Org
  setOrgId: (id: string) => void
  paletteOpen: boolean
  setPaletteOpen: (open: boolean) => void
  shortcutsOpen: boolean
  setShortcutsOpen: (open: boolean) => void
  switcherOpen: boolean
  setSwitcherOpen: (open: boolean) => void
  spacesDialogOpen: boolean
  setSpacesDialogOpen: (open: boolean) => void
  selection: PaletteSelection | null
  setSelection: (s: PaletteSelection | null) => void
}

const noop = () => {}

export const PreviewContext = createContext<PreviewContextValue>({
  theme: 'dark',
  setTheme: noop,
  portal: null,
  org: orgs[0],
  setOrgId: noop,
  paletteOpen: false,
  setPaletteOpen: noop,
  shortcutsOpen: false,
  setShortcutsOpen: noop,
  switcherOpen: false,
  setSwitcherOpen: noop,
  spacesDialogOpen: false,
  setSpacesDialogOpen: noop,
  selection: null,
  setSelection: noop,
})

/** usePreview gives a screen the theme, the current org and the palette and sheet controls. */
export function usePreview(): PreviewContextValue {
  return useContext(PreviewContext)
}

/** usePortal is the container for radix portals in the preview (Dialog, Drawer, menus). */
export function usePortal(): HTMLElement | null {
  return useContext(PreviewContext).portal
}

/**
 * usePaletteSelection tells the command palette what this page has selected, so ⌘K acts on it: its actions come
 * first. Pass null when nothing is selected. The selection is cleared when the page unmounts.
 */
export function usePaletteSelection(selection: PaletteSelection | null): void {
  const { setSelection } = useContext(PreviewContext)
  const key = selection?.key ?? ''
  // Re-read only when the key changes: callers rebuild the selection object on every render.
  const publish = useEffectEvent(() => setSelection(selection))
  useEffect(() => {
    publish()
  }, [key])
  useEffect(() => () => setSelection(null), [setSelection])
}

/** themeStorageKey keeps the viewer's theme between visits (a per-viewer convenience only). */
export const themeStorageKey = 'koinon-preview-theme'
