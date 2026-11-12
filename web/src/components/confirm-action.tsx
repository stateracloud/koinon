// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useEffect, useRef, useState } from 'react'
import { Button } from '@/components/ui/button'

export type ConfirmActionProps = {
  /** The button that opens the confirmation, for example "Delete". */
  label: string
  /** Names the thing, for the button's accessible name: "Delete provider openrouter". */
  subject: string
  /** What the action does, said before the person confirms. */
  consequence: string
  confirmLabel: string
  /** Called when the question opens, for example to look up what the action would do. */
  onOpen?: () => void
  /** Keeps the confirm button off, for example while the consequence is still being looked up. */
  confirmDisabled?: boolean
  onConfirm: () => Promise<unknown>
  /** Called with the server's message, and the error itself for its details, when the action is refused. */
  onError?: (message: string, err: ConnectError) => void
}

/**
 * ConfirmAction asks once, in place, before a destructive action. The consequence is read out when the
 * question opens (role=alertdialog is avoided: nothing is trapped, the page stays usable).
 */
export function ConfirmAction({ label, subject, consequence, confirmLabel, onOpen, confirmDisabled, onConfirm, onError }: ConfirmActionProps) {
  const [asking, setAsking] = useState(false)
  const [pending, setPending] = useState(false)
  const trigger = useRef<HTMLButtonElement>(null)
  const confirmButton = useRef<HTMLButtonElement>(null)
  const returnFocus = useRef(false)

  // Focus follows the question: onto the confirm button when it opens, back onto the trigger when
  // it is dismissed (WCAG 2.4.3), so a keyboard user never lands at the top of the page.
  useEffect(() => {
    if (asking) confirmButton.current?.focus()
    else if (returnFocus.current) {
      returnFocus.current = false
      trigger.current?.focus()
    }
  }, [asking])

  if (!asking) {
    return (
      <Button ref={trigger} variant="outline" size="sm" onClick={() => {
          setAsking(true)
          onOpen?.()
        }} aria-label={`${label} ${subject}`}>
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
      returnFocus.current = true
      setAsking(false)
      const err = ConnectError.from(e)
      onError?.(err.rawMessage, err)
    } finally {
      setPending(false)
    }
  }
  return (
    <div role="group" aria-label={`Confirm: ${label} ${subject}`} className="flex flex-wrap items-center gap-2 text-sm">
      <span role="status">{consequence}</span>
      <Button ref={confirmButton} variant="destructive" size="sm" disabled={pending || confirmDisabled} onClick={() => void confirm()}>
        {confirmLabel}
      </Button>
      <Button variant="outline" size="sm" disabled={pending} onClick={() => {
          returnFocus.current = true
          setAsking(false)
        }}
      >
        Keep it
      </Button>
    </div>
  )
}
