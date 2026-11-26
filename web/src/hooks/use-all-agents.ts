// SPDX-License-Identifier: Apache-2.0

import { useInfiniteQuery } from '@connectrpc/connect-query'
import { useEffect, useMemo } from 'react'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'

/**
 * useAllAgents reads every page of ListAgents (500 agents per page) and returns the agents of all
 * pages that have loaded. `data` is set after the first page; the rest of the pages follow at once,
 * so an org with more than 500 agents is complete when `isFetchingMore` is false.
 */
export function useAllAgents(orgId: string, options: { refetchInterval?: number } = {}) {
  const list = useInfiniteQuery(
    AgentService.method.listAgents,
    { orgId, pageToken: '' },
    {
      pageParamKey: 'pageToken',
      getNextPageParam: (last) => last.nextPageToken || undefined,
      refetchInterval: options.refetchInterval,
    },
  )
  // Two observers (the inbox page and the badge) share one query: cancelRefetch false keeps the second one from repeating the fetch.
  const { hasNextPage, isFetching, fetchNextPage, isError } = list
  useEffect(() => {
    // Run again when any fetch ends: fetchNextPage does nothing while another fetch (a refetch after an
    // invalidation, for example) is in flight, and no other dependency would change after it.
    if (hasNextPage && !isFetching && !isError) void fetchNextPage({ cancelRefetch: false })
  }, [hasNextPage, isFetching, isError, fetchNextPage])

  const pages = list.data?.pages
  const data = useMemo(() => (pages ? { agents: pages.flatMap((p) => p.agents) } : undefined), [pages])
  return {
    data,
    isPending: list.isPending,
    isError: list.isError,
    error: list.error,
    isFetchingMore: Boolean(hasNextPage) && !isError,
  }
}
