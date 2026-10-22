// SPDX-License-Identifier: Apache-2.0

import { timestampDate } from '@bufbuild/protobuf/wkt'
import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link } from '@tanstack/react-router'
import { useMemo } from 'react'
import { SetupGuideSteps } from '@/components/setup-guide'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { ConnectionService, ConnectionStatus } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { isApprover } from '@/lib/approvals'
import { setupGuide } from '@/lib/connector'
import { connectionStatusLabel } from '@/lib/labels'
import { useOrg } from '@/lib/org-context'

const route = getRouteApi('/app/connections/$connectionId')

/** ConnectionPage shows a connection, its last test by setup-guide step, and runs the test again. */
export function ConnectionPage() {
  const { connectionId } = route.useParams()
  const { org } = useOrg()
  const orgId = org.orgId
  const queryClient = useQueryClient()
  const get = useQuery(ConnectionService.method.getConnection, { orgId, connectionId })
  const types = useQuery(ConnectionService.method.listConnectorTypes, {})
  const c = get.data?.connection
  const guide = useMemo(() => setupGuide(types.data?.connectorTypes.find((t) => t.id === c?.connector)), [types.data, c?.connector])

  const test = useMutation(ConnectionService.method.testConnection, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ConnectionService, cardinality: undefined }) }),
  })

  if (get.isPending) return <p role="status">Loading connection…</p>
  if (get.isError || !c) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>{get.error ? ConnectError.from(get.error).rawMessage : 'Connection not found'}</AlertDescription>
      </Alert>
    )
  }
  const results = test.data?.results ?? c.lastTest
  const stepIds = new Set(guide.steps.map((s) => s.id))
  const loose = results.filter((r) => !stepIds.has(r.step))
  const gh = c.settings.case === 'github' ? c.settings.value : undefined

  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <Link to="/connections" className="text-muted-foreground text-sm hover:underline">
        ← Connections
      </Link>
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">{c.displayName}</h1>
        <Badge variant={c.status === ConnectionStatus.FAILING ? 'destructive' : 'secondary'}>{connectionStatusLabel(c.status)}</Badge>
        {isApprover(org.role) && (
          <Button size="sm" className="ml-auto" disabled={test.isPending} onClick={() => test.mutate({ orgId, connectionId })}>
            {test.isPending ? 'Testing…' : 'Test connection'}
          </Button>
        )}
      </div>
      {gh && (
        <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
          <dt className="text-muted-foreground">App ID</dt>
          <dd>{gh.appId.toString()}</dd>
          <dt className="text-muted-foreground">Installation ID</dt>
          <dd>{gh.installationId.toString()}</dd>
          <dt className="text-muted-foreground">Private key</dt>
          <dd>
            Stored in a Kubernetes Secret{gh.keyFingerprint && <> · fingerprint <code className="text-xs">{gh.keyFingerprint}</code></>}
          </dd>
          {c.lastTestedAt && (
            <>
              <dt className="text-muted-foreground">Last tested</dt>
              <dd>{timestampDate(c.lastTestedAt).toLocaleString()}</dd>
            </>
          )}
        </dl>
      )}
      {test.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not test the connection: {ConnectError.from(test.error).rawMessage}</AlertDescription>
        </Alert>
      )}
      {test.isSuccess && (
        <p role="status" className="text-sm">
          {test.data.results.every((r) => r.ok) ? 'All checks passed.' : 'Some checks failed; see the steps below.'}
        </p>
      )}
      {loose.length > 0 && (
        <ul className="grid gap-1 text-sm">
          {loose.map((r) => (
            <li key={r.check}>
              {r.ok ? 'Passed' : 'Failed'}: {r.message} ({r.check})
            </li>
          ))}
        </ul>
      )}
      <SetupGuideSteps steps={guide.steps} results={results} />
    </div>
  )
}
