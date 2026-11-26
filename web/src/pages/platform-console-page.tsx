// SPDX-License-Identifier: Apache-2.0

import { useQuery } from '@connectrpc/connect-query'
import { Link } from '@tanstack/react-router'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { PageHeader } from '@/components/ui/page-header'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'

const items = [
  { to: '/platform/budgets', label: 'Budget ceilings', text: 'The cap, the spend and the platform ceiling of every org.' },
  { to: '/platform/prices', label: 'Platform prices', text: 'The model prices of the platform.' },
  { to: '/platform/observability', label: 'Observability', text: 'Where the platform sends its traces.' },
] as const

/**
 * PlatformConsolePage is the entry of the platform console at /platform (ADR 0126 section 4). This first version
 * links the three platform pages; the console area with its own sidebar comes with issue #1278.
 */
export function PlatformConsolePage() {
  const who = useQuery(AuthService.method.whoAmI, {})
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <PageHeader title="Platform console" description="Settings for all the orgs of this Koinon." />
      {who.isPending ? (
        <p role="status">Loading…</p>
      ) : who.data?.platformAdmin ? (
        <ul className="grid gap-3">
          {items.map((i) => (
            <li key={i.to} className="bg-card rounded-lg border p-4">
              <Link to={i.to} className="text-link font-medium hover:underline">
                {i.label}
              </Link>
              <p className="text-muted-foreground text-sm">{i.text}</p>
            </li>
          ))}
        </ul>
      ) : (
        <Alert role="alert">
          <AlertDescription>Only platform admins can open the platform console.</AlertDescription>
        </Alert>
      )}
    </div>
  )
}
