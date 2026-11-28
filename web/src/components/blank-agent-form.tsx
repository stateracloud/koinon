// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { useQuery } from '@connectrpc/connect-query'
import { useId, useState, type FormEvent, type ReactNode } from 'react'
import { RepoPicker } from '@/components/repo-picker'
import { SaveAsTemplateFields } from '@/components/save-as-template'
import { TriggerRows } from '@/components/agent-triggers'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { AgentDefinitionSchema, type AgentDefinition } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import type { Connection, ConnectorType } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { QuestionService } from '@/gen/koinon/question/v1alpha1/question_pb'
import { GrantSchema, Preset, RunLimitsSchema, type Trigger } from '@/gen/koinon/template/v1alpha1/template_pb'
import { noTemplate, templateChoiceError, useTakenTemplateSlugs, type TemplateChoice } from '@/hooks/use-new-agent'
import { validateRepositories } from '@/lib/agents'
import { codeHostInfo, setupGuide } from '@/lib/connector'
import { builtInHarnesses, harnessLabel, presetLabel } from '@/lib/labels'
import { amountError } from '@/lib/money'
import { triggerProblem } from '@/lib/triggers'

export type BlankAgentFormProps = {
  orgId: string
  /** The connector types; the code hosts among them can get a grant. */
  connectorTypes: readonly ConnectorType[]
  connections: readonly Connection[]
  /** The models the org serves, when the viewer may list them (admins and owners); otherwise the org default only. */
  models?: readonly string[]
  /** Whether the viewer may search a connection's repositories (admins and owners). */
  canListRepos?: boolean
  onCreate: (d: AgentDefinition, template: TemplateChoice) => void
  pending?: boolean
  error?: string
}

type Access = { on: boolean; preset: Preset; scopes: string[]; connectionId: string; repositories: string[] }

const presets = [Preset.READ_ONLY, Preset.STANDARD, Preset.TRUSTED] as const
const wholeNumber = /^[0-9]{1,6}$/

/**
 * BlankAgentForm is "Blank agent" (ADR 0126 section 5): one form in sections for an expert, with no
 * template. It sends CreateAgent, which applies the checks a template has; the result is a draft.
 */
