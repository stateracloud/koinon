// SPDX-License-Identifier: Apache-2.0

import { useQuery } from '@connectrpc/connect-query'
import { Link } from '@tanstack/react-router'
import { useMemo, useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { BudgetService } from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { bannerNotices, isAgentUnhealthy, noticeText } from '@/lib/budgets'

/** The banner shows this many notices; the rest are counted and listed on the Budgets page. */
export const maxBannerNotices = 3

/**
 * NoticeBanner tells org owners and admins, on every page, that a spend limit is at 80% or reached. It lists the
 * highest unread notice of each limit in the window now running, at most three, and counts the rest; reading them
 * is on the Budgets page. Mount it for admins only: members get no notices and make no request.
 */
export function NoticeBanner({ orgId, platformAdmin }: { orgId: string; platformAdmin: boolean }) {
  const list = useQuery(BudgetService.method.listNotifications, { orgId, maxResults: 50 }, { refetchInterval: 60_000 })
  // The window now running is read when the page opens: a banner left open past midnight UTC refreshes on the next visit.
  const [now] = useState(() => new Date())
  const all = useMemo(() => bannerNotices(list.data?.notifications ?? [], now, platformAdmin), [list.data, now, platformAdmin])
  const shown = all.slice(0, maxBannerNotices)
  // Agent names are only fetched when a shown notice is about an agent.
  const agents = useQuery(BudgetService.method.listAgentBudgets, { orgId }, { enabled: shown.some((n) => n.agentId !== '') })
  const names = useMemo(() => new Map((agents.data?.budgets ?? []).map((a) => [a.agentId, a.agentName])), [agents.data])
  if (shown.length === 0) return null
  return (
    <div className="grid gap-2 px-4 pt-3 md:px-6" role="region" aria-label="Notices">
      {shown.map((n) => {
        const unhealthy = isAgentUnhealthy(n)
        const urgent = unhealthy || n.threshold >= 100
        return (
          <Alert key={n.id} variant={urgent ? 'destructive' : 'default'} role={urgent ? 'alert' : 'status'}>
            <AlertDescription>
              {noticeText(n, names)}{' '}
              {unhealthy && n.agentId ? (
                <Link to="/agents/$agentId" params={{ agentId: n.agentId }} className="underline">
                  Open the agent
                </Link>
              ) : (
                <Link to="/settings/budgets" className="underline">
                  See budgets
                </Link>
              )}
            </AlertDescription>
          </Alert>
        )
      })}
      {all.length > shown.length && (
        <p className="text-sm">
          {all.length - shown.length} more.{' '}
          <Link to="/settings/budgets" className="underline">
            See all notices
          </Link>
        </p>
      )}
    </div>
  )
}
