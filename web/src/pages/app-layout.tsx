// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { Link, Navigate, Outlet, useLocation, useNavigate } from '@tanstack/react-router'
import { useState } from 'react'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { AuthService, type WhoAmIResponse } from '@/gen/koinon/auth/v1alpha1/auth_pb'
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

  return (
    <div className="mx-auto flex min-h-screen max-w-7xl flex-col">
      <header className="flex flex-wrap items-center gap-4 border-b px-4 py-3">
        <Link to="/" className="text-lg font-semibold">
          Koinon
        </Link>
        <nav aria-label="Main" className="flex gap-3 text-sm">
          <Link to="/" className="hover:underline" activeOptions={{ exact: true }} activeProps={{ 'aria-current': 'page', className: 'font-medium' }}>
            Board
          </Link>
          <Link to="/agents" className="hover:underline" activeProps={{ 'aria-current': 'page', className: 'font-medium' }}>
            Agents
          </Link>
          <Link to="/connections" className="hover:underline" activeProps={{ 'aria-current': 'page', className: 'font-medium' }}>
            Connections
          </Link>
        </nav>
        <div className="ml-auto flex items-center gap-3 text-sm">
          {memberships.length > 1 ? (
            <Select value={org.orgId} onValueChange={setOrgId}>
              <SelectTrigger size="sm" aria-label="Organization">
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
        </div>
      </header>
      <main className="flex-1 px-4 py-6">
        {/* Remount pages when the org changes so no query mixes two orgs. */}
        <div key={org.orgId}>
          <Outlet />
        </div>
      </main>
    </div>
  )
}
