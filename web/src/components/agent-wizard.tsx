// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import { useId, useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { RepoPicker } from '@/components/repo-picker'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import type { GrantBinding } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import type { Connection } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { Flavor, type Grant, type Template } from '@/gen/koinon/template/v1alpha1/template_pb'
import { grantNeedsTargets, validateRepositories } from '@/lib/agents'
import { flavorLabel, presetLabel } from '@/lib/labels'
import { cn } from '@/lib/utils'

export type NewAgent = { templateId: string; displayName: string; flavor: Flavor; grants: Pick<GrantBinding, 'connector' | 'connectionId' | 'repositories'>[] }

export type AgentWizardProps = {
  templates: readonly Template[]
  connections: readonly Connection[]
  /** Target patterns by connector type, from the connector type documents. */
  targetPatterns?: ReadonlyMap<string, string>
  /** Whether the viewer may add a connection (admins and owners). */
  canConnect?: boolean
  /**
   * Whether the viewer may list a connection's repositories (admins and
   * owners): the repository inputs then suggest the installation's.
   */
  canListRepos?: boolean
  onCreate: (a: NewAgent) => void
  pending?: boolean
  error?: string
}

type Binding = { connectionId: string; repositories: string[] }

const steps = ['Template', 'Flavor', 'Access', 'Review'] as const

function bounds(g: Grant) {
  return { min: g.targetsMin, max: g.targetsMax }
}

function boundsText(g: Grant): string {
  const { min, max } = bounds(g)
  if (min === max) return `exactly ${min} ${min === 1 ? 'repository' : 'repositories'}`
  if (!max) return `at least ${min}`
  return `${min} to ${max} repositories`
}

/**
 * AgentWizard creates a draft agent from a template (ADR 0020): template,
 * flavor, the GitHub grant (connection and repositories within the
 * template's bounds), then a summary of what the draft will propose.
 */
