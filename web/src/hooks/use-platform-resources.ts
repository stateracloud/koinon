// SPDX-License-Identifier: Apache-2.0

import { useInfiniteQuery } from '@connectrpc/connect-query'
import { useEffect, useMemo } from 'react'
import {
  PlatformResourceService,
  type ListMcpServersResponse,
  type ListProvidersResponse,
  type ListTemplatesResponse,
} from '@/gen/koinon/platform/v1alpha1/resources_pb'

/** The number of orgs a page covers: the server maximum, so the console needs few requests. */
const orgsPerPage = 100

const pageOptions = {
  pageParamKey: 'pageToken',
  getNextPageParam: (last: { nextPageToken: string }) => last.nextPageToken || undefined,
} as const

type PagedList<P> = {
  data?: { pages: P[] }
  isPending: boolean
  error: Error | null
  isError: boolean
  isFetching: boolean
  hasNextPage: boolean
  fetchNextPage: (o: { cancelRefetch: boolean }) => Promise<unknown>
}

/**
 * useEveryPage reads the next page of a list until there is none, and returns the rows of the pages that have loaded.
 * The list is complete when `isFetchingMore` is false.
 */
function useEveryPage<P, T>(list: PagedList<P>, rowsOf: (page: P) => T[]) {
  const { hasNextPage, isFetching, fetchNextPage, isError } = list
  useEffect(() => {
    if (hasNextPage && !isFetching && !isError) void fetchNextPage({ cancelRefetch: false })
  }, [hasNextPage, isFetching, isError, fetchNextPage])
  const pages = list.data?.pages
  const rows = useMemo(() => pages?.flatMap(rowsOf), [pages, rowsOf])
  return {
    rows,
    isPending: list.isPending,
    error: list.error,
    isFetchingMore: hasNextPage && !isError,
  }
}

// Stable row readers, so the rows are not rebuilt on each render.
const providerRows = (p: ListProvidersResponse) => p.providers
const templateRows = (p: ListTemplatesResponse) => p.templates
const serverRows = (p: ListMcpServersResponse) => p.servers

/** usePlatformProviders reads the providers of every team org, or of the one org that orgId names. */
export function usePlatformProviders(orgId: string) {
  const list = useInfiniteQuery(PlatformResourceService.method.listProviders, { orgId, pageSize: orgsPerPage, pageToken: '' }, pageOptions)
  return useEveryPage(list, providerRows)
}

/** usePlatformTemplates reads the platform templates and the templates of every team org, or the templates of the one org that orgId names. */
export function usePlatformTemplates(orgId: string) {
  const list = useInfiniteQuery(PlatformResourceService.method.listTemplates, { orgId, pageSize: orgsPerPage, pageToken: '' }, pageOptions)
  return useEveryPage(list, templateRows)
}

/** usePlatformMcpServers reads the remote MCP servers of every team org, or of the one org that orgId names. */
export function usePlatformMcpServers(orgId: string) {
  const list = useInfiniteQuery(PlatformResourceService.method.listMcpServers, { orgId, pageSize: orgsPerPage, pageToken: '' }, pageOptions)
  return useEveryPage(list, serverRows)
}
