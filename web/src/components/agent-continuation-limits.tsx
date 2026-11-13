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
import { BudgetService, type ContinuationOverride, type ContinuationValues } from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { formatUSD } from '@/lib/cost'
import { amountError } from '@/lib/money'

const maxContinuations = 10

function describe(v: ContinuationValues): string {
  const runs = v.maxContinuations === 0 ? 'No continuation' : `Up to ${v.maxContinuations} continuation ${v.maxContinuations === 1 ? 'run' : 'runs'}`
  return `${runs}, ${formatUSD(v.taskSpendUsd)} per task`
}

/**
 * AgentContinuationLimits is where an admin or owner sees and overrides how often a task of the
 * agent continues after a run hits its time limit, and the spend at which it stops (issue #99,
 * ADR 0081). An empty field follows the template (the count) or the org's default (the cap).
 */
export function AgentContinuationLimits({ agent }: { agent: Agent }) {
  const queryClient = useQueryClient()
  const limits = useQuery(BudgetService.method.getAgentContinuationLimits, { orgId: agent.orgId, agentId: agent.id })
  const save = useMutation(BudgetService.method.setAgentContinuationLimits, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: BudgetService, cardinality: undefined }) }),
  })
  const [editing, setEditing] = useState(false)
  const l = limits.data?.limits
  if (limits.error) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>Could not load the continuation limits: {ConnectError.from(limits.error).rawMessage}</AlertDescription>
      </Alert>
    )
  }
  if (!l?.effective || !l.templateDefault || !l.override) return <p role="status">Loading the continuation limits…</p>
  const overridden = l.override.maxContinuations !== undefined || l.override.taskSpendUsd !== ''
  return (
    <section aria-label="Continuation limits" className="bg-card grid gap-2 rounded-lg border p-4 text-sm">
      <h2 className="font-semibold">Continuation limits</h2>
      <p>
        {describe(l.effective)}. When a run hits its time limit, the task continues in a new run until it used all its continuation runs or reached the spend cap; then it waits on its owner.
        {overridden ? ` Changed from ${describe(l.templateDefault)}.` : ' These are the template’s and the org’s values.'}
      </p>
      {editing ? (
        <ContinuationForm
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
            Change continuation limits
          </Button>
        </div>
      )}
    </section>
  )
}

function ContinuationForm({
  current,
  template,
  pending,
  error,
  onCancel,
  onSave,
}: {
  current: ContinuationOverride
  template: ContinuationValues
  pending: boolean
  error?: string
  onCancel: () => void
  onSave: (override: { maxContinuations?: number; taskSpendUsd: string }) => Promise<unknown>
}) {
  const [count, setCount] = useState(current.maxContinuations === undefined ? '' : String(current.maxContinuations))
  const [spend, setSpend] = useState(current.taskSpendUsd)
  const [touched, setTouched] = useState(false)
  const countText = count.trim()
  const problems = {
    count: countText !== '' && (!/^[0-9]{1,2}$/.test(countText) || Number(countText) > maxContinuations) ? `Continuation runs must be a whole number from 0 to ${maxContinuations}` : undefined,
    spend: spend.trim() !== '' ? amountError('Task spend cap', spend) : undefined,
  }
  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (problems.count || problems.spend) return
    await onSave({ maxContinuations: countText === '' ? undefined : Number(countText), taskSpendUsd: spend.trim() }).catch(() => undefined)
  }
  return (
    <form onSubmit={(e) => void submit(e)} noValidate aria-label="Change continuation limits" className="grid gap-3">
      <div className="flex flex-wrap items-start gap-3">
        <div className="grid gap-1.5">
          <Label htmlFor="continuation-count">Continuation runs</Label>
          <Input id="continuation-count" inputMode="numeric" value={count} placeholder={String(template.maxContinuations)} onChange={(e) => setCount(e.target.value)} aria-invalid={touched && !!problems.count} />
          {touched && problems.count && <p className="text-destructive text-xs">{problems.count}</p>}
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="continuation-spend">Task spend cap (USD)</Label>
          <Input id="continuation-spend" inputMode="decimal" value={spend} placeholder={template.taskSpendUsd} onChange={(e) => setSpend(e.target.value)} aria-invalid={touched && !!problems.spend} />
          {touched && problems.spend && <p className="text-destructive text-xs">{problems.spend}</p>}
        </div>
      </div>
      <p className="text-muted-foreground text-xs">Leave a field empty to follow the template (runs) or the org’s default (cap). 0 switches continuation off for this agent. A change applies from the next time a run hits its time limit.</p>
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <div className="flex gap-2">
        <Button type="submit" size="sm" disabled={pending}>
          Save continuation limits
        </Button>
        <Button type="button" variant="ghost" size="sm" onClick={onCancel}>
          Cancel
        </Button>
      </div>
    </form>
  )
}
