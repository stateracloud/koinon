// SPDX-License-Identifier: Apache-2.0

import { Link, Navigate, Outlet } from '@tanstack/react-router'
import { ChevronLeftIcon } from 'lucide-react'
import { useEffect } from 'react'
import { Logo } from '@/components/logo'
import { UserMenu } from '@/components/user-menu'
import { useSignOut } from '@/hooks/use-sign-out'
import { orgName, useOrgSession } from '@/lib/org-context'
import { navActive, navGroup, navGroupHeading, navLink, sidebar, skipLink } from '@/lib/shell-styles'

/**
 * The sidebar of the platform console (ADR 0126 section 4), in the order of the board "Platform console: home". The
 * usage links match their exact path: "Usage" is /platform/usage, and the other pages are under it.
 */
const usageItems = [
  { to: '/platform/usage', label: 'Usage' },
  { to: '/platform/usage/budgets', label: 'Budget ceilings' },
  { to: '/platform/usage/prices', label: 'Prices' },
] as const
const resourceItems = [
  { to: '/platform/providers', label: 'Providers' },
  { to: '/platform/templates', label: 'Templates' },
  { to: '/platform/mcp-servers', label: 'MCP servers' },
] as const
const settingsItems = [{ to: '/platform/settings/observability', label: 'Observability' }] as const

/**
 * PlatformLayout is the platform console: its own area of the app at /platform, with its own sidebar (ADR 0126
 * section 4). Only platform admins can open it; any other user goes to the org area. Opening the console ends the
 * "acting as platform admin" state of an org.
 */
export function PlatformLayout() {
  const s = useOrgSession()
  const { signOut, signingOut } = useSignOut()
  const { endActing } = s
  useEffect(() => endActing(), [endActing])
  if (!s.platformAdmin) return <Navigate to="/" replace />
  const name = s.who.user?.displayName || s.who.user?.email || 'You'
  return (
    <div className="flex min-h-screen flex-col md:flex-row">
      <a href="#main" className={skipLink}>
        Skip to content
      </a>
      <aside className={sidebar}>
        <div className="grid gap-2">
          <Link to="/platform" aria-label="Koinon platform console" className="block px-3">
            <Logo />
          </Link>
          <span className="bg-attention text-attention-foreground mx-3 w-fit rounded-md px-2 py-0.5 text-xs font-semibold">Platform console</span>
        </div>
        <nav aria-label="Platform console" className="flex flex-wrap gap-1 md:grid">
          <Link to="/platform" className={navLink} activeOptions={{ exact: true }} activeProps={navActive}>
            Home
          </Link>
          <Link to="/platform/orgs" className={navLink} activeProps={navActive}>
            Orgs
          </Link>
        </nav>
        <nav aria-labelledby="platform-usage-nav" className={navGroup}>
          <h2 id="platform-usage-nav" className={navGroupHeading}>
            Usage and cost
          </h2>
          {usageItems.map((i) => (
            <Link key={i.to} to={i.to} className={navLink} activeOptions={{ exact: true }} activeProps={navActive}>
              {i.label}
            </Link>
          ))}
        </nav>
        <nav aria-labelledby="platform-resources-nav" className={navGroup}>
          <h2 id="platform-resources-nav" className={navGroupHeading}>
            Org resources
          </h2>
          {resourceItems.map((i) => (
            <Link key={i.to} to={i.to} className={navLink} activeProps={navActive}>
              {i.label}
            </Link>
          ))}
        </nav>
        <nav aria-labelledby="platform-settings-nav" className={navGroup}>
          <h2 id="platform-settings-nav" className={navGroupHeading}>
            Platform settings
          </h2>
          {settingsItems.map((i) => (
            <Link key={i.to} to={i.to} className={navLink} activeProps={navActive}>
              {i.label}
            </Link>
          ))}
        </nav>
        <div className="grid gap-1 md:mt-auto">
          {s.lastOrg && (
            <Link to="/" className={`${navLink} flex items-center gap-2 border`}>
              <ChevronLeftIcon aria-hidden className="size-4 shrink-0" />
              <span className="truncate">Back to {orgName(s.lastOrg)}</span>
            </Link>
          )}
          <UserMenu name={name} email={s.who.user?.email ?? ''} onSignOut={signOut} signingOut={signingOut} />
        </div>
      </aside>
      <main id="main" tabIndex={-1} className="min-w-0 flex-1 px-4 py-6 outline-none md:px-6">
        {!s.lastOrg && (
          <p role="note" className="text-muted-foreground mx-auto mb-4 max-w-3xl text-sm">
            You are not a member of an org yet. You can open an org from the Orgs page as platform admin.
          </p>
        )}
        <Outlet />
      </main>
    </div>
  )
}