export function AgentWizard({ templates, connections, targetPatterns, canConnect, canListRepos, onCreate, pending, error }: AgentWizardProps) {
  const [step, setStep] = useState(0)
  const [templateId, setTemplateId] = useState(templates.length === 1 ? templates[0].id : '')
  const template = templates.find((t) => t.id === templateId)
  const [flavor, setFlavor] = useState<Flavor>(Flavor.UNSPECIFIED)
  const [name, setName] = useState('')
  const [bindings, setBindings] = useState<Record<number, Binding>>({})
  const [showErrors, setShowErrors] = useState(false)

  const chosenFlavor = template && template.allowedFlavors.includes(flavor) ? flavor : (template?.defaultFlavor ?? Flavor.UNSPECIFIED)
  const targetGrants = (template?.grants ?? []).map((g, i) => ({ g, i })).filter(({ g }) => grantNeedsTargets(g))

  function binding(i: number, g: Grant): Binding {
    const b = bindings[i]
    const own = connections.filter((c) => c.connector === g.connector)
    const fallback = own.length === 1 ? own[0].id : ''
    return {
      connectionId: b?.connectionId && own.some((c) => c.id === b.connectionId) ? b.connectionId : fallback,
      repositories: b?.repositories ?? Array.from({ length: Math.max(1, g.targetsMin) }, () => ''),
    }
  }
  function setBinding(i: number, g: Grant, patch: Partial<Binding>) {
    setBindings((s) => ({ ...s, [i]: { ...binding(i, g), ...patch } }))
  }

  function grantErrors(i: number, g: Grant): string[] {
    const b = binding(i, g)
    const errs: string[] = []
    if (!b.connectionId) errs.push('Choose a connection.')
    errs.push(...validateRepositories(b.repositories, bounds(g), targetPatterns?.get(g.connector)))
    return errs
  }

  const stepValid = [
    !!template,
    !!template && chosenFlavor !== Flavor.UNSPECIFIED,
    targetGrants.every(({ g, i }) => grantErrors(i, g).length === 0),
    !!name.trim(),
  ]

  function next() {
    if (!stepValid[step]) {
      setShowErrors(true)
      return
    }
    setShowErrors(false)
    if (step === 0 && template && !name) setName(template.displayName)
    setStep((s) => Math.min(s + 1, steps.length - 1))
  }

  function submit(e: FormEvent) {
    e.preventDefault()
    if (step < steps.length - 1) return next()
    if (!template || !stepValid.every(Boolean)) {
      setShowErrors(true)
      return
    }
    onCreate({
      templateId: template.id,
      displayName: name.trim(),
      flavor: chosenFlavor,
      grants: targetGrants.map(({ g, i }) => {
        const b = binding(i, g)
        return { connector: g.connector, connectionId: b.connectionId, repositories: b.repositories.map((r) => r.trim()).filter(Boolean) }
      }),
    })
  }

  return (
    <form onSubmit={submit} aria-label="New agent" className="grid gap-4" noValidate>
      <ol className="flex flex-wrap gap-2 text-sm" aria-label="Steps">
        {steps.map((s, i) => (
          <li key={s} aria-current={i === step ? 'step' : undefined} className={cn('rounded-full border px-3 py-1', i === step ? 'bg-primary text-primary-foreground' : 'text-muted-foreground')}>
            {i + 1}. {s}
          </li>
        ))}
      </ol>

      {step === 0 && (
        <fieldset className="grid gap-2">
          <legend className="mb-2 font-semibold">Choose a template</legend>
          {templates.length === 0 && <p className="text-muted-foreground text-sm">This install ships no templates.</p>}
          {templates.map((t) => (
            <label key={t.id} className={cn('bg-card flex cursor-pointer gap-3 rounded-lg border p-3', t.id === templateId && 'ring-ring ring-2')}>
              <input type="radio" name="template" value={t.id} checked={t.id === templateId} onChange={() => setTemplateId(t.id)} className="mt-1" />
              <span>
                <span className="font-medium">{t.displayName}</span> <span className="text-muted-foreground text-xs">v{t.version}</span>
                <span className="text-muted-foreground block text-sm">{t.description}</span>
              </span>
            </label>
          ))}
        </fieldset>
      )}

      {step === 1 && template && (
        <fieldset className="grid gap-2">
          <legend className="mb-2 font-semibold">Choose a flavor</legend>
          <p className="text-muted-foreground text-sm">The harness the agent runs on in its sandbox.</p>
          {template.allowedFlavors.map((f) => (
            <label key={f} className="flex items-center gap-2 text-sm">
              <input type="radio" name="flavor" checked={f === chosenFlavor} onChange={() => setFlavor(f)} />
              {flavorLabel(f)}
              {f === template.defaultFlavor && <span className="text-muted-foreground text-xs">(recommended)</span>}
            </label>
          ))}
        </fieldset>
      )}

      {step === 2 && template && (
        <div className="grid gap-4">
          {targetGrants.length === 0 && <p className="text-muted-foreground text-sm">This template needs no connections.</p>}
          {targetGrants.map(({ g, i }) => (
            <GrantStep
              key={i}
              grant={g}
              binding={binding(i, g)}
              connections={connections.filter((c) => c.connector === g.connector)}
              canConnect={canConnect}
              canListRepos={canListRepos}
              errors={showErrors ? grantErrors(i, g) : []}
              boundsText={boundsText(g)}
              onChange={(patch) => setBinding(i, g, patch)}
            />
          ))}
        </div>
      )}

      {step === 3 && template && (
        <div className="grid gap-4">
          <div className="grid gap-1.5">
            <Label htmlFor="agent-name">Name</Label>
            <Input id="agent-name" value={name} onChange={(e) => setName(e.target.value)} aria-invalid={showErrors && !name.trim()} />
          </div>
          <section aria-label="Draft summary" className="bg-card grid gap-2 rounded-lg border p-4 text-sm">
            <h2 className="font-semibold">The draft will propose</h2>
            <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
              <dt className="text-muted-foreground">Template</dt>
              <dd>
                {template.displayName} v{template.version}
              </dd>
              <dt className="text-muted-foreground">Flavor</dt>
              <dd>{flavorLabel(chosenFlavor)}</dd>
              <dt className="text-muted-foreground">Model</dt>
              <dd>{template.model || 'default'}</dd>
              {template.runLimits && (
                <>
                  <dt className="text-muted-foreground">Run limits</dt>
                  <dd>
                    {Math.round(Number(template.runLimits.maxDurationSeconds) / 60)} min, {Number(template.runLimits.maxTokens).toLocaleString()} tokens per run
                  </dd>
                </>
              )}
              {template.spendLimitUsd && (
                <>
                  <dt className="text-muted-foreground">Spend limit</dt>
                  <dd>${template.spendLimitUsd}</dd>
                </>
              )}
            </dl>
            {template.grants.map((g, i) => {
              const b = grantNeedsTargets(g) ? binding(i, g) : undefined
              const conn = connections.find((c) => c.id === b?.connectionId)
              return (
                <div key={i} className="border-t pt-2">
                  <p className="font-medium">
                    {g.connector} grant · {presetLabel(g.preset)}
                  </p>
                  <p className="text-muted-foreground">Scopes: {g.scopes.join(', ')}</p>
                  {conn && <p>Connection: {conn.displayName}</p>}
                  {b && <p>Repositories: {b.repositories.filter((r) => r.trim()).join(', ')}</p>}
                </div>
              )
            })}
            <p className="text-muted-foreground border-t pt-2">
              Creating it makes a draft. Nothing is granted until an org admin or owner reviews and activates it.
            </p>
          </section>
        </div>
      )}

      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      {showErrors && step < 2 && !stepValid[step] && (
        <p className="text-destructive text-sm" role="alert">
          {step === 0 ? 'Choose a template.' : 'Choose a flavor.'}
        </p>
      )}
      <div className="flex gap-2">
        {step > 0 && (
          <Button type="button" variant="outline" onClick={() => setStep((s) => s - 1)}>
            Back
          </Button>
        )}
        {step < steps.length - 1 ? (
          // Distinct keys: reusing one element would turn the clicked Next into a
          // submit button before the click finishes, and submit the form.
          <Button key="next" type="button" className="ml-auto" onClick={next}>
            Next
          </Button>
        ) : (
          <Button key="submit" type="submit" className="ml-auto" disabled={pending}>
            Create draft
          </Button>
        )}
      </div>
    </form>
  )
}

