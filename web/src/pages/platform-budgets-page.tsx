// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useState } from 'react'
import { AmountForm } from '@/components/amount-form'
import { ConfirmAction } from '@/components/confirm-action'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { BudgetService, type OrgBudgetSummary } from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { usePlatformActions } from '@/hooks/use-spend-actions'
import { formatUSD } from '@/lib/cost'

/**
 * PlatformBudgetsPage is where a platform admin sees every org's cap, spend and platform ceiling, and sets the
 * ceilings in place (ADR 0056). Org admins see their ceiling, read only, on the Budgets page. Others get a notice.
 */
export function PlatformBudgetsPage() {
  const who = useQuery(AuthService.method.whoAmI, {})
  return (
    <div className="mx-auto grid max-w-3xl grid-cols-[minmax(0,1fr)] gap-4">
      {who.isPending ? (
        <p role="status">Loading…</p>
      ) : who.data?.platformAdmin ? (
        <PlatformBudgets />
      ) : (
        <Alert role="alert">
          <AlertDescription>Only platform admins can set the platform ceiling.</AlertDescription>
        </Alert>
      )}
    </div>
  )
}

function PlatformBudgets() {
  const actions = usePlatformActions()
  const orgs = useQuery(BudgetService.method.listOrgBudgets, {})
  const [refused, setRefused] = useState<string>()
  const [editing, setEditing] = useState<string>()
  return (
    <>
      <h1 className="text-xl font-semibold">Platform budget ceilings</h1>
      <p className="text-muted-foreground text-sm">
        You are acting as a platform admin. The ceiling is the most an org can spend in a calendar month (UTC), whatever its own cap says. It is off until you set it. An org’s cap must already fit under its ceiling.
      </p>
      {orgs.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(orgs.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      {refused && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refused}</AlertDescription>
        </Alert>
      )}
      {orgs.isPending ? (
        <p role="status">Loading orgs…</p>
      ) : (
        <ul className="grid gap-3" aria-label="Orgs">
          {(orgs.data?.orgs ?? []).map((o) => (
            <OrgRow key={o.orgId} org={o} editing={editing === o.orgId} setEditing={(e) => setEditing(e ? o.orgId : undefined)} actions={actions} onRefused={setRefused} />
          ))}
        </ul>
      )}
    </>
  )
}

function OrgRow({
  org: o,
  editing,
  setEditing,
  actions,
  onRefused,
}: {
  org: OrgBudgetSummary
  editing: boolean
  setEditing: (e: boolean) => void
  actions: ReturnType<typeof usePlatformActions>
  onRefused: (m: string) => void
}) {
  return (
    <li className="bg-card grid gap-2 rounded-md border p-3">
      <h2 className="text-base font-semibold">{o.orgName || o.orgSlug}</h2>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted-foreground">Ceiling</dt>
        <dd>{o.platformCeilingUsd ? formatUSD(o.platformCeilingUsd) : 'Off'}</dd>
        <dt className="text-muted-foreground">Org cap</dt>
        <dd>{o.monthlyCapUsd ? formatUSD(o.monthlyCapUsd) : 'No cap'}</dd>
        <dt className="text-muted-foreground">Spent this month</dt>
        <dd>{formatUSD(o.spentThisMonthUsd)}</dd>
      </dl>
      {editing ? (
        <AmountForm
          id={`ceiling-${o.orgId}`}
          label={`Ceiling for ${o.orgName || o.orgSlug}, in US dollars`}
          submitLabel={`Save ceiling for ${o.orgName || o.orgSlug}`}
          initial={o.platformCeilingUsd}
          help="The org’s cap must already fit under it."
          onSave={(usd) => actions.setCeiling(o.orgId, usd)}
          onSaved={() => setEditing(false)}
        />
      ) : (
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" aria-label={`${o.platformCeilingUsd ? 'Change' : 'Set a'} ceiling of ${o.orgName || o.orgSlug}`} onClick={() => setEditing(true)}>
            {o.platformCeilingUsd ? 'Change ceiling' : 'Set a ceiling'}
          </Button>
          {o.platformCeilingUsd && (
            <ConfirmAction
              label="Remove"
              subject={`the ceiling of ${o.orgName || o.orgSlug}`}
              consequence="Remove the ceiling? The org is then limited by its own cap only."
              confirmLabel="Remove ceiling"
              onConfirm={() => actions.setCeiling(o.orgId, '')}
              onError={onRefused}
            />
          )}
        </div>
      )}
    </li>
  )
}
