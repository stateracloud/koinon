// SPDX-License-Identifier: Apache-2.0

import { createClient } from '@connectrpc/connect'
import { createConnectQueryKey, useQuery, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { useMemo, useState } from 'react'
import { GitHubAppSetup } from '@/components/github-app-setup'
import { GitHubConnectionForm, type GitHubConnectionInput } from '@/components/github-connection-form'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { ConnectionService } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { describeError } from '@/lib/api-error'
import { isApprover } from '@/lib/approvals'
import { setupGuide } from '@/lib/connector'
import { useOrg } from '@/lib/org-context'

/** NewGitHubConnectionPage creates a GitHub App connection by its setup guide (ADR 0021). */
export function NewGitHubConnectionPage() {
  const { org } = useOrg()
  const orgId = org.orgId
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const transport = useTransport()
  const types = useQuery(ConnectionService.method.listConnectorTypes, {})
  const github = types.data?.connectorTypes.find((t) => t.id === 'github')
  const guide = useMemo(() => setupGuide(github), [github])
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()

  // A plain client call rather than useMutation: TanStack Query keeps a
  // mutation's variables in its cache, and these hold the private key.
  async function create(input: GitHubConnectionInput) {
    setPending(true)
    setError(undefined)
    try {
      const res = await createClient(ConnectionService, transport).createGitHubConnection({ orgId, ...input, test: true })
      await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ConnectionService, cardinality: undefined }) })
      if (res.connection) await navigate({ to: '/connections/$connectionId', params: { connectionId: res.connection.id } })
    } catch (err) {
      setError(describeError(err).message)
      throw err
    } finally {
      setPending(false)
    }
  }

  if (!isApprover(org.role)) {
    return (
      <Alert role="alert">
        <AlertDescription>Only org admins and owners can connect GitHub.</AlertDescription>
      </Alert>
    )
  }
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <Link to="/connections" className="text-muted-foreground text-sm hover:underline">
        ← Connections
      </Link>
      <h1 className="text-xl font-semibold">Connect GitHub</h1>
      {github?.description && <p className="text-muted-foreground text-sm">{github.description}</p>}
      {types.isPending && <p role="status">Loading the setup guide…</p>}
      {types.isError && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>{describeError(types.error).message}</AlertDescription>
        </Alert>
      )}
      {types.isSuccess && !github && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>This install has no GitHub connector type.</AlertDescription>
        </Alert>
      )}
      {github && (
        <>
          <GitHubAppSetup orgId={orgId} />
          <h2 className="font-medium">Or connect an App you created yourself</h2>
          <p className="text-muted-foreground text-sm">
            An App made by hand cannot sign users in, so Koinon cannot ask the requester to review the pull requests of agents.
          </p>
          <GitHubConnectionForm guide={guide} pending={pending} error={error} onSubmit={create} />
        </>
      )}
    </div>
  )
}
