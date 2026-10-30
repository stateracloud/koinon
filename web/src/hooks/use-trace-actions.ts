// SPDX-License-Identifier: Apache-2.0

import { createClient } from '@connectrpc/connect'
import { createConnectQueryKey, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useMemo } from 'react'
import { TraceDestinationService, type ContentLevel } from '@/gen/koinon/trace/v1alpha1/trace_pb'
import type { DestinationWrite } from '@/lib/trace'

// Plain client calls rather than useMutation, like useMcpActions: TanStack Query would keep the
// request, with its credentials, in its mutation cache.

function useClientAndRefresh() {
  const transport = useTransport()
  const queryClient = useQueryClient()
  return useMemo(() => {
    const client = createClient(TraceDestinationService, transport)
    const refresh = () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: TraceDestinationService, cardinality: undefined }) })
    const done = async <T>(p: Promise<T>) => {
      const res = await p
      await refresh()
      return res
    }
    return { client, done, refresh }
  }, [transport, queryClient])
}

/** useTraceActions returns the write calls of TraceDestinationService for one org; each refreshes the queries. */
export function useTraceActions(orgId: string) {
  const { client, done, refresh } = useClientAndRefresh()
  return useMemo(
    () => ({
      createDestination: (destination: DestinationWrite) => done(client.createDestination({ orgId, destination })),
      updateDestination: (destinationId: string, destination: DestinationWrite, expectedVersion?: bigint) =>
        done(client.updateDestination({ orgId, destinationId, destination, expectedVersion })),
      reload: () => refresh(),
      deleteDestination: (destinationId: string) => done(client.deleteDestination({ orgId, destinationId })),
      testDestination: (destinationId: string) => done(client.testDestination({ orgId, destinationId })),
      updateSettings: (contentLevel: ContentLevel, sampleRate: number | undefined, platformCopyLevel: ContentLevel) =>
        done(client.updateSettings({ orgId, contentLevel, sampleRate, platformCopyLevel })),
    }),
    [client, done, refresh, orgId],
  )
}

export type TraceActions = ReturnType<typeof useTraceActions>

/** usePlatformTraceActions returns the platform admin calls. */
export function usePlatformTraceActions() {
  const { client, done } = useClientAndRefresh()
  return useMemo(
    () => ({
      setDestination: (destination: DestinationWrite) => done(client.setPlatformDestination({ destination })),
      deleteDestination: () => done(client.deletePlatformDestination({})),
      testDestination: () => done(client.testPlatformDestination({})),
      setCeiling: (ceiling: ContentLevel) => done(client.setPlatformCeiling({ ceiling })),
    }),
    [client, done],
  )
}

export type PlatformTraceActions = ReturnType<typeof usePlatformTraceActions>
