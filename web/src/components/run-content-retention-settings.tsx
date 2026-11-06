// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { OrgSettingsService } from '@/gen/koinon/orgsettings/v1alpha1/orgsettings_pb'

/**
 * RunContentRetentionSettings is where an org admin sets how many days a finished run's stored
 * content (messages, thoughts, tool calls) is kept before the purge job blanks it (issue #69, ADR 0074).
 * The run, its state, its cost, its traces and the spend ledger stay. Every change is audited.
 */
export function RunContentRetentionSettings({ orgId }: { orgId: string }) {
  const queryClient = useQueryClient()
  const query = useQuery(OrgSettingsService.method.getRunContentRetention, { orgId })
  const save = useMutation(OrgSettingsService.method.setRunContentRetention, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: OrgSettingsService, cardinality: undefined }) }),
  })
  const [days, setDays] = useState<string>()

  if (query.error) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>Could not load the run content retention: {ConnectError.from(query.error).rawMessage}</AlertDescription>
      </Alert>
    )
  }
  const r = query.data?.retention
  if (!r) return <p role="status">Loading the run content retention…</p>
  const text = days ?? String(r.days)
  const n = Number(text)
  const valid = Number.isInteger(n) && n >= r.minDays && n <= r.maxDays

  function submit(e: FormEvent) {
    e.preventDefault()
    if (!valid) return
    save.mutate({ orgId, days: n }, { onSuccess: () => setDays(undefined) })
  }

  return (
    <section aria-labelledby="run-content-retention" className="bg-card grid gap-4 rounded-lg border p-4 text-sm">
      <h2 id="run-content-retention" className="font-semibold">
        Run content retention
      </h2>
      <form onSubmit={submit} className="grid gap-1.5" aria-label="Run content retention">
        <Label htmlFor="retention-days">Keep run content for (days)</Label>
        <div className="flex flex-wrap items-center gap-2">
          <Input
            id="retention-days"
            type="number"
            inputMode="numeric"
            className="w-28"
            min={r.minDays}
            max={r.maxDays}
            value={text}
            onChange={(e) => setDays(e.target.value)}
            aria-invalid={!valid}
            aria-describedby="retention-days-help"
          />
          <Button type="submit" size="sm" disabled={!valid || text === String(r.days) || save.isPending}>
            Save retention
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={save.isPending || r.days === r.defaultDays}
            onClick={() => save.mutate({ orgId, days: 0 }, { onSuccess: () => setDays(undefined) })}
          >
            Use the default ({r.defaultDays})
          </Button>
        </div>
        <p id="retention-days-help" className="text-muted-foreground text-xs">
          After a run has finished for this many days, its messages, thoughts and tool calls are deleted. The run page then shows it without its content; its state, cost and traces stay. Between {r.minDays} and {r.maxDays} days; the default is{' '}
          {r.defaultDays}.
        </p>
        {!valid && (
          <p role="alert" className="text-destructive text-xs">
            Enter a whole number from {r.minDays} to {r.maxDays}.
          </p>
        )}
      </form>
      {save.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not save: {ConnectError.from(save.error).rawMessage}</AlertDescription>
        </Alert>
      )}
    </section>
  )
}
