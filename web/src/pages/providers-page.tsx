// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { Link } from '@tanstack/react-router'
import { useState, type Dispatch, type SetStateAction } from 'react'
import { ConfirmAction } from '@/components/confirm-action'
import { ModelForm } from '@/components/model-form'
import { OrgDefaultModel } from '@/components/org-default-model'
import { ProviderEchoCheck } from '@/components/provider-echo-check'
import { ProviderForm } from '@/components/provider-form'
import { RequireAdmin } from '@/components/require-admin'
import { OutwardSwitch } from '@/components/outward-switch'
import { OutwardTargetKind } from '@/gen/koinon/outward/v1alpha1/outward_pb'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ProviderService, type Model, type Provider } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { useProviderActions } from '@/hooks/use-catalog-actions'
import { closeOnly } from '@/lib/editing'
import { useOrg } from '@/lib/org-context'
import { servingPollInterval, savedNotice, servingBadge, modelsOf, providerDeletable, providerEditable, providerTypeLabel, scopedModelName, formatLabelOf } from '@/lib/providers'

/** ProvidersPage is where org admins and owners manage the org's model providers and models (ADR 0056). */
export function ProvidersPage() {
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <RequireAdmin what="model providers">
        <ProvidersContent />
      </RequireAdmin>
    </div>
  )
}

type Editing = { kind: 'new-provider' } | { kind: 'provider'; id: string } | { kind: 'new-model'; providerId: string } | { kind: 'model'; id: string }

function ProvidersContent() {
  const { org } = useOrg()
  const orgId = org.orgId
  // While a provider waits for the gateway or is failed (which can pass), re-read its serving status (issue #17).
  const providers = useQuery(ProviderService.method.listProviders, { orgId }, { refetchInterval: (q) => servingPollInterval(q.state.data?.providers ?? []) })
  const models = useQuery(ProviderService.method.listModels, { orgId })
  const actions = useProviderActions(orgId)
  const [editing, setEditing] = useState<Editing>()
  const [notice, setNotice] = useState<string>()
  const [refused, setRefused] = useState<string>()
  const [warnings, setWarnings] = useState<string[]>([])
  const saved = (warns: string[] = []) => {
    setRefused(undefined)
    setWarnings(warns)
    setNotice(savedNotice)
  }

  const failed = providers.error ?? models.error
  const list = providers.data?.providers ?? []
  const allModels = models.data?.models ?? []

  return (
    <>
      <div className="flex items-center gap-3">
        <h1 className="text-xl font-semibold">Providers</h1>
        <Button size="sm" className="ml-auto" onClick={() => setEditing({ kind: 'new-provider' })} disabled={editing?.kind === 'new-provider'}>
          Add provider
        </Button>
      </div>
      <p className="text-muted-foreground text-sm">
        A provider is the model API your agents call, such as OpenRouter or Ollama. Koinon keeps its key encrypted and hands it only to the gateway; sandboxes never see it. Providers can also be declared with a Kubernetes Secret, which are read-only here. A new or changed API key reaches the gateway through Kubernetes, which takes about three minutes; an agent run that starts sooner can fail.
      </p>
      <OrgDefaultModel orgId={orgId} />
      {editing?.kind === 'new-provider' && <ProviderForm onCreate={actions.createProvider} onUpdate={actions.updateProvider} onDone={closeOnly(setEditing, { kind: 'new-provider' })} onSaved={() => saved()} />}
      {refused && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refused}</AlertDescription>
        </Alert>
      )}
      {notice && (
        <p role="status" className="text-sm">
          {notice}
        </p>
      )}
      {warnings.length > 0 && (
        <Alert role="status" aria-label="Warnings">
          <AlertDescription>
            {warnings.map((w) => (
              <p key={w}>{w}</p>
            ))}
          </AlertDescription>
        </Alert>
      )}
      {failed && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(failed).rawMessage}</AlertDescription>
        </Alert>
      )}
      {providers.isPending || models.isPending ? (
        <p role="status">Loading providers…</p>
      ) : list.length === 0 ? (
        <p className="text-muted-foreground text-sm">No providers yet. Add OpenRouter or Ollama, then add a model so agents can run. Prices are set on the Prices page.</p>
      ) : (
        <ul className="grid gap-4" aria-label="Providers">
          {list.map((p) => (
            <li key={p.id}>
              <ProviderCard
                provider={p}
                models={modelsOf(allModels, p.id)}
                orgSlug={org.orgSlug}
                editing={editing}
                setEditing={setEditing}
                actions={actions}
                onSaved={saved}
                onNotice={(m) => {
                  setRefused(undefined)
                  setNotice(m)
                }}
                onRefused={(m) => {
                  setNotice(undefined)
                  setRefused(m)
                }}
              />
            </li>
          ))}
        </ul>
      )}
    </>
  )
}

