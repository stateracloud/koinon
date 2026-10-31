// SPDX-License-Identifier: Apache-2.0

import { timestampDate } from '@bufbuild/protobuf/wkt'
import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { EchoDiffSegment_Op, ProviderService, type EchoCheck, type Model, type Provider } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { useOrg } from '@/lib/org-context'

/** Diff shows one side of an echo check's diff, the line sent or the reply, marking what differs. */
function Diff({ check, side }: { check: EchoCheck; side: 'sent' | 'received' }) {
  const hide = side === 'sent' ? EchoDiffSegment_Op.INSERTED : EchoDiffSegment_Op.DELETED
  const mark = side === 'sent' ? EchoDiffSegment_Op.DELETED : EchoDiffSegment_Op.INSERTED
  return (
    <code className="break-words whitespace-pre-wrap">
      {check.diff
        .filter((d) => d.op !== hide)
        .map((d, i) =>
          d.op === mark ? (
            side === 'sent' ? (
              <del key={i} className="bg-destructive/15">
                {d.text}
              </del>
            ) : (
              <ins key={i} className="bg-primary/15">
                {d.text}
              </ins>
            )
          ) : (
            <span key={i}>{d.text}</span>
          ),
        )}
    </code>
  )
}

/**
 * ProviderEchoCheck runs and shows the provider's echo check: one short request through the gateway
 * that asks a model to repeat a line exactly. A provider-side setting that masks names and addresses
 * changes the line, and masking corrupts what agents send. It runs only when an admin asks.
 */
export function ProviderEchoCheck({ provider, models }: { provider: Provider; models: Model[] }) {
  const { org } = useOrg()
  const queryClient = useQueryClient()
  const usable = models.filter((m) => m.disabledReason === '')
  const [picked, setPicked] = useState<string>()
  const model = picked ?? provider.echoCheck?.model ?? usable[0]?.name ?? ''
  const check = useMutation(ProviderService.method.checkProviderEcho, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ProviderService, cardinality: undefined }) }),
  })
  const last = provider.echoCheck
  return (
    <div className="grid gap-2 border-t pt-3" aria-label={`Echo check of ${provider.name}`} role="group">
      <h3 className="text-sm font-semibold">Echo check</h3>
      <p className="text-muted-foreground text-xs">
        Sends one short request through the gateway and asks the model to repeat a line with a name, an address and a Kubernetes namespace exactly. If the reply differs, the provider is changing the text it is sent, for example with
        an account setting that masks personal data. Masking corrupts agent work: the names, paths and namespaces in prompts and tool calls are no longer the ones the agent meant. The request is billed like any other. It never runs on its own.
      </p>
      {usable.length === 0 ? (
        <p className="text-muted-foreground text-sm">Add a model with a price to check this provider.</p>
      ) : (
        <div className="flex flex-wrap items-end gap-2">
          <div className="grid gap-1.5">
            <Label htmlFor={`echo-model-${provider.id}`}>Model to check</Label>
            <NativeSelect id={`echo-model-${provider.id}`} value={model} onChange={(e) => setPicked(e.target.value)}>
              {usable.map((m) => (
                <option key={m.id} value={m.name}>
                  {m.name}
                </option>
              ))}
            </NativeSelect>
          </div>
          <Button
            variant="outline"
            size="sm"
            aria-label={`Run echo check on ${provider.name}`}
            disabled={check.isPending || model === ''}
            onClick={() => check.mutate({ orgId: org.orgId, providerId: provider.id, model })}
          >
            {check.isPending ? 'Checking…' : 'Run echo check'}
          </Button>
        </div>
      )}
      {check.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>The check did not run: {ConnectError.from(check.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      {last && (
        <div className="grid gap-1 text-sm" role="status" aria-label="Latest echo check">
          <p className="flex flex-wrap items-center gap-2">
            {last.unchanged ? <Badge variant="secondary">Unchanged</Badge> : <Badge variant="destructive">Changed</Badge>}
            <span>
              <code>{last.model}</code>, checked {last.checkedAt ? timestampDate(last.checkedAt).toLocaleString() : ''}
            </span>
          </p>
          {last.unchanged ? (
            <p className="text-muted-foreground">The reply matched the line character for character.</p>
          ) : (
            <>
              <p>The provider changed the line. Agents on this provider may send altered names, paths and namespaces; turn off the masking on the provider&apos;s side.</p>
              <p>
                Sent: <Diff check={last} side="sent" />
              </p>
              <p>
                Received: <Diff check={last} side="received" />
              </p>
            </>
          )}
        </div>
      )}
    </div>
  )
}
