// SPDX-License-Identifier: Apache-2.0

import { useInfiniteQuery } from '@connectrpc/connect-query'
import { useEffect, useMemo } from 'react'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { inboxPollMs } from '@/hooks/use-inbox'

/** The page size of ListAgentSummaries on the org home (the server allows at most 200). */
export const agentSummaryPageSize = 100

/**
 * useAgentSummaries reads the org's agent cards page by page (ListAgentSummaries, ADR 0126 section 4). The Agents tab
 * asks for the next page with `loadMore`; the Chart tab passes `all` because a reports-to tree needs every agent of a
 * team. Both tabs share one query, so a switch of tab does not load the pages again. The cards poll like the inbox:
 * "running now" and "waiting on you" change without a reload.
 */
export function useAgentSummaries(orgId: string, options: { all?: boolean } = {}) {
  const list = useInfiniteQuery(
    AgentService.method.listAgentSummaries,
    { orgId, pageSize: agentSummaryPageSize, pageToken: '' },
    {
      pageParamKey: 'pageToken',
      getNextPageParam: (last) => last.nextPageToken || undefined,
      refetchInterval: inboxPollMs,
    },
  )
  const { hasNextPage, isFetching, fetchNextPage, isError } = list
  const all = options.all ?? false
  useEffect(() => {
    if (all && hasNextPage && !isFetching && !isError) void fetchNextPage({ cancelRefetch: false })
  }, [all, hasNextPage, isFetching, isError, fetchNextPage])

  const pages = list.data?.pages
  const agents = useMemo(() => pages?.flatMap((p) => p.agents), [pages])
  return {
    agents,
    isPending: list.isPending,
    isError: list.isError,
    error: list.error,
    hasMore: Boolean(hasNextPage),
    isFetchingMore: list.isFetchingNextPage,
    loadMore: () => void fetchNextPage({ cancelRefetch: false }),
  }
}
