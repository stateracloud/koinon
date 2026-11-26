// SPDX-License-Identifier: Apache-2.0

import { timestampDate } from '@bufbuild/protobuf/wkt'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link } from '@tanstack/react-router'
import { useState } from 'react'
import { AgentApprovalScopes } from '@/components/agent-approval-scopes'
import { AgentDraftEdit } from '@/components/agent-draft-edit'
import { AgentSaveAsTemplate } from '@/components/save-as-template'
import { AgentKeepWarm } from '@/components/agent-keep-warm'
import { AgentMcpServers } from '@/components/agent-mcp-servers'
import { AgentModel } from '@/components/agent-model'
import { AgentContinuationLimits } from '@/components/agent-continuation-limits'
import { AgentRunLimits } from '@/components/agent-run-limits'
import { AgentSandboxResources } from '@/components/agent-sandbox-resources'
import { AgentTriggers } from '@/components/agent-triggers'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { AgentService, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import type { Grant } from '@/gen/koinon/template/v1alpha1/template_pb'
import { describeError } from '@/lib/api-error'
import { runLimitsLabel } from '@/lib/cost'
import { agentStatusLabel, harnessLabel, presetLabel } from '@/lib/labels'
import { isApprover } from '@/lib/approvals'
import { useOrg } from '@/lib/org-context'

const route = getRouteApi('/app/org/agents/$agentId')

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
  const who = useQuery(AuthService.method.whoAmI, {})
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
        <AlertDescription>{get.error ? describeError(get.error).message : 'Agent not found'}</AlertDescription>
      </Alert>
    )
  }
  const a = get.data.agent
  const draft = a.status === AgentStatus.DRAFT
  const pendingVersion = a.latestVersion !== a.activeVersion
  // A new draft has no tasks yet. An agent that ran before (and went back to draft for a grant
  // change, ADR 0020) can have open tasks, so it gets "Apply to open tasks".
  const wasActive = a.activeVersion > 0
  const grants = draft || pendingVersion ? a.proposedGrants : a.approvedGrants
  // The server's rule (UpdateAgent): an admin or owner edits any draft; a member edits only a draft
  // they created that was never activated. After activation the agent is an admin's to change.
  const ownDraft = draft && !wasActive && org.role >= OrgRole.MEMBER && a.createdBy !== '' && a.createdBy === who.data?.user?.id
  const canEditDraft = draft && (isApprover(org.role) || ownDraft)

  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <Link to="/" className="text-muted-foreground text-sm hover:underline">
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
              <AlertDescription>Could not resume the agent: {describeError(resume.error).message}</AlertDescription>
            </Alert>
          )}
        </section>
      )}
      {(draft || pendingVersion) && a.status !== AgentStatus.ARCHIVED && (
        <section aria-label="Activation" className="bg-card grid gap-2 rounded-lg border p-4 text-sm">
          <p>
            {draft
              ? 'This agent is a draft. It does not run, and its triggers are off, until an org admin or owner activates it.'
              : `Version ${a.latestVersion} is proposed. The agent runs version ${a.activeVersion} until it is activated.`}
          </p>
          {isApprover(org.role) ? (
            <>
              <p>Activating approves these grants and triggers for version {a.latestVersion}. The agent can then take tasks.</p>
              {wasActive && (
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
              )}
              <div>
                <Button disabled={activate.isPending} onClick={() => activate.mutate({ orgId, agentId: a.id, version: a.latestVersion, applyToOpenTickets: wasActive && applyToOpenTickets })}>
                  Activate
                </Button>
              </div>
            </>
          ) : (
            <p className="text-muted-foreground font-medium">Waiting for an admin to activate.</p>
          )}
          {activate.error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>Could not activate: {describeError(activate.error).message}</AlertDescription>
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
          <Link to="/inbox" className="underline">
            Hand it a task from the inbox
          </Link>
        </p>
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
      {canEditDraft && <AgentDraftEdit agent={a} />}
      {isApprover(org.role) && <AgentModel agent={a} />}
      {isApprover(org.role) && <AgentSandboxResources agent={a} />}
      {isApprover(org.role) && <AgentKeepWarm agent={a} />}
      {isApprover(org.role) && a.status !== AgentStatus.ARCHIVED && <AgentRunLimits agent={a} />}
      {isApprover(org.role) && a.status !== AgentStatus.ARCHIVED && <AgentContinuationLimits agent={a} />}
      <AgentTriggers agent={a} canEdit={isApprover(org.role)} />
      {!draft && <AgentApprovalScopes agentId={a.id} canRevoke={isApprover(org.role)} />}
      {isApprover(org.role) && <AgentMcpServers agentId={a.id} />}
      {org.role >= OrgRole.MEMBER && a.status !== AgentStatus.ARCHIVED && <AgentSaveAsTemplate agent={a} />}
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
