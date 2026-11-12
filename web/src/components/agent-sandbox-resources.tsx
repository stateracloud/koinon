// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { AgentService, AgentStatus, type Agent, type SandboxResources } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { boundsText } from '@/lib/sandbox-resources'

type Field = 'cpuRequest' | 'cpuLimit' | 'memoryRequest' | 'memoryLimit'

const fields: { key: Field; label: string; bound: 'cpuRequest' | 'cpuLimit' | 'memoryRequest' | 'memoryLimit' }[] = [
  { key: 'cpuRequest', label: 'CPU request', bound: 'cpuRequest' },
  { key: 'cpuLimit', label: 'CPU limit', bound: 'cpuLimit' },
  { key: 'memoryRequest', label: 'Memory request', bound: 'memoryRequest' },
  { key: 'memoryLimit', label: 'Memory limit', bound: 'memoryLimit' },
]

function valuesOf(r?: SandboxResources): Record<Field, string> {
  return { cpuRequest: r?.cpuRequest ?? '', cpuLimit: r?.cpuLimit ?? '', memoryRequest: r?.memoryRequest ?? '', memoryLimit: r?.memoryLimit ?? '' }
}

/**
 * AgentSandboxResources is where an admin or owner overrides the CPU and memory of one agent's
 * sandbox inside the platform's bounds (ADR 0083). A value left empty follows the template, then the
 * platform default. The override is a setting of the agent, not a new version: the platform refuses
 * a value outside the bounds, names the bounds, and records every change in the audit log.
 */
export function AgentSandboxResources({ agent }: { agent: Agent }) {
  const queryClient = useQueryClient()
  const res = useQuery(AgentService.method.getAgentSandboxResources, { orgId: agent.orgId, agentId: agent.id })
  const [picked, setPicked] = useState<Partial<Record<Field, string>>>()
  const save = useMutation(AgentService.method.setAgentSandboxResources, {
    onSuccess: async () => {
      setPicked(undefined)
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: AgentService, cardinality: undefined }) })
    },
  })
  if (res.error) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>Could not load the sandbox size: {ConnectError.from(res.error).rawMessage}</AlertDescription>
      </Alert>
    )
  }
  const s = res.data?.state
  if (!s) return <p role="status">Loading the sandbox size…</p>
  const stored = valuesOf(s.override)
  const value = { ...stored, ...picked }
  const dirty = fields.some((f) => value[f.key] !== stored[f.key])
  const hasOverride = fields.some((f) => stored[f.key] !== '')
  const archived = agent.status === AgentStatus.ARCHIVED
  const effective = s.effective
  const submit = (v: Record<Field, string>) => save.mutate({ orgId: agent.orgId, agentId: agent.id, resources: v })
  return (
    <section aria-labelledby="agent-sandbox-resources" className="bg-card grid gap-3 rounded-lg border p-4 text-sm">
      <h2 id="agent-sandbox-resources" className="font-semibold">
        Sandbox size
      </h2>
      <p>
        Next sandbox: CPU <code>{effective?.cpuRequest || 'none'}</code> to <code>{effective?.cpuLimit || 'none'}</code>, memory <code>{effective?.memoryRequest || 'none'}</code> to{' '}
        <code>{effective?.memoryLimit || 'none'}</code>. {hasOverride ? 'Set on this agent.' : "From the agent's template or the platform default."}
      </p>
      <div className="grid gap-3 sm:grid-cols-2">
        {fields.map((f) => (
          <div key={f.key} className="grid gap-1.5">
            <Label htmlFor={`agent-sandbox-${f.key}`}>{f.label}</Label>
            <Input
              id={`agent-sandbox-${f.key}`}
              value={value[f.key]}
              placeholder={s.template?.[f.key] || 'template or default'}
              disabled={archived}
              aria-describedby={`agent-sandbox-${f.key}-help`}
              onChange={(e) => setPicked({ ...picked, [f.key]: e.target.value })}
            />
            <p id={`agent-sandbox-${f.key}-help`} className="text-muted-foreground text-xs">
              Platform allows {boundsText(s.bounds?.[f.bound])}.
            </p>
          </div>
        ))}
      </div>
      <p className="text-muted-foreground text-xs">
        Kubernetes quantities such as 500m or 2 for CPU and 1Gi for memory. A field left empty follows the template, then the platform default.
        {s.memoryRequestEqualsLimit && ' On this install the memory request is raised to the memory limit.'} A change recreates the sandbox at the next run and is recorded in the audit log. It is not a new version of the agent.
      </p>
      {save.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not save: {ConnectError.from(save.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      <div className="flex gap-2">
        <Button size="sm" disabled={save.isPending || archived || !dirty} onClick={() => submit(value)}>
          Save sandbox size
        </Button>
        <Button size="sm" variant="outline" disabled={save.isPending || archived || !hasOverride} onClick={() => submit({ cpuRequest: '', cpuLimit: '', memoryRequest: '', memoryLimit: '' })}>
          Clear override
        </Button>
      </div>
    </section>
  )
}
