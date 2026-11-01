// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { ProviderService } from '@/gen/koinon/provider/v1alpha1/provider_pb'

/** noDefaultChoice is the select's value for "no default model". */
const noDefaultChoice = ''

/**
 * OrgDefaultModel is where an org admin sets the model that agents without a model setting run on
 * (ADR 0086). It lists the models the org serves and has a price for. The org's first model becomes
 * the default on its own; deleting the default model clears it, and nothing picks another one, so an
 * org whose first model had no price, or whose default was deleted, sets it here.
 */
export function OrgDefaultModel({ orgId }: { orgId: string }) {
  const queryClient = useQueryClient()
  const query = useQuery(ProviderService.method.getDefaultModel, { orgId })
  const [picked, setPicked] = useState<string>()
  const save = useMutation(ProviderService.method.setDefaultModel, {
    onSuccess: async () => {
      setPicked(undefined)
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ProviderService, cardinality: undefined }) })
    },
  })
  if (query.error) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>Could not load the org default model: {ConnectError.from(query.error).rawMessage}</AlertDescription>
      </Alert>
    )
  }
  const d = query.data
  if (!d) return <p role="status">Loading the org default model…</p>
  const current = d.model
  const value = picked ?? current
  // The current default stays selectable when it is no longer offered (unpriced since), so the
  // select never shows a value that is not the stored one.
  const offered = d.available.some((c) => c.name === current)
  return (
    <section aria-labelledby="org-default-model" className="bg-card grid gap-3 rounded-lg border p-4 text-sm">
      <h2 id="org-default-model" className="font-semibold">
        Org default model
      </h2>
      {current === '' && <p className="text-muted-foreground">The org has no default model, so an agent that names no model fails its runs. Pick one below.</p>}
      <div className="grid gap-1.5">
        <Label htmlFor="org-default-model-pick">Model for agents that name none</Label>
        <NativeSelect id="org-default-model-pick" value={value} onChange={(e) => setPicked(e.target.value)} aria-describedby="org-default-model-help">
          <option value={noDefaultChoice}>No default model</option>
          {current !== '' && !offered && <option value={current}>{current} (not served or not priced)</option>}
          {d.available.map((c) => (
            <option key={c.name} value={c.name}>
              {c.name} ({c.provider})
            </option>
          ))}
        </NativeSelect>
        <p id="org-default-model-help" className="text-muted-foreground text-xs">
          Changing it applies to the next run of every agent that names no model, and is recorded in the audit log. The org&apos;s first model is the default until you change it. If the default model is deleted, the default is cleared and no other model is picked for you.
        </p>
      </div>
      {save.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not save: {ConnectError.from(save.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      <div>
        <Button size="sm" disabled={save.isPending || value === current} onClick={() => save.mutate({ orgId, model: value })}>
          Save default model
        </Button>
      </div>
    </section>
  )
}
