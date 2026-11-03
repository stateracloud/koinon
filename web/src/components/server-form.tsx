// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useState, type FormEvent } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import type { Server } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import type { McpActions } from '@/hooks/use-mcp-actions'
import { buildCreateRequest, buildUpdateRequest, dataLegs, formErrors, initialForm, initialMode, maxServerNameLength, serverErrorField, serverKinds, toggleLeg, transports, type CredentialMode } from '@/lib/mcp-servers'

export type ServerFormProps = {
  /** Set to edit that server; unset to add one. */
  server?: Server
  actions: Pick<McpActions, 'createServer' | 'updateServer'>
  onDone: () => void
  onSaved?: (name: string) => void
}

/**
 * ServerForm adds or edits a remote MCP server entered in the UI. The credential is write-only: the
 * field is always empty, an edit keeps the stored one unless a new one is typed or removal is asked
 * for, and the typed value is cleared from the page as soon as it is sent.
 */
export function ServerForm({ server, actions, onDone, onSaved }: ServerFormProps) {
  const editing = !!server
  const [f, setF] = useState(() => initialForm(server))
  const [touched, setTouched] = useState(false)
  const [pending, setPending] = useState(false)
  const [refusal, setRefusal] = useState<{ message: string; field?: 'name' | 'url' | 'credential' }>()
  const set = (p: Partial<typeof f>) => setF((cur) => ({ ...cur, ...p }))

  const errors = formErrors(f, server)
  const invalid = Object.values(errors).some(Boolean)
  const err = (k: keyof typeof errors) => (touched ? errors[k] : undefined)
  const stored = !!server?.hasCredential && initialMode(server) === f.mode && !f.removeCredential

  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (invalid) return
    setPending(true)
    setRefusal(undefined)
    const typed = f.value
    set({ value: '' }) // the secret leaves this page with the request
    try {
      if (server) await actions.updateServer(server.id, buildUpdateRequest(server, f))
      else await actions.createServer(buildCreateRequest(f))
      onSaved?.(server?.name ?? f.name)
      onDone()
    } catch (e2) {
      const message = ConnectError.from(e2).rawMessage
      setRefusal({ message: typed ? `${message} The secret was cleared from this page. Type it again.` : message, field: serverErrorField(message) })
    } finally {
      setPending(false)
    }
  }

  const refused = (field: 'name' | 'url' | 'credential') => (refusal?.field === field ? refusal.message : undefined)
  const unplaced = refusal && !refusal.field ? refusal.message : undefined
  const describe = (id: string, ...extra: (string | undefined | false)[]) => [`${id}-help`, ...extra.filter(Boolean)].join(' ')

  return (
    <form onSubmit={(e) => void submit(e)} aria-label={editing ? `Edit server ${server.name}` : 'Add server'} className="bg-card grid gap-4 rounded-md border p-4" noValidate>
      <h2 className="text-base font-semibold">{editing ? `Edit ${server.name}` : 'Add a remote MCP server'}</h2>
      <div className="grid gap-1.5">
        <Label htmlFor="server-name">Name</Label>
        <Input
          id="server-name"
          value={f.name}
          onChange={(e) => set({ name: e.target.value })}
          disabled={editing}
          maxLength={maxServerNameLength}
          autoComplete="off"
          aria-invalid={!!(err('name') || refused('name'))}
          aria-describedby={describe('server-name', err('name') && 'server-name-error', refused('name') && 'server-name-refused')}
        />
        <p id="server-name-help" className="text-muted-foreground text-xs">
          {editing ? 'A server keeps its name.' : `Up to ${maxServerNameLength} lowercase letters, digits and dashes. Agents see its tools under this name.`}
        </p>
        {err('name') && (
          <p id="server-name-error" className="text-destructive text-xs">
            {err('name')}
          </p>
        )}
        {refused('name') && (
          <p id="server-name-refused" role="alert" className="text-destructive text-xs">
            {refused('name')}
          </p>
        )}
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="server-url">URL</Label>
        <Input
          id="server-url"
          type="url"
          inputMode="url"
          value={f.url}
          onChange={(e) => set({ url: e.target.value })}
          autoComplete="off"
          aria-invalid={!!(err('url') || refused('url'))}
          aria-describedby={describe('server-url', err('url') && 'server-url-error', refused('url') && 'server-url-refused')}
        />
        <p id="server-url-help" className="text-muted-foreground text-xs">
          The MCP endpoint. Koinon refuses private, loopback and cluster-internal addresses unless a platform admin allows them, and servers that ask each user to sign in with OAuth.
        </p>
        {err('url') && (
          <p id="server-url-error" className="text-destructive text-xs">
            {err('url')}
          </p>
        )}
        {refused('url') && (
          <p id="server-url-refused" role="alert" className="text-destructive text-xs">
            {refused('url')}
          </p>
        )}
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="server-transport">Transport</Label>
        <NativeSelect id="server-transport" value={f.transport} onChange={(e) => set({ transport: e.target.value })}>
          {transports.map((t) => (
            <option key={t.value} value={t.value}>
              {t.label}
            </option>
          ))}
        </NativeSelect>
      </div>
      <div className="grid gap-1.5">
        <Label htmlFor="server-credential-mode">Credential</Label>
        <NativeSelect id="server-credential-mode" value={f.mode} onChange={(e) => set({ mode: e.target.value as CredentialMode, removeCredential: false })}>
          <option value="none">No credential</option>
          <option value="bearer">Bearer token</option>
          <option value="custom">Custom header</option>
        </NativeSelect>
      </div>
      {f.mode === 'custom' && (
        <div className="grid gap-1.5">
          <Label htmlFor="server-header">Header name</Label>
          <Input
            id="server-header"
            value={f.header}
            onChange={(e) => set({ header: e.target.value })}
            placeholder="X-Api-Key"
            autoComplete="off"
            aria-invalid={!!err('header')}
            aria-describedby={err('header') ? 'server-header-error' : undefined}
          />
          {err('header') && (
            <p id="server-header-error" className="text-destructive text-xs">
              {err('header')}
            </p>
          )}
        </div>
      )}
      {f.mode !== 'none' && (
        <div className="grid gap-1.5">
          <Label htmlFor="server-secret">{f.mode === 'bearer' ? 'Token' : 'Header value'}</Label>
          <Input
            id="server-secret"
            type="password"
            value={f.value}
            onChange={(e) => set({ value: e.target.value })}
            autoComplete="new-password"
            spellCheck={false}
            disabled={f.removeCredential}
            aria-invalid={!!(err('value') || refused('credential'))}
            aria-describedby={describe('server-secret', err('value') && 'server-secret-error', refused('credential') && 'server-secret-refused')}
          />
          <p id="server-secret-help" className="text-muted-foreground text-xs">
            {stored
              ? 'A credential is stored. Leave empty to keep it.'
              : f.mode === 'bearer'
                ? 'Sent as Authorization: Bearer <token>.'
                : 'Sent as the value of this header, without a prefix.'}{' '}
            Koinon stores it encrypted and never shows it again. A new credential takes about three minutes to reach the gateway.
          </p>
          {err('value') && (
            <p id="server-secret-error" className="text-destructive text-xs">
              {err('value')}
            </p>
          )}
          {refused('credential') && (
            <p id="server-secret-refused" role="alert" className="text-destructive text-xs">
              {refused('credential')}
            </p>
          )}
        </div>
      )}
      {editing && server.hasCredential && (
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={f.removeCredential} onChange={(e) => set({ removeCredential: e.target.checked, value: e.target.checked ? '' : f.value })} />
          Remove credential
        </label>
      )}
      {!editing && (
        <fieldset className="grid gap-2 rounded-md border p-3">
          <legend className="px-1 text-sm font-medium">What this server exposes</legend>
          <div className="grid gap-1.5">
            <Label htmlFor="server-kind">Kind</Label>
            <NativeSelect id="server-kind" value={f.kind} onChange={(e) => set({ kind: e.target.value })}>
              {serverKinds.map((k) => (
                <option key={k.value} value={k.value}>
                  {k.label}
                </option>
              ))}
            </NativeSelect>
          </div>
          {dataLegs.map((l) => (
            <label key={l.value} className="flex items-center gap-2 text-sm" title={l.help}>
              <input type="checkbox" checked={f.legs.includes(l.value)} onChange={(e) => set({ legs: toggleLeg(f.legs, l.value, e.target.checked) })} />
              {l.label}
            </label>
          ))}
          <p className="text-muted-foreground text-xs">The leak guard (ADR 0084) uses this. A server nobody classified counts as every leg, so leave a leg on unless you know the server does not have it.</p>
        </fieldset>
      )}
      {!editing && <p className="text-muted-foreground text-xs">The tools of a new server start in the Ask tier: a human approves each call until you set another tier.</p>}
      {unplaced && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{unplaced}</AlertDescription>
        </Alert>
      )}
      <div className="flex justify-end gap-2">
        <Button type="button" variant="outline" onClick={onDone} disabled={pending}>
          Cancel
        </Button>
        <Button type="submit" disabled={pending}>
          {editing ? 'Save server' : 'Add server'}
        </Button>
      </div>
    </form>
  )
}
