// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { Link, Navigate, Outlet, useLocation } from '@tanstack/react-router'
import { useEffect, useMemo } from 'react'
import { Logo } from '@/components/logo'
import { NoticeBanner } from '@/components/notice-banner'
import { OrgSwitcher } from '@/components/org-switcher'
import { UserMenu } from '@/components/user-menu'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { AuthService, type WhoAmIResponse } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { useInbox } from '@/hooks/use-inbox'
import { isApprover } from '@/lib/approvals'
import { OrgProvider } from '@/lib/org'
import { teamMemberships, useOrg } from '@/lib/org-context'
import { useSignOut } from '@/hooks/use-sign-out'
import { cn } from '@/lib/utils'
import { ChangePasswordPage } from '@/pages/change-password-page'
import { FirstLoginPage } from '@/pages/first-login-page'

/**
 * AppLayout gates every signed-in page on WhoAmI: no session goes to the
 * login page, a pending bootstrap password change shows only that form.
 */
export function AppLayout() {
  const location = useLocation()
  const who = useQuery(AuthService.method.whoAmI, {}, { retry: false, refetchOnWindowFocus: true })

  if (who.isPending) {
    return (
      <p className="p-6" role="status">
        Loading…
      </p>
    )
  }
  if (who.isError) {
    if (ConnectError.from(who.error).code === Code.Unauthenticated) {
      // While the redirect is pending this layout re-renders at the login
      // URL; redirecting again from there would nest the redirect forever.
      if (location.pathname === '/login') return null
      return <Navigate to="/login" search={{ redirect: location.href }} replace />
    }
    return (
      <div className="p-6">
        <Alert variant="destructive">
          <AlertTitle>Could not reach Koinon</AlertTitle>
          <AlertDescription>
            <p>{who.error.message}</p>
            <Button variant="outline" size="sm" className="mt-2" onClick={() => void who.refetch()}>
              Try again
            </Button>
          </AlertDescription>
        </Alert>
      </div>
    )
  }
  if (who.data.passwordChangeRequired) return <ChangePasswordPage />
  return <SignedIn who={who.data} />
}

/**
 * SignedIn shows the org shell to a user with a team org. A user with no team org gets the first-login screen, and a
 * platform admin with no team org (the bootstrap admin of a fresh install) gets the platform console, its way in.
 */
function SignedIn({ who }: { who: WhoAmIResponse }) {
  const teams = useMemo(() => teamMemberships(who.memberships, who.user?.personalOrgId), [who.memberships, who.user?.personalOrgId])
  if (teams.length === 0) {
    if (who.platformAdmin) return <PlatformShell who={who} />
    return <FirstLoginPage name={who.user?.displayName || who.user?.email || ''} />
  }
  return (
    <OrgProvider memberships={teams}>
      <Shell who={who} />
    </OrgProvider>
  )
}

/**
 * PlatformShell is the shell of a platform admin with no team org: the platform pages need no org, the org pages do,
 * so every other URL goes to the platform console.
 */
function PlatformShell({ who }: { who: WhoAmIResponse }) {
  const location = useLocation()
  const { signOut, signingOut } = useSignOut()
  if (location.pathname !== '/platform' && !location.pathname.startsWith('/platform/')) return <Navigate to="/platform" replace />
  const name = who.user?.displayName || who.user?.email || 'You'
  return (
    <div className="flex min-h-screen flex-col md:flex-row">
      <a href="#main" className={skipLink}>
        Skip to content
      </a>
      <aside className={sidebar}>
        <Link to="/platform" aria-label="Koinon" className="block px-3">
          <Logo />
        </Link>
        <nav aria-label="Platform">
          <Link to="/platform" className={navLink} activeOptions={{ exact: true }} activeProps={active}>
            Platform console
          </Link>
        </nav>
        <div className="grid gap-1 md:mt-auto">
          <UserMenu name={name} email={who.user?.email ?? ''} onSignOut={signOut} signingOut={signingOut} />
        </div>
      </aside>
      <main id="main" tabIndex={-1} className="min-w-0 flex-1 px-4 py-6 outline-none md:px-6">
        <p role="note" className="text-muted-foreground mx-auto mb-4 max-w-3xl text-sm">
          You are not a member of an org yet. An org admin must add you to their org to use the org pages.
        </p>
        <Outlet />
      </main>
    </div>
  )
}

