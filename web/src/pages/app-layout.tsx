// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { Code, ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { Link, Navigate, Outlet, useLocation } from '@tanstack/react-router'
import { ShieldIcon } from 'lucide-react'
import { useEffect, useMemo } from 'react'
import { Logo } from '@/components/logo'
import { NoticeBanner } from '@/components/notice-banner'
import { OrgSwitcher } from '@/components/org-switcher'
import { UserMenu } from '@/components/user-menu'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { QueryError } from '@/components/ui/inline-error'
import { AuthService, MembershipSchema, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { PlatformService } from '@/gen/koinon/platform/v1alpha1/platform_pb'
import { useInbox } from '@/hooks/use-inbox'
import { isApprover } from '@/lib/approvals'
import { OrgProvider } from '@/lib/org'
import { OrgContext, orgName, teamMemberships, useOrg, useOrgSession, type OrgContextValue } from '@/lib/org-context'
import { navActive, navGroup, navGroupHeading, navLink, sidebar, skipLink } from '@/lib/shell-styles'
import { useSignOut } from '@/hooks/use-sign-out'
import { cn } from '@/lib/utils'
import { ChangePasswordPage } from '@/pages/change-password-page'
import { FirstLoginPage } from '@/pages/first-login-page'

/**
 * AppLayout gates every signed-in page on WhoAmI: no session goes to the
 * login page, a pending bootstrap password change shows only that form.
 * A user with no team org gets the first-login screen, except a platform
 * admin (the bootstrap admin of a fresh install), whose way in is the
 * platform console. The org area and the platform console are the two
 * areas under this layout (ADR 0126 section 4).
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
  if (!who.data.platformAdmin && teamMemberships(who.data.memberships, who.data.user?.personalOrgId).length === 0) {
    return <FirstLoginPage name={who.data.user?.displayName || who.data.user?.email || ''} />
  }
  return (
    <OrgProvider who={who.data}>
      <Outlet />
    </OrgProvider>
  )
}

/**
 * OrgArea is the org area: the org sidebar and the org pages, for one org at a time. A platform admin who opened an
 * org from the console without being a member of it works in that org; a platform admin with no team org and no such
 * org goes to the console.
 */
export function OrgArea() {
  const s = useOrgSession()
  const member = useMemo<OrgContextValue | undefined>(
    () => (s.lastOrg ? { org: s.lastOrg, memberships: s.memberships, setOrgId: s.openOrg, acting: false } : undefined),
    [s.lastOrg, s.memberships, s.openOrg],
  )
  if (s.actingOrgId) return <ActingOrgArea key={s.actingOrgId} orgId={s.actingOrgId} />
  if (!member) return <Navigate to="/platform" replace />
  return (
    <OrgContext.Provider value={member}>
      <Shell />
    </OrgContext.Provider>
  )
}

/**
 * ActingOrgArea is the org area of an org that a platform admin opened without being a member of it. A platform admin
 * holds every role in every org (auth.Principal.HasRole), so the UI shows the org as to an owner.
 */
function ActingOrgArea({ orgId }: { orgId: string }) {
  const s = useOrgSession()
  const got = useQuery(PlatformService.method.getOrg, { orgId })
  const summary = got.data?.org
  const value = useMemo<OrgContextValue | undefined>(
    () =>
      summary && {
        org: create(MembershipSchema, { orgId: summary.orgId, orgSlug: summary.slug, orgDisplayName: summary.displayName, role: OrgRole.OWNER }),
        memberships: s.memberships,
        setOrgId: s.openOrg,
        acting: true,
      },
    [summary, s.memberships, s.openOrg],
  )
  if (got.isPending) {
    return (
      <main id="main" className="p-6">
        <p role="status">Loading…</p>
      </main>
    )
  }
  if (!value) {
    return (
      <main id="main" className="mx-auto grid max-w-xl gap-3 p-6">
        <h1 className="text-xl font-semibold">Could not open the org</h1>
        <QueryError error={got.error} />
        <p className="flex flex-wrap items-center gap-3">
          <Button variant="outline" size="sm" onClick={() => void got.refetch()}>
            Try again
          </Button>
          <Link to="/platform/orgs" className="text-link font-medium hover:underline">
            Back to platform console
          </Link>
        </p>
      </main>
    )
  }
  return (
    <OrgContext.Provider value={value}>
      <Shell />
    </OrgContext.Provider>
  )
}

function Shell() {
  const { who } = useOrgSession()
  const { org, acting } = useOrg()
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
          <Link to="/" className={navLink} activeOptions={{ exact: true, includeSearch: false }} activeProps={navActive} {...(inAgents ? section : {})}>
            Overview
          </Link>
          <Link to="/inbox" className={navLink} activeProps={navActive}>
            Inbox
            {waiting > 0 && (
              <span data-testid="inbox-badge" aria-label={`${waiting} waiting on you`} className="ml-2 inline-flex min-w-5 items-center justify-center rounded-full bg-attention text-attention-foreground px-1.5 text-xs font-semibold">
                {waiting}
              </span>
            )}
          </Link>
          <Link to="/connections" className={navLink} activeProps={navActive}>
            Connections
          </Link>
        </nav>
        {admin && (
          <nav aria-labelledby="settings-nav" className={navGroup}>
            <h2 id="settings-nav" className={navGroupHeading}>
              Settings
            </h2>
            {settingsItems.map((i) => (
              <Link key={i.to} to={i.to} className={navLink} activeProps={navActive}>
                {i.label}
              </Link>
            ))}
          </nav>
        )}
        <div className="grid gap-1 md:mt-auto">
          {who.platformAdmin && (
            <nav aria-label="Platform">
              <Link to="/platform" className={navLink} activeProps={navActive}>
                Platform console
              </Link>
            </nav>
          )}
          <UserMenu name={name} email={who.user?.email ?? ''} onSignOut={signOut} signingOut={signingOut} />
        </div>
      </aside>
      <div className="flex min-w-0 flex-1 flex-col">
        {acting && <ActingBanner name={orgName(org)} />}
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

/**
 * ActingBanner shows on every page of an org that a platform admin opened without being a member of it, until the
 * admin goes back to the platform console (ADR 0126 section 4; board "Org opened from the console: acting as platform
 * admin").
 */
function ActingBanner({ name }: { name: string }) {
  return (
    <section aria-label="Acting as platform admin" data-testid="acting-banner" className="bg-attention text-attention-foreground flex flex-wrap items-center gap-3 px-4 py-2 text-sm font-medium md:px-6">
      <ShieldIcon aria-hidden className="size-4 shrink-0" />
      <p className="min-w-0 flex-1">
        You act as platform admin in <strong>{name}</strong>. You are not a member of this org.
      </p>
      <Link to="/platform/orgs" className="rounded-md border border-current px-3 py-1 font-semibold underline-offset-4 hover:underline">
        Back to platform console
      </Link>
    </section>
  )
}

/** A section item: the current page is under it (an agent page under Overview). It looks like `navActive`. */
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