type Actions = ReturnType<typeof useProviderActions>

function ProviderCard({
  provider: p,
  models,
  orgSlug,
  editing,
  setEditing,
  actions,
  onSaved,
  onNotice,
  onRefused,
}: {
  provider: Provider
  models: Model[]
  orgSlug: string
  editing: Editing | undefined
  setEditing: Dispatch<SetStateAction<Editing | undefined>>
  actions: Actions
  onSaved: (warnings?: string[]) => void
  onNotice: (m: string) => void
  onRefused: (m: string) => void
}) {
  const editable = providerEditable(p)
  const systemDisabled = p.disabledReason !== '' || p.removed
  // An admin's outward switch (ADR 0110) also takes the provider out of use: no Active badge and no echo check (the server answers 409).
  const disabled = systemDisabled || p.adminDisabled !== undefined
  return (
    <section aria-labelledby={`provider-${p.id}`} className="bg-card grid gap-3 rounded-md border p-4">
      <div className="flex flex-wrap items-center gap-2">
        <h2 id={`provider-${p.id}`} className="text-base font-semibold">
          {p.name}
        </h2>
        <Badge variant="outline">{providerTypeLabel(p.type)}</Badge>
        {p.removed ? (
          <Badge variant="destructive">Removed</Badge>
        ) : disabled ? (
          <Badge variant="destructive">Disabled</Badge>
        ) : p.credentialLostAt ? (
          <Badge variant="destructive">Key lost</Badge>
        ) : (
          <ServingBadge provider={p} />
        )}
        <span className="text-muted-foreground ml-auto text-sm">{p.source === 'secret' ? `Managed by Secret ${p.managedBySecret}` : 'Added in the UI'}</span>
      </div>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted-foreground">URL</dt>
        <dd className="break-all">{p.url}</dd>
        <dt className="text-muted-foreground">API key</dt>
        <dd>{p.credentialLostAt ? 'Lost; enter it again' : p.hasKey ? 'Stored encrypted; it cannot be shown' : 'None'}</dd>
        {p.anthropicWorkspaceId && (
          <>
            <dt className="text-muted-foreground">Workspace ID</dt>
            <dd className="break-all">{p.anthropicWorkspaceId}</dd>
          </>
        )}
        {(p.hasCostKey || p.costKeyLostAt) && (
          <>
            <dt className="text-muted-foreground">Cost API key</dt>
            <dd className="break-all">{p.costKeyLostAt ? 'Lost; enter it again' : `Stored encrypted; it cannot be shown. Scope ${p.costScope}`}</dd>
          </>
        )}
      </dl>
      {systemDisabled && (
        <Alert variant="destructive" role="note">
          <AlertDescription>
            {p.removed ? 'Removed' : 'Disabled'}: {p.disabledReason || 'its Kubernetes Secret was deleted'}.
          </AlertDescription>
        </Alert>
      )}
      {p.credentialLostAt && !p.removed && (
        <Alert variant="destructive" role="note">
          <AlertDescription>
            {editable
              ? 'The stored API key was lost with a key-encryption key, so agents cannot use this provider. Edit it and enter the key again.'
              : `The stored API key was lost with a key-encryption key, so agents cannot use this provider. Koinon reads it again from the Secret ${p.managedBySecret} on its next sync.`}
          </AlertDescription>
        </Alert>
      )}
      {!editable && !p.removed && (
        <p className="text-muted-foreground text-xs">
          This provider and its models are declared by the Secret {p.managedBySecret}. Change or delete that Secret to change them here.
        </p>
      )}
      <OutwardSwitch kind={OutwardTargetKind.MODEL_PROVIDER} targetId={p.id} subject={`provider ${p.name}`} disabled={p.adminDisabled} />
      {editing?.kind === 'provider' && editing.id === p.id ? (
        <ProviderForm provider={p} onCreate={actions.createProvider} onUpdate={actions.updateProvider} onDone={closeOnly(setEditing, { kind: 'provider', id: p.id })} onSaved={() => onSaved()} />
      ) : (
        (editable || providerDeletable(p)) && (
          <div className="flex flex-wrap gap-2">
            {editable && (
              <Button variant="outline" size="sm" aria-label={`Edit provider ${p.name}`} onClick={() => setEditing({ kind: 'provider', id: p.id })}>
                Edit
              </Button>
            )}
            {providerDeletable(p) && (
              <ConfirmAction
                label="Delete"
                subject={`provider ${p.name}`}
                consequence={`Delete ${p.name} and its ${models.length} ${models.length === 1 ? 'model' : 'models'}? Agents that use them stop working.`}
                confirmLabel="Delete provider"
                onConfirm={async () => {
                  await actions.deleteProvider(p.id)
                  onNotice(`Deleted provider ${p.name}.`)
                }}
                onError={onRefused}
              />
            )}
          </div>
        )
      )}

      <div className="grid gap-2 border-t pt-3">
        <div className="flex items-center gap-2">
          <h3 className="text-sm font-semibold">Models</h3>
          {editable && (
            <Button
              variant="outline"
              size="sm"
              className="ml-auto"
              aria-label={`Add model to ${p.name}`}
              onClick={() => setEditing({ kind: 'new-model', providerId: p.id })}
              disabled={editing?.kind === 'new-model' && editing.providerId === p.id}
            >
              Add model
            </Button>
          )}
        </div>
        {editing?.kind === 'new-model' && editing.providerId === p.id && (
          <ModelForm provider={p} orgSlug={orgSlug} onCreate={actions.createModel} onUpdate={actions.updateModel} onDone={closeOnly(setEditing, { kind: 'new-model', providerId: p.id })} onSaved={onSaved} />
        )}
        {models.length === 0 ? (
          <p className="text-muted-foreground text-sm">No models yet. Agents can only use models you add.</p>
        ) : (
          <ul className="grid gap-2" aria-label={`Models of ${p.name}`}>
            {models.map((m) => (
              <li key={m.id} className="grid gap-2 rounded-md border p-3">
                <ModelRow
                  provider={p}
                  model={m}
                  orgSlug={orgSlug}
                  editing={editing?.kind === 'model' && editing.id === m.id}
                  edit={() => setEditing({ kind: 'model', id: m.id })}
                  done={closeOnly(setEditing, { kind: 'model', id: m.id })}
                  actions={actions}
                  onSaved={onSaved}
                  onNotice={onNotice}
                  onRefused={onRefused}
                />
              </li>
            ))}
          </ul>
        )}
      </div>
      {!disabled && <ProviderEchoCheck provider={p} models={models} />}
    </section>
  )
}

