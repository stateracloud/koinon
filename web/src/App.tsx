// SPDX-License-Identifier: Apache-2.0

import { TransportProvider } from '@connectrpc/connect-query'
import type { Transport } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'
import { QueryClientProvider, type QueryClient } from '@tanstack/react-query'
import { RouterProvider, type RouterHistory } from '@tanstack/react-router'
import { useState } from 'react'
import { createQueryClient } from '@/lib/query'
import { createAppRouter } from '@/router'

export type AppProps = {
  /** Defaults to connect-web against this origin; tests pass a router transport. */
  transport?: Transport
  queryClient?: QueryClient
  history?: RouterHistory
}

function defaultTransport(): Transport {
  // The session cookie rides along (same origin); connect-web always sends
  // Connect-Protocol-Version, which the API's CSRF check requires.
  return createConnectTransport({ baseUrl: window.location.origin })
}

export default function App(props: AppProps) {
  const [transport] = useState(() => props.transport ?? defaultTransport())
  const [queryClient] = useState(() => props.queryClient ?? createQueryClient())
  const [router] = useState(() => createAppRouter(queryClient, props.history))
  return (
    <TransportProvider transport={transport}>
      <QueryClientProvider client={queryClient}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </TransportProvider>
  )
}
