// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useState } from 'react'
import { closeOnly } from '@/lib/editing'
import { RequireAdmin } from '@/components/require-admin'
import { FullContentBadge, TraceDestinationCard } from '@/components/trace-destination-card'
import { HomeBackupRetentionSettings } from '@/components/home-backup-retention-settings'
import { RunContentRetentionSettings } from '@/components/run-content-retention-settings'
import { TraceDestinationForm } from '@/components/trace-destination-form'
import { TraceSettingsForm } from '@/components/trace-settings-form'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ContentLevel, TraceDestinationService, type Destination } from '@/gen/koinon/trace/v1alpha1/trace_pb'
import { useTraceActions } from '@/hooks/use-trace-actions'
import { useOrg } from '@/lib/org-context'
import { savedNotice, testNotice } from '@/lib/trace'
import { describeTraceError } from '@/lib/trace-errors'

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

// The edit form gets the destination as it was when Edit was clicked, so a refetch while the form is open
// cannot move the version the save is checked against.
type Editing = { kind: 'new' } | { kind: 'destination'; snapshot: Destination }

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
  // While the org sends full content, only an owner may add a destination or change a connection.
  const locked = full && org.role !== OrgRole.OWNER
  // A refusal is worded from its reason; one that says the destination changed also closes the form and reloads.
  const refuse = (e: unknown) => {
    const r = describeTraceError(e)
    setNotice(undefined)
    setRefused(r.message)
    if (r.changed) {
      setEditing(undefined)
      void actions.reload()
    }
  }
  const say = (m: string) => {
    setRefused(undefined)
    setNotice(m)
  }

  return (
    <>
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">Observability</h1>
        {full && <FullContentBadge />}
        <Button size="sm" className="ml-auto" onClick={() => setEditing({ kind: 'new' })} disabled={editing?.kind === 'new' || locked}>
          Add destination
        </Button>
      </div>
      <p className="text-muted-foreground text-sm">
        A destination is a trace backend, such as Langfuse or Datadog, that receives one trace for each run your agents make: the run, its turns, model calls and tool calls, with timings, status and cost. Content stays off unless you choose a level below. Koinon stores each destination&apos;s credentials encrypted and never shows them again.
      </p>
      {locked && (
        <p role="note" className="text-sm">
          Your org sends full content, so only an org owner can add a destination or change a destination&apos;s connection. You can still rename a destination or turn it on or off.
        </p>
      )}
      {editing?.kind === 'new' && (
        <TraceDestinationForm
          connectionLocked={locked}
          save={(input) => actions.createDestination(input).then((r) => r.destination!)}
          onDone={closeOnly(setEditing, { kind: 'new' })}
          onSaved={(d, tested) => say(savedNotice(d.name, d, tested))}
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
                full={full && d.enabled}
                subject={`destination ${d.name}`}
                test={async () => {
                  const r = await actions.testDestination(d.id)
                  say(testNotice(d.name, r.result))
                }}
                edit={() => setEditing({ kind: 'destination', snapshot: d })}
                remove={() => actions.deleteDestination(d.id)}
                onRemoved={() => say(`Deleted destination ${d.name}.`)}
                onRefused={refuse}
                editing={
                  editing?.kind === 'destination' && editing.snapshot.id === d.id ? (
                    <TraceDestinationForm
                      destination={editing.snapshot}
                      save={(input) => actions.updateDestination(d.id, input, editing.snapshot.configVersion).then((r) => r.destination!)}
                      connectionLocked={locked}
                      onConflict={(m) => {
                        closeOnly(setEditing, editing)()
                        setNotice(undefined)
                        setRefused(m)
                        void actions.reload()
                      }}
                      onDone={closeOnly(setEditing, editing)}
                      onSaved={(saved, tested) => say(savedNotice(saved.name, saved, tested))}
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
        s && <TraceSettingsForm key={`${s.contentLevel}-${s.sampleRate}-${s.platformCopyLevel}-${s.ceiling}-${s.hasPlatformDestination}`} settings={s} isOwner={org.role === OrgRole.OWNER} save={actions.updateSettings} onSaved={() => say('Saved the settings.')} />
      )}
      <RunContentRetentionSettings orgId={orgId} />
      <HomeBackupRetentionSettings orgId={orgId} />
    </>
  )
}
