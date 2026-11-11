// SPDX-License-Identifier: Apache-2.0

import { timestampDate } from '@bufbuild/protobuf/wkt'
import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link } from '@tanstack/react-router'
import { useState } from 'react'
import { AgentApprovalScopes } from '@/components/agent-approval-scopes'
import { AgentMcpServers } from '@/components/agent-mcp-servers'
import { AgentModel } from '@/components/agent-model'
import { AgentRunLimits } from '@/components/agent-run-limits'
import { AgentTriggers } from '@/components/agent-triggers'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { AgentService, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import type { Grant } from '@/gen/koinon/template/v1alpha1/template_pb'
import { runLimitsLabel } from '@/lib/cost'
import { agentStatusLabel, harnessLabel, presetLabel } from '@/lib/labels'
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

  const resume = useMutation(AgentService.method.resumeAgent, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AgentService, cardinality: undefined }) }),
  })
  const [applyToOpenTickets, setApplyToOpenTickets] = useState(false)

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
      {a.unhealthyAt && (
        <section aria-label="Agent health" className="grid gap-2">
          <Alert variant="destructive" role="alert">
            <AlertDescription className="grid gap-1">
              <span className="font-medium">This agent is unhealthy, so new runs are refused until it is resumed.</span>
              {a.unhealthyReason && <span>Reason: {a.unhealthyReason}</span>}
              <span>Marked unhealthy {timestampDate(a.unhealthyAt).toLocaleString()}.</span>
            </AlertDescription>
          </Alert>
          {isApprover(org.role) ? (
            <div>
              <Button variant="outline" disabled={resume.isPending} onClick={() => resume.mutate({ orgId, agentId: a.id })}>
                Resume agent
              </Button>
            </div>
          ) : (
            <p className="text-muted-foreground text-sm">An org admin or owner can resume this agent.</p>
          )}
          {resume.error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>Could not resume the agent: {ConnectError.from(resume.error).rawMessage}</AlertDescription>
            </Alert>
          )}
        </section>
      )}
      <section aria-label="Agent summary" className="bg-card grid gap-2 rounded-lg border p-4 text-sm">
        <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
          <dt className="text-muted-foreground">Template</dt>
          <dd>{a.templateSlug || 'none'}</dd>
          <dt className="text-muted-foreground">Harness</dt>
          <dd>{harnessLabel(a.harness)}</dd>
          <dt className="text-muted-foreground">Model</dt>
          <dd>{a.modelSetting ? `${a.modelSetting} (set on this agent)` : a.model || 'default'}</dd>
          <dt className="text-muted-foreground">Sandbox</dt>
          <dd>
            {a.ephemeral ? 'Ephemeral: fresh sandbox per task, no home' : 'Persistent home'}
            {a.sandboxArchivedAt && ` (archived ${timestampDate(a.sandboxArchivedAt).toLocaleString()}, restores on next run)`}
          </dd>
          <dt className="text-muted-foreground">Version</dt>
          <dd>
            {a.latestVersion}
            {a.activeVersion > 0 && a.activeVersion !== a.latestVersion && ` (active: ${a.activeVersion})`}
          </dd>
          {a.runLimits && (
            <>
              <dt className="text-muted-foreground">Run limits</dt>
              <dd>
                {runLimitsLabel(a.runLimits)} per run
              </dd>
            </>
          )}
        </dl>
        <h2 className="mt-2 font-semibold">{draft || pendingVersion ? 'Proposed grants' : 'Approved grants'}</h2>
        {grants.length === 0 ? <p className="text-muted-foreground">None.</p> : grants.map((g, i) => <GrantSummary key={i} grant={g} connectionName={connName} />)}
      </section>
      {isApprover(org.role) && <AgentModel agent={a} />}
      {isApprover(org.role) && a.status !== AgentStatus.ARCHIVED && <AgentRunLimits agent={a} />}
      <AgentTriggers agent={a} canEdit={isApprover(org.role)} />
      {!draft && <AgentApprovalScopes agentId={a.id} canRevoke={isApprover(org.role)} />}
      {isApprover(org.role) && <AgentMcpServers agentId={a.id} />}
      {(draft || pendingVersion) && a.status !== AgentStatus.ARCHIVED && (
        <section aria-label="Activation" className="grid gap-2">
          {isApprover(org.role) ? (
            <>
              <p className="text-sm">Activating approves these grants and triggers for version {a.latestVersion}. The agent can then take tasks.</p>
              <div className="flex items-start gap-2">
                <input
                  id="apply-to-open-tickets"
                  type="checkbox"
                  className="mt-1"
                  checked={applyToOpenTickets}
                  aria-describedby="apply-to-open-tickets-help"
                  onChange={(e) => setApplyToOpenTickets(e.target.checked)}
                />
                <div className="grid gap-0.5">
                  <Label htmlFor="apply-to-open-tickets">Apply to open tasks now (starts fresh sessions with a handoff)</Label>
                  <p id="apply-to-open-tickets-help" className="text-muted-foreground text-sm">
                    The next run on each open task starts a new session with the agent&apos;s last notes and replies, so the new instructions apply. Tasks keep their messages.
                  </p>
                </div>
              </div>
              <div>
                <Button disabled={activate.isPending} onClick={() => activate.mutate({ orgId, agentId: a.id, version: a.latestVersion, applyToOpenTickets })}>
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
          Activated.
          {activate.data.ticketsRestarting > 0 && (
            <>
              {' '}
              {activate.data.ticketsRestarting} open {activate.data.ticketsRestarting === 1 ? 'task' : 'tasks'} will start a fresh session at their next run.
            </>
          )}{' '}
          <Link to="/" className="underline">
            Hand it a task from the inbox
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
