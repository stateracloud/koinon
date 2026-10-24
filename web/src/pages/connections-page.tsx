// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { Link } from '@tanstack/react-router'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ConnectionService, ConnectionStatus } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { isApprover } from '@/lib/approvals'
import { connectionStatusLabel } from '@/lib/labels'
import { useOrg } from '@/lib/org-context'

/** ConnectionsPage lists the org's connections (ADR 0007). */
export function ConnectionsPage() {
  const { org } = useOrg()
  const list = useQuery(ConnectionService.method.listConnections, { orgId: org.orgId })
  const admin = isApprover(org.role)

  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <div className="flex items-center gap-3">
        <h1 className="text-xl font-semibold">Connections</h1>
        {admin && (
          <Button asChild size="sm" className="ml-auto">
            <Link to="/connections/new/github">Connect GitHub</Link>
          </Button>
        )}
      </div>
      {list.isError && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(list.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      {list.isPending ? (
        <p role="status">Loading connections…</p>
      ) : list.data?.connections.length ? (
        <ul className="grid gap-2">
          {list.data.connections.map((c) => (
            <li key={c.id} className="bg-card flex items-center gap-3 rounded-md border p-3">
              <Link to="/connections/$connectionId" params={{ connectionId: c.id }} className="font-medium hover:underline">
                {c.displayName}
              </Link>
              <span className="text-muted-foreground text-sm">{c.connector}</span>
              {c.source === 'secret' && (
                <span className="text-muted-foreground text-sm">{c.removed ? `Removed (Secret ${c.managedBySecret} deleted)` : `Managed by Secret ${c.managedBySecret}`}</span>
              )}
              <Badge variant={c.status === ConnectionStatus.FAILING ? 'destructive' : 'secondary'} className="ml-auto">
                {connectionStatusLabel(c.status)}
              </Badge>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-muted-foreground text-sm">
          No connections yet. {admin ? 'Connect GitHub to let agents work on your repositories.' : 'An org admin or owner can connect GitHub.'}
        </p>
      )}
    </div>
  )
}
