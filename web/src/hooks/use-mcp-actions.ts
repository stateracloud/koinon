// SPDX-License-Identifier: Apache-2.0

import { createClient } from '@connectrpc/connect'
import { createConnectQueryKey, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useMemo } from 'react'
import { McpServerService, type Tier } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import type { buildSink, CreateServerInput, UpdateServerInput } from '@/lib/mcp-servers'

// Plain client calls rather than useMutation, like useProviderActions: TanStack Query would keep the
// request, with its credential, in its mutation cache.

/** useMcpActions returns the write calls of McpServerService for one org; each refreshes the lists. */
export function useMcpActions(orgId: string) {
  const transport = useTransport()
  const queryClient = useQueryClient()
  return useMemo(() => {
    const client = createClient(McpServerService, transport)
    const refresh = () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: McpServerService, cardinality: undefined }) })
    // Refresh after a refusal too: a decision refused because the tool text changed (#1173) must show
    // the new text and its hash, or the next try sends the same stale hash.
    const done = async <T>(p: Promise<T>) => {
      try {
        return await p
      } finally {
        await refresh()
      }
    }
    return {
      createServer: (i: CreateServerInput) => done(client.createServer({ orgId, ...i })),
      updateServer: (serverId: string, i: UpdateServerInput) => done(client.updateServer({ orgId, serverId, ...i })),
      setServerClass: (serverId: string, kind: string, dataClass: string[], codeHostConnectionId = '') => done(client.setServerClass({ orgId, serverId, class: { kind, dataClass, codeHostConnectionId } })),
      deleteServer: (serverId: string) => done(client.deleteServer({ orgId, serverId })),
      // toolHash is the hash of the tool text the admin read (Tool.toolHash). The server refuses the
      // decision when the text changed since (#1173).
      setToolTier: (serverId: string, tool: string, tier: Tier, toolHash: string) => done(client.setToolTier({ orgId, serverId, tool, tier, toolHash })),
      setToolSink: (serverId: string, tool: string, contentHash: string, sink: ReturnType<typeof buildSink>) => done(client.setToolSink({ orgId, serverId, tool, contentHash, sink })),
      setToolTwoApprovers: (serverId: string, tool: string, required: boolean) => done(client.setToolTwoApprovers({ orgId, serverId, tool, required })),
      acknowledgeReview: (serverId: string, tool: string, toolHash: string) => done(client.acknowledgeToolReview({ orgId, serverId, tool, toolHash })),
      setAgentServers: (agentId: string, servers: { serverId: string; tools: string[] }[]) => done(client.setAgentServers({ orgId, agentId, servers })),
    }
  }, [transport, queryClient, orgId])
}

export type McpActions = ReturnType<typeof useMcpActions>
