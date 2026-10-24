// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useState } from 'react'
import { AmountForm } from '@/components/amount-form'
import { ConfirmAction } from '@/components/confirm-action'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { BudgetService } from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { useBudgetActions } from '@/hooks/use-spend-actions'
import { formatUSD } from '@/lib/cost'
import { useOrg } from '@/lib/org-context'

/**
 * PlatformBudgetsPage is where a platform admin sets the spend ceiling of the org chosen in the org switcher
 * (ADR 0056). Org admins see the ceiling, read only, on the Budgets page. Others get a notice.
 */
export function PlatformBudgetsPage() {
  const who = useQuery(AuthService.method.whoAmI, {}).data
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      {who?.platformAdmin ? (
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
  const { org } = useOrg()
  const orgId = org.orgId
  const actions = useBudgetActions(orgId)
  const budget = useQuery(BudgetService.method.getOrgBudget, { orgId })
  const [refused, setRefused] = useState<string>()
  const [editing, setEditing] = useState(false)
  const b = budget.data?.budget
  return (
    <>
      <h1 className="text-xl font-semibold">Platform budget ceiling</h1>
      <p className="text-muted-foreground text-sm">
        You are acting as a platform admin for {org.orgDisplayName || org.orgSlug}. The ceiling is the most this org can spend in a calendar month (UTC), whatever its own cap says. It is off until you set it. Pick another
        org in the switcher at the top to set its ceiling.
      </p>
      {budget.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(budget.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      {budget.isPending ? (
        <p role="status">Loading…</p>
      ) : (
        b && (
          <section aria-labelledby="ceiling" className="bg-card grid gap-3 rounded-md border p-4">
            <h2 id="ceiling" className="text-lg font-semibold">
              Ceiling for {org.orgDisplayName || org.orgSlug}
            </h2>
            <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
              <dt className="text-muted-foreground">Ceiling</dt>
              <dd>{b.platformCeilingUsd ? formatUSD(b.platformCeilingUsd) : 'Off'}</dd>
              <dt className="text-muted-foreground">Org cap</dt>
              <dd>{b.monthlyCapUsd ? formatUSD(b.monthlyCapUsd) : 'No cap'}</dd>
              <dt className="text-muted-foreground">Spent this month</dt>
              <dd>{formatUSD(b.spentThisMonthUsd)}</dd>
            </dl>
            {refused && (
              <Alert variant="destructive" role="alert">
                <AlertDescription>{refused}</AlertDescription>
              </Alert>
            )}
            {editing ? (
              <AmountForm
                id="ceiling-amount"
                label="Ceiling in US dollars"
                submitLabel="Save ceiling"
                initial={b.platformCeilingUsd}
                help="The org’s cap must already fit under it."
                onSave={actions.setPlatformCeiling}
                onSaved={() => setEditing(false)}
              />
            ) : (
              <div className="flex flex-wrap gap-2">
                <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
                  {b.platformCeilingUsd ? 'Change ceiling' : 'Set a ceiling'}
                </Button>
                {b.platformCeilingUsd && (
                  <ConfirmAction
                    label="Remove"
                    subject="the platform ceiling"
                    consequence="Remove the ceiling? The org is then limited by its own cap only."
                    confirmLabel="Remove ceiling"
                    onConfirm={() => actions.setPlatformCeiling('')}
                    onError={setRefused}
                  />
                )}
              </div>
            )}
          </section>
        )
      )}
    </>
  )
}
