// SPDX-License-Identifier: Apache-2.0

import { createClient } from '@connectrpc/connect'
import { useQuery, useTransport } from '@connectrpc/connect-query'
import { useMemo, useState } from 'react'
import { ExportUsageButton, UsageTable } from '@/components/usage-table'
import { QueryError } from '@/components/ui/inline-error'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { PageHeader } from '@/components/ui/page-header'
import { SkeletonRows } from '@/components/ui/skeleton'
import { StatTile } from '@/components/ui/stat-tile'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { UsageService } from '@/gen/koinon/usage/v1alpha1/usage_pb'
import { formatUSD } from '@/lib/cost'
import { formatCount, formatShare, groupUsage, monthLabel, monthRange, tokensOf, usageMonths } from '@/lib/usage'

/**
 * PlatformUsagePage is "Usage and cost" of the platform console (issue #1281, ADR 0126 section 4): the spend of every
 * org in one calendar month (UTC), by org, model and day, with a CSV export of the same rows. It reads
 * UsageService.GetPlatformUsage (issue #1268), which reads the spend ledger, not Prometheus.
 */
export function PlatformUsagePage() {
  const [months] = useState(() => usageMonths(new Date()))
  const [month, setMonth] = useState(months[0])
  const range = useMemo(() => monthRange(month), [month])
  const usage = useQuery(UsageService.method.getPlatformUsage, { range })
  const transport = useTransport()
  const rows = usage.data?.rows
  const byOrg = useMemo(() => groupUsage(rows ?? [], (r) => ({ key: r.orgId, label: r.orgName || r.orgSlug, detail: r.orgSlug })), [rows])
  const byModel = useMemo(() => groupUsage(rows ?? [], (r) => ({ key: r.model, label: r.model || 'Unknown model' })), [rows])
  const byDay = useMemo(() => groupUsage(rows ?? [], (r) => ({ key: r.day, label: r.day })), [rows])
  const t = usage.data?.totals
  return (
    <div className="mx-auto grid max-w-5xl grid-cols-[minmax(0,1fr)] gap-4">
      <PageHeader
        title="Usage and cost"
        description="The recorded spend of every org, from the spend ledger. Days and months are UTC."
        actions={<ExportUsageButton disabled={!usage.data} load={() => createClient(UsageService, transport).exportPlatformUsage({ range })} />}
      />
      <div className="grid max-w-xs gap-1.5">
        <Label htmlFor="usage-month">Month</Label>
        <NativeSelect id="usage-month" value={month} onChange={(e) => setMonth(e.target.value)}>
          {months.map((m) => (
            <option key={m} value={m}>
              {monthLabel(m)}
            </option>
          ))}
        </NativeSelect>
      </div>
      <QueryError error={usage.error ?? undefined} />
      {usage.isPending ? (
        <SkeletonRows label="Loading usage…" />
      ) : (
        t && (
          <>
            <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
              <StatTile label="Spend" value={formatUSD(t.costUsd)} hint={`${formatUSD(t.estimatedUsd)} of it estimated`} />
              <StatTile label="Requests" value={formatCount(t.requests)} />
              <StatTile label="Tokens" value={formatCount(tokensOf(t))} hint={`${formatCount(t.outputTokens)} output`} />
              <StatTile label="Cache read share" value={formatShare(t.cacheReadShare)} hint="Of all input tokens" />
            </div>
            <Tabs defaultValue="org">
              <TabsList aria-label="Group usage by">
                <TabsTrigger value="org">By org</TabsTrigger>
                <TabsTrigger value="model">By model</TabsTrigger>
                <TabsTrigger value="day">By day</TabsTrigger>
              </TabsList>
              <TabsContent value="org">
                <UsageTable caption="Usage by org" name="Org" groups={byOrg} />
              </TabsContent>
              <TabsContent value="model">
                <UsageTable caption="Usage by model" name="Model" groups={byModel} />
              </TabsContent>
              <TabsContent value="day">
                <UsageTable caption="Usage by day" name="Day" groups={byDay} sortByName />
              </TabsContent>
            </Tabs>
          </>
        )
      )}
    </div>
  )
}
