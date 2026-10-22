// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { useMemo } from 'react'
import { AgentWizard } from '@/components/agent-wizard'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { TemplateService } from '@/gen/koinon/template/v1alpha1/template_pb'
import { isApprover } from '@/lib/approvals'
import { setupGuide } from '@/lib/connector'
import { useOrg } from '@/lib/org-context'

/** NewAgentPage creates a draft agent from a template (ADR 0020). Members and above. */
export function NewAgentPage() {
  const { org } = useOrg()
  const orgId = org.orgId
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const templates = useQuery(TemplateService.method.listTemplates, {})
  const connections = useQuery(ConnectionService.method.listConnections, { orgId })
  const types = useQuery(ConnectionService.method.listConnectorTypes, {})
  const patterns = useMemo(
    () => new Map((types.data?.connectorTypes ?? []).flatMap((t) => (setupGuide(t).targetPattern ? [[t.id, setupGuide(t).targetPattern!] as const] : []))),
    [types.data],
  )

  const create = useMutation(AgentService.method.createAgentFromTemplate, {
    onSuccess: async (res) => {
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AgentService, cardinality: undefined }) })
      if (res.agent) await navigate({ to: '/agents/$agentId', params: { agentId: res.agent.id } })
    },
  })

  if (org.role < OrgRole.MEMBER) {
    return (
      <Alert role="alert">
        <AlertDescription>Viewers cannot create agents.</AlertDescription>
      </Alert>
    )
  }
  const loadError = templates.error ?? connections.error
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <Link to="/agents" className="text-muted-foreground text-sm hover:underline">
        ← Agents
      </Link>
      <h1 className="text-xl font-semibold">New agent from a template</h1>
      {loadError && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(loadError).rawMessage}</AlertDescription>
        </Alert>
      )}
      {templates.isPending || connections.isPending ? (
        <p role="status">Loading templates…</p>
      ) : (
        templates.data &&
        connections.data && (
          <AgentWizard
            templates={templates.data.templates}
            connections={connections.data.connections}
            targetPatterns={patterns}
            canConnect={isApprover(org.role)}
            pending={create.isPending}
            error={create.error ? ConnectError.from(create.error).rawMessage : undefined}
            onCreate={(a) => create.mutate({ orgId, ...a })}
          />
        )
      )}
    </div>
  )
}
