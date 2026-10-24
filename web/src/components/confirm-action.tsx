// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useState } from 'react'
import { Button } from '@/components/ui/button'

export type ConfirmActionProps = {
  /** The button that opens the confirmation, for example "Delete". */
  label: string
  /** Names the thing, for the button's accessible name: "Delete provider openrouter". */
  subject: string
  /** What the action does, said before the person confirms. */
  consequence: string
  confirmLabel: string
  onConfirm: () => Promise<unknown>
  /** Called with the server's message when the action is refused. */
  onError?: (message: string) => void
}

/**
 * ConfirmAction asks once, in place, before a destructive action. The consequence is read out when the
 * question opens (role=alertdialog is avoided: nothing is trapped, the page stays usable).
 */
export function ConfirmAction({ label, subject, consequence, confirmLabel, onConfirm, onError }: ConfirmActionProps) {
  const [asking, setAsking] = useState(false)
  const [pending, setPending] = useState(false)

  if (!asking) {
    return (
      <Button variant="outline" size="sm" onClick={() => setAsking(true)} aria-label={`${label} ${subject}`}>
        {label}
      </Button>
    )
  }
  async function confirm() {
    setPending(true)
    try {
      await onConfirm()
      setAsking(false)
    } catch (e) {
      setAsking(false)
      onError?.(ConnectError.from(e).rawMessage)
    } finally {
      setPending(false)
    }
  }
  return (
    <div role="group" aria-label={`Confirm: ${label} ${subject}`} className="flex flex-wrap items-center gap-2 text-sm">
      <span role="status">{consequence}</span>
      <Button variant="destructive" size="sm" disabled={pending} onClick={() => void confirm()}>
        {confirmLabel}
      </Button>
      <Button variant="outline" size="sm" disabled={pending} onClick={() => setAsking(false)}>
        Keep it
      </Button>
    </div>
  )
}
