// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { createClient } from '@connectrpc/connect'
import { useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { OutwardService, OutwardTargetKind, type AdminDisabled } from '@/gen/koinon/outward/v1alpha1/outward_pb'
import { formatTime } from '@/lib/providers'
import { useOrg } from '@/lib/org-context'

export type OutwardSwitchProps = {
  kind: OutwardTargetKind
  /** The target's id; empty for the platform trace destination. */
  targetId: string
  /** Names the target for the buttons' accessible names: "Switch off outward calls of provider openrouter". */
  subject: string
  /** The target's adminDisabled from Get or List; unset when it is enabled. */
  disabled?: AdminDisabled
}

/**
 * OutwardSwitch is the one admin switch for a target Koinon calls outside the install (ADR 0110 decision 5).
 * Disabling is always offered; enabling is refused by the server while the install is held for a restore,
 * and for a target whose declaring Secret says disabled: true. The server's message is shown as it is.
 */
export function OutwardSwitch({ kind, targetId, subject, disabled }: OutwardSwitchProps) {
  const { org } = useOrg()
  const transport = useTransport()
  const queryClient = useQueryClient()
  const [pending, setPending] = useState(false)
  const [refused, setRefused] = useState('')
  const [note, setNote] = useState('')

  async function set(off: boolean) {
    setPending(true)
    setRefused('')
    try {
      await createClient(OutwardService, transport).setOutwardTargetDisabled({ orgId: org.orgId, kind, targetId, disabled: off, note: off ? note.trim() : '' })
      setNote('')
      await queryClient.invalidateQueries()
    } catch (e) {
      setRefused(ConnectError.from(e).rawMessage)
    } finally {
      setPending(false)
    }
  }

  return (
    <div className="grid gap-2">
      {disabled && (
        <Alert variant="destructive" role="note">
          <AlertDescription>
            <Badge variant="destructive">Switched off by an admin</Badge> Koinon does not call it, probe it or offer it to runs. Its settings and credential are kept. Since {formatTime(disabled.at, '')} by {disabled.by}
            {disabled.note ? `: ${disabled.note}` : '.'}
            {disabled.declared && ' Its declaring Secret says disabled: true; remove that field from the Secret to enable it.'}
          </AlertDescription>
        </Alert>
      )}
      <div className="flex flex-wrap items-center gap-2">
        {disabled ? (
          !disabled.declared && (
            <Button variant="outline" size="sm" disabled={pending} onClick={() => void set(false)} aria-label={`Enable outward calls of ${subject}`}>
              Enable
            </Button>
          )
        ) : (
          <>
            <input
              className="bg-background h-8 min-w-48 rounded-md border px-2 text-sm"
              aria-label={`Reason for switching off ${subject}`}
              placeholder="Reason (optional)"
              maxLength={500}
              value={note}
              onChange={(e) => setNote(e.target.value)}
            />
            <Button variant="outline" size="sm" disabled={pending} onClick={() => void set(true)} aria-label={`Switch off outward calls of ${subject}`}>
              Switch off
            </Button>
          </>
        )}
      </div>
      {refused && (
        <p role="alert" className="text-destructive text-sm">
          {refused}
        </p>
      )}
    </div>
  )
}
