// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useState } from 'react'
import { RequireAdmin } from '@/components/require-admin'
import { FullContentBadge, TraceDestinationCard } from '@/components/trace-destination-card'
import { TraceDestinationForm } from '@/components/trace-destination-form'
import { TraceSettingsForm } from '@/components/trace-settings-form'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ContentLevel, TraceDestinationService } from '@/gen/koinon/trace/v1alpha1/trace_pb'
import { useTraceActions } from '@/hooks/use-trace-actions'
import { useOrg } from '@/lib/org-context'
import { testNotice } from '@/lib/trace'

/** ObservabilityPage is where org admins send their runs' traces to a backend and choose what the traces carry (ADR 0074). */
export function ObservabilityPage() {
  return (
    <div className="mx-auto grid max-w-3xl grid-cols-[minmax(0,1fr)] gap-4">
      <RequireAdmin what="observability">
        <ObservabilityContent />
      </RequireAdmin>
    </div>
  )
}

type Editing = { kind: 'new' } | { kind: 'destination'; id: string }

function ObservabilityContent() {
  const { org } = useOrg()
  const orgId = org.orgId
  const destinations = useQuery(TraceDestinationService.method.listDestinations, { orgId })
  const settings = useQuery(TraceDestinationService.method.getSettings, { orgId })
  const actions = useTraceActions(orgId)
  const [editing, setEditing] = useState<Editing>()
  const [notice, setNotice] = useState<string>()
  const [refused, setRefused] = useState<string>()
  const failed = destinations.error ?? settings.error
  const list = destinations.data?.destinations ?? []
  const s = settings.data?.settings
  const full = s?.contentLevel === ContentLevel.FULL
  const say = (m: string) => {
    setRefused(undefined)
    setNotice(m)
  }

  return (
    <>
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">Observability</h1>
        {full && <FullContentBadge />}
        <Button size="sm" className="ml-auto" onClick={() => setEditing({ kind: 'new' })} disabled={editing?.kind === 'new'}>
          Add destination
        </Button>
      </div>
      <p className="text-muted-foreground text-sm">
        A destination is a trace backend, such as Langfuse or Datadog, that receives one trace for each run your agents make: the run, its turns, model calls and tool calls, with timings, status and cost. Content stays off unless you choose a level below. Koinon stores each destination&apos;s credentials encrypted and never shows them again.
      </p>
      {editing?.kind === 'new' && (
        <TraceDestinationForm
          save={(input) => actions.createDestination(input).then((r) => r.destination!)}
          onDone={() => setEditing(undefined)}
          onSaved={(d) => say(`Saved ${d.name}. ${testNotice(d.name, d.lastTest)}`)}
        />
      )}
      {refused && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refused}</AlertDescription>
        </Alert>
      )}
      {notice && (
        <p role="status" className="text-sm">
          {notice}
        </p>
      )}
      {failed && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(failed).rawMessage}</AlertDescription>
        </Alert>
      )}
      {destinations.isPending ? (
        <p role="status">Loading destinations…</p>
      ) : list.length === 0 ? (
        <p className="text-muted-foreground text-sm">No destinations yet. Add one to send your runs&apos; traces to a backend.</p>
      ) : (
        <ul className="grid gap-4" aria-label="Destinations">
          {list.map((d) => (
            <li key={d.id}>
              <TraceDestinationCard
                destination={d}
                full={full}
                subject={`destination ${d.name}`}
                test={async () => {
                  const r = await actions.testDestination(d.id)
                  say(testNotice(d.name, r.result))
                }}
                edit={() => setEditing({ kind: 'destination', id: d.id })}
                remove={() => actions.deleteDestination(d.id)}
                onRemoved={() => say(`Deleted destination ${d.name}.`)}
                onRefused={(m) => {
                  setNotice(undefined)
                  setRefused(m)
                }}
                editing={
                  editing?.kind === 'destination' && editing.id === d.id ? (
                    <TraceDestinationForm
                      destination={d}
                      save={(input) => actions.updateDestination(d.id, input).then((r) => r.destination!)}
                      onDone={() => setEditing(undefined)}
                      onSaved={(saved) => say(`Saved ${saved.name}. ${testNotice(saved.name, saved.lastTest)}`)}
                    />
                  ) : undefined
                }
              />
            </li>
          ))}
        </ul>
      )}
      {settings.isPending ? (
        <p role="status">Loading settings…</p>
      ) : (
        s && <TraceSettingsForm key={`${s.contentLevel}-${s.sampleRate}-${s.platformCopyLevel}-${s.ceiling}-${s.hasPlatformDestination}`} settings={s} isOwner={org.role === OrgRole.OWNER} save={actions.updateSettings} />
      )}
    </>
  )
}
