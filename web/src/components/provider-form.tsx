// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { ProviderService, type Provider } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import type { ProviderChange, ProviderCreate } from '@/hooks/use-catalog-actions'
import { useOrg } from '@/lib/org-context'
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
  const [workspaceId, setWorkspaceId] = useState(provider?.anthropicWorkspaceId ?? '')
  const [costKey, setCostKey] = useState('')
  const [costScope, setCostScope] = useState(provider?.costScope ?? '')
  const [removeKey, setRemoveKey] = useState(false)
  const [removeCostKey, setRemoveCostKey] = useState(false)
  const [touched, setTouched] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()

  const type = typeChoice === other ? customType.trim() : typeChoice
  const errors = {
    name: namePattern.test(name) ? undefined : 'Name must be lowercase letters, digits and dashes, such as openrouter.',
    type: /^[a-z][a-z0-9-]{0,62}$/.test(type) ? undefined : 'Type must be a provider name such as openai, openrouter or ollama.',
    url: /^https?:\/\/\S+$/.test(url.trim()) ? undefined : 'URL must start with http:// or https://.',
  }
  // Whether the type takes a workspace ID is the server's rule; its refusal of a malformed ID is shown as is.
  const { org } = useOrg()
  const typeInfo = useQuery(ProviderService.method.providerTypeInfo, { orgId: org.orgId, type }, { enabled: !errors.type })
  // Until the server has said what the selected type takes, the form knows nothing about the workspace ID
  // and must neither send nor clear it.
  const typeKnown = !!typeInfo.data
  const takesWorkspaceId = !!typeInfo.data?.takesWorkspaceId
  // Likewise for the cost API key, which only types whose cost API Koinon reads take.
  const takesCostKey = Boolean(typeInfo.data?.takesCostKey)
  // A cost scope names which workspace or project the cost key reads, so it has no meaning alone.
  // On an edit a stored key counts, unless its removal is asked for. A lost key does not: the server
  // cannot read it, so it refuses a changed scope until the key is typed again.
  const costKeyHeld = Boolean(costKey) || (editing && !removeCostKey && provider.hasCostKey)
  // An unchanged scope of a lost key is not a new request, so it does not block other edits.
  const costScopeChanged = !editing || costScope.trim() !== provider.costScope
  const costScopeError = takesCostKey && !removeCostKey && costScope.trim() && costScopeChanged && !costKeyHeld ? 'A cost scope needs a cost API key. Type the key, or clear the scope.' : undefined
  const invalid = Object.values(errors).some(Boolean) || Boolean(costScopeError)

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
    const costKeyValue = costKey
    setApiKey('') // the keys leave this page with the request
    setCostKey('')
    try {
      if (provider) {
        const change: ProviderChange = {}
        if (type !== provider.type) change.type = type
        if (url.trim() !== provider.url) change.url = url.trim()
        if (key) change.apiKey = key
        else if (removeKey) change.apiKey = ''
        // The ID exists for Anthropic providers only: leaving the type clears it.
        if (typeKnown) {
          const ws = takesWorkspaceId ? workspaceId.trim() : ''
          if (ws !== provider.anthropicWorkspaceId) change.anthropicWorkspaceId = ws
        }
        // The cost key is write-only like the API key; its scope goes with it, and changes alone.
        if (typeKnown) {
          const scope = costScope.trim()
          if (!takesCostKey || removeCostKey) {
            if (provider.hasCostKey || provider.costKeyLostAt) change.costApiKey = ''
          } else {
            if (costKeyValue) change.costApiKey = costKeyValue
            if (scope !== provider.costScope && (costKeyValue || provider.hasCostKey)) change.costScope = scope
            // Clearing the scope of a lost key drops the lost marker with it.
            else if (!scope && provider.costKeyLostAt && !costKeyValue) change.costApiKey = ''
          }
        }
        await onUpdate(provider.id, change)
      } else {
        await onCreate({ name, type, url: url.trim(), apiKey: key, anthropicWorkspaceId: takesWorkspaceId ? workspaceId.trim() : undefined,
          ...(takesCostKey && costKeyValue ? { costApiKey: costKeyValue, costScope: costScope.trim() } : {}),
        })
      }
      onSaved?.(provider?.name ?? name)
      onDone()
    } catch (err) {
      const msg = ConnectError.from(err).rawMessage
      // The key left the page with the request, so the person has to supply it again.
      const cleared = [key && 'API key', costKeyValue && 'cost API key'].filter(Boolean)
      const names = cleared.join(' and the ')
      setError(cleared.length ? `${msg} The ${names} ${cleared.length > 1 ? 'were' : 'was'} cleared from this page. Type the ${names} again.` : msg)
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
            <Input id="provider-type-name" value={customType} onChange={(e) => setCustomType(e.target.value)} placeholder="for example groq" autoComplete="off" aria-invalid={!!err('type')} aria-describedby={err('type') ? 'provider-type-error' : undefined} />
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
          The base URL of the provider&apos;s API, such as https://api.anthropic.com/v1 or an OpenAI-compatible API. Koinon refuses private, loopback and cluster-internal addresses unless a platform admin allows them.
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
            ? provider.credentialLostAt
              ? 'The stored key was lost with a key-encryption key. Type it again to restore the provider.'
              : provider.hasKey
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
      {takesWorkspaceId && (
        <div className="grid gap-1.5">
          <Label htmlFor="provider-workspace">Anthropic workspace ID</Label>
          <Input id="provider-workspace" value={workspaceId} onChange={(e) => setWorkspaceId(e.target.value)} placeholder="wrkspc_…" autoComplete="off" spellCheck={false} aria-describedby="provider-workspace-help provider-workspace-hint" />
          <p id="provider-workspace-help" className="text-muted-foreground text-xs">
            Optional. Needed when the API key is not scoped to a workspace; Koinon sends it with every request to this provider. It identifies the workspace and is not a secret.
          </p>
          <p id="provider-workspace-hint" className="text-muted-foreground text-xs">
            It looks like wrkspc_ followed by letters and digits.
          </p>
        </div>
      )}
      {takesCostKey && (
        <fieldset className="grid gap-4 rounded-md border p-3">
          <legend className="px-1 text-sm font-medium">Cost reconciliation (optional)</legend>
          <div className="grid gap-1.5">
            <Label htmlFor="provider-cost-key">{editing ? 'New cost API key' : 'Cost API key'}</Label>
            <Input id="provider-cost-key" type="password" value={costKey} onChange={(e) => setCostKey(e.target.value)} autoComplete="new-password" spellCheck={false} aria-describedby="provider-cost-key-help" disabled={removeCostKey} />
            <p id="provider-cost-key-help" className="text-muted-foreground text-xs">
              {editing && provider.costKeyLostAt
                ? 'The stored cost key was lost with a key-encryption key. Type it again to restore the daily comparison.'
                : editing && provider.hasCostKey
                ? 'A cost key is stored and cannot be shown. Leave this empty to keep it, or type a new key to replace it.'
                : `An ${type === 'anthropic' ? 'Anthropic' : 'OpenAI'} admin key that may read the cost API. Koinon reads it once a day to compare the provider's bill with its own ledger. It is stored encrypted, never shown again and never sent to the gateway.`}
            </p>
            {editing && (provider.hasCostKey || provider.costKeyLostAt) && (
              <label className="flex items-center gap-2 text-sm">
                <input type="checkbox" checked={removeCostKey} onChange={(e) => setRemoveCostKey(e.target.checked)} />
                Remove the stored cost key
              </label>
            )}
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="provider-cost-scope">Cost scope</Label>
            <Input id="provider-cost-scope" value={costScope} onChange={(e) => setCostScope(e.target.value)} placeholder={type === 'anthropic' ? 'wrkspc_… or apikey_…' : 'proj_… or key_…'} autoComplete="off" spellCheck={false} disabled={removeCostKey} aria-invalid={Boolean(touched && costScopeError)} aria-describedby={touched && costScopeError ? 'provider-cost-scope-help provider-cost-scope-error' : 'provider-cost-scope-help'} />
            <p id="provider-cost-scope-help" className="text-muted-foreground text-xs">
              {type === 'anthropic'
                ? 'The workspace whose cost is this provider key’s: its ID (wrkspc_…), or one API key’s ID (apikey_…). Needed with a cost key.'
                : 'The OpenAI project ID (proj_…) or API key ID (key_…, the key’s ID, never the key) whose cost is this provider key’s. Needed with a cost key.'}
            </p>
            {touched && costScopeError && (
              <p id="provider-cost-scope-error" className="text-destructive text-xs">
                {costScopeError}
              </p>
            )}
          </div>
        </fieldset>
      )}
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <div className="flex items-center justify-end gap-2">
        {!typeKnown && !errors.type && (
          <p id="provider-type-pending" role="status" className="text-muted-foreground mr-auto text-xs">
            Checking what this provider type takes…
          </p>
        )}
        <Button type="button" variant="outline" onClick={onDone} disabled={pending}>
          Cancel
        </Button>
        <Button type="submit" disabled={pending || !typeKnown} aria-describedby={typeKnown ? undefined : 'provider-type-pending'}>
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
    case 'anthropic':
      return 'https://api.anthropic.com/v1'
    default:
      return ''
  }
}
