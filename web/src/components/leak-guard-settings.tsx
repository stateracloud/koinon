// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { LeakGuardService } from '@/gen/koinon/leakguard/v1alpha1/leakguard_pb'

/**
 * LeakGuardSettings is where an org admin sets the leak guard's match threshold and the hosts a fetch
 * server may reach without the guard's say (ADR 0084, old issue 350). The list starts empty and a host is an
 * exact DNS name, or *.example.com for any subdomain of it (never example.com itself). Every change is audited.
 */
export function LeakGuardSettings({ orgId }: { orgId: string }) {
  const queryClient = useQueryClient()
  const query = useQuery(LeakGuardService.method.getLeakGuard, { orgId })
  const refresh = () =>
    queryClient.invalidateQueries({
      queryKey: createConnectQueryKey({
        schema: LeakGuardService,
        cardinality: undefined,
      }),
    })
  const setThreshold = useMutation(LeakGuardService.method.setMatchThreshold, {
    onSuccess: refresh,
  })
  const addHost = useMutation(LeakGuardService.method.addTrustedHost, {
    onSuccess: refresh,
  })
  const removeHost = useMutation(LeakGuardService.method.removeTrustedHost, {
    onSuccess: refresh,
  })
  const [threshold, setThresholdText] = useState<string>()
  const [host, setHost] = useState('')

  if (query.error) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>Could not load the leak guard settings: {ConnectError.from(query.error).rawMessage}</AlertDescription>
      </Alert>
    )
  }
  const g = query.data?.leakGuard
  if (!g) return <p role="status">Loading the leak guard settings…</p>
  const text = threshold ?? String(g.matchThreshold)
  const n = Number(text)
  const valid = Number.isInteger(n) && n >= g.minMatchThreshold && n <= g.maxMatchThreshold
  const error = setThreshold.error ?? addHost.error ?? removeHost.error

  function saveThreshold(e: FormEvent) {
    e.preventDefault()
    if (!valid) return
    setThreshold.mutate({ orgId, matchThreshold: n }, { onSuccess: () => setThresholdText(undefined) })
  }
  function add(e: FormEvent) {
    e.preventDefault()
    if (host.trim() === '') return
    addHost.mutate({ orgId, host: host.trim() }, { onSuccess: () => setHost('') })
  }

  return (
    <section aria-labelledby="leak-guard" className="bg-card grid gap-4 rounded-lg border p-4 text-sm">
      <h2 id="leak-guard" className="font-semibold">
        Leak guard
      </h2>
      <form onSubmit={saveThreshold} className="grid gap-1.5" aria-label="Match threshold">
        <Label htmlFor="leak-threshold">Match threshold (characters)</Label>
        <div className="flex flex-wrap items-center gap-2">
          <Input
            id="leak-threshold"
            type="number"
            inputMode="numeric"
            className="w-28"
            min={g.minMatchThreshold}
            max={g.maxMatchThreshold}
            value={text}
            onChange={(e) => setThresholdText(e.target.value)}
            aria-invalid={!valid}
            aria-describedby="leak-threshold-help"
          />
          <Button type="submit" size="sm" disabled={!valid || text === String(g.matchThreshold) || setThreshold.isPending}>
            Save threshold
          </Button>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={setThreshold.isPending || g.matchThreshold === g.defaultMatchThreshold}
            onClick={() => setThreshold.mutate({ orgId, matchThreshold: 0 }, { onSuccess: () => setThresholdText(undefined) })}
          >
            Use the default ({g.defaultMatchThreshold})
          </Button>
        </div>
        <p id="leak-threshold-help" className="text-muted-foreground text-xs">
          How many characters of private text, copied exactly, make the guard act. Between {g.minMatchThreshold} and {g.maxMatchThreshold}; the default is {g.defaultMatchThreshold}. A lower value catches shorter copies and raises false alarms.
        </p>
        {!valid && (
          <p role="alert" className="text-destructive text-xs">
            Enter a whole number from {g.minMatchThreshold} to {g.maxMatchThreshold}.
          </p>
        )}
      </form>

      <div className="grid gap-2">
        <h3 className="font-semibold">Fetch-trusted hosts</h3>
        <p className="text-muted-foreground text-xs">
          Hosts a fetch server may reach without the guard stopping the call. An entry such as docs.example.com trusts exactly that host. An entry such as *.example.com trusts any subdomain at any depth, but not example.com itself, which needs its
          own entry. A wildcard over a public suffix such as *.com or *.co.uk is refused, and an entry is never a URL, a port or an IP address. The list starts empty.
        </p>
        {g.trustedHosts.length === 0 ? (
          <p className="text-muted-foreground">No trusted hosts.</p>
        ) : (
          <ul className="grid gap-1" aria-label="Fetch-trusted hosts">
            {g.trustedHosts.map((h) => (
              <li key={h} className="flex items-center gap-2">
                <code className="break-all">{h}</code>
                {h.startsWith('*.') && <Badge variant="secondary">Any subdomain of {h.slice(2)}</Badge>}
                <Button variant="outline" size="sm" className="ml-auto" aria-label={`Remove ${h}`} disabled={removeHost.isPending} onClick={() => removeHost.mutate({ orgId, host: h })}>
                  Remove
                </Button>
              </li>
            ))}
          </ul>
        )}
        <form onSubmit={add} className="flex flex-wrap items-end gap-2" aria-label="Add a trusted host">
          <div className="grid gap-1.5">
            <Label htmlFor="leak-host">Host</Label>
            <Input id="leak-host" value={host} onChange={(e) => setHost(e.target.value)} placeholder="docs.example.com or *.example.com" autoComplete="off" spellCheck={false} />
          </div>
          <Button type="submit" size="sm" disabled={host.trim() === '' || addHost.isPending}>
            Add host
          </Button>
        </form>
      </div>
      <div className="grid gap-2">
        <h3 className="font-semibold">Code-host scope</h3>
        <p className="text-muted-foreground text-xs">
          A write to a repository counts as internal only when the repository is in a code-host connection of this org and the connector read it as not public within the last {Math.round((g.codeHostScope?.visibilityMaxAgeSeconds ?? 3600) / 60)}{' '}
          minutes. A public, unknown, stale or unlisted repository counts as external, free text.
        </p>
        {!g.codeHostScope || g.codeHostScope.repositories === 0 ? (
          <p className="text-muted-foreground">No repositories listed: every code-host write counts as external, free text.</p>
        ) : (
          <p aria-label="Code-host scope">
            {g.codeHostScope.repositories} repositories: {g.codeHostScope.private} private, {g.codeHostScope.internal} internal, {g.codeHostScope.public} public, {g.codeHostScope.unknown} unknown; {g.codeHostScope.stale} stale.
          </p>
        )}
      </div>
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not save: {ConnectError.from(error).rawMessage}</AlertDescription>
        </Alert>
      )}
    </section>
  )
}
