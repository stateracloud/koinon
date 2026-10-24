// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useState, type FormEvent } from 'react'
import { ConfirmAction } from '@/components/confirm-action'
import { RequireAdmin } from '@/components/require-admin'
import { TokenSecret } from '@/components/token-secret'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TokenService } from '@/gen/koinon/token/v1alpha1/token_pb'
import { useTokenActions } from '@/hooks/use-catalog-actions'
import { tokenRoleLabel } from '@/lib/labels'
import { useOrg } from '@/lib/org-context'
import { createdByLabel, daysToSeconds, defaultTokenDays, formatTime, maxTokenDays, tokenDaysError, tokenStatus } from '@/lib/providers'

/** TokensPage manages the org's admin API tokens (ADR 0056): create, see once, list, revoke. */
export function TokensPage() {
  return (
    <div className="mx-auto grid max-w-4xl grid-cols-[minmax(0,1fr)] gap-4">
      <RequireAdmin what="API tokens">
        <TokensContent />
      </RequireAdmin>
    </div>
  )
}

function TokensContent() {
  const { org } = useOrg()
  const orgId = org.orgId
  const list = useQuery(TokenService.method.listTokens, { orgId })
  const actions = useTokenActions(orgId)
  const [shown, setShown] = useState<{ name: string; secret: string }>()
  const [refused, setRefused] = useState<string>()
  const tokens = list.data?.tokens ?? []

  return (
    <>
      <h1 className="text-xl font-semibold">API tokens</h1>
      <p className="text-muted-foreground text-sm">
        An API token lets a script or a pipeline call the admin API for this org, for example to manage providers from CI. It acts with the role you give it and stops working when it expires.
      </p>
      {shown ? (
        <TokenSecret name={shown.name} secret={shown.secret} onDismiss={() => setShown(undefined)} />
      ) : (
        <CreateTokenForm ownerAllowed={org.role === OrgRole.OWNER} onCreate={actions.create} onCreated={setShown} />
      )}
      {refused && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refused}</AlertDescription>
        </Alert>
      )}
      {list.isError && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(list.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      {list.isPending ? (
        <p role="status">Loading tokens…</p>
      ) : tokens.length === 0 ? (
        <p className="text-muted-foreground text-sm">No tokens yet.</p>
      ) : (
        <div className="relative overflow-x-auto" tabIndex={0} role="region" aria-label="API tokens table">
        <table className="w-full text-left text-sm">
          <caption className="sr-only">API tokens</caption>
          <thead>
            <tr className="border-b">
              <th scope="col" className="py-2 pr-3">Name</th>
              <th scope="col" className="py-2 pr-3">Prefix</th>
              <th scope="col" className="py-2 pr-3">Role</th>
              <th scope="col" className="py-2 pr-3">Created</th>
              <th scope="col" className="py-2 pr-3">Expires</th>
              <th scope="col" className="py-2 pr-3">Last used</th>
              <th scope="col" className="py-2 pr-3">Created by</th>
              <th scope="col" className="py-2">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {tokens.map((t) => {
              const status = tokenStatus(t)
              return (
                <tr key={t.id} className="border-b align-top">
                  <th scope="row" className="py-2 pr-3 font-medium">
                    {t.name}{' '}
                    {status !== 'active' && <Badge variant="destructive">{status === 'revoked' ? 'Revoked' : 'Expired'}</Badge>}
                  </th>
                  <td className="py-2 pr-3">
                    <code className="text-xs">{t.prefix}</code>
                  </td>
                  <td className="py-2 pr-3">{tokenRoleLabel(t.role)}</td>
                  <td className="py-2 pr-3">{formatTime(t.createdAt)}</td>
                  <td className="py-2 pr-3">{formatTime(t.expiresAt)}</td>
                  <td className="py-2 pr-3">{formatTime(t.lastUsedAt)}</td>
                  <td className="py-2 pr-3">{createdByLabel(t, tokens)}</td>
                  <td className="py-2">
                    {status !== 'revoked' && (
                      <ConfirmAction
                        label="Revoke"
                        subject={`token ${t.name}`}
                        consequence={`Revoke ${t.name}? This also revokes every token it created.`}
                        confirmLabel="Revoke token"
                        onConfirm={async () => {
                          setRefused(undefined)
                          await actions.revoke(t.id)
                        }}
                        onError={setRefused}
                      />
                    )}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
        </div>
      )}
    </>
  )
}

function CreateTokenForm({
  ownerAllowed,
  onCreate,
  onCreated,
}: {
  ownerAllowed: boolean
  onCreate: (i: { name: string; role: OrgRole; expiresInSeconds: bigint }) => Promise<{ secret: string; token?: { name: string } }>
  onCreated: (s: { name: string; secret: string }) => void
}) {
  const [name, setName] = useState('')
  const [role, setRole] = useState<OrgRole>(OrgRole.ADMIN)
  const [days, setDays] = useState(String(defaultTokenDays))
  const [touched, setTouched] = useState(false)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()
  const nameError = name.trim() ? undefined : 'Name is required.'
  const daysError = tokenDaysError(days)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (nameError || daysError) return
    setPending(true)
    setError(undefined)
    try {
      const res = await onCreate({ name: name.trim(), role, expiresInSeconds: daysToSeconds(Number(days)) })
      setName('')
      setDays(String(defaultTokenDays))
      onCreated({ name: res.token?.name ?? name.trim(), secret: res.secret })
    } catch (err) {
      setError(ConnectError.from(err).rawMessage)
    } finally {
      setPending(false)
    }
  }

  return (
    <form onSubmit={(e) => void submit(e)} aria-label="Create API token" className="bg-card grid gap-4 rounded-md border p-4" noValidate>
      <h2 className="text-base font-semibold">Create a token</h2>
      <div className="grid gap-4 sm:grid-cols-3">
        <div className="grid gap-1.5">
          <Label htmlFor="token-name">Name</Label>
          <Input id="token-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="CI pipeline" autoComplete="off" aria-invalid={touched && !!nameError} aria-describedby={touched && nameError ? 'token-name-error' : undefined} />
          {touched && nameError && (
            <p id="token-name-error" className="text-destructive text-xs">
              {nameError}
            </p>
          )}
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="token-role">Role</Label>
          <NativeSelect id="token-role" value={String(role)} onChange={(e) => setRole(Number(e.target.value) as OrgRole)} aria-describedby="token-role-help">
            <option value={OrgRole.ADMIN}>Admin</option>
            {ownerAllowed && <option value={OrgRole.OWNER}>Owner</option>}
          </NativeSelect>
          <p id="token-role-help" className="text-muted-foreground text-xs">
            Never more than your own role.
          </p>
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="token-days">Expires in (days)</Label>
          <Input id="token-days" type="number" min={1} max={maxTokenDays} value={days} onChange={(e) => setDays(e.target.value)} aria-invalid={touched && !!daysError} aria-describedby={touched && daysError ? 'token-days-help token-days-error' : 'token-days-help'} />
          <p id="token-days-help" className="text-muted-foreground text-xs">
            {defaultTokenDays} by default, {maxTokenDays} at most.
          </p>
          {touched && daysError && (
            <p id="token-days-error" className="text-destructive text-xs">
              {daysError}
            </p>
          )}
        </div>
      </div>
      {error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{error}</AlertDescription>
        </Alert>
      )}
      <div className="flex justify-end">
        <Button type="submit" disabled={pending}>
          Create token
        </Button>
      </div>
    </form>
  )
}
