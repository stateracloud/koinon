// SPDX-License-Identifier: Apache-2.0

import { useInfiniteQuery } from '@connectrpc/connect-query'
import { useEffect, useMemo } from 'react'
import { PlatformService } from '@/gen/koinon/platform/v1alpha1/platform_pb'

/**
 * useAllOrgs reads every page of PlatformService.ListOrgs (200 orgs per page, the server maximum) and returns the
 * team orgs of all pages that have loaded. The list is complete when `isFetchingMore` is false. Personal orgs are
 * left out until v0.2 (ADR 0126 section 4).
 */
export function useAllOrgs() {
  const list = useInfiniteQuery(
    PlatformService.method.listOrgs,
    { pageSize: 200, pageToken: '' },
    {
      pageParamKey: 'pageToken',
      getNextPageParam: (last) => last.nextPageToken || undefined,
    },
  )
  const { hasNextPage, isFetching, fetchNextPage, isError } = list
  useEffect(() => {
    if (hasNextPage && !isFetching && !isError) void fetchNextPage({ cancelRefetch: false })
  }, [hasNextPage, isFetching, isError, fetchNextPage])

  const pages = list.data?.pages
  const orgs = useMemo(() => pages?.flatMap((p) => p.orgs), [pages])
  return {
    orgs,
    isPending: list.isPending,
    error: list.error,
    isFetchingMore: Boolean(hasNextPage) && !isError,
  }
}
