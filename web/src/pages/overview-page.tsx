// SPDX-License-Identifier: Apache-2.0

import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { BotIcon } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { AgentCard } from '@/components/agent-card'
import { AgentChart } from '@/components/agent-chart'
import { WaitingStrip } from '@/components/waiting-strip'
import { Button } from '@/components/ui/button'
import { EmptyState } from '@/components/ui/empty-state'
import { QueryError } from '@/components/ui/inline-error'
import { PageHeader } from '@/components/ui/page-header'
import { SkeletonRows } from '@/components/ui/skeleton'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { useAgentSummaries } from '@/hooks/use-agent-summaries'
import { useInbox } from '@/hooks/use-inbox'
import { orgName, useOrg } from '@/lib/org-context'
import { groupByTeam, parseOverviewTab } from '@/lib/overview'
import { BoardView } from '@/pages/board-view'

/**
 * OverviewPage is the org home at "/" (ADR 0126 section 4): the "Waiting on you" strip, then the Agents tab (cards by
 * team, the default), the Chart tab (reports-to trees) and the Board tab (the task board). The Agents tab is also the
 * agent list: /agents redirects here.
 */
export function OverviewPage() {
  const tab = parseOverviewTab(useSearch({ strict: false }).tab) ?? 'agents'
  const { org } = useOrg()
  const navigate = useNavigate()
  const { items } = useInbox()
  return (
    <div className="mx-auto grid max-w-6xl gap-4">
      <PageHeader title="Overview" description={orgName(org)} actions={<NewAgentButton />} />
      <WaitingStrip items={items} />
      <Tabs
        value={tab}
        onValueChange={(v) => {
          const next = parseOverviewTab(v)
          if (next) void navigate({ to: '/', search: next === 'agents' ? {} : { tab: next }, replace: true })
        }}
      >
        <TabsList aria-label="Overview">
          <TabsTrigger value="agents">Agents</TabsTrigger>
          <TabsTrigger value="chart">Chart</TabsTrigger>
          <TabsTrigger value="board">Board</TabsTrigger>
        </TabsList>
        <TabsContent value="agents">
          <AgentsTab />
        </TabsContent>
        <TabsContent value="chart">
          <ChartTab />
        </TabsContent>
        <TabsContent value="board">
          <BoardView />
        </TabsContent>
      </Tabs>
    </div>
  )
}

/** NewAgentButton opens the new agent flow; it does not show for a role that cannot create an agent. */
export function NewAgentButton() {
  const { org } = useOrg()
  if (org.role < OrgRole.MEMBER) return null
  return (
    <Button asChild size="sm">
      <Link to="/agents/new">New agent</Link>
    </Button>
  )
}

function NoAgents() {
  const { org } = useOrg()
  const canCreate = org.role >= OrgRole.MEMBER
  return <EmptyState icon={BotIcon} title="No agents yet." description={canCreate ? 'Create an agent to give it work.' : 'An org admin creates the agents of this org.'} />
}

/** useNow is the clock of the relative times on the cards, one tick a minute. */
function useNow() {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 60_000)
    return () => clearInterval(id)
  }, [])
  return now
}

/** AgentsTab is the agent cards grouped by team, one server page at a time ("Show more agents"). */
function AgentsTab() {
  const { org } = useOrg()
  const list = useAgentSummaries(org.orgId)
  const teams = useMemo(() => groupByTeam(list.agents ?? []), [list.agents])
  const now = useNow()
  return (
    <div className="grid gap-6">
      <QueryError error={list.isError ? list.error : undefined} />
      {list.isPending ? (
        <SkeletonRows label="Loading agents…" />
      ) : list.isError && !list.agents ? null : teams.length === 0 ? (
        <NoAgents />
      ) : (
        teams.map((t, i) => (
          <section key={t.team} aria-labelledby={`team-${t.team === '' ? 'none' : encodeURIComponent(t.team)}`} className="grid gap-3">
            <h2 id={`team-${t.team === '' ? 'none' : encodeURIComponent(t.team)}`} className="text-sm font-semibold">
              {t.label} <span className="text-muted-foreground font-normal">· {teamCount(t.agents.length, list.hasMore && i === teams.length - 1)}</span>
            </h2>
            <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
              {t.agents.map((a) => (
                <AgentCard key={a.agentId} agent={a} now={now} />
              ))}
            </div>
          </section>
        ))
      )}
      {list.hasMore && !list.isError && (
        <Button variant="outline" size="sm" className="w-fit" disabled={list.isFetchingMore} onClick={list.loadMore}>
          {list.isFetchingMore ? 'Loading more agents…' : 'Show more agents'}
        </Button>
      )}
      {list.isError && list.agents && <p className="text-muted-foreground text-sm">This list may be incomplete.</p>}
    </div>
  )
}

/**
 * teamCount is the size of a team heading. The server orders by team with "No team" last, so while pages remain only
 * the last group can be partial: it says "so far".
 */
function teamCount(n: number, partial: boolean) {
  return `${n === 1 ? '1 agent' : `${n} agents`}${partial ? ' so far' : ''}`
}

/** ChartTab is the reports-to trees. It loads every page first: a tree with a missing page would put agents in the wrong place. */
function ChartTab() {
  const { org } = useOrg()
  const list = useAgentSummaries(org.orgId, { all: true })
  const teams = useMemo(() => groupByTeam(list.agents ?? []), [list.agents])
  // Wait for every page only on the first load. A later poll can find one more page (an agent added earlier in the
  // order); the chart then stays, with its open branches and its focus, while that page loads.
  const [loaded, setLoaded] = useState(false)
  const complete = !list.isPending && !list.hasMore
  if (complete && !loaded) setLoaded(true)
  return (
    <div className="grid gap-4">
      <QueryError error={list.isError ? list.error : undefined} />
      {!loaded && !list.isError ? (
        <SkeletonRows label="Loading agents…" />
      ) : list.isError && !list.agents ? null : teams.length === 0 ? (
        <NoAgents />
      ) : (
        <>
          <p className="text-muted-foreground text-sm">
            Each team is a tree of who reports to whom. Use the arrow keys to move and to open or close a branch, and Enter to open an agent.
          </p>
          <AgentChart teams={teams} />
        </>
      )}
      {list.isError && list.agents && <p className="text-muted-foreground text-sm">This chart may be incomplete.</p>}
    </div>
  )
}
