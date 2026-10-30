// SPDX-License-Identifier: Apache-2.0

import { Activity, Inbox, LayoutDashboard, LayoutGrid, Menu, Search, Users } from 'lucide-react'
import { Fragment, type ReactNode } from 'react'
import { cn } from '@/lib/utils'
import { Kbd } from '@/preview/components/layout'
import { isTheme, themes, usePreview } from '@/preview/lib/context'
import { PreviewLink } from '@/preview/lib/link'
import { usePathname } from '@/preview/lib/navigation'
import { me } from '@/preview/mock/data'
import type { Space } from '@/preview/shell/nav'
import { Sidebar } from '@/preview/shell/sidebar'
import { useSpaceMeta } from '@/preview/lib/space-meta'

/** Crumb is one step of the top bar's breadcrumbs after the space; the last is the current page. */
export type Crumb = { label: ReactNode; href?: string; mono?: boolean }

/** ThemeSwitch picks dark, light (L2) or high contrast for the whole preview. */
export function ThemeSwitch({ id = 'kp-theme' }: { id?: string }) {
  const { theme, setTheme } = usePreview()
  return (
    <span className="kp-theme">
      <label htmlFor={id} className="ks-meta">
        Theme
      </label>
      <select id={id} className="kn-field kn-native-select" value={theme} onChange={(e) => isTheme(e.target.value) && setTheme(e.target.value)}>
        {themes.map((t) => (
          <option key={t.id} value={t.id}>
            {t.label}
          </option>
        ))}
      </select>
    </span>
  )
}

/** TopBar is the breadcrumbs, the ⌘K button, the theme switch and the person. */
export function TopBar({ space, crumbs }: { space: Space; crumbs: Crumb[] }) {
  const { setPaletteOpen } = usePreview()
  const meta = useSpaceMeta(space)
  const all: Crumb[] = [{ label: meta.name }, ...crumbs]
  return (
    <header className="ks-top">
      <nav className="ks-crumbs" aria-label="Breadcrumbs">
        {all.map((c, i) => {
          const last = i === all.length - 1
          const cls = c.mono ? 'ks-mono' : undefined
          return (
            <Fragment key={i}>
              {i > 0 && <span aria-hidden="true">/</span>}
              {last ? (
                <strong className={cls} aria-current="page">
                  {c.label}
                </strong>
              ) : c.href ? (
                <PreviewLink className={cn('ks-a', cls)} href={c.href}>
                  {c.label}
                </PreviewLink>
              ) : (
                <span className={cls}>{c.label}</span>
              )}
            </Fragment>
          )
        })}
      </nav>
      <div className="ks-row" style={{ marginLeft: 'auto' }}>
        <button type="button" className="ks-cmdk-btn" onClick={() => setPaletteOpen(true)} aria-keyshortcuts="Meta+K Control+K">
          <Search className="ks-icon" aria-hidden="true" />
          <span style={{ flex: 1, textAlign: 'left' }}>Search or run a command</span>
          <Kbd>⌘K</Kbd>
        </button>
        <span className="ks-hide-sm">
          <ThemeSwitch />
        </span>
        <span className="ks-user ks-hide-sm">
          <span className="ks-face" aria-hidden="true">
            {me.initials}
          </span>
          {me.name}
        </span>
      </div>
    </header>
  )
}

/** MobileBar is the phone's bottom bar: three places of the space, Search (the palette) and Spaces. */
export function MobileBar({ space }: { space: Space }) {
  const { setPaletteOpen, setSpacesDialogOpen } = usePreview()
  const pathname = usePathname()
  const places: { label: string; href: string; icon: ReactNode; count?: number }[] =
    space === 'platform'
      ? [
          { label: 'Orgs', href: '/preview/platform/orgs', icon: <Users className="ks-icon" aria-hidden="true" /> },
          { label: 'Agents', href: '/preview/platform/agents', icon: <LayoutGrid className="ks-icon" aria-hidden="true" /> },
          { label: 'Usage', href: '/preview/platform/usage', icon: <Activity className="ks-icon" aria-hidden="true" /> },
        ]
      : space === 'idion'
        ? [
            { label: 'Home', href: '/preview/idion', icon: <LayoutDashboard className="ks-icon" aria-hidden="true" /> },
            { label: 'Inbox', href: '/preview/inbox', icon: <Inbox className="ks-icon" aria-hidden="true" />, count: 1 },
            { label: 'Runs', href: '/preview/runs', icon: <Activity className="ks-icon" aria-hidden="true" /> },
          ]
        : [
            { label: 'Overview', href: '/preview/overview', icon: <LayoutDashboard className="ks-icon" aria-hidden="true" /> },
            { label: 'Inbox', href: '/preview/inbox', icon: <Inbox className="ks-icon" aria-hidden="true" />, count: 7 },
            { label: 'Runs', href: '/preview/runs', icon: <Activity className="ks-icon" aria-hidden="true" /> },
          ]
  return (
    <nav className="ks-bar-mobile" aria-label="Main, phone">
      {places.map((p) => (
        <PreviewLink key={p.label} href={p.href} aria-current={pathname === p.href ? 'page' : undefined}>
          {p.icon}
          {p.label}
          {p.count ? (
            <span className="kn-count" aria-label={`${p.count} waiting on you`}>
              {p.count}
            </span>
          ) : null}
        </PreviewLink>
      ))}
      <button type="button" onClick={() => setPaletteOpen(true)}>
        <Search className="ks-icon" aria-hidden="true" />
        Search
      </button>
      <button type="button" onClick={() => setSpacesDialogOpen(true)}>
        <Menu className="ks-icon" aria-hidden="true" />
        Spaces
      </button>
    </nav>
  )
}

export type PreviewShellProps = {
  /** Which sidebar to show. */
  space: Space
  /** The sidebar label of this page, such as "Overview" or "Spend & usage". */
  current?: string
  /** Breadcrumbs after the space name, which the shell adds. */
  crumbs: Crumb[]
  /** Rendered between the top bar and the page, such as <NoticeBanner />. */
  banner?: ReactNode
  /** 48rem column instead of 76rem. */
  narrow?: boolean
  /** Renders children straight into <main> without the centred page column. */
  bare?: boolean
  children: ReactNode
}

/**
 * PreviewShell is the app frame every screen sits in: sidebar (hidden on phones), top bar with breadcrumbs, the
 * page column and the phone's bottom bar. The command palette and the ? sheet are mounted once by PreviewRoot.
 */
export function PreviewShell({ space, current, crumbs, banner, narrow, bare, children }: PreviewShellProps) {
  return (
    <div className="ks-shell">
      <a className="kp-skip kn-btn kn-btn-outline kn-btn-size-sm" href="#kp-main">
        Skip to content
      </a>
      <div className="ks-side-slot">
        <div className="kp-side">
          <Sidebar space={space} current={current} />
        </div>
      </div>
      <div className="ks-col">
        <TopBar space={space} crumbs={crumbs} />
        {banner}
        <main className="ks-main" id="kp-main" tabIndex={-1}>
          {bare ? children : <div className={cn('ks-page', narrow && 'ks-page-narrow')}>{children}</div>}
        </main>
        <MobileBar space={space} />
      </div>
    </div>
  )
}
