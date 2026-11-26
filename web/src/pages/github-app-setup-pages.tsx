// SPDX-License-Identifier: Apache-2.0

import { createClient } from '@connectrpc/connect'
import { createConnectQueryKey, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router'
import { useEffect, useRef, useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { describeError } from '@/lib/api-error'

// The pages GitHub sends the browser to during the guided App setup (issue #1175). The state names
// the org before its first dot; the server checks the rest.

const createdRoute = getRouteApi('/app/org/connections/new/github/app-created')
const installedRoute = getRouteApi('/app/org/connections/new/github/app-installed')

function setupOrg(state: string) {
  return state.split('.')[0] ?? ''
}

function Shell({ status, error }: { status: string; error?: string }) {
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <h1 className="text-xl font-semibold">Connect GitHub</h1>
      {error ? (
        <>
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
          <Link to="/connections/new/github" className="text-sm hover:underline">
            Start the setup again
          </Link>
        </>
      ) : (
        <p role="status">{status}</p>
      )}
    </div>
  )
}

/** useOnce runs fn once per mount, also under StrictMode's double effects. */
function useOnce(fn: () => Promise<void>) {
  const done = useRef(false)
  useEffect(() => {
    if (done.current) return
    done.current = true
    void fn()
  })
}

/** GitHubAppCreatedPage takes GitHub's code for the new App and sends the browser on to install it. */
export function GitHubAppCreatedPage() {
  const { code, state } = createdRoute.useSearch()
  const transport = useTransport()
  const [error, setError] = useState<string>()
  useOnce(async () => {
    if (!code || !state) {
      setError('GitHub did not send back a code. The setup did not create an App.')
      return
    }
    try {
      const res = await createClient(ConnectionService, transport).continueGitHubAppSetup({ orgId: setupOrg(state), state, code })
      window.location.assign(res.installUrl)
    } catch (err) {
      setError(`Could not get the App from GitHub: ${describeError(err).message}`)
    }
  })
  return <Shell status="GitHub created the App. Koinon stores its key and opens the install page…" error={error} />
}

/** GitHubAppInstalledPage makes the connection after the admin installed the App. */
export function GitHubAppInstalledPage() {
  const { installation_id: installationId, setup_action: action, state } = installedRoute.useSearch()
  const transport = useTransport()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const [error, setError] = useState<string>()
  useOnce(async () => {
    if (action === 'request') {
      setError('An owner of the GitHub organization must approve the install. Ask them to approve it, then install the App again.')
      return
    }
    if (!state || !installationId || !/^[1-9][0-9]{0,18}$/.test(installationId)) {
      setError('GitHub did not send back an installation of the App.')
      return
    }
    try {
      const res = await createClient(ConnectionService, transport).finishGitHubAppSetup({
        orgId: setupOrg(state),
        state,
        installationId: BigInt(installationId),
      })
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ConnectionService, cardinality: undefined }) })
      if (res.connection) await navigate({ to: '/connections/$connectionId', params: { connectionId: res.connection.id } })
    } catch (err) {
      setError(`Could not make the connection: ${describeError(err).message}`)
    }
  })
  return <Shell status="Koinon checks the installation and makes the connection…" error={error} />
}
