// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useState, type ReactNode } from 'react'
import { ConfirmAction } from '@/components/confirm-action'
import { OutwardSwitch } from '@/components/outward-switch'
import { OutwardTargetKind } from '@/gen/koinon/outward/v1alpha1/outward_pb'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import type { Destination } from '@/gen/koinon/trace/v1alpha1/trace_pb'
import { formatTime } from '@/lib/providers'
import { backendLabel, formatCount } from '@/lib/trace'

/** FullContentBadge marks anything that sends full content (ADR 0074, decision 6). */
export function FullContentBadge() {
  return <Badge variant="destructive">Full content</Badge>
}

export type TraceDestinationCardProps = {
  destination: Destination
  /** Heading shown instead of the destination's name, for the platform destination. */
  title?: string
  /** The destination sends full content. */
  full?: boolean
  /** Runs the saved destination's test again. */
  test: () => Promise<unknown>
  /** Set to offer Edit. */
  edit?: () => void
  /** Set to offer Delete. */
  remove?: () => Promise<unknown>
  subject: string
  onRemoved?: () => void
  onRefused: (err: ConnectError) => void
  /** The edit form, shown in place of the buttons. */
  editing?: ReactNode
}

/** TraceDestinationCard shows one destination: its backend, endpoint, last test and health, and its actions. */
export function TraceDestinationCard({ destination: d, title, full, test, edit, remove, subject, onRemoved, onRefused, editing }: TraceDestinationCardProps) {
  const [testing, setTesting] = useState(false)
  const id = `dest-card-${d.id || 'platform'}`
  async function runTest() {
    setTesting(true)
    try {
      await test()
    } catch (e) {
      onRefused(ConnectError.from(e))
    } finally {
      setTesting(false)
    }
  }
  const h = d.health
  const t = d.lastTest
  return (
    <section aria-labelledby={id} className="bg-card grid gap-3 rounded-md border p-4">
      <div className="flex flex-wrap items-center gap-2">
        <h2 id={id} className="text-base font-semibold">
          {title ?? d.name}
        </h2>
        <Badge variant="outline">{backendLabel(d.backend)}</Badge>
        <Badge variant={d.enabled ? 'secondary' : 'outline'}>{d.enabled ? 'Enabled' : 'Disabled'}</Badge>
        {full && <FullContentBadge />}
        {d.adminDisabled && <Badge variant="destructive">Switched off</Badge>}
        {d.credentialLostAt && <Badge variant="destructive">Credentials lost</Badge>}
      </div>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted-foreground">Endpoint</dt>
        <dd className="break-all">{d.endpointUrl}</dd>
        {Object.entries(d.params).map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-muted-foreground capitalize">{k}</dt>
            <dd className="break-all">{v}</dd>
          </div>
        ))}
        <dt className="text-muted-foreground">Credentials</dt>
        <dd>
          {d.credentialLostAt
            ? `Lost (${d.secretFields.join(', ')}); edit the destination and enter them again. Nothing is exported until then.`
            : d.secretFields.length === 0
              ? 'None'
              : `Set (${d.secretFields.join(', ')}); stored encrypted, never shown`}
        </dd>
        <dt className="text-muted-foreground">Last test</dt>
        <dd>{t ? `${t.ok ? 'Passed' : 'Failed'}, ${formatTime(t.testedAt, 'time unknown')}` : 'Not tested yet'}</dd>
        <dt className="text-muted-foreground">Last success</dt>
        <dd>{formatTime(h?.lastSuccessAt, 'Never')}</dd>
        <dt className="text-muted-foreground">Last error</dt>
        <dd>{h?.lastErrorAt ? `${formatTime(h.lastErrorAt)}${h.lastError ? `: ${h.lastError}` : ''}` : 'None'}</dd>
        <dt className="text-muted-foreground">Spans dropped</dt>
        <dd>{formatCount(h?.spansDropped ?? 0n)}</dd>
      </dl>
      {t && !t.ok && (
        <Alert variant="destructive" role="note">
          <AlertDescription>The last test failed{t.message ? `: ${t.message}` : '.'}</AlertDescription>
        </Alert>
      )}
      <OutwardSwitch kind={d.id ? OutwardTargetKind.TRACE_DESTINATION : OutwardTargetKind.PLATFORM_TRACE_DESTINATION} targetId={d.id} subject={subject} disabled={d.adminDisabled} />
      {editing ?? (
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" aria-label={`Test ${subject}`} disabled={testing} onClick={() => void runTest()}>
            {testing ? 'Testing…' : 'Test'}
          </Button>
          {edit && (
            <Button variant="outline" size="sm" aria-label={`Edit ${subject}`} onClick={edit}>
              Edit
            </Button>
          )}
          {remove && (
            <ConfirmAction
              label="Delete"
              subject={subject}
              consequence={`Delete ${subject}? Traces stop going to it. Traces already sent stay in the backend.`}
              confirmLabel="Delete destination"
              onConfirm={async () => {
                await remove()
                onRemoved?.()
              }}
              onError={(_, err) => onRefused(err)}
            />
          )}
        </div>
      )}
    </section>
  )
}
