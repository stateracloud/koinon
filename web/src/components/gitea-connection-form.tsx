// SPDX-License-Identifier: Apache-2.0

import { useState, type FormEvent } from 'react'
import { SetupGuideSteps } from '@/components/setup-guide'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import type { CheckResult } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { caBundleError } from '@/lib/ca-bundle'
import { ciDirsError, parseCiDirs } from '@/lib/ci-paths'
import type { GuideField, SetupGuide } from '@/lib/connector'
import { giteaBaseUrlError } from '@/lib/gitea-url'

export type GiteaConnectionInput = {
  displayName: string
  baseUrl: string
  readToken: string
  writeToken: string
  caBundle: string
  ciDirs: string[]
}

export type GiteaConnectionFormProps = {
  guide: SetupGuide
  /** Resolves when the connection is created; rejects to keep the form open. */
  onSubmit: (input: GiteaConnectionInput) => Promise<unknown>
  pending?: boolean
  error?: string
  results?: readonly CheckResult[]
}

/** tokenError: a pasted token is one word; Gitea's are 40 hex characters but the format is not ours to enforce. */
function tokenError(label: string, v: string): string | undefined {
  if (!v.trim()) return `${label} is required.`
  if (/\s/.test(v.trim())) return `${label} must be only the token, without spaces or line breaks.`
  if (v.length > 256) return `${label} is longer than 256 characters.`
  return undefined
}

/**
 * GiteaConnectionForm walks the Gitea setup guide (ADR 0063): the server's URL (with its path when
 * Gitea is served under one), and the bot's read token and write token in the step that asks for
 * them. Both tokens are secure inputs: they are cleared as soon as the form is submitted, whatever
 * the outcome, and never shown again (the API does not return them).
 */
