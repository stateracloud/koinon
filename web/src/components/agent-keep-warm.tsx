// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Label } from '@/components/ui/label'
import { AgentService, AgentStatus, type Agent } from '@/gen/koinon/agent/v1alpha1/agent_pb'

/** keepWarmText says what the agent's keep-warm request comes to: off, granted, or asked for and held back by the operator's cap or not yet decided. */
function keepWarmText(agent: Pick<Agent, 'keepWarm' | 'keepWarmGranted' | 'status'>): string {
  if (agent.status === AgentStatus.ARCHIVED) return 'Keep-warm is off while the agent is archived. The sandbox starts when a run needs it and stops when it is idle.'
  if (!agent.keepWarm) return 'Off. The sandbox starts when a run needs it and stops when it is idle.'
  if (agent.keepWarmGranted) return 'Granted. The sandbox stays up and is not stopped when idle.'
  if (agent.status === AgentStatus.DRAFT) return 'Requested. It is granted once the agent is active, if the operator’s cap allows.'
  return 'Requested, not granted. The operator caps keep-warm sandboxes per organisation and the oldest agents come first, so this one starts on demand. A new request waits for the next reconciler pass.'
}

/**
 * AgentKeepWarm is where an admin or owner asks for an agent's sandbox to stay up (ADR 0049
 * amendment 2026-11-01). It is a request: the operator caps the keep-warm sandboxes per org
 * (reconciler.keepWarmPerOrg) and the reconciler records which requests it granted, so the
 * section says whether this one was. A setting of the agent, not a new version; audited.
 */
export function AgentKeepWarm({ agent }: { agent: Agent }) {
  const queryClient = useQueryClient()
  const save = useMutation(AgentService.method.setAgentKeepWarm, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AgentService, cardinality: undefined }) }),
  })
  const archived = agent.status === AgentStatus.ARCHIVED
  return (
    <section aria-labelledby="agent-keep-warm-title" className="bg-card grid gap-2 rounded-lg border p-4 text-sm">
      <h2 id="agent-keep-warm-title" className="font-semibold">
        Sandbox
      </h2>
      <div className="flex items-center gap-2">
        <input
          id="agent-keep-warm"
          type="checkbox"
          checked={agent.keepWarm}
          disabled={save.isPending || archived}
          aria-describedby="agent-keep-warm-help"
          onChange={(e) => save.mutate({ orgId: agent.orgId, agentId: agent.id, keepWarm: e.target.checked })}
        />
        <Label htmlFor="agent-keep-warm">Keep warm</Label>
      </div>
      <p id="agent-keep-warm-help" className="text-muted-foreground text-xs">
        {keepWarmText(agent)} Keeping a sandbox up uses the organisation&apos;s resources. Changes are recorded in the audit log.
      </p>
      {save.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not save: {ConnectError.from(save.error).rawMessage}</AlertDescription>
        </Alert>
      )}
    </section>
  )
}
