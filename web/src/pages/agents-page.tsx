// SPDX-License-Identifier: Apache-2.0

import { Link } from '@tanstack/react-router'
import { BotIcon } from 'lucide-react'
import { AgentStatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { DataTable } from '@/components/ui/data-table'
import { EmptyState } from '@/components/ui/empty-state'
import { QueryError } from '@/components/ui/inline-error'
import { PageHeader } from '@/components/ui/page-header'
import { SkeletonRows } from '@/components/ui/skeleton'
import { useAllAgents } from '@/hooks/use-all-agents'
import type { Agent } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { dataTableColumns, type DataTableColumn } from '@/lib/data-table'
import { harnessLabel } from '@/lib/labels'
import { useOrg } from '@/lib/org-context'

const col = dataTableColumns<Agent>()
const columns: DataTableColumn<Agent>[] = [
  col.accessor('displayName', {
    header: 'Name',
    sortFn: 'text',
    meta: { rowHeader: true },
    cell: (c) => (
      <Link to="/agents/$agentId" params={{ agentId: c.row.original.id }} className="text-link hover:underline">
        {c.getValue()}
      </Link>
    ),
  }),
  col.accessor((a) => harnessLabel(a.harness), { id: 'harness', header: 'Harness', sortFn: 'text', meta: { className: 'text-muted-foreground' } }),
  col.display({
    id: 'status',
    header: 'Status',
    cell: (c) => (
      <span className="flex flex-wrap items-center gap-2">
        {c.row.original.unhealthyAt && <AgentStatusBadge status={c.row.original.status} unhealthy />}
        <AgentStatusBadge status={c.row.original.status} />
      </span>
    ),
  }),
]

/** AgentsPage lists the org's agents. */
export function AgentsPage() {
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <PageHeader title="Agents" actions={<NewAgentButton />} />
      <AgentsList />
    </div>
  )
}

/** NewAgentButton opens the new agent form; it does not show for a role that cannot create an agent. */
export function NewAgentButton() {
  const { org } = useOrg()
  if (org.role < OrgRole.MEMBER) return null
  return (
    <Button asChild size="sm">
      <Link to="/agents/new">New agent</Link>
    </Button>
  )
}

/** AgentsList is the table of the org's agents, on the Agents page and the Agents tab of the overview. */
export function AgentsList() {
  const { org } = useOrg()
  const list = useAllAgents(org.orgId)
  const canCreate = org.role >= OrgRole.MEMBER
  return (
    <div className="grid gap-4">
      <QueryError error={list.isError ? list.error : undefined} />
      {list.isPending ? (
        <SkeletonRows label="Loading agents…" />
      ) : list.isError && !list.data ? null : (
        <DataTable
          caption="Agents"
          columns={columns}
          data={list.data?.agents ?? []}
          getRowId={(a) => a.id}
          initialSorting={[{ id: 'displayName', desc: false }]}
          empty={<EmptyState icon={BotIcon} title="No agents yet." description={canCreate ? 'Create an agent to give it work.' : 'An org admin creates the agents of this org.'} />}
        />
      )}
      {list.isFetchingMore && <p role="status" className="text-muted-foreground text-sm">Loading more agents…</p>}
      {list.isError && list.data && <p className="text-muted-foreground text-sm">This list may be incomplete.</p>}
    </div>
  )
}
