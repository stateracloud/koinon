// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link } from '@tanstack/react-router'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { AgentService, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import type { Grant } from '@/gen/koinon/template/v1alpha1/template_pb'
import { agentStatusLabel, flavorLabel, presetLabel } from '@/lib/agents'
import { isApprover } from '@/lib/approvals'
import { useOrg } from '@/lib/org-context'

const route = getRouteApi('/app/agents/$agentId')

/**
 * AgentPage shows an agent and, for a draft, the grants it proposes. An org
 * admin or owner activates it, which approves exactly the version shown
 * (ADR 0020).
 */
export function AgentPage() {
  const { agentId } = route.useParams()
  const { org } = useOrg()
  const orgId = org.orgId
  const queryClient = useQueryClient()
  const get = useQuery(AgentService.method.getAgent, { orgId, agentId })
  const connections = useQuery(ConnectionService.method.listConnections, { orgId })
  const connName = (id: string) => connections.data?.connections.find((c) => c.id === id)?.displayName ?? id

  const activate = useMutation(AgentService.method.activateAgent, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AgentService, cardinality: undefined }) }),
  })

  if (get.isPending) return <p role="status">Loading agent…</p>
  if (get.isError || !get.data.agent) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>{get.error ? ConnectError.from(get.error).rawMessage : 'Agent not found'}</AlertDescription>
      </Alert>
    )
  }
  const a = get.data.agent
  const draft = a.status === AgentStatus.DRAFT
  const pendingVersion = a.latestVersion !== a.activeVersion
  const grants = draft || pendingVersion ? a.proposedGrants : a.approvedGrants

  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <Link to="/agents" className="text-muted-foreground text-sm hover:underline">
        ← Agents
      </Link>
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">{a.displayName}</h1>
        <Badge variant={draft ? 'outline' : 'secondary'}>{agentStatusLabel(a.status)}</Badge>
      </div>
      <section aria-label="Agent summary" className="bg-card grid gap-2 rounded-lg border p-4 text-sm">
        <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
          <dt className="text-muted-foreground">Template</dt>
          <dd>{a.templateSlug || 'none'}</dd>
          <dt className="text-muted-foreground">Flavor</dt>
          <dd>{flavorLabel(a.flavor)}</dd>
          <dt className="text-muted-foreground">Model</dt>
          <dd>{a.model || 'default'}</dd>
          <dt className="text-muted-foreground">Version</dt>
          <dd>
            {a.latestVersion}
            {a.activeVersion > 0 && a.activeVersion !== a.latestVersion && ` (active: ${a.activeVersion})`}
          </dd>
          {a.runLimits && (
            <>
              <dt className="text-muted-foreground">Run limits</dt>
              <dd>
                {Math.round(Number(a.runLimits.maxDurationSeconds) / 60)} min, {Number(a.runLimits.maxTokens).toLocaleString()} tokens per run
              </dd>
            </>
          )}
        </dl>
        <h2 className="mt-2 font-semibold">{draft || pendingVersion ? 'Proposed grants' : 'Approved grants'}</h2>
        {grants.length === 0 ? <p className="text-muted-foreground">None.</p> : grants.map((g, i) => <GrantSummary key={i} grant={g} connectionName={connName} />)}
      </section>
      {(draft || pendingVersion) && a.status !== AgentStatus.ARCHIVED && (
        <section aria-label="Activation" className="grid gap-2">
          {isApprover(org.role) ? (
            <>
              <p className="text-sm">Activating approves these grants for version {a.latestVersion}. The agent can then take tickets.</p>
              <div>
                <Button disabled={activate.isPending} onClick={() => activate.mutate({ orgId, agentId: a.id, version: a.latestVersion })}>
                  Activate
                </Button>
              </div>
            </>
          ) : (
            <p className="text-muted-foreground text-sm">An org admin or owner reviews and activates this draft.</p>
          )}
          {activate.error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>Could not activate: {ConnectError.from(activate.error).rawMessage}</AlertDescription>
            </Alert>
          )}
        </section>
      )}
      {activate.isSuccess && (
        <p role="status" className="text-sm">
          Activated.{' '}
          <Link to="/" className="underline">
            Create a ticket for it on the board
          </Link>
        </p>
      )}
    </div>
  )
}

function GrantSummary({ grant: g, connectionName }: { grant: Grant; connectionName: (id: string) => string }) {
  return (
    <div className="border-t pt-2">
      <p className="font-medium">
        {g.connector} · {presetLabel(g.preset)}
      </p>
      <p className="text-muted-foreground">Scopes: {g.scopes.join(', ')}</p>
      {g.connectionId && <p>Connection: {connectionName(g.connectionId)}</p>}
      {g.repositories.length > 0 && <p>Repositories: {g.repositories.join(', ')}</p>}
      {g.purpose && <p className="text-muted-foreground">{g.purpose}</p>}
    </div>
  )
}
