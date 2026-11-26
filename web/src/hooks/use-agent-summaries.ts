// SPDX-License-Identifier: Apache-2.0

import type { Transport } from '@connectrpc/connect'
import { type InfiniteData, useQueryClient } from '@tanstack/react-query'
import { createConnectQueryKey, useInfiniteQuery, useQuery, useTransport } from '@connectrpc/connect-query'
import { useEffect, useMemo, useState } from 'react'
import { AgentService, type AgentSummary, type ListAgentSummariesResponse } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { inboxPollMs } from '@/hooks/use-inbox'

/** The page size of ListAgentSummaries on the org home (the server allows at most 200). */
export const agentSummaryPageSize = 100

/**
 * useAgentSummaries reads the org's agent cards page by page (ListAgentSummaries, ADR 0126 section 4). The Agents tab
 * asks for the next page with `loadMore`; the Chart tab passes `all` because a reports-to tree needs every agent of a
 * team. Both tabs share the queries, so a switch of tab does not start from the first page again.
 *
 * Only the first page polls (like the inbox: "running now" and "waiting on you" change without a reload), so one poll
 * is one request whatever the number of loaded pages (issue #1411). The later pages are one infinite query that starts
 * at the first page's next_page_token and does not poll. They load again on demand: when the view mounts, when an
 * AgentService change invalidates them, and when the first page's next_page_token changes (an agent added or removed
 * early in the order: the token is a cursor after the first page's last agent). Then the same number of later pages
 * loads again from the new token, with the old pages on screen until it ends.
 */
export function useAgentSummaries(orgId: string, options: { all?: boolean } = {}) {
  const all = options.all ?? false
  const queryClient = useQueryClient()
  const transport = useTransport()
  const first = useQuery(
    AgentService.method.listAgentSummaries,
    { orgId, pageSize: agentSummaryPageSize, pageToken: '' },
    { refetchInterval: inboxPollMs },
  )
  const restToken = first.data?.nextPageToken ?? ''
  // want is the number of pages after the first that this view asked for.
  const [want, setWant] = useState(0)
  // A later-pages query that follows an older first page stays off until it points at the new token (below), so a
  // mount does not refetch it from the old cursor first.
  const restData = queryClient.getQueryData<InfiniteData<ListAgentSummariesResponse, string>>(restKey(transport, orgId))
  const movedInCache = restToken !== '' && restData !== undefined && restData.pageParams[0] !== restToken
  const restEnabled = restToken !== '' && (all || want > 0 || restData !== undefined) && !movedInCache
  const rest = useInfiniteQuery(AgentService.method.listAgentSummaries, restInput(orgId, restToken), {
    pageParamKey: 'pageToken',
    getNextPageParam: (last) => last.nextPageToken || undefined,
    enabled: restEnabled,
    // The first page polls; the later pages load again only on demand.
    refetchOnWindowFocus: false,
    // After a cursor move (below), turning the query on again refetches it only because it is stale at once.
    staleTime: 0,
  })
  const restPages = restToken === '' ? undefined : rest.data?.pages
  const loaded = restPages?.length ?? 0
  // The later pages follow an older first page: the infinite query key leaves the page token out, so a new token is
  // the same query with the old start in pageParams.
  const moved = restPages !== undefined && rest.data?.pageParams[0] !== restToken

  const { hasNextPage, isFetching, fetchNextPage, isError, isFetchNextPageError, refetch } = rest
  const firstFetching = first.isFetching
  useEffect(() => {
    // Wait for the first page too: an invalidation refetches both, and the first page can move the token again.
    if (!moved || isFetching || firstFetching) return
    // A refetch of an infinite query starts at pageParams[0] and loads as many pages as it holds. The new start turns
    // the query on again (restEnabled), and the stale query then refetches once.
    queryClient.setQueryData<InfiniteData<ListAgentSummariesResponse, string>>(restKey(transport, orgId), (old) =>
      old ? { ...old, pageParams: [restToken, ...old.pageParams.slice(1)] } : old,
    )
  }, [moved, isFetching, firstFetching, restToken, orgId, transport, queryClient])

  // The later pages do not poll, so each poll of the first page also retries a failed later page with one request: the
  // next page when only that failed, else the first later page alone (the pages after it then load one by one again,
  // so a page that keeps failing costs one request per poll).
  const firstUpdatedAt = first.dataUpdatedAt
  useEffect(() => {
    if (!restEnabled || !isError || isFetching) return
    if (isFetchNextPageError) {
      void fetchNextPage({ cancelRefetch: false })
      return
    }
    queryClient.setQueryData<InfiniteData<ListAgentSummariesResponse, string>>(restKey(transport, orgId), (old) =>
      old ? { pages: old.pages.slice(0, 1), pageParams: old.pageParams.slice(0, 1) } : old,
    )
    void refetch()
    // Only a new first page starts a retry, not a change of the error state.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [firstUpdatedAt])

  // A view that mounts with later pages in the cache (from the other tab) keeps them.
  const target = all ? Infinity : Math.max(want, loaded)
  useEffect(() => {
    if (restEnabled && !moved && loaded > 0 && loaded < target && hasNextPage && !isFetching && !isError) {
      void fetchNextPage({ cancelRefetch: false })
    }
  }, [restEnabled, moved, loaded, target, hasNextPage, isFetching, isError, fetchNextPage])

  const firstAgents = first.data?.agents
  // anchor is the first page that the loaded later pages follow. After a cursor move its last agents are on neither the
  // new first page nor the old later pages, so they show from the anchor until the later pages load again.
  const [anchor, setAnchor] = useState<{ token: string; agents: AgentSummary[] }>()
  if (restPages && !moved && !isFetching && firstAgents && (anchor?.token !== restToken || anchor.agents !== firstAgents)) {
    setAnchor({ token: restToken, agents: firstAgents })
  }
  const gap = restPages && anchor && anchor.token !== restToken ? anchor.agents : noAgents
  const agents = useMemo(
    () => (firstAgents ? dedupe([firstAgents, gap, ...(restPages ?? []).map((p) => p.agents)]) : undefined),
    [firstAgents, gap, restPages],
  )
  const restError = restEnabled && isError
  return {
    agents,
    isPending: first.isPending,
    isError: first.isError || restError,
    error: first.error ?? (restError ? rest.error : null),
    hasMore: restToken !== '' && (restPages === undefined || moved || Boolean(hasNextPage)),
    isFetchingMore: rest.isLoading || rest.isFetchingNextPage || (want > loaded && isFetching),
    loadMore: () => setWant((w) => Math.max(w, loaded) + 1),
  }
}

const noAgents: AgentSummary[] = []

function restInput(orgId: string, pageToken: string) {
  return { orgId, pageSize: agentSummaryPageSize, pageToken }
}

/** restKey is the key of the later pages' infinite query. It leaves the page token out, as connect-query does. */
function restKey(transport: Transport, orgId: string) {
  return createConnectQueryKey({
    schema: AgentService.method.listAgentSummaries,
    input: restInput(orgId, ''),
    transport,
    cardinality: 'infinite',
    pageParamKey: 'pageToken',
  })
}

/**
 * dedupe joins the pages and keeps the first card of each agent. A later page can repeat an agent of the first page
 * until the later pages load again from a new first-page token.
 */
function dedupe(pages: AgentSummary[][]) {
  const seen = new Set<string>()
  return pages.flat().filter((a) => !seen.has(a.agentId) && seen.add(a.agentId))
}
