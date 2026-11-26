// SPDX-License-Identifier: Apache-2.0

import { useQuery } from '@connectrpc/connect-query'
import { timestampMs, type Timestamp } from '@bufbuild/protobuf/wkt'
import { KeyRoundIcon } from 'lucide-react'
import { useMemo, useState, type FormEvent } from 'react'
import { ConfirmAction } from '@/components/confirm-action'
import { RequireAdmin } from '@/components/require-admin'
import { TokenStatusBadge } from '@/components/status-badge'
import { TokenSecret } from '@/components/token-secret'
import { Button } from '@/components/ui/button'
import { DataTable } from '@/components/ui/data-table'
import { EmptyState } from '@/components/ui/empty-state'
import { InlineError, QueryError } from '@/components/ui/inline-error'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { PageHeader } from '@/components/ui/page-header'
import { SkeletonRows } from '@/components/ui/skeleton'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TokenService, type Token } from '@/gen/koinon/token/v1alpha1/token_pb'
import { useTokenActions } from '@/hooks/use-catalog-actions'
import { useFeedback } from '@/hooks/use-feedback'
import { dataTableColumns, type DataTableColumn } from '@/lib/data-table'
import { tokenRoleLabel } from '@/lib/labels'
import { useOrg } from '@/lib/org-context'
import { createdByLabel, daysToSeconds, defaultTokenDays, formatTime, maxTokenDays, tokenDaysError, tokenStatus } from '@/lib/providers'
import { toast } from '@/lib/toast'

const col = dataTableColumns<Token>()

/** timeMs is the sort key of an optional time: no time sorts first. */
function timeMs(ts: Timestamp | undefined): number {
  return ts ? timestampMs(ts) : 0
}

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
  const revoke = useFeedback()
  const [shown, setShown] = useState<{ name: string; secret: string }>()
  const tokens = useMemo(() => list.data?.tokens ?? [], [list.data])
  const { revoke: revokeToken } = actions
  const { setError: setRevokeError } = revoke

  const columns = useMemo<DataTableColumn<Token>[]>(
    () => [
      col.accessor('name', { header: 'Name', sortFn: 'text', meta: { rowHeader: true } }),
      col.display({ id: 'status', header: 'Status', cell: (c) => <TokenStatusBadge status={tokenStatus(c.row.original)} /> }),
      col.accessor('prefix', { header: 'Prefix', enableSorting: false, cell: (c) => <code className="font-mono text-xs">{c.getValue()}</code> }),
      col.accessor((t) => tokenRoleLabel(t.role), { id: 'role', header: 'Role', sortFn: 'text' }),
      col.accessor((t) => timeMs(t.createdAt), { id: 'created', header: 'Created', sortFn: 'basic', cell: (c) => formatTime(c.row.original.createdAt) }),
      col.accessor((t) => timeMs(t.expiresAt), { id: 'expires', header: 'Expires', sortFn: 'basic', cell: (c) => formatTime(c.row.original.expiresAt) }),
      col.accessor((t) => timeMs(t.lastUsedAt), { id: 'lastUsed', header: 'Last used', sortFn: 'basic', cell: (c) => formatTime(c.row.original.lastUsedAt) }),
      col.display({ id: 'createdBy', header: 'Created by', cell: (c) => createdByLabel(c.row.original, tokens) }),
      col.display({
        id: 'actions',
        header: () => <span className="sr-only">Actions</span>,
        cell: (c) => {
          const t = c.row.original
          return (
            tokenStatus(t) !== 'revoked' && (
              <ConfirmAction
                label="Revoke"
                subject={`token ${t.name}`}
                consequence={`Revoke ${t.name}? This also revokes every token it created.`}
                confirmLabel="Revoke token"
                onConfirm={async () => {
                  setRevokeError(undefined)
                  await revokeToken(t.id)
                  toast(`Token ${t.name} revoked.`)
                }}
                onError={setRevokeError}
              />
            )
          )
        },
      }),
    ],
    [tokens, revokeToken, setRevokeError],
  )

  return (
    <>
      <PageHeader
        title="API tokens"
        description="An API token lets a script or a pipeline call the admin API for this org, for example to manage providers from CI. It acts with the role you give it and stops working when it expires."
      />
      {shown ? (
        <TokenSecret name={shown.name} secret={shown.secret} onDismiss={() => setShown(undefined)} />
      ) : (
        <CreateTokenForm ownerAllowed={org.role === OrgRole.OWNER} onCreate={actions.create} onCreated={setShown} />
      )}
      <InlineError message={revoke.error} />
      <QueryError error={list.isError ? list.error : undefined} />
      {list.isPending ? (
        <SkeletonRows label="Loading tokens…" />
      ) : list.isError && !list.data ? null : (
        <DataTable
          caption="API tokens"
          columns={columns}
          data={tokens}
          getRowId={(t) => t.id}
          empty={<EmptyState icon={KeyRoundIcon} title="No tokens yet." description="Create a token above for a script or a pipeline." />}
        />
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
  const feedback = useFeedback()
  const nameError = name.trim() ? undefined : 'Name is required.'
  const daysError = tokenDaysError(days)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setTouched(true)
    if (nameError || daysError) return
    setPending(true)
    // No toast: the secret panel that replaces the form is the success feedback.
    const res = await feedback.run(() => onCreate({ name: name.trim(), role, expiresInSeconds: daysToSeconds(Number(days)) }))
    setPending(false)
    if (!res) return
    setName('')
    setDays(String(defaultTokenDays))
    onCreated({ name: res.token?.name ?? name.trim(), secret: res.secret })
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
      <InlineError message={feedback.error} />
      <div className="flex justify-end">
        <Button type="submit" disabled={pending}>
          Create token
        </Button>
      </div>
    </form>
  )
}
