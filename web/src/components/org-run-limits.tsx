// SPDX-License-Identifier: Apache-2.0

import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { OrgSettingsService, type RunLimit } from '@/gen/koinon/orgsettings/v1alpha1/orgsettings_pb'
import { describeError } from '@/lib/api-error'

type Key = 'maxConcurrentRuns' | 'triggerConcurrency' | 'triggerRatePerMinute' | 'sandboxCreationsPerMinute'

const fields: { key: Key; label: string; help: string }[] = [
  { key: 'maxConcurrentRuns', label: 'Runs at the same time', help: 'The most runs of the org that start or run at the same time. Other runs wait in the queue.' },
  { key: 'triggerConcurrency', label: 'Trigger items at the same time', help: 'The most schedule, webhook and task-creation items of the org that run at the same time.' },
  { key: 'triggerRatePerMinute', label: 'Trigger items per minute', help: 'The most schedule, webhook and task-creation items of the org that start in one minute.' },
  { key: 'sandboxCreationsPerMinute', label: 'Sandbox starts per minute', help: 'The most sandboxes that Koinon creates or starts for the org in one minute. More starts wait.' },
]

function problemOf(text: string, l: RunLimit): string | undefined {
  const n = Number(text)
  if (text.trim() === '' || !Number.isInteger(n) || n < l.min || n > l.max) return `Enter a whole number from ${l.min} to ${l.max}.`
  return undefined
}

/**
 * OrgRunLimits is where an org admin sees and changes the org's run limits (issue #492, ADR 0066): the runs at the
 * same time, the trigger share of the queue and the sandbox start rate. A save sends only the limits that changed.
 * Runs that are queued but not started get the new limits when they start. Every change is audited
 * (org.run_limits.set).
 */
export function OrgRunLimits({ orgId }: { orgId: string }) {
  const queryClient = useQueryClient()
  const query = useQuery(OrgSettingsService.method.getRunLimits, { orgId })
  const save = useMutation(OrgSettingsService.method.setRunLimits, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: OrgSettingsService, cardinality: undefined }) }),
  })
  const [edits, setEdits] = useState<Partial<Record<Key, string>>>({})
  const [saved, setSaved] = useState(false)

  if (query.error) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>Could not load the run limits: {describeError(query.error).message}</AlertDescription>
      </Alert>
    )
  }
  const limits = query.data?.limits
  if (!limits) return <p role="status">Loading the run limits…</p>
  const l = (k: Key): RunLimit => limits[k] ?? ({ value: 0, defaultValue: 0, min: 0, max: 0 } as RunLimit)
  const text = (k: Key) => edits[k] ?? String(l(k).value)
  const problems = Object.fromEntries(fields.map(({ key }) => [key, problemOf(text(key), l(key))])) as Record<Key, string | undefined>
  const changed = fields.filter(({ key }) => Number(text(key)) !== l(key).value).map(({ key }) => key)
  const valid = fields.every(({ key }) => !problems[key])
  const allDefault = fields.every(({ key }) => l(key).value === l(key).defaultValue)

  function send(patch: Partial<Record<Key, number>>) {
    setSaved(false)
    save.mutate(
      { orgId, ...patch },
      {
        onSuccess: () => {
          setEdits({})
          setSaved(true)
        },
      },
    )
  }

  function submit(e: FormEvent) {
    e.preventDefault()
    if (!valid || changed.length === 0) return
    send(Object.fromEntries(changed.map((k) => [k, Number(text(k))])))
  }

  return (
    <section aria-labelledby="run-limits" className="bg-card grid gap-4 rounded-lg border p-4 text-sm">
      <h2 id="run-limits" className="font-semibold">
        Run limits
      </h2>
      <p className="text-muted-foreground text-xs">
        These limits keep one org from using all the capacity of the platform. A change applies to runs that start after it, also to runs that wait in the queue. Each change is recorded in the audit
        log.
      </p>
      <form onSubmit={submit} className="grid gap-3" aria-label="Run limits" noValidate>
        {fields.map(({ key, label, help }) => {
          const lim = l(key)
          const id = `run-limit-${key}`
          return (
            <div key={key} className="grid gap-1.5">
              <Label htmlFor={id}>{label}</Label>
              <Input
                id={id}
                type="number"
                inputMode="numeric"
                className="w-36"
                min={lim.min}
                max={lim.max}
                value={text(key)}
                onChange={(e) => {
                  setSaved(false)
                  setEdits((cur) => ({ ...cur, [key]: e.target.value }))
                }}
                aria-invalid={!!problems[key]}
                aria-describedby={`${id}-help`}
              />
              <p id={`${id}-help`} className="text-muted-foreground text-xs">
                {help} From {lim.min} to {lim.max}; the default is {lim.defaultValue}.
              </p>
              {problems[key] && (
                <p role="alert" className="text-destructive text-xs">
                  {problems[key]}
                </p>
              )}
            </div>
          )
        })}
        <div className="flex flex-wrap items-center gap-2">
          <Button type="submit" size="sm" disabled={!valid || changed.length === 0 || save.isPending}>
            Save run limits
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={save.isPending || allDefault}
            onClick={() => send(Object.fromEntries(fields.filter(({ key }) => l(key).value !== l(key).defaultValue).map(({ key }) => [key, 0])))}
          >
            Use the defaults
          </Button>
        </div>
      </form>
      {saved && (
        <p role="status" className="text-xs">
          Saved the run limits.
        </p>
      )}
      {save.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not save: {describeError(save.error).message}</AlertDescription>
        </Alert>
      )}
    </section>
  )
}