export function GiteaConnectionForm({ guide, onSubmit, pending, error, results }: GiteaConnectionFormProps) {
  const [displayName, setDisplayName] = useState('Gitea')
  const [baseUrl, setBaseUrl] = useState('')
  const [caBundle, setCaBundle] = useState('')
  const [ciDirsText, setCiDirsText] = useState('')
  const [tokens, setTokens] = useState<Record<string, string>>({})
  const [secretCleared, setSecretCleared] = useState(false)
  const [touched, setTouched] = useState(false)

  const fields = new Map(guide.fields.map((f) => [f.id, f] as const))
  const label = (id: string, fallback: string) => fields.get(id)?.label ?? fallback
  const errors: Record<string, string | undefined> = {
    readToken: tokenError(label('readToken', 'Read token'), tokens.readToken ?? ''),
    writeToken: tokenError(label('writeToken', 'Write token'), tokens.writeToken ?? ''),
  }
  if (!errors.writeToken && !errors.readToken && tokens.readToken?.trim() === tokens.writeToken?.trim()) {
    errors.writeToken = 'The write token must be a different token from the read token.'
  }
  const urlErr = giteaBaseUrlError(baseUrl)
  const caErr = caBundleError(caBundle)
  const ciErr = ciDirsError(ciDirsText)
  const nameErr = displayName.trim() ? undefined : 'Name is required.'

  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (nameErr || urlErr || caErr || ciErr || errors.readToken || errors.writeToken) return
    const input: GiteaConnectionInput = {
      displayName: displayName.trim(),
      baseUrl: baseUrl.trim(),
      readToken: (tokens.readToken ?? '').trim(),
      writeToken: (tokens.writeToken ?? '').trim(),
      caBundle: caBundle.trim(),
      ciDirs: parseCiDirs(ciDirsText),
    }
    // Clear both tokens before the request leaves: they live in this component only for as long as
    // it takes to send them.
    setTokens({})
    setSecretCleared(true)
    try {
      await onSubmit(input)
    } catch {
      // shown through error
    }
  }

  function renderField(f: GuideField) {
    const id = `field-${f.id}`
    const err = touched ? errors[f.id] : undefined
    const describedBy = [f.help && `${id}-help`, err && `${id}-error`].filter(Boolean).join(' ') || undefined
    return (
      <div key={f.id} className="grid gap-1.5">
        <Label htmlFor={id}>{f.label}</Label>
        <Input
          id={id}
          type="password"
          autoComplete="off"
          autoCorrect="off"
          autoCapitalize="off"
          spellCheck={false}
          data-1p-ignore
          data-lpignore="true"
          data-secret="true"
          className="font-mono"
          value={tokens[f.id] ?? ''}
          onChange={(e) => {
            setTokens((s) => ({ ...s, [f.id]: e.target.value }))
            setSecretCleared(false)
          }}
          aria-invalid={!!err}
          aria-describedby={describedBy}
        />
        {f.help && (
          <p id={`${id}-help`} className="text-muted-foreground text-xs">
            {f.help}
          </p>
        )}
        {secretCleared && !tokens[f.id] && (
          <p className="text-muted-foreground text-xs" role="status">
            The token was sent and cleared from this page. Paste it again only to retry.
          </p>
        )}
        {err && (
          <p id={`${id}-error`} className="text-destructive text-xs">
            {err}
          </p>
        )}
      </div>
    )
  }

  const placed = new Set(guide.steps.flatMap((s) => s.fields))
  const unplaced = guide.fields.filter((f) => !placed.has(f.id))

  return (
    <form onSubmit={(e) => void submit(e)} aria-label="Connect Gitea" className="grid gap-4" noValidate>
      <div className="grid gap-1.5">
        <Label htmlFor="connection-name">Name</Label>
        <Input id="connection-name" value={displayName} onChange={(e) => setDisplayName(e.target.value)} aria-invalid={touched && !!nameErr} />
        <p className="text-muted-foreground text-xs">How the connection is shown when you grant it to an agent.</p>
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="field-baseUrl">Server URL</Label>
        <Input
          id="field-baseUrl"
          autoComplete="off"
          placeholder="https://git.example.com"
          value={baseUrl}
          onChange={(e) => setBaseUrl(e.target.value)}
          aria-invalid={touched && !!urlErr}
          aria-describedby="field-baseUrl-help"
        />
        <p id="field-baseUrl-help" className="text-muted-foreground text-xs">
          Where Gitea is served, with its path if it has one (https://example.com/gitea). Koinon reaches the REST API at /api/v1 under it, and agents&apos;
          sandboxes may reach the repositories they are bound to.
        </p>
        {touched && urlErr && <p className="text-destructive text-xs">{urlErr}</p>}
      </div>
      <SetupGuideSteps
        steps={guide.steps}
        results={results}
        renderFields={(s) => {
          const fs = s.fields.flatMap((id) => guide.fields.filter((f) => f.id === id))
          return fs.length ? <div className="grid gap-3 border-t pt-3">{fs.map(renderField)}</div> : null
        }}
      />
      <details className="grid gap-3 rounded-md border p-3" open={!!caBundle}>
        <summary className="cursor-pointer text-sm font-medium">Private certificate authority (optional)</summary>
        <div className="mt-3 grid gap-1.5">
          <Label htmlFor="field-caBundle">CA bundle</Label>
          <textarea
            id="field-caBundle"
            rows={5}
            spellCheck={false}
            autoComplete="off"
            className="border-input bg-background w-full rounded-md border px-3 py-2 font-mono text-xs"
            placeholder="-----BEGIN CERTIFICATE-----"
            value={caBundle}
            onChange={(e) => setCaBundle(e.target.value)}
            aria-invalid={touched && !!caErr}
            aria-describedby="field-caBundle-help"
          />
          <p id="field-caBundle-help" className="text-muted-foreground text-xs">
            PEM certificates that sign the server&apos;s TLS certificate, when the system roots do not. Public: it is stored as plain configuration and
            copied into sandboxes for git.
          </p>
          {touched && caErr && <p className="text-destructive text-xs">{caErr}</p>}
        </div>
      </details>
      <div className="grid gap-1.5">
        <Label htmlFor="connection-ci-dirs">Extra CI directories (optional)</Label>
        <Textarea
          id="connection-ci-dirs"
          rows={3}
          autoComplete="off"
          spellCheck={false}
          value={ciDirsText}
          onChange={(e) => setCiDirsText(e.target.value)}
          aria-invalid={touched && !!ciErr}
          aria-describedby="connection-ci-dirs-help"
          placeholder="ci/pipelines"
        />
        <p id="connection-ci-dirs-help" className="text-muted-foreground text-xs">
          One directory per line, relative to the repository root: your Gitea server&apos;s WORKFLOW_DIRS, if you changed them. Koinon never proposes changes
          under these or under Gitea&apos;s own workflow directories (.gitea/workflows, .gitea/scoped_workflows, .github/workflows), because a pushed branch
          can run them and Gitea cannot refuse a token the right to change them.
        </p>
        {touched && ciErr && <p className="text-destructive text-xs">{ciErr}</p>}
      </div>
      {unplaced.length > 0 && <div className="grid gap-3">{unplaced.map(renderField)}</div>}
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <div className="flex justify-end">
        <Button type="submit" disabled={pending}>
          Create and test connection
        </Button>
      </div>
    </form>
  )
}
