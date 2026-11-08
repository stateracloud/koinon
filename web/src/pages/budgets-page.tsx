// SPDX-License-Identifier: Apache-2.0

import { ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useMemo, useState } from 'react'
import { closeOnly } from '@/lib/editing'
import { AmountForm } from '@/components/amount-form'
import { ConfirmAction } from '@/components/confirm-action'
import { RequireAdmin } from '@/components/require-admin'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { BudgetService, type AgentBudget, type OrgBudget } from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { useBudgetActions } from '@/hooks/use-spend-actions'
import { formatDate, isAgentUnhealthy, nextMonthStart, noticeText, overridden } from '@/lib/budgets'
import { formatUSD } from '@/lib/cost'
import { percent, remaining } from '@/lib/money'
import { useOrg } from '@/lib/org-context'
import { Link } from '@tanstack/react-router'

/** BudgetsPage shows the org’s monthly cap, the agents’ daily limits and the notices about them (ADR 0056, plan item BG1). */
export function BudgetsPage() {
  return (
    <div className="mx-auto grid max-w-3xl grid-cols-[minmax(0,1fr)] gap-6">
      <RequireAdmin what="budgets">
        <BudgetsContent />
      </RequireAdmin>
    </div>
  )
}

function BudgetsContent() {
  const { org } = useOrg()
  const orgId = org.orgId
  const actions = useBudgetActions(orgId)
  const budget = useQuery(BudgetService.method.getOrgBudget, { orgId })
  const agents = useQuery(BudgetService.method.listAgentBudgets, { orgId })
  const failed = budget.error ?? agents.error
  return (
    <>
      <h1 className="text-xl font-semibold">Budgets</h1>
      <p className="text-muted-foreground text-sm">
        Budgets stop spending, not just report it. The first limit an agent reaches stops its run: its own daily limit (UTC day), the org’s monthly cap (calendar month, UTC) or the platform ceiling. The amounts count
        what the models cost, using the <Link to="/settings/prices" className="underline">prices</Link> you see.
      </p>
      {failed && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(failed).rawMessage}</AlertDescription>
        </Alert>
      )}
      {budget.isPending ? <p role="status">Loading the budget…</p> : budget.data?.budget && <OrgCap budget={budget.data.budget} actions={actions} />}
      <AgentLimits loading={agents.isPending} list={agents.data?.budgets ?? []} actions={actions} />
      <Notices orgId={orgId} actions={actions} agents={agents.data?.budgets ?? []} />
    </>
  )
}

type Actions = ReturnType<typeof useBudgetActions>

function OrgCap({ budget: b, actions }: { budget: OrgBudget; actions: Actions }) {
  const [refused, setRefused] = useState<string>()
  const [editing, setEditing] = useState(false)
  const used = b.monthlyCapUsd ? percent(b.spentThisMonthUsd, b.monthlyCapUsd) : undefined
  const left = b.monthlyCapUsd ? remaining(b.monthlyCapUsd, b.spentThisMonthUsd) : undefined
  const estShare = percent(b.estimatedThisMonthUsd, b.spentThisMonthUsd)
  const [now] = useState(() => new Date())
  const resets = nextMonthStart(now)
  return (
    <section aria-labelledby="org-cap" className="bg-card grid gap-3 rounded-md border p-4">
      <h2 id="org-cap" className="text-lg font-semibold">
        Monthly cap
      </h2>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
        <dt className="text-muted-foreground">Cap</dt>
        <dd>{b.monthlyCapUsd ? formatUSD(b.monthlyCapUsd) : 'No cap'}</dd>
        <dt className="text-muted-foreground">Spent this month</dt>
        <dd>{formatUSD(b.spentThisMonthUsd)}</dd>
        {Number(b.systemChecksThisMonthUsd) > 0 && (
          <>
            <dt className="text-muted-foreground">System checks</dt>
            <dd>
              {formatUSD(b.systemChecksThisMonthUsd)}
              <span className="text-muted-foreground"> Koinon&apos;s own requests, such as the provider echo check. Included in the spend above; they belong to no agent.</span>
            </dd>
          </>
        )}
        {left !== undefined && (
          <>
            <dt className="text-muted-foreground">Remaining</dt>
            <dd>{formatUSD(left)}</dd>
          </>
        )}
        <dt className="text-muted-foreground">Estimated share</dt>
        <dd>
          {estShare === undefined ? 'Nothing spent yet' : `${estShare}% of the spend (${formatUSD(b.estimatedThisMonthUsd)})`}
          <span className="text-muted-foreground"> Estimated when a provider reports no usage; it counts toward the cap.</span>
        </dd>
        <dt className="text-muted-foreground">Window</dt>
        <dd>
          Calendar month, UTC. It starts again on {resets.toISOString().slice(0, 10)}.
        </dd>
        <dt className="text-muted-foreground">Platform ceiling</dt>
        <dd>{b.platformCeilingUsd ? `${formatUSD(b.platformCeilingUsd)}. Platform admins set it; your cap cannot be higher.` : 'None. Platform admins can set one.'}</dd>
      </dl>
      {used !== undefined && (
        <div>
          <progress className="h-2 w-full" max={100} value={Math.min(used, 100)} aria-label="Share of the monthly cap spent" />
          <p className="text-xs">{used}% of the cap is spent.</p>
        </div>
      )}
      {refused && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{refused}</AlertDescription>
        </Alert>
      )}
      {editing ? (
        <AmountForm id="cap" label="Monthly cap in US dollars" submitLabel="Save cap" initial={b.monthlyCapUsd} help="When the month’s spend reaches it, every open run of the org stops. You are told at 80% and at 100%." onSave={actions.setMonthlyCap} onSaved={() => setEditing(false)} />
      ) : (
        <div className="flex flex-wrap gap-2">
          <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
            {b.monthlyCapUsd ? 'Change cap' : 'Set a cap'}
          </Button>
          {b.monthlyCapUsd && (
            <ConfirmAction
              label="Remove"
              subject="the monthly cap"
              consequence="Remove the monthly cap? Agents keep their daily limits."
              confirmLabel="Remove cap"
              onConfirm={() => actions.setMonthlyCap('')}
              onError={setRefused}
            />
          )}
        </div>
      )}
    </section>
  )
}

