// SPDX-License-Identifier: Apache-2.0

import { useQuery } from '@connectrpc/connect-query'
import { Link } from '@tanstack/react-router'
import { useMemo, useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { BudgetService } from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { bannerNotices, noticeText } from '@/lib/budgets'

/**
 * NoticeBanner tells org owners and admins, on every page, that a spend limit is at 80% or reached. It lists the
 * highest unread notice of each limit in the window now running; reading them is on the Budgets page.
 * Mount it for admins only: members get no notices and make no request.
 */
export function NoticeBanner({ orgId, platformAdmin }: { orgId: string; platformAdmin: boolean }) {
  const list = useQuery(BudgetService.method.listNotifications, { orgId, maxResults: 50 }, { refetchInterval: 60_000 })
  // The window now running is read when the page opens: a banner left open past midnight UTC refreshes on the next visit.
  const [now] = useState(() => new Date())
  const shown = useMemo(() => bannerNotices(list.data?.notifications ?? [], now, platformAdmin), [list.data, now, platformAdmin])
  if (shown.length === 0) return null
  return (
    <div className="grid gap-2 px-4 pt-3 md:px-6" role="region" aria-label="Budget notices">
      {shown.map((n) => (
        <Alert key={n.id} variant={n.threshold >= 100 ? 'destructive' : 'default'} role={n.threshold >= 100 ? 'alert' : 'status'}>
          <AlertDescription>
            {noticeText(n)}{' '}
            <Link to="/settings/budgets" className="underline">
              See budgets
            </Link>
          </AlertDescription>
        </Alert>
      ))}
    </div>
  )
}
