// SPDX-License-Identifier: Apache-2.0

import { createConnectQueryKey } from '@connectrpc/connect-query'
import { Code, ConnectError } from '@connectrpc/connect'
import { QueryCache, QueryClient } from '@tanstack/react-query'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'

/** Every cached WhoAmI, whatever the transport. */
export const whoAmIKey = createConnectQueryKey({ schema: AuthService.method.whoAmI, cardinality: undefined })

const noRetry = new Set([Code.Unauthenticated, Code.PermissionDenied, Code.NotFound, Code.InvalidArgument, Code.FailedPrecondition])

export function createQueryClient(): QueryClient {
  const client: QueryClient = new QueryClient({
    queryCache: new QueryCache({
      // A session that expires mid-use makes any call fail Unauthenticated;
      // re-checking WhoAmI sends the layout back to the login page.
      onError: (err, query) => {
        if (ConnectError.from(err).code !== Code.Unauthenticated) return
        const k = query.queryKey[1] as { serviceName?: string; methodName?: string } | undefined
        if (k?.serviceName === AuthService.typeName && k.methodName === AuthService.method.whoAmI.name) return
        void client.invalidateQueries({ queryKey: whoAmIKey })
      },
    }),
    defaultOptions: {
      queries: {
        retry: (count, err) => count < 2 && !noRetry.has(ConnectError.from(err).code),
        refetchOnWindowFocus: true,
      },
    },
  })
  return client
}
