// SPDX-License-Identifier: Apache-2.0

import { useState, type FormEvent, type KeyboardEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'

export type ComposerProps = {
  onSend: (text: string) => Promise<unknown>
  pending: boolean
  error?: string
  /** Distinguishes two composers on one page (ids and the form's name). */
  idPrefix?: string
  /** The textarea's label; "Message" by default. */
  label?: string
  /** The form's accessible name; "Reply" by default. */
  formLabel?: string
  /** The send button's text; "Send" by default. */
  submitLabel?: string
}

/** Composer is the reply box of a task's conversation. */
export function Composer({ onSend, pending, error, idPrefix = 'composer', label = 'Message', formLabel = 'Reply', submitLabel = 'Send' }: ComposerProps) {
  const [text, setText] = useState('')

  async function submit() {
    const msg = text.trim()
    if (!msg) return
    try {
      await onSend(msg)
      setText('')
    } catch {
      // shown through error; keep the draft
    }
  }

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    void submit()
  }

  function onKeyDown(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault()
      void submit()
    }
  }

  return (
    <form onSubmit={onSubmit} className="grid gap-2" aria-label={formLabel}>
      <Label htmlFor={idPrefix}>{label}</Label>
      <Textarea id={idPrefix} rows={3} value={text} onChange={(e) => setText(e.target.value)} onKeyDown={onKeyDown} aria-describedby={`${idPrefix}-hint`} />
      <div className="flex items-center gap-3">
        <span id={`${idPrefix}-hint`} className="text-muted-foreground text-xs">
          Ctrl or ⌘ + Enter sends.
        </span>
        <Button type="submit" className="ml-auto" disabled={pending || !text.trim()}>
          {submitLabel}
        </Button>
      </div>
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
    </form>
  )
}
