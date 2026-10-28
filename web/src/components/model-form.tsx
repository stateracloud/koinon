// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { Link } from '@tanstack/react-router'
import { useEffect, useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { ProviderService, type CreateModelResponse, type Model, type Provider } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import type { ModelChange, ModelCreate } from '@/hooks/use-catalog-actions'
import { useOrg } from '@/lib/org-context'
import { formatLabel, guardProviderType, scopedModelName } from '@/lib/providers'

const namePattern = /^[A-Za-z0-9_.:/@-]{1,256}$/

export type ModelFormProps = {
  provider: Provider
  orgSlug: string
  /** Set to edit that model; unset to add one. */
  model?: Model
  onCreate: (input: ModelCreate) => Promise<CreateModelResponse>
  onUpdate: (id: string, input: ModelChange) => Promise<unknown>
  onDone: () => void
  /** Called after a save, to say when the model will be available. */
  onSaved?: (warnings: string[]) => void
}

/**
 * ModelForm adds or edits a model of a provider. A model has no rates of its own: its price is an org price or a
 * default one (the Prices page). The routing guard exists for OpenRouter providers only.
 */
export function ModelForm({ provider, orgSlug, model, onCreate, onUpdate, onDone, onSaved }: ModelFormProps) {
  const editing = !!model
  const [name, setName] = useState(model?.name ?? '')
  const [guard, setGuard] = useState(model?.guard ?? false)
  const { org } = useOrg()
  // The default depends on the provider and the name, and only the server knows the table (ADR 0069),
  // so ask it, once the name has stopped changing.
  const [askedName, setAskedName] = useState(name)
  useEffect(() => {
    const t = setTimeout(() => setAskedName(name), 300)
    return () => clearTimeout(t)
  }, [name])
  const formats = useQuery(ProviderService.method.modelFormats, { orgId: org.orgId, providerId: provider.id, name: askedName }, { enabled: namePattern.test(askedName) })
  const offered = formats.data?.offered.length ? formats.data.offered : [...provider.formats]
  const defaultFormat = formats.data?.defaultFormat ?? ''
  // The choice: null follows the default; a value is what the person picked.
  const [picked, setPicked] = useState<string | null>(model?.formatOverridden ? model.format : null)
  const format = picked ?? defaultFormat
  const [touched, setTouched] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()

  const errors = {
    name: namePattern.test(name) ? undefined : 'Model name is required: letters, digits and _ . : / @ -',
  }
  const invalid = Object.values(errors).some(Boolean)
  const err = (k: keyof typeof errors) => (touched ? errors[k] : undefined)
  const canGuard = provider.type === guardProviderType

  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (invalid) return
    setPending(true)
    setError(undefined)
    try {
      let warnings: string[] = []
      // Picking the default means following it, so a later change of the default reaches this model.
      const override = picked !== null && picked !== defaultFormat ? picked : ''
      if (model) {
        const change: ModelChange = { guard: canGuard ? guard : undefined }
        if (override !== (model.formatOverridden ? model.format : '')) change.format = override
        await onUpdate(model.id, change)
      } else warnings = (await onCreate({ providerId: provider.id, name, guard: canGuard && guard, format: override })).warnings
      onSaved?.(warnings)
      onDone()
    } catch (e2) {
      setError(ConnectError.from(e2).rawMessage)
    } finally {
      setPending(false)
    }
  }

  return (
    <form onSubmit={(e) => void submit(e)} aria-label={editing ? `Edit model ${model.name}` : `Add model to ${provider.name}`} className="bg-background grid gap-4 rounded-md border p-4" noValidate>
      <h3 className="text-base font-semibold">{editing ? `Edit ${model.name}` : `Add a model to ${provider.name}`}</h3>
      <div className="grid gap-1.5">
        <Label htmlFor="model-name">Model name</Label>
        <Input id="model-name" value={name} onChange={(e) => setName(e.target.value)} disabled={editing} autoComplete="off" aria-invalid={!!err('name')} aria-describedby={err('name') ? 'model-name-help model-name-error' : 'model-name-help'} />
        <p id="model-name-help" className="text-muted-foreground text-xs">
          The provider&apos;s own name for the model. Agents ask for it by this name; Koinon serves it as{' '}
          <code>{scopedModelName(orgSlug, name || '<model>')}</code>.
        </p>
        {err('name') && (
          <p id="model-name-error" className="text-destructive text-xs">
            {err('name')}
          </p>
        )}
      </div>
      <p className="text-muted-foreground text-xs">
        The price of a model is set on the <Link to="/settings/prices" className="underline">Prices page</Link>. Koinon ships prices for common models; add one of your own for the rest.
      </p>
      <div className="grid gap-1.5">
        <Label htmlFor="model-format">API format</Label>
        <NativeSelect id="model-format" value={format} onChange={(e) => setPicked(e.target.value)} disabled={offered.length < 2 || !defaultFormat} aria-describedby="model-format-help">
          {!defaultFormat && <option value="">Loading…</option>}
          {offered.map((f) => (
            <option key={f} value={f}>
              {formatLabel(f)}
              {f === defaultFormat ? ' (default)' : ''}
            </option>
          ))}
        </NativeSelect>
        <p id="model-format-help" className="text-muted-foreground text-xs">
          {offered.length < 2
            ? `${provider.name} offers one format for its models.`
            : 'The API the gateway speaks to this model. Leave the default unless you know the model works better another way; a format you choose is kept when Koinon changes its defaults.'}
        </p>
      </div>
      {canGuard && (
        <div className="grid gap-1">
          <label className="flex items-center gap-2 text-sm font-medium">
            <input type="checkbox" checked={guard} onChange={(e) => setGuard(e.target.checked)} aria-describedby="model-guard-help" />
            Routing guard
          </label>
          <p id="model-guard-help" className="text-muted-foreground text-xs">
            Only route this model to OpenRouter hosts that keep no data (zero data retention) and refuse data collection.
          </p>
        </div>
      )}
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
          {editing ? 'Save model' : 'Add model'}
        </Button>
      </div>
    </form>
  )
}
