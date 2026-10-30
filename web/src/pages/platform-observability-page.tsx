// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useState, type FormEvent } from 'react'
import { TraceDestinationCard } from '@/components/trace-destination-card'
import { TraceDestinationForm } from '@/components/trace-destination-form'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ContentLevel, TraceDestinationService } from '@/gen/koinon/trace/v1alpha1/trace_pb'
import { usePlatformTraceActions, type PlatformTraceActions } from '@/hooks/use-trace-actions'
import { levelLabel, levelRank, levels, normalLevel, testNotice } from '@/lib/trace'

/**
 * PlatformObservabilityPage is where a platform admin sets the platform's trace destination and the
 * highest content level an org may pick (ADR 0074). Others get a notice.
 */
export function PlatformObservabilityPage() {
  const who = useQuery(AuthService.method.whoAmI, {})
  return (
    <div className="mx-auto grid max-w-3xl grid-cols-[minmax(0,1fr)] gap-4">
      {who.isPending ? (
        <p role="status">Loading…</p>
      ) : who.data?.platformAdmin ? (
        <PlatformObservability />
      ) : (
        <Alert role="alert">
          <AlertDescription>Only platform admins can manage platform observability.</AlertDescription>
        </Alert>
      )}
    </div>
  )
}

function PlatformObservability() {
  const platform = useQuery(TraceDestinationService.method.getPlatform, {})
  const actions = usePlatformTraceActions()
  const [editing, setEditing] = useState(false)
  const [notice, setNotice] = useState<string>()
  const [refused, setRefused] = useState<string>()
  const d = platform.data?.destination
  const ceiling = normalLevel(platform.data?.ceiling ?? ContentLevel.NONE)
  const say = (m: string) => {
    setRefused(undefined)
    setNotice(m)
  }
  return (
    <>
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">Platform observability</h1>
        {!platform.isPending && !d && !editing && (
          <Button size="sm" className="ml-auto" onClick={() => setEditing(true)}>
            Set destination
          </Button>
        )}
      </div>
      <p className="text-muted-foreground text-sm">
        You are acting as a platform admin. The platform destination receives a copy of every org&apos;s traces. The copy carries no content, and org names are replaced with IDs, unless an org lets its copy carry more, up to its own level and never above the ceiling below.
      </p>
      {platform.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(platform.error).rawMessage}</AlertDescription>
        </Alert>
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
      {platform.isPending ? (
        <p role="status">Loading…</p>
      ) : (
        <>
          {editing && !d && (
            <TraceDestinationForm
              platform
              save={(input) => actions.setDestination(input).then((r) => r.destination!)}
              onDone={() => setEditing(false)}
              onSaved={(saved) => say(`Saved the platform destination. ${testNotice('the platform destination', saved.lastTest)}`)}
            />
          )}
          {d ? (
            <TraceDestinationCard
              destination={d}
              title="Platform destination"
              subject="the platform destination"
              test={async () => {
                const r = await actions.testDestination()
                say(testNotice('the platform destination', r.result))
              }}
              edit={() => setEditing(true)}
              remove={() => actions.deleteDestination()}
              onRemoved={() => say('Deleted the platform destination.')}
              onRefused={(m) => {
                setNotice(undefined)
                setRefused(m)
              }}
              editing={
                editing ? (
                  <TraceDestinationForm
                    platform
                    destination={d}
                    save={(input) => actions.setDestination(input).then((r) => r.destination!)}
                    onDone={() => setEditing(false)}
                    onSaved={(saved) => say(`Saved the platform destination. ${testNotice('the platform destination', saved.lastTest)}`)}
                  />
                ) : undefined
              }
            />
          ) : (
            !editing && <p className="text-muted-foreground text-sm">No platform destination. Orgs still send traces to their own destinations.</p>
          )}
          <CeilingForm key={ceiling} ceiling={ceiling} actions={actions} onSaved={say} onRefused={setRefused} />
        </>
      )}
    </>
  )
}

function CeilingForm({ ceiling, actions, onSaved, onRefused }: { ceiling: ContentLevel; actions: PlatformTraceActions; onSaved: (m: string) => void; onRefused: (m: string) => void }) {
  const [level, setLevel] = useState(ceiling)
  const [pending, setPending] = useState(false)
  const lowering = levelRank(level) < levelRank(ceiling)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setPending(true)
    try {
      const r = await actions.setCeiling(level)
      onSaved(
        r.orgsLowered > 0
          ? `The ceiling is now ${levelLabel(normalLevel(r.ceiling)).toLowerCase()}. ${r.orgsLowered} ${r.orgsLowered === 1 ? 'org was' : 'orgs were'} lowered to it.`
          : `The ceiling is now ${levelLabel(normalLevel(r.ceiling)).toLowerCase()}. No org was above it.`,
      )
    } catch (e2) {
      onRefused(ConnectError.from(e2).rawMessage)
    } finally {
      setPending(false)
    }
  }

  return (
    <form onSubmit={(e) => void submit(e)} aria-label="Content ceiling" className="bg-card grid gap-4 rounded-md border p-4">
      <h2 className="text-base font-semibold">Content ceiling</h2>
      <p className="text-muted-foreground text-sm">The highest content level any org may choose. It is {levelLabel(ceiling).toLowerCase()} now.</p>
      <fieldset className="grid gap-2">
        <legend className="sr-only">Ceiling</legend>
        {levels.map((l) => (
          <label key={l.value} className="flex items-start gap-2 text-sm">
            <input type="radio" name="ceiling" className="mt-1" value={l.value} checked={level === l.value} disabled={pending} aria-describedby={`ceiling-${l.value}-help`} onChange={() => setLevel(l.value)} />
            <span>
              <span className="font-medium">{l.label}</span>
              <span id={`ceiling-${l.value}-help`} className="text-muted-foreground block text-xs">
                {l.help}
              </span>
            </span>
          </label>
        ))}
      </fieldset>
      {lowering && (
        <Alert role="note">
          <AlertDescription>Lowering the ceiling lowers every org that is above it to {levelLabel(level).toLowerCase()}, at once. The change is recorded in the audit log.</AlertDescription>
        </Alert>
      )}
      <div className="flex justify-end">
        <Button type="submit" variant={lowering ? 'destructive' : 'default'} disabled={pending || level === ceiling}>
          {lowering ? 'Lower the ceiling' : 'Save ceiling'}
        </Button>
      </div>
    </form>
  )
}
