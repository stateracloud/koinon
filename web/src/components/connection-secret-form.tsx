// SPDX-License-Identifier: Apache-2.0

import { useState, type FormEvent } from 'react'
import { SecretTextarea } from '@/components/secret-textarea'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { type ConnectionSecretInput, secretErrors } from '@/lib/connection-secret'

export type ConnectionSecretFormProps = {
  connector: 'github' | 'gitea'
  /** The stored secret was lost: the form says so and starts open. */
  lost: boolean
  /** Resolves when the secret is stored; rejects to keep the form open. */
  onSubmit: (input: ConnectionSecretInput) => Promise<unknown>
  pending?: boolean
  error?: string
}

const secretInputProps = {
  autoComplete: 'off',
  autoCorrect: 'off',
  autoCapitalize: 'off',
  spellCheck: false,
  'data-1p-ignore': true,
  'data-lpignore': 'true',
  'data-secret': 'true',
} as const

/**
 * ConnectionSecretForm enters a connection's secret fields again: a GitHub App's private key, or a
 * Gitea bot's two tokens (ADR 0085 decision 13), to replace one that was lost with its
 * key-encryption key or to rotate one. Every field is a secure input (ADR 0021): cleared as soon as
 * the form is submitted, whatever the outcome, and never shown again.
 */
export function ConnectionSecretForm({ connector, lost, onSubmit, pending, error }: ConnectionSecretFormProps) {
  const [values, setValues] = useState<Record<string, string>>({})
  const [cleared, setCleared] = useState(false)
  const [touched, setTouched] = useState(false)
  const input: ConnectionSecretInput =
    connector === 'github'
      ? { connector, privateKey: values.privateKey ?? '' }
      : { connector, readToken: values.readToken ?? '', writeToken: values.writeToken ?? '' }
  const errors = secretErrors(input)
  const noun = connector === 'github' ? 'private key' : 'tokens'

  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (Object.values(errors).some(Boolean)) return
    // Clear the secret before the request leaves: it lives in this component only for as long as it
    // takes to send it.
    setValues({})
    setCleared(true)
    setTouched(false)
    try {
      await onSubmit(input)
    } catch {
      // shown through error
    }
  }

  function field(id: string, label: string, help: string) {
    const htmlId = `secret-${id}`
    const err = touched ? errors[id] : undefined
    const props = {
      id: htmlId,
      value: values[id] ?? '',
      'aria-invalid': !!err,
      'aria-describedby': [`${htmlId}-help`, err && `${htmlId}-error`].filter(Boolean).join(' '),
      onChange: (e: { target: { value: string } }) => {
        setValues((s) => ({ ...s, [id]: e.target.value }))
        setCleared(false)
      },
    }
    return (
      <div key={id} className="grid gap-1.5">
        <Label htmlFor={htmlId}>{label}</Label>
        {connector === 'github' ? (
          <SecretTextarea rows={6} placeholder="-----BEGIN RSA PRIVATE KEY-----" {...props} />
        ) : (
          <Input type="password" className="font-mono" {...secretInputProps} {...props} />
        )}
        <p id={`${htmlId}-help`} className="text-muted-foreground text-xs">
          {help}
        </p>
        {err && (
          <p id={`${htmlId}-error`} className="text-destructive text-xs">
            {err}
          </p>
        )}
      </div>
    )
  }

  return (
    <details className="rounded-md border p-3" open={lost || undefined}>
      <summary className="cursor-pointer text-sm font-medium">{lost ? `Enter the ${noun} again` : `Replace the ${noun}`}</summary>
      <form onSubmit={(e) => void submit(e)} aria-label={`Set the connection's ${noun}`} className="mt-3 grid gap-3" noValidate>
        {connector === 'github'
          ? field(
              'privateKey',
              'Private key',
              "The App's .pem private key: the one GitHub still lists under the App's private keys, or a new one you generate there. The App ID and installation stay.",
            )
          : [
              field('readToken', 'Read token', "The bot's read-scoped token. The same token as before keeps the date it was entered."),
              field('writeToken', 'Write token', "The bot's write-scoped token, a different token from the read token."),
            ]}
        {cleared && (
          <p className="text-muted-foreground text-xs" role="status">
            The {noun} {connector === 'github' ? 'was' : 'were'} sent and cleared from this page. Paste {connector === 'github' ? 'it' : 'them'} again only to
            retry.
          </p>
        )}
        {error && (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}
        <div className="flex justify-end">
          <Button type="submit" disabled={pending}>
            {pending ? 'Storing…' : `Store and test`}
          </Button>
        </div>
      </form>
    </details>
  )
}
