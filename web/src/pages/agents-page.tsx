// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { AgentStatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { useAllAgents } from '@/hooks/use-all-agents'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { describeError } from '@/lib/api-error'
import { harnessLabel } from '@/lib/labels'
import { useOrg } from '@/lib/org-context'

/** AgentsPage lists the org's agents. */
export function AgentsPage() {
  const { org } = useOrg()
  const list = useAllAgents(org.orgId)
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <div className="flex items-center gap-3">
        <h1 className="text-xl font-semibold">Agents</h1>
        {org.role >= OrgRole.MEMBER && (
          <Button asChild size="sm" className="ml-auto">
            <Link to="/agents/new">New agent</Link>
          </Button>
        )}
      </div>
      {list.isError && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{describeError(list.error).message}</AlertDescription>
        </Alert>
      )}
      {list.isPending ? (
        <p role="status">Loading agents…</p>
      ) : list.data?.agents.length ? (
        <ul className="grid gap-2">
          {list.data.agents.map((a) => (
            <li key={a.id} className="bg-card flex items-center gap-3 rounded-md border p-3">
              <Link to="/agents/$agentId" params={{ agentId: a.id }} className="font-medium hover:underline">
                {a.displayName}
              </Link>
              <span className="text-muted-foreground text-sm">{harnessLabel(a.harness)}</span>
              <span className="ml-auto flex flex-wrap items-center gap-2">
                {a.unhealthyAt && <AgentStatusBadge status={a.status} unhealthy />}
                <AgentStatusBadge status={a.status} />
              </span>
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-muted-foreground text-sm">No agents yet.</p>
      )}
      {list.isFetchingMore && <p role="status" className="text-muted-foreground text-sm">Loading more agents…</p>}
      {list.isError && list.data && <p className="text-muted-foreground text-sm">This list may be incomplete.</p>}
    </div>
  )
}
