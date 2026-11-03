// SPDX-License-Identifier: Apache-2.0

import { createClient } from '@connectrpc/connect'
import { createConnectQueryKey, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useMemo } from 'react'
import { McpServerService, type Tier } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import type { CreateServerInput, UpdateServerInput } from '@/lib/mcp-servers'

// Plain client calls rather than useMutation, like useProviderActions: TanStack Query would keep the
// request, with its credential, in its mutation cache.

/** useMcpActions returns the write calls of McpServerService for one org; each refreshes the lists. */
export function useMcpActions(orgId: string) {
  const transport = useTransport()
  const queryClient = useQueryClient()
  return useMemo(() => {
    const client = createClient(McpServerService, transport)
    const refresh = () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: McpServerService, cardinality: undefined }) })
    const done = async <T>(p: Promise<T>) => {
      const res = await p
      await refresh()
      return res
    }
    return {
      createServer: (i: CreateServerInput) => done(client.createServer({ orgId, ...i })),
      updateServer: (serverId: string, i: UpdateServerInput) => done(client.updateServer({ orgId, serverId, ...i })),
      setServerClass: (serverId: string, kind: string, dataClass: string[]) => done(client.setServerClass({ orgId, serverId, class: { kind, dataClass } })),
      deleteServer: (serverId: string) => done(client.deleteServer({ orgId, serverId })),
      setToolTier: (serverId: string, tool: string, tier: Tier) => done(client.setToolTier({ orgId, serverId, tool, tier })),
      setToolTwoApprovers: (serverId: string, tool: string, required: boolean) => done(client.setToolTwoApprovers({ orgId, serverId, tool, required })),
      acknowledgeReview: (serverId: string, tool: string) => done(client.acknowledgeToolReview({ orgId, serverId, tool })),
      setAgentServers: (agentId: string, servers: { serverId: string; tools: string[] }[]) => done(client.setAgentServers({ orgId, agentId, servers })),
    }
  }, [transport, queryClient, orgId])
}

export type McpActions = ReturnType<typeof useMcpActions>