function Shell({ who }: { who: WhoAmIResponse }) {
  const { org } = useOrg()
  const { signOut, signingOut } = useSignOut()
  const admin = isApprover(org.role)
  // The agents live on the overview (its Agents and Chart tabs), so an agent page and the new-agent flow are in the
  // Overview section: the item shows as current with aria-current="true", not "page" (issue #1274).
  const inAgents = useLocation().pathname.startsWith('/agents/')
  // The same polled queries the inbox page uses: one request serves both.
  const waiting = useInbox().items.length
  useDocumentTitle(waiting)
  const name = who.user?.displayName || who.user?.email || 'You'
  return (
    <div className="flex min-h-screen flex-col md:flex-row">
      <a href="#main" className={skipLink}>
        Skip to content
      </a>
      <aside className={sidebar}>
        <Link to="/" aria-label="Koinon" className="block px-3">
          <Logo />
        </Link>
        <OrgSwitcher platformAdmin={who.platformAdmin} />
        <nav aria-label="Main" className="flex flex-wrap gap-1 md:grid">
          <Link to="/" className={navLink} activeOptions={{ exact: true, includeSearch: false }} activeProps={active} {...(inAgents ? section : {})}>
            Overview
          </Link>
          <Link to="/inbox" className={navLink} activeProps={active}>
            Inbox
            {waiting > 0 && (
              <span data-testid="inbox-badge" aria-label={`${waiting} waiting on you`} className="ml-2 inline-flex min-w-5 items-center justify-center rounded-full bg-attention text-attention-foreground px-1.5 text-xs font-semibold">
                {waiting}
              </span>
            )}
          </Link>
          <Link to="/connections" className={navLink} activeProps={active}>
            Connections
          </Link>
        </nav>
        {admin && (
          <nav aria-labelledby="settings-nav" className="flex flex-wrap items-center gap-1 md:mt-3 md:grid">
            <h2 id="settings-nav" className="text-muted-foreground px-3 text-xs font-semibold tracking-wide uppercase md:pb-1">
              Settings
            </h2>
            {settingsItems.map((i) => (
              <Link key={i.to} to={i.to} className={navLink} activeProps={active}>
                {i.label}
              </Link>
            ))}
          </nav>
        )}
        <div className="grid gap-1 md:mt-auto">
          {who.platformAdmin && (
            <nav aria-label="Platform">
              <Link to="/platform" className={navLink} activeProps={active}>
                Platform console
              </Link>
            </nav>
          )}
          <UserMenu name={name} email={who.user?.email ?? ''} onSignOut={signOut} signingOut={signingOut} />
        </div>
      </aside>
      <div className="flex min-w-0 flex-1 flex-col">
        {admin && <NoticeBanner key={org.orgId} orgId={org.orgId} platformAdmin={who.platformAdmin} />}
        <div className="flex min-w-0 flex-1">
          <main id="main" tabIndex={-1} className="min-w-0 flex-1 px-4 py-6 outline-none md:px-6">
            {/* Remount pages when the org changes so no query mixes two orgs. */}
            <div key={org.orgId}>
              <Outlet />
            </div>
          </main>
          <ConciergeSlot />
        </div>
      </div>
    </div>
  )
}

const skipLink = 'bg-raised sr-only z-50 rounded-md px-3 py-1.5 text-sm focus:not-sr-only focus:absolute focus:top-2 focus:left-2'
const sidebar = 'bg-card flex w-full shrink-0 flex-col gap-3 border-b p-3 md:sticky md:top-0 md:h-screen md:w-60 md:overflow-y-auto md:border-r md:border-b-0'
const navLink = 'block rounded-md px-3 py-1.5 text-sm hover:bg-accent'
const active = { 'aria-current': 'page' as const, className: 'bg-accent font-medium' }
/** A section item: the current page is under it (an agent page under Overview). It looks like `active`. */
const section = { 'aria-current': true as const, className: cn(navLink, 'bg-accent font-medium') }

/** The Settings group of the org sidebar, in the order of ADR 0126 section 4. Only org admins and owners see it. */
const settingsItems = [
  { to: '/settings/org', label: 'Org settings' },
  { to: '/settings/providers', label: 'Providers' },
  { to: '/mcp-servers', label: 'MCP servers' },
  { to: '/settings/prices', label: 'Prices' },
  { to: '/settings/budgets', label: 'Budgets' },
  { to: '/settings/observability', label: 'Observability' },
  { to: '/settings/tokens', label: 'API tokens' },
] as const

/**
 * ConciergeSlot reserves the layout region of the concierge side panel (ADR 0126 section 6, ADR 0067). The concierge
 * comes in v0.3: until then the region renders nothing, with no disabled control and no placeholder text.
 */
function ConciergeSlot() {
  return <div data-slot="concierge" hidden />
}

/** useDocumentTitle puts the waiting count in the tab title so a background tab shows it. */
function useDocumentTitle(waiting: number) {
  useEffect(() => {
    const base = document.title.replace(/^\(\d+\) /, '')
    document.title = waiting > 0 ? `(${waiting}) ${base}`.trim() : base
    return () => {
      document.title = base
    }
  }, [waiting])
}