type GrantStepProps = {
  grant: Grant
  binding: Binding
  connections: readonly Connection[]
  canConnect?: boolean
  canListRepos?: boolean
  errors: readonly string[]
  boundsText: string
  onChange: (patch: Partial<Binding>) => void
}

function GrantStep({ grant: g, binding: b, connections, canConnect, canListRepos, errors, boundsText, onChange }: GrantStepProps) {
  const id = useId()
  const max = g.targetsMax
  const atMax = max > 0 && b.repositories.length >= max
  return (
    <fieldset className="bg-card grid gap-3 rounded-lg border p-4" aria-describedby={errors.length ? `${id}-errors` : undefined}>
      <legend className="px-1 font-semibold">{g.connector === 'github' ? 'GitHub access' : `${g.connector} access`}</legend>
      {g.purpose && <p className="text-muted-foreground text-sm">{g.purpose}</p>}
      <p className="text-sm">
        Scopes: <span className="font-mono text-xs">{g.scopes.join(', ')}</span> · {presetLabel(g.preset)}
      </p>
      {connections.length === 0 ? (
        <p className="text-sm">
          No {g.connector} connection yet.{' '}
          {canConnect ? (
            <Link to="/connections/new/github" className="underline">
              Connect GitHub first
            </Link>
          ) : (
            'Ask an org admin or owner to connect GitHub.'
          )}
        </p>
      ) : (
        <div className="grid gap-1.5">
          <Label htmlFor={`${id}-conn`}>Connection</Label>
          <Select value={b.connectionId} onValueChange={(v) => onChange({ connectionId: v })}>
            <SelectTrigger id={`${id}-conn`} className="w-full">
              <SelectValue placeholder="Choose a connection" />
            </SelectTrigger>
            <SelectContent>
              {connections.map((c) => (
                <SelectItem key={c.id} value={c.id}>
                  {c.displayName}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </div>
      )}
      <div className="grid gap-1.5">
        <span className="text-sm font-medium" id={`${id}-repos`}>
          Repositories ({boundsText})
        </span>
        <p className="text-muted-foreground text-xs">
          As owner/name. The connection’s GitHub App installation must be able to access each one
          {canListRepos && g.connector === 'github' ? '; type to search its repositories.' : '.'}
        </p>
        {b.repositories.map((r, ri) => (
          <div key={ri} className="flex gap-2">
            <RepoPicker
              aria-label={`Repository ${ri + 1}`}
              connectionId={b.connectionId}
              searchable={!!canListRepos && g.connector === 'github'}
              value={r}
              onChange={(v) => onChange({ repositories: b.repositories.map((x, xi) => (xi === ri ? v : x)) })}
            />
            {b.repositories.length > 1 && (
              <Button type="button" variant="ghost" size="sm" aria-label={`Remove repository ${ri + 1}`} onClick={() => onChange({ repositories: b.repositories.filter((_, xi) => xi !== ri) })}>
                Remove
              </Button>
            )}
          </div>
        ))}
        <div>
          <Button type="button" variant="outline" size="sm" disabled={atMax} onClick={() => onChange({ repositories: [...b.repositories, ''] })}>
            Add repository
          </Button>
        </div>
      </div>
      {errors.length > 0 && (
        <ul id={`${id}-errors`} role="alert" className="text-destructive grid gap-0.5 text-sm">
          {errors.map((e) => (
            <li key={e}>{e}</li>
          ))}
        </ul>
      )}
    </fieldset>
  )
}
