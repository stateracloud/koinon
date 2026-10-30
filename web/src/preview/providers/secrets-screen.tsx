// SPDX-License-Identifier: Apache-2.0

import { useState, type FormEvent } from 'react'
import { Badge, Button, Dialog, DialogClose, Field, Input, NativeSelect, PageHeader, PreviewShell, Segmented } from '@/preview/ui'
import { agentSecrets, orgSecrets, type SecretRow } from '@/preview/mock/data'

type Scope = 'org' | 'agent'
type Modal = { kind: 'add-secret' } | { kind: 'add-config' } | { kind: 'rotate'; row: SecretRow } | { kind: 'edit'; row: SecretRow } | null

const TODAY = '30 Oct'

export function SecretsScreen() {
  const [scope, setScope] = useState<Scope>('org')
  const [rows, setRows] = useState<Record<Scope, SecretRow[]>>({ org: orgSecrets, agent: agentSecrets })
  const [modal, setModal] = useState<Modal>(null)
  const [saved, setSaved] = useState('')

  const list = rows[scope]
  const sourceFor = scope === 'org' ? 'Org' : 'Agent override'

  function change(fn: (r: SecretRow[]) => SecretRow[], message: string) {
    setRows((all) => ({ ...all, [scope]: fn(all[scope]) }))
    setSaved(message)
    setModal(null)
  }

  return (
    <PreviewShell space="org" current="Secrets" crumbs={[{ label: 'Secrets' }]}>
      <PageHeader
        title="Secrets and config"
        description="Config is readable. Secrets are write-only: you can rotate them, never read them. Neither ever enters a sandbox."
        actions={
          <>
            <Button variant="outline" onClick={() => setModal({ kind: 'add-config' })}>Add config</Button>
            <Button onClick={() => setModal({ kind: 'add-secret' })}>Add secret</Button>
          </>
        }
      />
      <div className="ks-row">
        <span className="ks-label" id="sc-scope">Scope</span>
        <Segmented<Scope>
          label="Scope"
          value={scope}
          onChange={setScope}
          options={[
            { id: 'org', label: 'Whole org' },
            { id: 'agent', label: 'LCM Upgrader' },
          ]}
        />
        <span className="ks-meta">Precedence: agent over org over platform. The most specific value wins.</span>
      </div>
      <p className="ks-sr" role="status">{saved}</p>
      <div className="ks-panel-flush ks-table-wrap" role="region" aria-label="Secrets and config" tabIndex={0}>
        <table className="ks-table">
          <thead>
            <tr>
              <th scope="col">Name</th>
              <th scope="col">Type</th>
              <th scope="col">Value</th>
              <th scope="col">Source</th>
              <th scope="col">Lives in</th>
              <th scope="col">Opens</th>
              <th scope="col"><span className="ks-sr">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            {list.map((s) => (
              <tr key={s.name}>
                <td>
                  <span className="ks-stack" style={{ gap: 0 }}>
                    <span className="ks-mono">{s.name}</span>
                    <span className="ks-meta">{s.addedBy}</span>
                  </span>
                </td>
                <td><Badge variant={s.type === 'Secret' ? 'outline' : 'secondary'}>{s.type}</Badge></td>
                <td className="ks-mono">{s.value}</td>
                <td><Badge variant={s.source.startsWith('Inherited') ? 'secondary' : 'outline'}>{s.source}</Badge></td>
                <td>{s.lives}</td>
                <td className="ks-meta ks-mono">{s.opens}</td>
                <td className="ks-r">
                  <Button variant="outline" size="xs" aria-label={`${s.type === 'Secret' ? 'Rotate' : 'Edit'} ${s.name}`} onClick={() => setModal({ kind: s.type === 'Secret' ? 'rotate' : 'edit', row: s })}>
                    {s.type === 'Secret' ? 'Rotate' : 'Edit'}
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <p className="ks-p ks-meta">Saving a secret that opens a domain also allows the gateway to reach it. Running agents pick up changes on their next run.</p>

      {modal?.kind === 'rotate' && (
        <ValueDialog
          title={`Rotate ${modal.row.name}`}
          description="Paste the new value. The old one is replaced and can't be read back. Running agents pick it up on their next run."
          label="New value"
          secret
          submitLabel="Rotate secret"
          onClose={() => setModal(null)}
          onSubmit={() => change((r) => r.map((x) => (x.name === modal.row.name ? { ...x, value: `•••••••• set ${TODAY}`, source: x.source.startsWith('Inherited') ? sourceFor : x.source } : x)), `${modal.row.name} rotated`)}
        />
      )}
      {modal?.kind === 'edit' && (
        <ValueDialog
          title={`Edit ${modal.row.name}`}
          description={modal.row.source.startsWith('Inherited') ? `Saving here overrides the inherited value for ${scope === 'org' ? 'this org' : 'LCM Upgrader'}.` : undefined}
          label="Value"
          initial={modal.row.value}
          submitLabel="Save config"
          onClose={() => setModal(null)}
          onSubmit={(v) => change((r) => r.map((x) => (x.name === modal.row.name ? { ...x, value: v, source: x.source.startsWith('Inherited') ? sourceFor : x.source } : x)), `${modal.row.name} saved`)}
        />
      )}
      {(modal?.kind === 'add-secret' || modal?.kind === 'add-config') && (
        <AddDialog
          secret={modal.kind === 'add-secret'}
          scopeLabel={scope === 'org' ? 'this org' : 'LCM Upgrader'}
          onClose={() => setModal(null)}
          onAdd={(row) => change((r) => [{ ...row, source: sourceFor }, ...r], `${row.name} added`)}
        />
      )}
    </PreviewShell>
  )
}

function ValueDialog({ title, description, label, secret, initial = '', submitLabel, onClose, onSubmit }: { title: string; description?: string; label: string; secret?: boolean; initial?: string; submitLabel: string; onClose: () => void; onSubmit: (v: string) => void }) {
  const [v, setV] = useState(secret ? '' : initial)
  function submit(e: FormEvent) {
    e.preventDefault()
    if (v.trim()) onSubmit(v)
  }
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title={title}
      description={description}
      footer={
        <>
          <DialogClose><Button variant="outline">Cancel</Button></DialogClose>
          <Button type="submit" form="sc-value-form" disabled={!v.trim()}>{submitLabel}</Button>
        </>
      }
    >
      <form id="sc-value-form" onSubmit={submit}>
        <Field id="sc-value" label={label} hint={secret ? 'Write-only. After saving you can rotate it, never read it.' : undefined}>
          <Input id="sc-value" className="ks-mono" type={secret ? 'password' : 'text'} value={v} onChange={(e) => setV(e.target.value)} autoComplete="off" spellCheck={false} aria-describedby={secret ? 'sc-value-hint' : undefined} />
        </Field>
      </form>
    </Dialog>
  )
}

function AddDialog({ secret, scopeLabel, onClose, onAdd }: { secret: boolean; scopeLabel: string; onClose: () => void; onAdd: (row: SecretRow) => void }) {
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [lives, setLives] = useState('gateway')
  const [opens, setOpens] = useState('')
  const ok = name.trim() !== '' && value.trim() !== ''
  function submit(e: FormEvent) {
    e.preventDefault()
    if (!ok) return
    onAdd(
      secret
        ? { name: name.trim(), addedBy: 'procoduck', type: 'Secret', value: `•••••••• set ${TODAY}`, source: '', lives, opens: opens.trim() || '—' }
        : { name: name.trim(), addedBy: 'procoduck', type: 'Config', value: value.trim(), source: '', lives: 'config', opens: '—' },
    )
  }
  return (
    <Dialog
      open
      onOpenChange={(o) => !o && onClose()}
      title={secret ? 'Add a secret' : 'Add config'}
      description={`Saved for ${scopeLabel}. ${secret ? 'You can rotate it later, never read it.' : 'Config is readable by people in the org.'}`}
      footer={
        <>
          <DialogClose><Button variant="outline">Cancel</Button></DialogClose>
          <Button type="submit" form="sc-add-form" disabled={!ok}>{secret ? 'Save secret' : 'Save config'}</Button>
        </>
      }
    >
      <form id="sc-add-form" className="ks-stack-4" onSubmit={submit}>
        <Field id="sc-name" label="Name">
          <Input id="sc-name" className="ks-mono" value={name} onChange={(e) => setName(e.target.value)} placeholder={secret ? 'my-service-token' : 'MY_SETTING'} autoComplete="off" spellCheck={false} />
        </Field>
        <Field id="sc-val" label="Value">
          <Input id="sc-val" className="ks-mono" type={secret ? 'password' : 'text'} value={value} onChange={(e) => setValue(e.target.value)} autoComplete="off" spellCheck={false} />
        </Field>
        {secret && (
          <>
            <Field id="sc-lives" label="Where it lives">
              <NativeSelect id="sc-lives" value={lives} onChange={(e) => setLives(e.target.value)}>
                <option value="gateway">gateway</option>
                <option value="OpenShell provider record">OpenShell provider record</option>
                <option value="secret store">secret store</option>
              </NativeSelect>
            </Field>
            <Field id="sc-opens" label="Opens domain" hint="Saving also allows the gateway to reach this domain.">
              <Input id="sc-opens" className="ks-mono" value={opens} onChange={(e) => setOpens(e.target.value)} placeholder="api.example.com" aria-describedby="sc-opens-hint" autoComplete="off" spellCheck={false} />
            </Field>
          </>
        )}
      </form>
    </Dialog>
  )
}
