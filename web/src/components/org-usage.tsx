// SPDX-License-Identifier: Apache-2.0

import { createClient } from '@connectrpc/connect'
import { useQuery, useTransport } from '@connectrpc/connect-query'
import { useMemo } from 'react'
import { ExportUsageButton, UsageTable } from '@/components/usage-table'
import { QueryError } from '@/components/ui/inline-error'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { UsageService, type OrgUsageRow } from '@/gen/koinon/usage/v1alpha1/usage_pb'
import { formatUSD } from '@/lib/cost'
import { formatShare, groupUsage } from '@/lib/usage'

/** agentKey groups an org row by its agent; the org's own system checks have no agent. */
function agentKey(r: OrgUsageRow) {
  return r.principal === 'system' || !r.agentId ? { key: 'system', label: 'System checks' } : { key: r.agentId, label: r.agentName || r.agentId.slice(0, 8) }
}

/**
 * OrgUsage is the org's own slice of "Usage and cost" on its Budgets page (issue #1281, ADR 0126 section 4): the
 * spend of this month (UTC) by agent, model, provider and day, with a CSV export of the same rows. It reads
 * UsageService.GetOrgUsage with no range, which is the month to date.
 */
export function OrgUsage({ orgId }: { orgId: string }) {
  const usage = useQuery(UsageService.method.getOrgUsage, { orgId })
  const transport = useTransport()
  const rows = usage.data?.rows
  const byAgent = useMemo(() => groupUsage(rows ?? [], agentKey), [rows])
  const byModel = useMemo(() => groupUsage(rows ?? [], (r) => ({ key: r.model, label: r.model || 'Unknown model' })), [rows])
  const byProvider = useMemo(() => groupUsage(rows ?? [], (r) => ({ key: r.providerId, label: r.providerName || (r.providerId ? r.providerId.slice(0, 8) : 'Not recorded') })), [rows])
  const byDay = useMemo(() => groupUsage(rows ?? [], (r) => ({ key: r.day, label: r.day })), [rows])
  const t = usage.data?.totals
  return (
    <section aria-labelledby="org-usage" className="grid gap-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <h2 id="org-usage" className="text-lg font-semibold">
          Usage this month
        </h2>
        <ExportUsageButton disabled={!usage.data} load={() => createClient(UsageService, transport).exportOrgUsage({ orgId })} />
      </div>
      <QueryError error={usage.error ?? undefined} />
      {usage.isPending ? (
        <SkeletonRows label="Loading usage…" />
      ) : (
        t && (
          <>
            <p className="text-muted-foreground text-sm">
              {formatUSD(t.costUsd)} in {t.requests.toString()} {t.requests === 1n ? 'request' : 'requests'}. {formatShare(t.cacheReadShare)} of the input tokens came from the cache.
            </p>
            <Tabs defaultValue="agent">
              <TabsList aria-label="Group usage by">
                <TabsTrigger value="agent">By agent</TabsTrigger>
                <TabsTrigger value="model">By model</TabsTrigger>
                <TabsTrigger value="provider">By provider</TabsTrigger>
                <TabsTrigger value="day">By day</TabsTrigger>
              </TabsList>
              <TabsContent value="agent">
                <UsageTable caption="Usage by agent" name="Agent" groups={byAgent} />
              </TabsContent>
              <TabsContent value="model">
                <UsageTable caption="Usage by model" name="Model" groups={byModel} />
              </TabsContent>
              <TabsContent value="provider">
                <UsageTable caption="Usage by provider" name="Provider" groups={byProvider} />
              </TabsContent>
              <TabsContent value="day">
                <UsageTable caption="Usage by day" name="Day" groups={byDay} sortByName />
              </TabsContent>
            </Tabs>
          </>
        )
      )}
    </section>
  )
}