export function BlankAgentForm({ orgId, connectorTypes, connections, models, canListRepos, onCreate, pending, error }: BlankAgentFormProps) {
  const id = useId()
  const hosts = connectorTypes.filter((t) => codeHostInfo(t) && t.scopes.length > 0)
  const [name, setName] = useState('')
  const [instructions, setInstructions] = useState('')
  const [model, setModel] = useState('')
  const [harness, setHarness] = useState('')
  const [access, setAccess] = useState<Record<string, Access>>({})
  const [triggers, setTriggers] = useState<Trigger[]>([])
  const [limits, setLimits] = useState({ requests: '', spend: '', minutes: '' })
  const [saveAs, setSaveAs] = useState<TemplateChoice>(noTemplate)
  const [showErrors, setShowErrors] = useState(false)
  const taken = useTakenTemplateSlugs(orgId)
  const members = useQuery(QuestionService.method.listAskableMembers, { orgId }, { enabled: triggers.length > 0 })

  function accessOf(t: ConnectorType): Access {
    const own = connections.filter((c) => c.connector === t.id)
    return (
      access[t.id] ?? { on: false, preset: Preset.STANDARD, scopes: t.scopes.map((s) => s.id), connectionId: own.length === 1 ? own[0].id : '', repositories: [''] }
    )
  }
  const patch = (t: ConnectorType, p: Partial<Access>) => setAccess((s) => ({ ...s, [t.id]: { ...accessOf(t), ...p } }))

  function accessErrors(t: ConnectorType): string[] {
    const a = accessOf(t)
    if (!a.on) return []
    const errs: string[] = []
    if (!a.connectionId) errs.push('Choose a connection.')
    if (a.scopes.length === 0) errs.push('Choose at least one scope.')
    errs.push(...validateRepositories(a.repositories, { min: 1, max: 0 }, setupGuide(t).targetPattern))
    return errs
  }

  const problems = {
    name: name.trim() === '' ? 'Give the agent a name.' : name.trim().length > 200 ? 'The name is longer than 200 characters.' : undefined,
    instructions: instructions.trim() === '' ? 'Write the instructions.' : undefined,
    access: hosts.flatMap(accessErrors),
    triggers: triggers.map(triggerProblem).find(Boolean),
    requests: limits.requests.trim() !== '' && (!wholeNumber.test(limits.requests.trim()) || Number(limits.requests) < 1 || Number(limits.requests) > 100000) ? 'Model requests must be a whole number from 1 to 100,000.' : undefined,
    spend: limits.spend.trim() !== '' ? amountError('Spend', limits.spend.trim()) : undefined,
    minutes: limits.minutes.trim() !== '' && (!wholeNumber.test(limits.minutes.trim()) || Number(limits.minutes) < 1 || Number(limits.minutes) > 1440) ? 'Minutes must be a whole number from 1 to 1,440.' : undefined,
    template: templateChoiceError(saveAs, taken),
  }
  const invalid = !!(problems.name || problems.instructions || problems.access.length || problems.triggers || problems.requests || problems.spend || problems.minutes || problems.template)
  const shown = (p: string | undefined) => (showErrors ? p : undefined)

  function submit(e: FormEvent) {
    e.preventDefault()
    if (invalid) {
      setShowErrors(true)
      return
    }
    onCreate(
      create(AgentDefinitionSchema, {
        displayName: name.trim(),
        instructions: instructions.trim(),
        model,
        harness,
        grants: hosts
          .map((t) => ({ t, a: accessOf(t) }))
          .filter(({ a }) => a.on)
          .map(({ t, a }) =>
            create(GrantSchema, {
              connector: t.id,
              scopes: a.scopes,
              preset: a.preset,
              connectionId: a.connectionId,
              repositories: a.repositories.map((r) => r.trim()).filter(Boolean),
            }),
          ),
        triggers,
        runLimits: create(RunLimitsSchema, {
          maxModelRequests: BigInt(limits.requests.trim() || '0'),
          maxSpendUsd: limits.spend.trim(),
          maxDurationSeconds: BigInt(limits.minutes.trim() || '0') * 60n,
        }),
      }),
      saveAs,
    )
  }

  return (
    <form onSubmit={submit} aria-label="Blank agent" className="grid gap-4" noValidate>
      <Section title="Name and instructions">
        <Label htmlFor={`${id}-name`}>Name</Label>
        <Input id={`${id}-name`} value={name} aria-invalid={!!shown(problems.name)} onChange={(e) => setName(e.target.value)} />
        <FieldError text={shown(problems.name)} />
        <Label htmlFor={`${id}-instructions`}>Instructions</Label>
        <Textarea id={`${id}-instructions`} rows={8} value={instructions} aria-invalid={!!shown(problems.instructions)} onChange={(e) => setInstructions(e.target.value)} />
        <p className="text-muted-foreground text-xs">Markdown. The agent reads them at the start of every task.</p>
        <FieldError text={shown(problems.instructions)} />
      </Section>

      <Section title="Model and harness">
        <Label htmlFor={`${id}-model`}>Model</Label>
        <NativeSelect id={`${id}-model`} value={model} onChange={(e) => setModel(e.target.value)}>
          <option value="">The org’s default model</option>
          {models?.map((m) => (
            <option key={m} value={m}>
              {m}
            </option>
          ))}
        </NativeSelect>
        {!models && <p className="text-muted-foreground text-xs">An org admin or owner can choose another model on the draft page.</p>}
        <Label htmlFor={`${id}-harness`}>Harness</Label>
        <NativeSelect id={`${id}-harness`} value={harness} onChange={(e) => setHarness(e.target.value)}>
          <option value="">The platform’s default harness</option>
          {builtInHarnesses.map((h) => (
            <option key={h} value={h}>
              {harnessLabel(h)}
            </option>
          ))}
        </NativeSelect>
      </Section>

      <Section title="Access">
        {hosts.length === 0 && <p className="text-muted-foreground text-sm">No connector can be granted here. The agent gets no access to a code host.</p>}
        {hosts.map((t) => {
          const a = accessOf(t)
          const own = connections.filter((c) => c.connector === t.id)
          const errs = showErrors ? accessErrors(t) : []
          const host = codeHostInfo(t)!
          return (
            <fieldset key={t.id} className="grid gap-2 rounded-md border p-3" aria-label={`${host.name} access`}>
              <div className="flex items-center gap-2">
                <input id={`${id}-${t.id}-on`} type="checkbox" checked={a.on} onChange={(e) => patch(t, { on: e.target.checked })} />
                <Label htmlFor={`${id}-${t.id}-on`}>Give {host.name} access</Label>
              </div>
              {a.on && (
                <>
                  {own.length === 0 ? (
                    <p className="text-sm">No {host.name} connection yet. An org admin or owner connects it first.</p>
                  ) : (
                    <>
                      <Label htmlFor={`${id}-${t.id}-conn`}>Connection</Label>
                      <NativeSelect id={`${id}-${t.id}-conn`} value={a.connectionId} onChange={(e) => patch(t, { connectionId: e.target.value })}>
                        <option value="">Choose a connection</option>
                        {own.map((c) => (
                          <option key={c.id} value={c.id}>
                            {c.displayName}
                          </option>
                        ))}
                      </NativeSelect>
                    </>
                  )}
                  <Label htmlFor={`${id}-${t.id}-preset`}>Preset</Label>
                  <NativeSelect id={`${id}-${t.id}-preset`} value={String(a.preset)} onChange={(e) => patch(t, { preset: Number(e.target.value) as Preset })}>
                    {presets.map((p) => (
                      <option key={p} value={String(p)}>
                        {presetLabel(p)}
                      </option>
                    ))}
                  </NativeSelect>
                  <fieldset className="grid gap-1">
                    <legend className="text-sm font-medium">Scopes</legend>
                    {t.scopes.map((s) => (
                      <label key={s.id} className="flex items-start gap-2 text-sm">
                        <input
                          type="checkbox"
                          className="mt-1"
                          checked={a.scopes.includes(s.id)}
                          onChange={(e) => patch(t, { scopes: e.target.checked ? [...a.scopes, s.id] : a.scopes.filter((x) => x !== s.id) })}
                        />
                        <span>
                          <span className="font-mono text-xs">{s.id}</span> {s.description && <span className="text-muted-foreground">{s.description}</span>}
                        </span>
                      </label>
                    ))}
                  </fieldset>
                  <span className="text-sm font-medium">Repositories (required)</span>
                  {/* The server's rule (bindGrants): a grant on a code host names at least one
                      repository, and the connection's installation must reach each one. */}
                  <p id={`${id}-${t.id}-repos-rule`} className="text-muted-foreground text-xs">
                    A {host.name} grant needs at least one repository. The connection must have access to each repository. Koinon checks this when you create the draft.
                  </p>
                  {a.repositories.map((r, ri) => (
                    <div key={ri} className="flex gap-2">
                      <RepoPicker
                        aria-label={`${host.name} repository ${ri + 1}`}
                        aria-describedby={`${id}-${t.id}-repos-rule`}
                        aria-required={ri === 0}
                        aria-invalid={showErrors && ri === 0 && a.repositories.every((x) => x.trim() === '')}
                        connectionId={a.connectionId}
                        searchable={!!canListRepos}
                        value={r}
                        onChange={(v) => patch(t, { repositories: a.repositories.map((x, xi) => (xi === ri ? v : x)) })}
                      />
                      {a.repositories.length > 1 && (
                        <Button type="button" variant="ghost" size="sm" onClick={() => patch(t, { repositories: a.repositories.filter((_, xi) => xi !== ri) })}>
                          Remove
                        </Button>
                      )}
                    </div>
                  ))}
                  <div>
                    <Button type="button" variant="outline" size="sm" onClick={() => patch(t, { repositories: [...a.repositories, ''] })}>
                      Add repository
                    </Button>
                  </div>
                  {errs.length > 0 && (
                    <ul role="alert" className="text-destructive grid gap-0.5 text-sm">
                      {errs.map((e) => (
                        <li key={e}>{e}</li>
                      ))}
                    </ul>
                  )}
                </>
              )}
            </fieldset>
          )
        })}
      </Section>

      <Section title="Triggers">
        <p className="text-muted-foreground text-sm">What starts work on the agent, besides tasks that people create. Triggers fire nothing until the draft is activated.</p>
        <TriggerRows rows={triggers} onChange={setTriggers} members={members} />
      </Section>

      <Section title="Limits">
        <p className="text-muted-foreground text-sm">The limits of one run. Leave a field empty to use the platform default. The first limit a run reaches ends it.</p>
        <div className="flex flex-wrap items-start gap-3">
          <LimitField id={`${id}-requests`} label="Model requests" inputMode="numeric" value={limits.requests} error={shown(problems.requests)} onChange={(v) => setLimits((l) => ({ ...l, requests: v }))} />
          <LimitField id={`${id}-spend`} label="Spend (USD)" inputMode="decimal" value={limits.spend} error={shown(problems.spend)} onChange={(v) => setLimits((l) => ({ ...l, spend: v }))} />
          <LimitField id={`${id}-minutes`} label="Time (minutes)" inputMode="numeric" value={limits.minutes} error={shown(problems.minutes)} onChange={(v) => setLimits((l) => ({ ...l, minutes: v }))} />
        </div>
      </Section>

      <SaveAsTemplateFields value={saveAs} onChange={setSaveAs} showErrors={showErrors} defaultName={name} taken={taken} />

      <p className="text-muted-foreground text-sm">Creating it makes a draft. Nothing is granted, and no trigger fires, until an org admin or owner reviews and activates it.</p>
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      {showErrors && invalid && (
        <p className="text-destructive text-sm" role="alert">
          Correct the problems above, then create the draft.
        </p>
      )}
      <div className="flex">
        <Button type="submit" className="ml-auto" disabled={pending}>
          Create draft
        </Button>
      </div>
    </form>
  )
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  const id = useId()
  return (
    <section aria-labelledby={id} className="bg-card grid gap-2 rounded-lg border p-4">
      <h2 id={id} className="font-semibold">
        {title}
      </h2>
      {children}
    </section>
  )
}

function FieldError({ text }: { text?: string }) {
  return text ? <p className="text-destructive text-xs">{text}</p> : null
}

function LimitField({ id, label, value, error, inputMode, onChange }: { id: string; label: string; value: string; error?: string; inputMode: 'numeric' | 'decimal'; onChange: (v: string) => void }) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <Input id={id} inputMode={inputMode} value={value} aria-invalid={!!error} onChange={(e) => onChange(e.target.value)} />
      <FieldError text={error} />
    </div>
  )
}
