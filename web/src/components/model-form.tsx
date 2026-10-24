// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import type { Model, Provider } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import type { ModelChange, ModelCreate } from '@/hooks/use-catalog-actions'
import { guardProviderType, rateError, scopedModelName } from '@/lib/providers'

const namePattern = /^[A-Za-z0-9_.:/@-]{1,256}$/

export type ModelFormProps = {
  provider: Provider
  orgSlug: string
  /** Set to edit that model; unset to add one. */
  model?: Model
  onCreate: (input: ModelCreate) => Promise<unknown>
  onUpdate: (id: string, input: ModelChange) => Promise<unknown>
  onDone: () => void
  /** Called after a save, to say when the model will be available. */
  onSaved?: () => void
}

/**
 * ModelForm adds or edits a model of a provider. Rates are USD per million tokens until PV2 brings the
 * price schema; a model without rates is not served. The routing guard exists for OpenRouter providers only.
 */
export function ModelForm({ provider, orgSlug, model, onCreate, onUpdate, onDone, onSaved }: ModelFormProps) {
  const editing = !!model
  const [name, setName] = useState(model?.name ?? '')
  const [guard, setGuard] = useState(model?.guard ?? false)
  const [input, setInput] = useState(model?.rates?.input ?? '')
  const [output, setOutput] = useState(model?.rates?.output ?? '')
  const [cache, setCache] = useState(model?.rates?.cacheRead ?? '')
  const [touched, setTouched] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()

  const errors = {
    name: namePattern.test(name) ? undefined : 'Model name is required: letters, digits and _ . : / @ -',
    input: rateError('Input rate', input, true),
    output: rateError('Output rate', output, true),
    cache: rateError('Cache read rate', cache, false),
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
    const rates = { input: input.trim(), output: output.trim(), cacheRead: cache.trim() }
    try {
      if (model) await onUpdate(model.id, { guard: canGuard ? guard : undefined, rates })
      else await onCreate({ providerId: provider.id, name, guard: canGuard && guard, rates })
      onSaved?.()
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
      <fieldset className="grid gap-3">
        <legend className="text-sm font-medium">Rates, in US dollars per million tokens</legend>
        <p className="text-muted-foreground text-xs">Koinon needs rates to serve a model and to count its spend against budgets.</p>
        <div className="grid gap-3 sm:grid-cols-3">
          <RateField id="model-input" label="Input rate" value={input} onChange={setInput} error={err('input')} />
          <RateField id="model-output" label="Output rate" value={output} onChange={setOutput} error={err('output')} />
          <RateField id="model-cache" label="Cache read rate (optional)" value={cache} onChange={setCache} error={err('cache')} />
        </div>
      </fieldset>
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

function RateField({ id, label, value, onChange, error }: { id: string; label: string; value: string; onChange: (v: string) => void; error?: string }) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <Input id={id} inputMode="decimal" value={value} onChange={(e) => onChange(e.target.value)} placeholder="0.30" autoComplete="off" aria-invalid={!!error} aria-describedby={error ? `${id}-error` : undefined} />
      {error && (
        <p id={`${id}-error`} className="text-destructive text-xs">
          {error}
        </p>
      )}
    </div>
  )
}
