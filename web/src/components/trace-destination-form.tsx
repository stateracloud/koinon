// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { Backend, type Destination } from '@/gen/koinon/trace/v1alpha1/trace_pb'
import {
  buildInput,
  formErrors,
  hasErrors,
  initialForm,
  isStored,
  maxDestinationNameLength,
  presetOf,
  presets,
  type DestinationFormState,
  type DestinationWrite,
  type HeaderRow,
} from '@/lib/trace'

export type TraceDestinationFormProps = {
  /** Set to edit that destination; unset to add one. */
  destination?: Destination
  /** The platform destination has no name. */
  platform?: boolean
  /** Tests the destination, then saves it; it rejects with the backend's reason when the test fails. */
  save: (input: DestinationWrite) => Promise<Destination>
  onDone: () => void
  onSaved?: (saved: Destination) => void
}

/**
 * TraceDestinationForm adds or edits a trace destination. Saving sends the backend one synthetic trace
 * first and saves only when it accepts it, so a refusal is shown with the backend's reason and nothing
 * is stored. Secrets are write-only: their fields are always empty, an edit keeps a stored secret
 * unless a new one is typed or its removal is asked for, and typed values are cleared from the page as
 * soon as they are sent.
 */
export function TraceDestinationForm({ destination, platform = false, save, onDone, onSaved }: TraceDestinationFormProps) {
  const editing = !!destination
  const [f, setF] = useState(() => initialForm(destination))
  const [touched, setTouched] = useState(false)
  const [pending, setPending] = useState(false)
  const [refusal, setRefusal] = useState<string>()
  const set = (p: Partial<DestinationFormState>) => setF((cur) => ({ ...cur, ...p }))
  const preset = presetOf(f.backend)
  const errors = formErrors(f, destination, platform)
  const invalid = hasErrors(errors)
  const shown = <T,>(v: T | undefined) => (touched ? v : undefined)
  // Changing the backend of a saved destination starts from no secrets, so the form asks for them again.
  const sameBackend = destination?.backend === f.backend

  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (invalid) return
    setPending(true)
    setRefusal(undefined)
    const typed = Object.values(f.secrets).some(Boolean) || f.headers.some((h) => h.value)
    const input = buildInput(f, destination)
    // The secrets leave this page with the request.
    set({ secrets: {}, headers: f.headers.map((h) => ({ ...h, value: '' })) })
    try {
      const saved = await save(input)
      onSaved?.(saved)
      onDone()
    } catch (e2) {
      const message = ConnectError.from(e2).rawMessage
      setRefusal(typed ? `${message} The secrets were cleared from this page. Type them again.` : message)
    } finally {
      setPending(false)
    }
  }

  const setHeader = (i: number, p: Partial<HeaderRow>) => set({ headers: f.headers.map((h, j) => (j === i ? { ...h, ...p } : h)) })
  const label = platform ? 'platform destination' : editing ? `destination ${destination.name}` : 'destination'

  return (
    <form onSubmit={(e) => void submit(e)} aria-label={editing ? `Edit ${label}` : `Add ${label}`} className="bg-card grid gap-4 rounded-md border p-4" noValidate>
      <h2 className="text-base font-semibold">{platform ? (editing ? 'Edit the platform destination' : 'Set the platform destination') : editing ? `Edit ${destination.name}` : 'Add a destination'}</h2>
      {!platform && (
        <div className="grid gap-1.5">
          <Label htmlFor="dest-name">Name</Label>
          <Input
            id="dest-name"
            value={f.name}
            onChange={(e) => set({ name: e.target.value })}
            disabled={editing}
            maxLength={maxDestinationNameLength}
            autoComplete="off"
            aria-invalid={!!shown(errors.name)}
            aria-describedby={['dest-name-help', shown(errors.name) && 'dest-name-error'].filter(Boolean).join(' ')}
          />
          <p id="dest-name-help" className="text-muted-foreground text-xs">
            {editing ? 'A destination keeps its name.' : 'Lowercase letters, digits and dashes. It only labels the destination in Koinon.'}
          </p>
          {shown(errors.name) && (
            <p id="dest-name-error" className="text-destructive text-xs">
              {errors.name}
            </p>
          )}
        </div>
      )}
      <div className="grid gap-1.5">
        <Label htmlFor="dest-backend">Backend</Label>
        <NativeSelect
          id="dest-backend"
          value={String(f.backend)}
          disabled={editing}
          onChange={(e) => {
            const backend = Number(e.target.value) as Backend
            set({ backend, secrets: {}, params: {}, headers: backend === Backend.GENERIC ? [{ name: '', value: '', stored: false, remove: false }] : [] })
          }}
        >
          {presets.map((p) => (
            <option key={p.backend} value={p.backend}>
              {p.label}
            </option>
          ))}
        </NativeSelect>
        {editing && <p className="text-muted-foreground text-xs">To use another backend, add a new destination.</p>}
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="dest-endpoint">Traces endpoint</Label>
        <Input
          id="dest-endpoint"
          type="url"
          inputMode="url"
          value={f.endpoint}
          onChange={(e) => set({ endpoint: e.target.value })}
          placeholder={preset.endpointExample}
          autoComplete="off"
          aria-invalid={!!shown(errors.endpoint)}
          aria-describedby={['dest-endpoint-help', shown(errors.endpoint) && 'dest-endpoint-error'].filter(Boolean).join(' ')}
        />
        <p id="dest-endpoint-help" className="text-muted-foreground text-xs">
          {preset.endpointHelp} Koinon refuses private, loopback and cluster-internal addresses unless a platform admin allows them, and does not follow redirects.
        </p>
        {shown(errors.endpoint) && (
          <p id="dest-endpoint-error" className="text-destructive text-xs">
            {errors.endpoint}
          </p>
        )}
      </div>
      {preset.fields.map((field) => {
        const id = `dest-${field.name}`
        const error = shown(errors.fields[field.name])
        const stored = field.secret && sameBackend && isStored(destination, field.name)
        return (
          <div key={field.name} className="grid gap-1.5">
            <Label htmlFor={id}>{field.label}</Label>
            {field.secret ? (
              <Input
                id={id}
                type="password"
                value={f.secrets[field.name] ?? ''}
                onChange={(e) => set({ secrets: { ...f.secrets, [field.name]: e.target.value } })}
                autoComplete="new-password"
                spellCheck={false}
                placeholder={stored ? 'Set' : undefined}
                aria-invalid={!!error}
                aria-describedby={[`${id}-help`, error && `${id}-error`].filter(Boolean).join(' ')}
              />
            ) : (
              <Input
                id={id}
                value={f.params[field.name] ?? ''}
                onChange={(e) => set({ params: { ...f.params, [field.name]: e.target.value } })}
                autoComplete="off"
                aria-invalid={!!error}
                aria-describedby={[`${id}-help`, error && `${id}-error`].filter(Boolean).join(' ')}
              />
            )}
            <p id={`${id}-help`} className="text-muted-foreground text-xs">
              {field.secret
                ? stored
                  ? 'Set. Leave empty to keep it, or type a new one to replace it. Koinon stores it encrypted and never shows it again.'
                  : 'Koinon stores it encrypted and never shows it again.'
                : (field.help ?? '')}
            </p>
            {error && (
              <p id={`${id}-error`} className="text-destructive text-xs">
                {error}
              </p>
            )}
          </div>
        )
      })}
      {f.backend === Backend.GENERIC && (
        <fieldset className="grid gap-3">
          <legend className="text-sm font-medium">Headers</legend>
          <p className="text-muted-foreground text-xs">Sent with every request. Values are stored encrypted and never shown again.</p>
          {f.headers.map((h, i) => {
            const error = shown(errors.headers[i])
            return (
              <div key={i} className="grid gap-1.5 sm:grid-cols-[1fr_1fr_auto] sm:items-end">
                <div className="grid gap-1.5">
                  <Label htmlFor={`dest-header-name-${i}`}>Header name {i + 1}</Label>
                  <Input
                    id={`dest-header-name-${i}`}
                    value={h.name}
                    onChange={(e) => setHeader(i, { name: e.target.value })}
                    disabled={h.stored}
                    placeholder="X-Api-Key"
                    autoComplete="off"
                    aria-invalid={!!error}
                  />
                </div>
                <div className="grid gap-1.5">
                  <Label htmlFor={`dest-header-value-${i}`}>Header value {i + 1}</Label>
                  <Input
                    id={`dest-header-value-${i}`}
                    type="password"
                    value={h.value}
                    onChange={(e) => setHeader(i, { value: e.target.value })}
                    disabled={h.remove}
                    placeholder={h.stored ? 'Set' : undefined}
                    autoComplete="new-password"
                    spellCheck={false}
                    aria-invalid={!!error}
                  />
                </div>
                {h.stored ? (
                  <label className="flex items-center gap-1.5 pb-2 text-sm">
                    <input type="checkbox" checked={h.remove} onChange={(e) => setHeader(i, { remove: e.target.checked, value: '' })} />
                    Remove header {h.name}
                  </label>
                ) : (
                  <Button type="button" variant="outline" size="sm" aria-label={`Drop header row ${i + 1}`} onClick={() => set({ headers: f.headers.filter((_, j) => j !== i) })}>
                    Drop
                  </Button>
                )}
                {error && (
                  <p role="alert" className="text-destructive text-xs sm:col-span-3">
                    {error}
                  </p>
                )}
              </div>
            )
          })}
          {shown(errors.headers[-1]) && (
            <p role="alert" className="text-destructive text-xs">
              {errors.headers[-1]}
            </p>
          )}
          <div>
            <Button type="button" variant="outline" size="sm" onClick={() => set({ headers: [...f.headers, { name: '', value: '', stored: false, remove: false }] })}>
              Add header
            </Button>
          </div>
        </fieldset>
      )}
      <label className="flex items-center gap-2 text-sm">
        <input type="checkbox" checked={f.enabled} onChange={(e) => set({ enabled: e.target.checked })} />
        Enabled
      </label>
      <p className="text-muted-foreground text-xs">
        Saving first sends the backend one test trace with no content. Koinon saves the destination only if the backend accepts it.
      </p>
      {pending && (
        <p role="status" className="text-sm">
          Testing the backend, then saving…
        </p>
      )}
      {refusal && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Not saved. {refusal}</AlertDescription>
        </Alert>
      )}
      <div className="flex justify-end gap-2">
        <Button type="button" variant="outline" onClick={onDone} disabled={pending}>
          Cancel
        </Button>
        <Button type="submit" disabled={pending}>
          {editing ? 'Test and save' : 'Test and add'}
        </Button>
      </div>
    </form>
  )
}
