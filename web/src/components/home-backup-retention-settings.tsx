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
 * HomeBackupRetentionSettings is where an org admin sets how many days an old home backup is kept
 * after a key change (issue #219, ADR 0085 decision 13). A key change starts a new backup of each
 * agent's home; the old one stays restorable for this long and is then deleted. Every change is
 * audited.
 */
export function HomeBackupRetentionSettings({ orgId }: { orgId: string }) {
  const queryClient = useQueryClient()
  const query = useQuery(OrgSettingsService.method.getHomeBackupRetention, { orgId })
  const save = useMutation(OrgSettingsService.method.setHomeBackupRetention, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: OrgSettingsService, cardinality: undefined }) }),
  })
  const [days, setDays] = useState<string>()

  if (query.error) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>Could not load the home backup retention: {ConnectError.from(query.error).rawMessage}</AlertDescription>
      </Alert>
    )
  }
  const r = query.data?.retention
  if (!r) return <p role="status">Loading the home backup retention…</p>
  const text = days ?? String(r.days)
  const n = Number(text)
  const valid = Number.isInteger(n) && n >= r.minDays && n <= r.maxDays

  function submit(e: FormEvent) {
    e.preventDefault()
    if (!valid) return
    save.mutate({ orgId, days: n }, { onSuccess: () => setDays(undefined) })
  }

  return (
    <section aria-labelledby="home-backup-retention" className="bg-card grid gap-4 rounded-lg border p-4 text-sm">
      <h2 id="home-backup-retention" className="font-semibold">
        Old home backups
      </h2>
      <form onSubmit={submit} className="grid gap-1.5" aria-label="Home backup retention">
        <Label htmlFor="home-backup-retention-days">Keep old home backups for (days)</Label>
        <div className="flex flex-wrap items-center gap-2">
          <Input
            id="home-backup-retention-days"
            type="number"
            inputMode="numeric"
            className="w-28"
            min={r.minDays}
            max={r.maxDays}
            value={text}
            onChange={(e) => setDays(e.target.value)}
            aria-invalid={!valid}
            aria-describedby="home-backup-retention-days-help"
          />
          <Button type="submit" size="sm" disabled={!valid || text === String(r.days) || save.isPending}>
            Save backup retention
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
        <p id="home-backup-retention-days-help" className="text-muted-foreground text-xs">
          When a key is lost or leaked, each agent's home is backed up anew. The old backup can still be restored for this many days after the change, then it is deleted. A change applies to later key changes. Between {r.minDays} and {r.maxDays} days; the default is {r.defaultDays}.
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
