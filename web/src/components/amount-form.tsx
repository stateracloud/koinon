// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { amountError } from '@/lib/money'

/**
 * AmountForm sets one dollar amount: the org cap, an agent’s daily limit or the platform ceiling. The amount is
 * checked like the server does (a positive decimal up to $1,000,000); the error shows beside the field.
 */
export function AmountForm({
  id,
  label,
  submitLabel,
  initial,
  help,
  onSave,
  onSaved,
}: {
  id: string
  label: string
  submitLabel: string
  initial: string
  help?: string
  onSave: (usd: string) => Promise<unknown>
  onSaved?: () => void
}) {
  const [value, setValue] = useState(initial)
  const [touched, setTouched] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()
  const invalid = amountError(label, value)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (invalid) return
    setPending(true)
    setError(undefined)
    try {
      await onSave(value.trim())
      onSaved?.()
    } catch (err) {
      setError(ConnectError.from(err).rawMessage)
    } finally {
      setPending(false)
    }
  }
  const describedBy = [help ? `${id}-help` : '', touched && invalid ? `${id}-error` : ''].filter(Boolean).join(' ') || undefined
  return (
    <form onSubmit={(e) => void submit(e)} noValidate aria-label={submitLabel} className="grid gap-2">
      <div className="flex flex-wrap items-end gap-2">
        <div className="grid gap-1.5">
          <Label htmlFor={id}>{label}</Label>
          <div className="flex items-center gap-1">
            <span aria-hidden="true">$</span>
            <Input id={id} inputMode="decimal" className="w-36" value={value} onChange={(e) => setValue(e.target.value)} autoComplete="off" aria-invalid={touched && !!invalid} aria-describedby={describedBy} />
          </div>
        </div>
        <Button type="submit" size="sm" disabled={pending}>
          {submitLabel}
        </Button>
      </div>
      {help && (
        <p id={`${id}-help`} className="text-muted-foreground text-xs">
          {help}
        </p>
      )}
      {touched && invalid && (
        <p id={`${id}-error`} className="text-destructive text-xs">
          {invalid}
        </p>
      )}
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
    </form>
  )
}
