// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import type { PriceVersion } from '@/gen/koinon/budget/v1alpha1/price_pb'
import type { usePriceActions } from '@/hooks/use-spend-actions'
import { emptyPriceForm, emptyRates, formFromVersion, providerChoices, toSetPrice, tokenizers, validatePrice, type PriceFormValues, type RateFields } from '@/lib/prices'

type Actions = ReturnType<typeof usePriceActions>

const rateFields: { key: keyof RateFields; label: string }[] = [
  { key: 'input', label: 'Input' },
  { key: 'output', label: 'Output' },
  { key: 'cacheRead', label: 'Cache read (optional)' },
  { key: 'cacheWrite', label: 'Cache write, 5 minutes (optional)' },
  { key: 'cacheWrite1h', label: 'Cache write, 1 hour (optional)' },
  { key: 'reasoning', label: 'Reasoning (optional)' },
]

/**
 * PriceForm adds an org price or a new version of one. It checks the fields the way the server does. A new version
 * starts from the date chosen (now if empty); earlier requests keep the price they were charged at.
 */
export function PriceForm({
  orgId,
  existing,
  actions,
  onDone,
  onSaved,
}: {
  orgId: string
  /** The current version, when adding a new version of a price. */
  existing?: PriceVersion
  actions: Actions
  onDone: () => void
  onSaved: (name: string, warnings: string[]) => void
}) {
  const editing = !!existing
  const [f, setF] = useState<PriceFormValues>(() => (existing ? formFromVersion(existing) : emptyPriceForm()))
  const [touched, setTouched] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()
  const errors = validatePrice(f, editing)
  const show = (k: string) => (touched ? errors[k] : undefined)
  const set = (patch: Partial<PriceFormValues>) => setF((p) => ({ ...p, ...patch }))
  const setRate = (k: keyof RateFields, v: string) => set({ rates: { ...f.rates, [k]: v } })

  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (Object.keys(errors).length > 0) return
    setPending(true)
    setError(undefined)
    try {
      const res = await actions.setPrice(toSetPrice(orgId, f))
      onSaved(f.name, res.warnings)
      onDone()
    } catch (err) {
      setError(ConnectError.from(err).rawMessage)
    } finally {
      setPending(false)
    }
  }

  const title = editing ? `New version of ${f.name}` : 'Add a price'
  return (
    <form onSubmit={(e) => void submit(e)} aria-label={title} className="bg-background grid gap-4 rounded-md border p-4" noValidate>
      <h3 className="text-base font-semibold">{title}</h3>
      {editing && <p className="text-muted-foreground text-xs">Saving adds a version that applies from the start date below. Earlier requests keep the price they were charged at.</p>}
      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="price-name" label="Name" error={show('name')} help="Names the price. Adding the same name again adds a version.">
          <Input id="price-name" value={f.name} onChange={(e) => set({ name: e.target.value })} disabled={editing} autoComplete="off" aria-invalid={!!show('name')} aria-describedby={describe('price-name', show('name'))} />
        </Field>
        <Field id="price-provider" label="Provider type" error={show('provider')} help="The provider type it applies to, such as openrouter, or * for any.">
          <Input id="price-provider" list="price-providers" value={f.provider} onChange={(e) => set({ provider: e.target.value })} autoComplete="off" aria-invalid={!!show('provider')} aria-describedby={describe('price-provider', show('provider'))} />
          <datalist id="price-providers">
            {providerChoices.map((p) => (
              <option key={p} value={p} />
            ))}
          </datalist>
        </Field>
      </div>
      <fieldset className="grid gap-3">
        <legend className="text-sm font-medium">Which models</legend>
        <div className="flex flex-wrap gap-4 text-sm">
          <label className="flex items-center gap-2">
            <input type="radio" name="price-match" checked={f.matchKind === 'glob'} onChange={() => set({ matchKind: 'glob' })} />
            Name pattern (* and ?)
          </label>
          <label className="flex items-center gap-2">
            <input type="radio" name="price-match" checked={f.matchKind === 'regex'} onChange={() => set({ matchKind: 'regex' })} />
            Regular expression
          </label>
        </div>
        <Field id="price-pattern" label="Model pattern" error={show('pattern')} help="Matches the whole model name, ignoring case, such as anthropic/claude-sonnet-*.">
          <Input id="price-pattern" value={f.pattern} onChange={(e) => set({ pattern: e.target.value })} autoComplete="off" aria-invalid={!!show('pattern')} aria-describedby={describe('price-pattern', show('pattern'))} />
        </Field>
      </fieldset>
      <fieldset className="grid gap-3">
        <legend className="text-sm font-medium">Prices, in US dollars per million tokens</legend>
        <p className="text-muted-foreground text-xs">Input and output are required; a free model needs an explicit 0. A bucket you leave empty is billed at the input rate (cache) or the output rate (reasoning).</p>
        <RateInputs prefix="price" labelPrefix="" rates={f.rates} onChange={setRate} error={show} />
      </fieldset>
      <fieldset className="grid gap-3">
        <legend className="text-sm font-medium">Tiers (optional)</legend>
        <p className="text-muted-foreground text-xs">A tier replaces the prices it names once a request’s input (including cache) is longer than its threshold.</p>
        {f.tiers.map((t, i) => (
          <div key={i} className="grid gap-3 rounded-md border p-3" role="group" aria-label={`Tier ${i + 1}`}>
            <div className="flex items-end gap-2">
              <Field id={`tier-${i}-over`} label={`Tier ${i + 1} applies above (input tokens)`} error={show(`tier${i}contextOver`)}>
                <Input id={`tier-${i}-over`} inputMode="numeric" value={t.contextOver} onChange={(e) => set({ tiers: f.tiers.map((x, j) => (j === i ? { ...x, contextOver: e.target.value } : x)) })} autoComplete="off" aria-invalid={!!show(`tier${i}contextOver`)} aria-describedby={describe(`tier-${i}-over`, show(`tier${i}contextOver`))} />
              </Field>
              <Button type="button" variant="outline" size="sm" aria-label={`Remove tier ${i + 1}`} onClick={() => set({ tiers: f.tiers.filter((_, j) => j !== i) })}>
                Remove
              </Button>
            </div>
            <RateInputs prefix={`tier-${i}`} labelPrefix={`Tier ${i + 1} `} errKey={`tier${i}`} rates={t.rates} onChange={(k, v) => set({ tiers: f.tiers.map((x, j) => (j === i ? { ...x, rates: { ...x.rates, [k]: v } } : x)) })} error={show} />
          </div>
        ))}
        <div>
          <Button type="button" variant="outline" size="sm" onClick={() => set({ tiers: [...f.tiers, { contextOver: '', rates: { ...emptyRates } }] })}>
            Add tier
          </Button>
        </div>
      </fieldset>
      <fieldset className="grid gap-4 sm:grid-cols-2">
        <legend className="mb-2 text-sm font-medium">Estimates when a provider reports no usage</legend>
        <Field id="price-max-output" label="Largest output (tokens)" error={show('maxOutput')} help="Required when any price is not 0. An aborted response without usage is charged up to it.">
          <Input id="price-max-output" inputMode="numeric" value={f.maxOutput} onChange={(e) => set({ maxOutput: e.target.value })} autoComplete="off" aria-invalid={!!show('maxOutput')} aria-describedby={describe('price-max-output', show('maxOutput'), true)} />
        </Field>
        <Field id="price-tokenizer" label="Tokenizer" error={show('tokenizer')}>
          <NativeSelect id="price-tokenizer" value={f.tokenizer} onChange={(e) => set({ tokenizer: e.target.value })}>
            {tokenizers.map((t) => (
              <option key={t.value} value={t.value}>
                {t.label}
              </option>
            ))}
          </NativeSelect>
        </Field>
        <Field id="price-ratio" label="Bytes per token" error={show('ratio')} help="Between 0.5 and 16. Claude: 3.0 (2.3 from Opus 4.7).">
          <Input id="price-ratio" inputMode="decimal" value={f.ratio} onChange={(e) => set({ ratio: e.target.value })} autoComplete="off" aria-invalid={!!show('ratio')} aria-describedby={describe('price-ratio', show('ratio'), true)} />
        </Field>
        <Field id="price-overhead" label="Tool overhead (tokens)" error={show('toolOverhead')} help="Fixed input a provider adds when a request defines tools.">
          <Input id="price-overhead" inputMode="numeric" value={f.toolOverhead} onChange={(e) => set({ toolOverhead: e.target.value })} autoComplete="off" aria-invalid={!!show('toolOverhead')} aria-describedby={describe('price-overhead', show('toolOverhead'), true)} />
        </Field>
        <label className="flex items-center gap-2 text-sm sm:col-span-2">
          <input type="checkbox" checked={f.hiddenThinking} onChange={(e) => set({ hiddenThinking: e.target.checked })} />
          The model bills thinking it may not show
        </label>
      </fieldset>
      <Field id="price-from" label="Applies from (optional)" error={show('validFrom')} help="A date in UTC. Empty means now.">
        <Input id="price-from" type="date" value={f.validFrom} onChange={(e) => set({ validFrom: e.target.value })} aria-invalid={!!show('validFrom')} aria-describedby={describe('price-from', show('validFrom'), true)} />
      </Field>
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
          {editing ? 'Save new version' : 'Add price'}
        </Button>
      </div>
    </form>
  )
}

