// SPDX-License-Identifier: Apache-2.0

import { useQuery } from '@connectrpc/connect-query'
import { Link } from '@tanstack/react-router'
import { useMemo } from 'react'
import { OpenOrgButton } from '@/components/open-org-button'
import { QueryError } from '@/components/ui/inline-error'
import { PageHeader } from '@/components/ui/page-header'
import { SkeletonRows } from '@/components/ui/skeleton'
import { StatTile } from '@/components/ui/stat-tile'
import { UsageService } from '@/gen/koinon/usage/v1alpha1/usage_pb'
import { useAllOrgs } from '@/hooks/use-all-orgs'
import { formatUSD } from '@/lib/cost'
import { useOrgSession } from '@/lib/org-context'
import { orgSummaryName, roleText } from '@/lib/platform-orgs'
import { formatCount, formatShare, groupUsage } from '@/lib/usage'

/** topOrgs is how many orgs "Orgs with the highest spend" shows; the Usage and cost page shows all of them. */
const topOrgs = 5

/**
 * PlatformConsolePage is the home of the platform console at /platform (ADR 0126 section 4, issue #1281). The tiles
 * show the orgs, their agents, the spend of this month (UTC) with its cache read share, and today's runs. Under them
 * are the orgs with the highest spend this month. The spend and the runs come from UsageService.GetPlatformUsage with
 * no range, which is the month to date.
 */
export function PlatformConsolePage() {
  const { memberships } = useOrgSession()
  const list = useAllOrgs()
  const usage = useQuery(UsageService.method.getPlatformUsage, {})
  const rows = usage.data?.rows
  const top = useMemo(() => groupUsage(rows ?? [], (r) => ({ key: r.orgId, label: r.orgName || r.orgSlug, detail: r.orgSlug })).slice(0, topOrgs), [rows])
  const summaries = useMemo(() => new Map((list.orgs ?? []).map((o) => [o.orgId, o])), [list.orgs])
  const orgCount = list.orgs?.length
  const agentCount = list.orgs?.reduce((n, o) => n + o.agentCount, 0)
  // While more pages of orgs load, the counts are a lower bound.
  const more = list.isFetchingMore ? '+' : ''
  const t = usage.data?.totals
  const runs = usage.data?.runs
  return (
    <div className="mx-auto grid max-w-4xl gap-4">
      <PageHeader title="Platform console" description="Settings for all the orgs of this Koinon." />
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <StatTile label="Orgs" value={orgCount === undefined ? '…' : `${formatCount(orgCount)}${more}`} hint="Team orgs" />
        <StatTile label="Active agents" value={agentCount === undefined ? '…' : `${formatCount(agentCount)}${more}`} hint="Agents that are not archived" />
        <StatTile label="Spend this month" value={t ? formatUSD(t.costUsd) : '…'} hint={t ? `${formatShare(t.cacheReadShare)} of input tokens from the cache` : undefined} />
        <StatTile
          label="Runs today"
          value={runs ? formatCount(runs.runsToday) : '…'}
          hint={runs ? (runs.failedRunsToday > 0n ? <span className="text-destructive">{formatCount(runs.failedRunsToday)} failed</span> : 'None failed') : undefined}
        />
      </div>
      <QueryError error={list.error ?? undefined} />
      <section aria-labelledby="home-top-orgs" className="grid gap-3">
        <div className="flex items-baseline justify-between gap-3">
          <h2 id="home-top-orgs" className="text-lg font-semibold">
            Orgs with the highest spend
          </h2>
          <span className="flex gap-3 text-sm">
            <Link to="/platform/usage" className="text-link font-medium hover:underline">
              Usage and cost
            </Link>
            <Link to="/platform/orgs" className="text-link font-medium hover:underline">
              View all orgs
            </Link>
          </span>
        </div>
        <QueryError error={usage.error ?? undefined} />
        {usage.isPending ? (
          <SkeletonRows label="Loading spend…" />
        ) : top.length === 0 ? (
          !usage.error && <p className="text-muted-foreground text-sm">No spend this month yet.</p>
        ) : (
          <ol aria-labelledby="home-top-orgs" className="grid gap-2">
            {top.map((g) => {
              const o = summaries.get(g.key)
              return (
                <li key={g.key} className="bg-card flex flex-wrap items-center gap-3 rounded-lg border p-3">
                  <span className="grid min-w-0 flex-1">
                    <span className="truncate font-medium">{o ? orgSummaryName(o) : g.label}</span>
                    <span className="text-muted-foreground text-xs">
                      {formatUSD(g.costUsd)} this month{o && ` · ${roleText(o, memberships)}`}
                    </span>
                  </span>
                  {o && <OpenOrgButton org={o} />}
                </li>
              )
            })}
          </ol>
        )}
      </section>
    </div>
  )
}