function AgentLimits({ loading, list, actions }: { loading: boolean; list: AgentBudget[]; actions: Actions }) {
  const [editing, setEditing] = useState<string>()
  return (
    <section aria-labelledby="agent-limits" className="grid gap-3">
      <h2 id="agent-limits" className="text-lg font-semibold">
        Agent daily limits
      </h2>
      <p className="text-muted-foreground text-sm">Each agent starts with the limit of its template. Change it here; the template is not touched. Days are UTC.</p>
      {loading ? (
        <p role="status">Loading the limits…</p>
      ) : list.length === 0 ? (
        <p className="text-muted-foreground text-sm">No agents yet.</p>
      ) : (
        <ul className="grid gap-3" aria-label="Agent daily limits">
          {list.map((a) => (
            <li key={a.agentId} className="bg-card grid gap-2 rounded-md border p-3">
              <div className="flex flex-wrap items-center gap-2">
                <h3 className="text-base font-semibold">{a.agentName || a.agentId.slice(0, 8)}</h3>
                {overridden(a.dailyLimitUsd, a.templateDefaultUsd) ? <Badge variant="secondary">Changed from the template</Badge> : <Badge variant="outline">Template default</Badge>}
              </div>
              <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
                <dt className="text-muted-foreground">Daily limit</dt>
                <dd>{formatUSD(a.dailyLimitUsd)}</dd>
                <dt className="text-muted-foreground">Template default</dt>
                <dd>{formatUSD(a.templateDefaultUsd)}</dd>
                <dt className="text-muted-foreground">Spent today</dt>
                <dd>
                  {formatUSD(a.spentTodayUsd)}
                  {remaining(a.dailyLimitUsd, a.spentTodayUsd) !== undefined && <span className="text-muted-foreground"> ({formatUSD(remaining(a.dailyLimitUsd, a.spentTodayUsd)!)} left)</span>}
                </dd>
              </dl>
              {editing === a.agentId ? (
                <AmountForm
                  id={`limit-${a.agentId}`}
                  label={`Daily limit for ${a.agentName || 'the agent'}, in US dollars`}
                  submitLabel={`Save limit for ${a.agentName || 'the agent'}`}
                  initial={a.dailyLimitUsd}
                  onSave={(usd) => actions.setAgentLimit(a.agentId, usd)}
                  onSaved={closeOnly(setEditing, a.agentId)}
                />
              ) : (
                <div>
                  <Button variant="outline" size="sm" aria-label={`Change limit of ${a.agentName || 'the agent'}`} onClick={() => setEditing(a.agentId)}>
                    Change limit
                  </Button>
                </div>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

function Notices({ orgId, actions, agents }: { orgId: string; actions: Actions; agents: AgentBudget[] }) {
  const list = useQuery(BudgetService.method.listNotifications, { orgId })
  const names = useMemo(() => new Map(agents.map((a) => [a.agentId, a.agentName])), [agents])
  const items = list.data?.notifications ?? []
  return (
    <section aria-labelledby="notices" className="grid gap-3">
      <h2 id="notices" className="text-lg font-semibold">
        Notifications
      </h2>
      {list.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(list.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      {list.isPending ? (
        <p role="status">Loading notifications…</p>
      ) : items.length === 0 ? (
        <p className="text-muted-foreground text-sm">Nothing yet. You are told when a limit reaches 80% and 100%.</p>
      ) : (
        <ul className="grid gap-2" aria-label="Notifications">
          {items.map((n) => (
            <li key={n.id} className="bg-card flex flex-wrap items-center gap-2 rounded-md border p-3 text-sm">
              <span className="min-w-0 flex-1">
                {!n.readAt && <Badge className="mr-2">New</Badge>}
                {noticeText(n, names)} <span className="text-muted-foreground">{formatDate(n.createdAt)}</span>
                {isAgentUnhealthy(n) && n.agentId && (
                  <>
                    {' '}
                    <Link to="/agents/$agentId" params={{ agentId: n.agentId }} className="underline">
                      Open the agent
                    </Link>
                  </>
                )}
              </span>
              {!n.readAt && (
                <Button variant="outline" size="sm" aria-label={`Mark as read: ${noticeText(n, names)}`} onClick={() => void actions.markRead(n.id)}>
                  Mark as read
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
