// SPDX-License-Identifier: Apache-2.0

import { useState, type FormEvent } from 'react'
import { SecretTextarea } from '@/components/secret-textarea'
import { SetupGuideSteps } from '@/components/setup-guide'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import type { CheckResult } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import type { GuideField, SetupGuide } from '@/lib/connector'
import { caBundleError } from '@/lib/ca-bundle'
import { ciDirsError, parseCiDirs } from '@/lib/ci-paths'

export type GitHubConnectionInput = {
  displayName: string
  appId: bigint
  installationId: bigint
  privateKey: string
  baseUrl: string
  caBundle: string
  ciDirs: string[]
}

export type GitHubConnectionFormProps = {
  guide: SetupGuide
  /** Resolves when the connection is created; rejects to keep the form open. */
  onSubmit: (input: GitHubConnectionInput) => Promise<unknown>
  pending?: boolean
  error?: string
  results?: readonly CheckResult[]
}

const integerPattern = /^[1-9][0-9]{0,18}$/

/** baseUrlError mirrors the API: an https URL that is only a host and an optional port. */
function baseUrlError(raw: string): string | undefined {
  const v = raw.trim()
  if (!v) return undefined
  let u: URL
  try {
    u = new URL(v)
  } catch {
    return 'Base URL must be an https URL such as https://github.example.com.'
  }
  if (u.protocol !== 'https:') return 'Base URL must start with https://.'
  if (u.username || u.password || u.search || u.hash || (u.pathname !== '/' && u.pathname !== '') || /[?#]/.test(v)) {
    return 'Base URL must be only a host and an optional port; the API is reached at /api/v3 on it.'
  }
  return undefined
}

function fieldError(f: GuideField, value: string): string | undefined {
  if (!value.trim()) return f.required ? `${f.label} is required.` : undefined
  if (f.type === 'integer' && !integerPattern.test(value.trim())) return `${f.label} must be a positive whole number.`
  if (f.pattern && !f.secret) {
    try {
      if (!new RegExp(f.pattern).test(value.trim())) return `${f.label} does not look right.`
    } catch {
      // a bad pattern in the guide does not block the form; the API validates
    }
  }
  if (f.type === 'pem' && !/-----BEGIN [A-Z ]*PRIVATE KEY-----/.test(value)) return `${f.label} must be the contents of the .pem file.`
  return undefined
}

/**
 * GitHubConnectionForm walks the GitHub setup guide with its fields in the
 * steps that ask for them. The private key is a secure input: it is cleared
 * as soon as the form is submitted, whatever the outcome, and never shown
 * again (the API does not return it).
 */
export function GitHubConnectionForm({ guide, onSubmit, pending, error, results }: GitHubConnectionFormProps) {
  const [displayName, setDisplayName] = useState('GitHub')
  const [values, setValues] = useState<Record<string, string>>({})
  const [ciDirsText, setCiDirsText] = useState('')
  const [secretCleared, setSecretCleared] = useState(false)
  const [touched, setTouched] = useState(false)
  const [baseUrl, setBaseUrl] = useState('')
  const [caBundle, setCaBundle] = useState('')
  const baseUrlErr = baseUrlError(baseUrl)
  const caBundleErr = caBundleError(caBundle)

  const errors = Object.fromEntries(guide.fields.map((f) => [f.id, fieldError(f, values[f.id] ?? '')]).filter(([, e]) => e)) as Record<string, string>
  const nameError = displayName.trim() ? undefined : 'Name is required.'
  const ciError = ciDirsError(ciDirsText)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (nameError || baseUrlErr || caBundleErr || ciError || Object.keys(errors).length) return
    const input: GitHubConnectionInput = {
      displayName: displayName.trim(),
      appId: BigInt((values.appId ?? '').trim()),
      installationId: BigInt((values.installationId ?? '').trim()),
      privateKey: values.privateKey ?? '',
      baseUrl: baseUrl.trim(),
      caBundle: caBundle.trim(),
      ciDirs: parseCiDirs(ciDirsText),
    }
    // Clear every secret before the request leaves: the key lives in this
    // component only for as long as it takes to send it.
    setValues((v) => Object.fromEntries(Object.entries(v).map(([k, val]) => [k, guide.fields.find((f) => f.id === k)?.secret ? '' : val])))
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
    const set = (v: string) => {
      setValues((s) => ({ ...s, [f.id]: v }))
      if (f.secret) setSecretCleared(false)
    }
    return (
      <div key={f.id} className="grid gap-1.5">
        <Label htmlFor={id}>{f.label}</Label>
        {f.secret ? (
          <SecretTextarea
            id={id}
            rows={6}
            value={values[f.id] ?? ''}
            onChange={(e) => set(e.target.value)}
            aria-invalid={!!err}
            aria-describedby={describedBy}
            placeholder="-----BEGIN RSA PRIVATE KEY-----"
          />
        ) : (
          <Input
            id={id}
            inputMode={f.type === 'integer' ? 'numeric' : undefined}
            autoComplete="off"
            value={values[f.id] ?? ''}
            onChange={(e) => set(e.target.value)}
            aria-invalid={!!err}
            aria-describedby={describedBy}
          />
        )}
        {f.help && (
          <p id={`${id}-help`} className="text-muted-foreground text-xs">
            {f.help}
          </p>
        )}
        {f.secret && secretCleared && !values[f.id] && (
          <p className="text-muted-foreground text-xs" role="status">
            The key was sent and cleared from this page. Paste it again only to retry.
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
    <form onSubmit={(e) => void submit(e)} aria-label="Connect GitHub" className="grid gap-4" noValidate>
      <div className="grid gap-1.5">
        <Label htmlFor="connection-name">Name</Label>
        <Input id="connection-name" value={displayName} onChange={(e) => setDisplayName(e.target.value)} aria-invalid={touched && !!nameError} />
        <p className="text-muted-foreground text-xs">How the connection is shown when you grant it to an agent.</p>
      </div>
      <SetupGuideSteps
        steps={guide.steps}
        results={results}
        renderFields={(s) => {
          const fs = s.fields.flatMap((id) => guide.fields.filter((f) => f.id === id))
          return fs.length ? <div className="grid gap-3 border-t pt-3">{fs.map(renderField)}</div> : null
        }}
      />
      <details className="grid gap-3 rounded-md border p-3" open={!!baseUrl || !!caBundle}>
        <summary className="cursor-pointer text-sm font-medium">GitHub Enterprise Server (optional)</summary>
        <div className="mt-3 grid gap-3">
          <div className="grid gap-1.5">
            <Label htmlFor="field-baseUrl">Base URL</Label>
            <Input
              id="field-baseUrl"
              autoComplete="off"
              placeholder="https://github.example.com"
              value={baseUrl}
              onChange={(e) => setBaseUrl(e.target.value)}
              aria-invalid={touched && !!baseUrlErr}
              aria-describedby="field-baseUrl-help"
            />
            <p id="field-baseUrl-help" className="text-muted-foreground text-xs">
              Leave empty for github.com. Koinon reaches the REST API at /api/v3 on this host, and agents' sandboxes may reach it.
            </p>
            {touched && baseUrlErr && <p className="text-destructive text-xs">{baseUrlErr}</p>}
          </div>
          <div className="grid gap-1.5">
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
              aria-invalid={touched && !!caBundleErr}
              aria-describedby="field-caBundle-help"
            />
            <p id="field-caBundle-help" className="text-muted-foreground text-xs">
              PEM certificates that sign the server&apos;s TLS certificate, when the system roots do not. Public: it is stored as plain configuration
              and copied into sandboxes for git.
            </p>
            {touched && caBundleErr && <p className="text-destructive text-xs">{caBundleErr}</p>}
          </div>
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
          aria-invalid={touched && !!ciError}
          aria-describedby="connection-ci-dirs-help"
          placeholder="ci/pipelines"
        />
        <p id="connection-ci-dirs-help" className="text-muted-foreground text-xs">
          One directory per line, relative to the repository root. Koinon never proposes changes under these or under GitHub&apos;s own workflow directory,
          because a pushed branch could run them with the repository&apos;s secrets.
        </p>
        {touched && ciError && <p className="text-destructive text-xs">{ciError}</p>}
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
