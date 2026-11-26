// SPDX-License-Identifier: Apache-2.0

import { useQuery } from '@connectrpc/connect-query'
import { Link } from '@tanstack/react-router'
import { ServerIcon } from 'lucide-react'
import { useMemo } from 'react'
import { LeakGuardSettings } from '@/components/leak-guard-settings'
import { OutwardSwitch } from '@/components/outward-switch'
import { RequireAdmin } from '@/components/require-admin'
import { HealthBadge, ServerStateBadges } from '@/components/server-tools'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { DataTable } from '@/components/ui/data-table'
import { EmptyState } from '@/components/ui/empty-state'
import { QueryError } from '@/components/ui/inline-error'
import { PageHeader } from '@/components/ui/page-header'
import { SkeletonRows } from '@/components/ui/skeleton'
import { McpServerService, type Server, type Tool } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import { OutwardTargetKind } from '@/gen/koinon/outward/v1alpha1/outward_pb'
import { dataTableColumns, type DataTableColumn } from '@/lib/data-table'
import { sourceLabel, summarizeTools, summaryText, tierExplanation, toolsOf, waitingNotice } from '@/lib/mcp-servers'
import { useOrg } from '@/lib/org-context'
import { availabilityMinutes } from '@/lib/providers'

const col = dataTableColumns<ServerRow>()

/** ServerRow is one server of the list with the tools that belong to it. */
type ServerRow = { server: Server; tools: Tool[] | undefined }

/** ServersPage lists the org's remote MCP servers (ADR 0057, ADR 0126 section 6). Org admins and owners only. */
export function ServersPage() {
  return (
    <div className="mx-auto grid max-w-5xl grid-cols-[minmax(0,1fr)] gap-4">
      <RequireAdmin what="remote MCP servers">
        <ServersContent />
      </RequireAdmin>
    </div>
  )
}

function ServersContent() {
  const { org } = useOrg()
  const orgId = org.orgId
  const servers = useQuery(McpServerService.method.listServers, { orgId })
  const tools = useQuery(McpServerService.method.listTools, { orgId, serverId: '' })
  const rows = useMemo<ServerRow[]>(() => {
    const all = tools.data?.tools
    return (servers.data?.servers ?? []).map((server) => ({ server, tools: all && toolsOf(all, server.id) }))
  }, [servers.data, tools.data])

  const columns = useMemo<DataTableColumn<ServerRow>[]>(
    () => [
      col.accessor((r) => r.server.name, {
        id: 'name',
        header: 'Name',
        sortFn: 'text',
        meta: { rowHeader: true },
        cell: (c) => (
          <Link to="/mcp-servers/$serverId" params={{ serverId: c.row.original.server.id }} className="font-medium underline-offset-4 hover:underline">
            {c.getValue()}
          </Link>
        ),
      }),
      col.display({
        id: 'state',
        header: 'State',
        cell: (c) => (
          <div className="flex flex-wrap gap-1">
            <HealthBadge health={c.row.original.server.health} />
            <ServerStateBadges server={c.row.original.server} />
          </div>
        ),
      }),
      col.display({
        id: 'tools',
        header: 'Tools',
        cell: (c) => {
          const { server, tools: list } = c.row.original
          // Without the tool list, a count would be wrong: the error above the table says why it is missing.
          if (!list) return null
          const waiting = waitingNotice(server, list)
          return (
            <div className="grid gap-1">
              <span className="text-sm">{summaryText(summarizeTools(list))}</span>
              {waiting && (
                // The review is on the server page ("Review all" or one tool at a time, ADR 0125 decision 10).
                <Badge asChild variant="outline" className="w-fit">
                  <Link to="/mcp-servers/$serverId" params={{ serverId: server.id }} aria-label={`${waiting}: review the tools of ${server.name}`}>
                    {waiting}
                  </Link>
                </Badge>
              )}
            </div>
          )
        },
      }),
      col.accessor((r) => sourceLabel(r.server), { id: 'source', header: 'Source', sortFn: 'text' }),
      col.display({
        id: 'outward',
        header: 'Outward calls',
        cell: (c) => {
          const s = c.row.original.server
          return <OutwardSwitch compact kind={OutwardTargetKind.MCP_SERVER} targetId={s.id} subject={`server ${s.name}`} disabled={s.adminDisabled} />
        },
      }),
    ],
    [],
  )

  const failed = servers.error ?? tools.error
  return (
    <>
      <PageHeader
        title="MCP servers"
        description={
          <>
            <p>
              A server is a remote MCP server your agents call through the gateway, such as a docs or issue-tracker server. Koinon keeps its credential encrypted and gives it only to the gateway; sandboxes never see it. A new or changed credential reaches the gateway in about {availabilityMinutes} minutes.
            </p>
            <p className="mt-1">
              {tierExplanation} A new tool is hidden from agents until an admin reviews its text. Switch off outward calls to stop all calls to a server at once; write the reason for the audit log.
            </p>
          </>
        }
        actions={
          <Button asChild size="sm">
            <Link to="/mcp-servers/new">Connect a remote server</Link>
          </Button>
        }
      />
      <QueryError error={failed ?? undefined} />
      {servers.isPending || tools.isPending ? (
        <SkeletonRows label="Loading servers…" />
      ) : failed && !servers.data ? null : (
        <DataTable
          caption="MCP servers"
          columns={columns}
          data={rows}
          getRowId={(r) => r.server.id}
          initialSorting={[{ id: 'name', desc: false }]}
          empty={<EmptyState icon={ServerIcon} title="No servers yet." description="Connect a remote MCP server so agents can use its tools." />}
        />
      )}
      <LeakGuardSettings orgId={orgId} />
    </>
  )
}
