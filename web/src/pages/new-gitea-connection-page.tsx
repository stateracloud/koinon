// SPDX-License-Identifier: Apache-2.0

import { ConnectError, createClient } from '@connectrpc/connect'
import { createConnectQueryKey, useQuery, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { useMemo, useState } from 'react'
import { GiteaConnectionForm, type GiteaConnectionInput } from '@/components/gitea-connection-form'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { isApprover } from '@/lib/approvals'
import { setupGuide } from '@/lib/connector'
import { useOrg } from '@/lib/org-context'

/** NewGiteaConnectionPage creates a Gitea connection (a bot with a read and a write token) by its setup guide (ADR 0021, 0063). */
export function NewGiteaConnectionPage() {
  const { org } = useOrg()
  const orgId = org.orgId
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const transport = useTransport()
  const types = useQuery(ConnectionService.method.listConnectorTypes, {})
  const gitea = types.data?.connectorTypes.find((t) => t.id === 'gitea')
  const guide = useMemo(() => setupGuide(gitea), [gitea])
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()

  // A plain client call rather than useMutation: TanStack Query keeps a mutation's variables in its
  // cache, and these hold the tokens.
  async function create(input: GiteaConnectionInput) {
    setPending(true)
    setError(undefined)
    try {
      const res = await createClient(ConnectionService, transport).createGiteaConnection({ orgId, ...input, test: true })
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ConnectionService, cardinality: undefined }) })
      if (res.connection) await navigate({ to: '/connections/$connectionId', params: { connectionId: res.connection.id } })
    } catch (err) {
      setError(ConnectError.from(err).rawMessage)
      throw err
    } finally {
      setPending(false)
    }
  }

  if (!isApprover(org.role)) {
    return (
      <Alert role="alert">
        <AlertDescription>Only org admins and owners can connect Gitea.</AlertDescription>
      </Alert>
    )
  }
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <Link to="/connections" className="text-muted-foreground text-sm hover:underline">
        ← Connections
      </Link>
      <h1 className="text-xl font-semibold">Connect Gitea</h1>
      {gitea?.description && <p className="text-muted-foreground text-sm">{gitea.description}</p>}
      <Alert role="note">
        <AlertDescription>
          A Gitea access token is weaker than the installation token Koinon uses for GitHub: it is not narrowed to repositories, it does not expire, and
          Gitea cannot refuse it the right to change workflow files. Koinon limits what agents&apos; sandboxes can reach to the repositories they are
          bound to, refuses workflow changes itself, and shows how old each token is. It never rotates them.
        </AlertDescription>
      </Alert>
      {types.isPending && <p role="status">Loading the setup guide…</p>}
      {types.isError && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{ConnectError.from(types.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      {types.isSuccess && !gitea && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>This install has no Gitea connector type.</AlertDescription>
        </Alert>
      )}
      {gitea && <GiteaConnectionForm guide={guide} pending={pending} error={error} onSubmit={create} />}
    </div>
  )
}
