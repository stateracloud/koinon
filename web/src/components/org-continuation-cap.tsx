// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { BudgetService } from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { formatUSD } from '@/lib/cost'
import { amountError } from '@/lib/money'

/**
 * OrgContinuationCap is where an admin or owner sees and sets the org's default spend cap for one
 * task across its continuation runs (issue #99, ADR 0081). An empty amount follows the platform
 * default; an agent can override it.
 */
export function OrgContinuationCap({ orgId }: { orgId: string }) {
  const queryClient = useQueryClient()
  const limits = useQuery(BudgetService.method.getOrgContinuationLimits, { orgId })
  const save = useMutation(BudgetService.method.setOrgContinuationTaskSpend, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: BudgetService, cardinality: undefined }) }),
  })
  const [editing, setEditing] = useState(false)
  const loaded = limits.data?.limits
  if (limits.error) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>Could not load the continuation cap: {ConnectError.from(limits.error).rawMessage}</AlertDescription>
      </Alert>
    )
  }
  if (!loaded) return <p role="status">Loading the continuation cap…</p>
  return (
    <section aria-labelledby="continuation-cap" className="grid gap-3">
      <h2 id="continuation-cap" className="text-lg font-semibold">
        Task spend cap for continuations
      </h2>
      <p className="text-muted-foreground text-sm">
        When a run hits its time limit, Koinon can continue the task in a new run. No further continuation starts once the task has spent this much across all its runs; the task then waits on its owner. An agent can set its own
        cap.
      </p>
      <p className="text-sm">
        {formatUSD(loaded.taskSpendUsd)} per task. {loaded.overridden ? `The platform default is ${formatUSD(loaded.platformDefaultTaskSpendUsd)}.` : 'This is the platform default.'}
      </p>
      {editing ? (
        <CapForm
          initial={loaded.overridden ? loaded.taskSpendUsd : ''}
          pending={save.isPending}
          error={save.error ? ConnectError.from(save.error).rawMessage : undefined}
          onSave={(usd) => save.mutateAsync({ orgId, taskSpendUsd: usd }).then(() => setEditing(false))}
          onCancel={() => setEditing(false)}
        />
      ) : (
        <div>
          <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
            Change task spend cap
          </Button>
        </div>
      )}
    </section>
  )
}

function CapForm({ initial, pending, error, onSave, onCancel }: { initial: string; pending: boolean; error?: string; onSave: (usd: string) => Promise<unknown>; onCancel: () => void }) {
  const [value, setValue] = useState(initial)
  const [touched, setTouched] = useState(false)
  // An empty amount clears the org's value, so the platform default applies.
  const problem = value.trim() !== '' ? amountError('Task spend cap', value) : undefined
  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (problem) return
    await onSave(value.trim()).catch(() => undefined)
  }
  return (
    <form onSubmit={(e) => void submit(e)} noValidate aria-label="Change task spend cap" className="grid gap-2">
      <div className="flex flex-wrap items-end gap-2">
        <div className="grid gap-1.5">
          <Label htmlFor="continuation-cap-amount">Task spend cap in US dollars</Label>
          <div className="flex items-center gap-1">
            <span aria-hidden="true">$</span>
            <Input id="continuation-cap-amount" inputMode="decimal" className="w-36" value={value} onChange={(e) => setValue(e.target.value)} autoComplete="off" aria-invalid={touched && !!problem} />
          </div>
        </div>
        <Button type="submit" size="sm" disabled={pending}>
          Save task spend cap
        </Button>
        <Button type="button" variant="ghost" size="sm" onClick={onCancel}>
          Cancel
        </Button>
      </div>
      <p className="text-muted-foreground text-xs">Leave empty to follow the platform default. A change applies from the next time a run hits its time limit.</p>
      {touched && problem && <p className="text-destructive text-xs">{problem}</p>}
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
    </form>
  )
}
