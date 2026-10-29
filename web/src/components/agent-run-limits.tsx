// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import type { Agent } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { BudgetService, type RunLimitValues } from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { runLimitsLabel } from '@/lib/cost'
import { amountError } from '@/lib/money'

const wholeNumber = /^[0-9]{1,6}$/

/**
 * AgentRunLimits is where an admin or owner sees the limits of one run of an agent and overrides
 * the template's values (ADR 0080). The first limit a run reaches ends it. An empty field follows
 * the template; a change applies to runs queued afterwards.
 */
export function AgentRunLimits({ agent }: { agent: Agent }) {
  const queryClient = useQueryClient()
  const limits = useQuery(BudgetService.method.getAgentRunLimits, { orgId: agent.orgId, agentId: agent.id })
  const save = useMutation(BudgetService.method.setAgentRunLimits, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: BudgetService, cardinality: undefined }) }),
  })
  const [editing, setEditing] = useState(false)
  const l = limits.data?.limits
  if (limits.error) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>Could not load the run limits: {ConnectError.from(limits.error).rawMessage}</AlertDescription>
      </Alert>
    )
  }
  if (!l?.effective || !l.templateDefault || !l.override) return <p role="status">Loading the run limits…</p>
  const overridden = l.override.maxModelRequests > 0n || l.override.maxSpendUsd !== '' || l.override.maxDurationSeconds > 0n
  return (
    <section aria-label="Run limits" className="bg-card grid gap-2 rounded-lg border p-4 text-sm">
      <h2 className="font-semibold">Run limits</h2>
      <p>
        {runLimitsLabel(l.effective)} per run. The first limit a run reaches ends it.
        {overridden ? ` Changed from the template's ${runLimitsLabel(l.templateDefault)}.` : ' These are the template’s values.'}
      </p>
      {editing ? (
        <LimitsForm
          current={l.override}
          template={l.templateDefault}
          pending={save.isPending}
          error={save.error ? ConnectError.from(save.error).rawMessage : undefined}
          onCancel={() => setEditing(false)}
          onSave={(override) => save.mutateAsync({ orgId: agent.orgId, agentId: agent.id, override }).then(() => setEditing(false))}
        />
      ) : (
        <div>
          <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
            Change run limits
          </Button>
        </div>
      )}
    </section>
  )
}

function LimitsForm({
  current,
  template,
  pending,
  error,
  onCancel,
  onSave,
}: {
  current: RunLimitValues
  template: RunLimitValues
  pending: boolean
  error?: string
  onCancel: () => void
  onSave: (override: { maxModelRequests: bigint; maxSpendUsd: string; maxDurationSeconds: bigint }) => Promise<unknown>
}) {
  const [requests, setRequests] = useState(current.maxModelRequests > 0n ? String(current.maxModelRequests) : '')
  const [spend, setSpend] = useState(current.maxSpendUsd)
  const [minutes, setMinutes] = useState(current.maxDurationSeconds > 0n ? String(Number(current.maxDurationSeconds) / 60) : '')
  const [touched, setTouched] = useState(false)
  const problems = {
    requests: requests.trim() !== '' && (!wholeNumber.test(requests.trim()) || Number(requests) < 1 || Number(requests) > 100000) ? 'Model requests must be a whole number from 1 to 100,000' : undefined,
    spend: spend.trim() !== '' ? amountError('Spend', spend) : undefined,
    minutes: minutes.trim() !== '' && (!wholeNumber.test(minutes.trim()) || Number(minutes) < 1 || Number(minutes) > 1440) ? 'Minutes must be a whole number from 1 to 1,440' : undefined,
  }
  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (problems.requests || problems.spend || problems.minutes) return
    await onSave({
      maxModelRequests: BigInt(requests.trim() || '0'),
      maxSpendUsd: spend.trim(),
      maxDurationSeconds: BigInt(minutes.trim() || '0') * 60n,
    }).catch(() => undefined)
  }
  return (
    <form onSubmit={(e) => void submit(e)} noValidate aria-label="Change run limits" className="grid gap-3">
      <div className="flex flex-wrap items-start gap-3">
        <div className="grid gap-1.5">
          <Label htmlFor="run-limit-requests">Model requests</Label>
          <Input id="run-limit-requests" inputMode="numeric" value={requests} placeholder={String(template.maxModelRequests)} onChange={(e) => setRequests(e.target.value)} aria-invalid={touched && !!problems.requests} />
          {touched && problems.requests && <p className="text-destructive text-xs">{problems.requests}</p>}
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="run-limit-spend">Spend (USD)</Label>
          <Input id="run-limit-spend" inputMode="decimal" value={spend} placeholder={template.maxSpendUsd} onChange={(e) => setSpend(e.target.value)} aria-invalid={touched && !!problems.spend} />
          {touched && problems.spend && <p className="text-destructive text-xs">{problems.spend}</p>}
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="run-limit-minutes">Time (minutes)</Label>
          <Input id="run-limit-minutes" inputMode="numeric" value={minutes} placeholder={String(Number(template.maxDurationSeconds) / 60)} onChange={(e) => setMinutes(e.target.value)} aria-invalid={touched && !!problems.minutes} />
          {touched && problems.minutes && <p className="text-destructive text-xs">{problems.minutes}</p>}
        </div>
      </div>
      <p className="text-muted-foreground text-xs">Leave a field empty to follow the template. Runs already queued keep the limits they were queued with.</p>
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <div className="flex gap-2">
        <Button type="submit" size="sm" disabled={pending}>
          Save run limits
        </Button>
        <Button type="button" variant="ghost" size="sm" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </form>
  )
}
