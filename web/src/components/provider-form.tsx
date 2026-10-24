// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import type { Provider } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import type { ProviderChange, ProviderCreate } from '@/hooks/use-catalog-actions'
import { providerTypes } from '@/lib/providers'

const other = '__other'
const namePattern = /^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$/

export type ProviderFormProps = {
  /** Set to edit that provider; unset to add one. */
  provider?: Provider
  onCreate: (input: ProviderCreate) => Promise<unknown>
  onUpdate: (id: string, input: ProviderChange) => Promise<unknown>
  onDone: () => void
  /** Called after a save, to say when the provider will be available. */
  onSaved?: (name: string) => void
}

/**
 * ProviderForm adds or edits a provider entered in the UI. The API key is write-only: the field is
 * always empty, an edit keeps the stored key unless a new one is typed or removal is asked for, and the
 * typed key is cleared from the page as soon as it is sent.
 */
export function ProviderForm({ provider, onCreate, onUpdate, onDone, onSaved }: ProviderFormProps) {
  const editing = !!provider
  const known = providerTypes.some((t) => t.value === provider?.type)
  const [name, setName] = useState(provider?.name ?? '')
  const [typeChoice, setTypeChoice] = useState(provider ? (known ? provider.type : other) : 'openrouter')
  const [customType, setCustomType] = useState(provider && !known ? provider.type : '')
  const [url, setUrl] = useState(provider?.url ?? 'https://openrouter.ai/api/v1')
  const [urlEdited, setUrlEdited] = useState(editing)
  const [apiKey, setApiKey] = useState('')
  const [removeKey, setRemoveKey] = useState(false)
  const [touched, setTouched] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()

  const type = typeChoice === other ? customType.trim() : typeChoice
  const errors = {
    name: namePattern.test(name) ? undefined : 'Name must be lowercase letters, digits and dashes, such as openrouter.',
    type: /^[a-z][a-z0-9-]{0,62}$/.test(type) ? undefined : 'Type must be a provider name such as openai, openrouter or ollama.',
    url: /^https?:\/\/\S+$/.test(url.trim()) ? undefined : 'URL must start with http:// or https://.',
  }
  const invalid = Object.values(errors).some(Boolean)

  function pickType(v: string) {
    setTypeChoice(v)
    // Offer the usual URL for the choice, but never overwrite one the person typed.
    if (!urlEdited) setUrl(defaultUrl(v))
  }

  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (invalid) return
    setPending(true)
    setError(undefined)
    const key = apiKey
    setApiKey('') // the key leaves this page with the request
    try {
      if (provider) {
        const change: ProviderChange = {}
        if (type !== provider.type) change.type = type
        if (url.trim() !== provider.url) change.url = url.trim()
        if (key) change.apiKey = key
        else if (removeKey) change.apiKey = ''
        await onUpdate(provider.id, change)
      } else {
        await onCreate({ name, type, url: url.trim(), apiKey: key })
      }
      onSaved?.(provider?.name ?? name)
      onDone()
    } catch (err) {
      const msg = ConnectError.from(err).rawMessage
      // The key left the page with the request, so the person has to supply it again.
      setError(key ? `${msg} The API key was cleared from this page. Type the API key again.` : msg)
    } finally {
      setPending(false)
    }
  }

  const err = (k: keyof typeof errors) => (touched ? errors[k] : undefined)
  return (
    <form onSubmit={(e) => void submit(e)} aria-label={editing ? `Edit provider ${provider.name}` : 'Add provider'} className="bg-card grid gap-4 rounded-md border p-4" noValidate>
      <h2 className="text-base font-semibold">{editing ? `Edit ${provider.name}` : 'Add a provider'}</h2>
      <div className="grid gap-1.5">
        <Label htmlFor="provider-name">Name</Label>
        <Input
          id="provider-name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          disabled={editing}
          autoComplete="off"
          aria-invalid={!!err('name')}
          aria-describedby={err('name') ? 'provider-name-help provider-name-error' : 'provider-name-help'}
        />
        <p id="provider-name-help" className="text-muted-foreground text-xs">
          {editing ? 'A provider keeps its name.' : 'Models are served under this org and their own name. Lowercase letters, digits and dashes.'}
        </p>
        {err('name') && (
          <p id="provider-name-error" className="text-destructive text-xs">
            {err('name')}
          </p>
        )}
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="provider-type">Provider type</Label>
        <NativeSelect id="provider-type" value={typeChoice} onChange={(e) => pickType(e.target.value)}>
          {providerTypes.map((t) => (
            <option key={t.value} value={t.value}>
              {t.label}
            </option>
          ))}
          <option value={other}>Another provider type</option>
        </NativeSelect>
        {typeChoice === other && (
          <div className="grid gap-1.5">
            <Label htmlFor="provider-type-name">Provider type name</Label>
            <Input id="provider-type-name" value={customType} onChange={(e) => setCustomType(e.target.value)} placeholder="for example anthropic" autoComplete="off" aria-invalid={!!err('type')} aria-describedby={err('type') ? 'provider-type-error' : undefined} />
          </div>
        )}
        {err('type') && (
          <p id="provider-type-error" className="text-destructive text-xs">
            {err('type')}
          </p>
        )}
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="provider-url">URL</Label>
        <Input id="provider-url" type="url" inputMode="url" value={url} onChange={(e) => {
            setUrl(e.target.value)
            setUrlEdited(true)
          }} autoComplete="off" aria-invalid={!!err('url')} aria-describedby={err('url') ? 'provider-url-help provider-url-error' : 'provider-url-help'} />
        <p id="provider-url-help" className="text-muted-foreground text-xs">
          The base URL of the OpenAI-compatible API. Koinon refuses private, loopback and cluster-internal addresses unless a platform admin allows them.
        </p>
        {err('url') && (
          <p id="provider-url-error" className="text-destructive text-xs">
            {err('url')}
          </p>
        )}
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="provider-key">{editing ? 'New API key' : 'API key'}</Label>
        <Input id="provider-key" type="password" value={apiKey} onChange={(e) => setApiKey(e.target.value)} autoComplete="new-password" spellCheck={false} aria-describedby="provider-key-help" disabled={removeKey} />
        <p id="provider-key-help" className="text-muted-foreground text-xs">
          {editing
            ? provider.hasKey
              ? 'A key is stored and cannot be shown. Leave this empty to keep it, or type a new key to replace it.'
              : 'No key is stored. Type one to add it.'
            : 'Optional for keyless upstreams such as Ollama. Koinon stores it encrypted and never shows it again.'}
        </p>
        {editing && provider.hasKey && (
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={removeKey} onChange={(e) => setRemoveKey(e.target.checked)} />
            Remove the stored key
          </label>
        )}
      </div>
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <div className="flex justify-end gap-2">
        <Button type="button" variant="outline" onClick={onDone} disabled={pending}>
          Cancel
        </Button>
        <Button type="submit" disabled={pending}>
          {editing ? 'Save provider' : 'Add provider'}
        </Button>
      </div>
    </form>
  )
}

function defaultUrl(type: string): string {
  switch (type) {
    case 'openrouter':
      return 'https://openrouter.ai/api/v1'
    case 'ollama':
      return 'http://ollama.example.internal:11434/v1'
    case 'openai':
      return 'https://api.openai.com/v1'
    default:
      return ''
  }
}