function describe(id: string, error: string | undefined, help = false): string | undefined {
  const ids = [help ? `${id}-help` : undefined, error ? `${id}-error` : undefined].filter(Boolean)
  return ids.length ? ids.join(' ') : undefined
}

function Field({ id, label, error, help, children }: { id: string; label: string; error?: string; help?: string; children: React.ReactNode }) {
  return (
    <div className="grid min-w-0 gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children}
      {help && (
        <p id={`${id}-help`} className="text-muted-foreground text-xs">
          {help}
        </p>
      )}
      {error && (
        <p id={`${id}-error`} className="text-destructive text-xs">
          {error}
        </p>
      )}
    </div>
  )
}

function RateInputs({
  prefix,
  labelPrefix,
  errKey = '',
  rates,
  onChange,
  error,
}: {
  prefix: string
  labelPrefix: string
  errKey?: string
  rates: RateFields
  onChange: (k: keyof RateFields, v: string) => void
  error: (k: string) => string | undefined
}) {
  return (
    <div className="grid gap-3 sm:grid-cols-3">
      {rateFields.map(({ key, label }) => {
        const id = `${prefix}-${key}`
        const err = error(`${errKey}${key}`)
        return (
          <Field key={key} id={id} label={labelPrefix ? `${labelPrefix}${label.toLowerCase()}` : label} error={err}>
            <Input id={id} inputMode="decimal" value={rates[key]} onChange={(e) => onChange(key, e.target.value)} placeholder="0.30" autoComplete="off" aria-invalid={!!err} aria-describedby={describe(id, err)} />
          </Field>
        )
      })}
    </div>
  )
}