function ModelRow({
  provider,
  model: m,
  orgSlug,
  editing,
  edit,
  done,
  actions,
  onSaved,
  onNotice,
  onRefused,
}: {
  provider: Provider
  model: Model
  orgSlug: string
  editing: boolean
  edit: () => void
  done: () => void
  actions: Actions
  onSaved: (warnings?: string[]) => void
  onNotice: (m: string) => void
  onRefused: (m: string) => void
}) {
  const editable = m.source !== 'secret'
  if (editing) return <ModelForm provider={provider} orgSlug={orgSlug} model={m} onCreate={actions.createModel} onUpdate={actions.updateModel} onDone={done} onSaved={onSaved} />
  return (
    <>
      <div className="flex flex-wrap items-center gap-2">
        <code className="text-sm font-medium">{scopedModelName(orgSlug, m.name)}</code>
        {m.format && (
          <Badge variant="outline" title={m.formatOverridden ? 'Set for this model' : 'The default for this model'}>
            {formatLabelOf(provider, m.format)}
            {m.formatOverridden ? ' (set)' : ''}
          </Badge>
        )}
        {m.guard && <Badge variant="secondary">Routing guard</Badge>}
        {m.disabledReason && <Badge variant="destructive">Disabled</Badge>}
      </div>
      <p className="text-muted-foreground text-sm">
        Priced on the <Link to="/settings/prices" className="underline">Prices page</Link>.
      </p>
      {m.disabledReason && <p className="text-destructive text-sm">Disabled: {m.disabledReason}.</p>}
      {editable && (
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" aria-label={`Edit model ${m.name}`} onClick={edit}>
            Edit
          </Button>
          <ConfirmAction
            label="Delete"
            subject={`model ${m.name}`}
            consequence={`Delete ${m.name}? Agents that use it stop working.`}
            confirmLabel="Delete model"
            onConfirm={async () => {
              await actions.deleteModel(m.id)
              onNotice(`Deleted model ${m.name}.`)
            }}
            onError={onRefused}
          />
        </div>
      )}
    </>
  )
}

/** ServingBadge shows whether the gateway serves the provider, and the reason when it does not. */
function ServingBadge({ provider }: { provider: Provider }) {
  const b = servingBadge(provider)
  if (!b) return <Badge variant="secondary">Active</Badge>
  return (
    <Badge variant={b.variant} title={provider.servingDetail || undefined} data-serving={b.state}>
      {b.label}
    </Badge>
  )
}
