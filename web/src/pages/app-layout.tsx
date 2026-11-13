// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { Link, Navigate, Outlet, useLocation, useNavigate } from '@tanstack/react-router'
import { useEffect, useState } from 'react'
import { NoticeBanner } from '@/components/notice-banner'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { AuthService, type WhoAmIResponse } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { useInbox } from '@/hooks/use-inbox'
import { isApprover } from '@/lib/approvals'
import { OrgProvider } from '@/lib/org'
import { useOrg } from '@/lib/org-context'
import { logout } from '@/lib/session'
import { ChangePasswordPage } from '@/pages/change-password-page'

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
  return (
    <OrgProvider memberships={who.data.memberships}>
      <Shell who={who.data} />
    </OrgProvider>
  )
}

function Shell({ who }: { who: WhoAmIResponse }) {
  const { org, memberships, setOrgId } = useOrg()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const [loggingOut, setLoggingOut] = useState(false)

  async function onLogout() {
    setLoggingOut(true)
    try {
      await logout()
    } finally {
      queryClient.clear()
      await navigate({ to: '/login', search: {} })
    }
  }

  const navLink = 'block rounded-md px-3 py-1.5 text-sm hover:bg-accent'
  const active = { 'aria-current': 'page' as const, className: 'bg-accent font-medium' }
  const admin = isApprover(org.role)
  // The same polled queries the inbox page uses: one request serves both.
  const waiting = useInbox().items.length
  useDocumentTitle(waiting)
  return (
    <div className="flex min-h-screen flex-col md:flex-row">
      <aside className="bg-card w-full shrink-0 border-b p-3 md:w-56 md:border-r md:border-b-0">
        <Link to="/" className="mb-2 block px-3 text-lg font-semibold md:mb-4">
          Koinon
        </Link>
        <nav aria-label="Main" className="flex flex-wrap gap-1 md:grid">
          <Link to="/" className={navLink} activeOptions={{ exact: true }} activeProps={active}>
            Inbox
            {waiting > 0 && (
              <span data-testid="inbox-badge" aria-label={`${waiting} waiting on you`} className="ml-2 inline-flex min-w-5 items-center justify-center rounded-full bg-amber-600 px-1.5 text-xs font-semibold text-white">
                {waiting}
              </span>
            )}
          </Link>
          <Link to="/board" className={navLink} activeProps={active}>
            Board
          </Link>
          <Link to="/agents" className={navLink} activeProps={active}>
            Agents
          </Link>
          <Link to="/connections" className={navLink} activeProps={active}>
            Connections
          </Link>
        </nav>
        {admin && (
          <nav aria-labelledby="settings-nav" className="mt-2 flex flex-wrap items-center gap-1 md:mt-6 md:grid">
            <h2 id="settings-nav" className="text-muted-foreground px-3 text-xs font-semibold tracking-wide uppercase md:pb-1">
              Settings
            </h2>
            <Link to="/settings/providers" className={navLink} activeProps={active}>
              Providers
            </Link>
            <Link to="/servers" className={navLink} activeProps={active}>
              MCP servers
            </Link>
            <Link to="/settings/prices" className={navLink} activeProps={active}>
              Prices
            </Link>
            <Link to="/settings/budgets" className={navLink} activeProps={active}>
              Budgets
            </Link>
            <Link to="/settings/observability" className={navLink} activeProps={active}>
              Observability
            </Link>
            <Link to="/settings/tokens" className={navLink} activeProps={active}>
              API tokens
            </Link>
          </nav>
        )}
        {who.platformAdmin && (
          <nav aria-labelledby="platform-nav" className="mt-2 flex flex-wrap items-center gap-1 md:mt-6 md:grid">
            <h2 id="platform-nav" className="text-muted-foreground px-3 text-xs font-semibold tracking-wide uppercase md:pb-1">
              Platform
            </h2>
            <Link to="/platform/budgets" className={navLink} activeProps={active}>
              Budget ceilings
            </Link>
            <Link to="/platform/prices" className={navLink} activeProps={active}>
              Platform prices
            </Link>
            <Link to="/platform/observability" className={navLink} activeProps={active}>
              Observability
            </Link>
          </nav>
        )}
      </aside>
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex flex-wrap items-center justify-end gap-3 border-b px-4 py-3 text-sm">
          {memberships.length > 1 ? (
            <Select value={org.orgId} onValueChange={setOrgId}>
              <SelectTrigger size="sm" aria-label="Org">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {memberships.map((m) => (
                  <SelectItem key={m.orgId} value={m.orgId}>
                    {m.orgDisplayName || m.orgSlug}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          ) : (
            <span>{org.orgDisplayName || org.orgSlug}</span>
          )}
          <span className="text-muted-foreground">{who.user?.displayName || who.user?.email}</span>
          <Button variant="outline" size="sm" onClick={() => void onLogout()} disabled={loggingOut}>
            Log out
          </Button>
        </header>
        {admin && <NoticeBanner key={org.orgId} orgId={org.orgId} platformAdmin={who.platformAdmin} />}
        {who.platformAdmin && !memberships.some((m) => m.orgId === org.orgId) && (
          <p role="note" className="bg-muted px-4 py-1.5 text-xs md:px-6">
            You are acting as a platform admin in an org you are not a member of.
          </p>
        )}
        <main className="min-w-0 flex-1 px-4 py-6 md:px-6">
          {/* Remount pages when the org changes so no query mixes two orgs. */}
          <div key={org.orgId}>
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  )
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
