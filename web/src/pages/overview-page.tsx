// SPDX-License-Identifier: Apache-2.0

import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { PageHeader } from '@/components/ui/page-header'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { useInbox } from '@/hooks/use-inbox'
import { orgName, useOrg } from '@/lib/org-context'
import { parseOverviewTab } from '@/lib/overview'
import { AgentsList, NewAgentButton } from '@/pages/agents-page'
import { BoardView } from '@/pages/board-view'

/**
 * OverviewPage is the org home at "/" (ADR 0126 section 4). This first version has the Agents and Board tabs and a
 * link to the inbox; the agent cards, the Chart tab and the "Waiting on you" strip come with issue #1274.
 */
export function OverviewPage() {
  const tab = parseOverviewTab(useSearch({ strict: false }).tab) ?? 'agents'
  const { org } = useOrg()
  const navigate = useNavigate()
  const waiting = useInbox().items.length
  return (
    <div className="mx-auto grid max-w-5xl gap-4">
      <PageHeader title="Overview" description={orgName(org)} actions={<NewAgentButton />} />
      <p className="text-sm">
        {waiting === 0 ? 'Nothing waits on you.' : waiting === 1 ? '1 item waits on you.' : `${waiting} items wait on you.`}{' '}
        <Link to="/inbox" className="text-link underline">
          Open inbox
        </Link>
      </p>
      <Tabs
        value={tab}
        onValueChange={(v) => {
          const next = parseOverviewTab(v)
          if (next) void navigate({ to: '/', search: next === 'agents' ? {} : { tab: next }, replace: true })
        }}
      >
        <TabsList aria-label="Overview">
          <TabsTrigger value="agents">Agents</TabsTrigger>
          <TabsTrigger value="board">Board</TabsTrigger>
        </TabsList>
        <TabsContent value="agents">
          <AgentsList />
        </TabsContent>
        <TabsContent value="board">
          <BoardView />
        </TabsContent>
      </Tabs>
    </div>
  )
}
