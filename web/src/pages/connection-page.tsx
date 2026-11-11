// SPDX-License-Identifier: Apache-2.0

import { timestampDate } from '@bufbuild/protobuf/wkt'
import { ConnectError, createClient } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router'
import { useMemo, useState } from 'react'
import { ConfirmAction } from '@/components/confirm-action'
import { ConnectionSecretForm } from '@/components/connection-secret-form'
import { InstallationRepos } from '@/components/installation-repos'
import { SetupGuideSteps } from '@/components/setup-guide'
import { OutwardSwitch } from '@/components/outward-switch'
import { OutwardTargetKind } from '@/gen/koinon/outward/v1alpha1/outward_pb'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { TokenAge } from '@/components/token-age'
import { BoundAgentsSchema, ConnectionService, ConnectionStatus, type SetConnectionSecretResponse } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { isApprover } from '@/lib/approvals'
import { setSecretRequest } from '@/lib/connection-secret'
import { codeHostInfo, setupGuide } from '@/lib/connector'
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
  const type = types.data?.connectorTypes.find((t) => t.id === c?.connector)
  const guide = useMemo(() => setupGuide(type), [type])
  const host = codeHostInfo(type)

  const navigate = useNavigate()
  const remove = useMutation(ConnectionService.method.deleteConnection)
  const [refusal, setRefusal] = useState<{ message: string; agents: { id: string; displayName: string }[] }>()
  const invalidate = () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ConnectionService, cardinality: undefined }) })
  const test = useMutation(ConnectionService.method.testConnection, { onSuccess: invalidate })
  const transport = useTransport()
  const [setSecret, setSetSecret] = useState<{ pending: boolean; error?: unknown; data?: SetConnectionSecretResponse }>({ pending: false })
  // A plain client call rather than useMutation: TanStack Query keeps a
  // mutation's variables in its cache, and these hold the private key or tokens.
  async function storeSecret(input: Parameters<typeof setSecretRequest>[2]) {
    setSetSecret({ pending: true })
    try {
      const data = await createClient(ConnectionService, transport).setConnectionSecret(setSecretRequest(orgId, connectionId, input))
      test.reset()
      setSetSecret({ pending: false, data })
      await invalidate()
    } catch (error) {
      setSetSecret({ pending: false, error })
      throw error
    }
  }

  if (get.isPending) return <p role="status">Loading connection…</p>
  if (get.isError || !c) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>{get.error ? ConnectError.from(get.error).rawMessage : 'Connection not found'}</AlertDescription>
      </Alert>
    )
  }
  const results = test.data?.results ?? setSecret.data?.results ?? c.lastTest
  const stepIds = new Set(guide.steps.map((s) => s.id))
  const loose = results.filter((r) => !stepIds.has(r.step))
  const gh = c.settings.case === 'github' ? c.settings.value : undefined
  const gt = c.settings.case === 'gitea' ? c.settings.value : undefined
  const declared = c.source === 'secret' && !c.removed

  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <Link to="/connections" className="text-muted-foreground text-sm hover:underline">
        ← Connections
      </Link>
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">{c.displayName}</h1>
        <Badge variant={c.status === ConnectionStatus.FAILING ? 'destructive' : 'secondary'}>{connectionStatusLabel(c.status)}</Badge>
        {c.credentialLostAt && <Badge variant="destructive">Credential lost</Badge>}
        {c.adminDisabled && <Badge variant="destructive">Switched off</Badge>}
        {isApprover(org.role) && (
          <Button size="sm" className="ml-auto" disabled={test.isPending} onClick={() => test.mutate({ orgId, connectionId })}>
            {test.isPending ? 'Testing…' : 'Test connection'}
          </Button>
        )}
      </div>
      {isApprover(org.role) && <OutwardSwitch kind={OutwardTargetKind.CONNECTION} targetId={connectionId} subject={`connection ${c.displayName}`} disabled={c.adminDisabled} />}
      {c.credentialLostAt && !c.removed && (
        <p role="note" className="text-destructive text-sm">
          {declared
            ? `The stored ${gt ? 'tokens were' : 'private key was'} lost with a key-encryption key, so runs that need this connection fail. Koinon reads ${gt ? 'them' : 'it'} again from the Secret ${c.managedBySecret} on its next sync.`
            : `The stored ${gt ? 'tokens were' : 'private key was'} lost with a key-encryption key, so runs that need this connection fail. Enter ${gt ? 'them' : 'it'} again below.`}
        </p>
      )}
      {c.source === 'secret' && (
        <p className="text-muted-foreground text-sm">
          {c.removed
            ? `Removed: the Secret ${c.managedBySecret} that declared this connection was deleted. Restore the Secret to bring it back, or delete the connection.`
            : `Managed by Secret ${c.managedBySecret}. Change or delete that Secret to change this connection.`}
        </p>
      )}
      {gh && (
        <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
          <dt className="text-muted-foreground">App ID</dt>
          <dd>{gh.appId.toString()}</dd>
          <dt className="text-muted-foreground">Installation ID</dt>
          <dd>{gh.installationId.toString()}</dd>
          {gh.baseUrl && (
            <>
              <dt className="text-muted-foreground">Base URL</dt>
              <dd className="break-all">{gh.baseUrl}</dd>
            </>
          )}
          {gh.caBundle && (
            <>
              <dt className="text-muted-foreground">CA bundle</dt>
              <dd>
                {gh.caBundle.match(/-----BEGIN CERTIFICATE-----/g)?.length ?? 0} certificate(s)
                <details className="mt-1">
                  <summary className="text-muted-foreground cursor-pointer text-xs">Show</summary>
                  <pre className="mt-1 max-h-48 overflow-auto text-xs">{gh.caBundle}</pre>
                </details>
              </dd>
            </>
          )}
          <dt className="text-muted-foreground">Private key</dt>
          <dd>
            Stored encrypted{gh.keyFingerprint && <> · fingerprint <code className="text-xs">{gh.keyFingerprint}</code></>}
          </dd>
          <dt className="text-muted-foreground">CI directories</dt>
          <dd>{gh.ciDirs.length ? gh.ciDirs.join(', ') : "none besides the connector's"}</dd>
          {c.lastTestedAt && (
            <>
              <dt className="text-muted-foreground">Last tested</dt>
              <dd>{timestampDate(c.lastTestedAt).toLocaleString()}</dd>
            </>
          )}
        </dl>
      )}
      {gt && (
        <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
          <dt className="text-muted-foreground">Server URL</dt>
          <dd className="break-all">{gt.baseUrl}</dd>
          {gt.caBundle && (
            <>
              <dt className="text-muted-foreground">CA bundle</dt>
              <dd>
                {gt.caBundle.match(/-----BEGIN CERTIFICATE-----/g)?.length ?? 0} certificate(s)
                <details className="mt-1">
                  <summary className="text-muted-foreground cursor-pointer text-xs">Show</summary>
                  <pre className="mt-1 max-h-48 overflow-auto text-xs">{gt.caBundle}</pre>
                </details>
              </dd>
            </>
          )}
          <dt className="text-muted-foreground">Read token</dt>
          <dd>
            Stored encrypted · <TokenAge setAt={gt.readTokenSetAt} warnAfterSeconds={Number(gt.tokenWarnAfterSeconds)} />
          </dd>
          <dt className="text-muted-foreground">Write token</dt>
          <dd>
            Stored encrypted · <TokenAge setAt={gt.writeTokenSetAt} warnAfterSeconds={Number(gt.tokenWarnAfterSeconds)} />
          </dd>
          <dt className="text-muted-foreground">CI directories</dt>
          <dd>{gt.ciDirs.length ? gt.ciDirs.join(', ') : "none besides the connector's"}</dd>
          {c.lastTestedAt && (
            <>
              <dt className="text-muted-foreground">Last tested</dt>
              <dd>{timestampDate(c.lastTestedAt).toLocaleString()}</dd>
            </>
          )}
        </dl>
      )}
      {host?.weakerCredential && (
        <Alert role="note">
          <AlertDescription>
            {host.name} credentials are weaker than GitHub&apos;s: a bot token is not narrowed to repositories, does not expire, and {host.name} cannot refuse
            it the right to change workflow files. Agents&apos; sandboxes can reach only the repositories they are bound to, and Koinon refuses workflow
            changes itself. Koinon shows the tokens&apos; age and never rotates them: replace them in {host.name}, then in Koinon.
          </AlertDescription>
        </Alert>
      )}
      {isApprover(org.role) && c.source === 'ui' && (gh || gt) && (
        <ConnectionSecretForm
          key={connectionId}
          connector={gt ? 'gitea' : 'github'}
          lost={Boolean(c.credentialLostAt)}
          pending={setSecret.pending}
          error={setSecret.error ? `Could not store the ${gt ? 'tokens' : 'private key'}: ${ConnectError.from(setSecret.error).rawMessage}` : undefined}
          onSubmit={storeSecret}
        />
      )}
      {setSecret.data && !test.data && (
        <p role="status" className="text-sm">
          {setSecret.data.results.every((r) => r.ok) ? 'Stored. All checks passed.' : 'Stored, but some checks failed; see the steps below.'}
        </p>
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
      {isApprover(org.role) && !declared && (
        <div className="grid gap-2 border-t pt-3">
          <ConfirmAction
            label="Delete"
            subject={`connection ${c.displayName}`}
            consequence={`Delete ${c.displayName} and its stored ${gt ? 'tokens' : 'private key'}? Agents can no longer use it.`}
            confirmLabel="Delete connection"
            onConfirm={async () => {
              setRefusal(undefined)
              await remove.mutateAsync({ orgId, connectionId })
              await queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ConnectionService, cardinality: undefined }) })
              await navigate({ to: '/connections' })
            }}
            onError={(message, err) => setRefusal({ message, agents: err.findDetails(BoundAgentsSchema).flatMap((d) => d.agents) })}
          />
        </div>
      )}
      {refusal && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>
            {refusal.agents.length > 0 ? (
              <>
                <p>Agents are still bound to this connection, so it was not deleted. Unbind them from it first:</p>
                <ul className="mt-1 list-disc pl-5">
                  {refusal.agents.map((a) => (
                    <li key={a.id}>
                      <Link to="/agents/$agentId" params={{ agentId: a.id }} className="underline">
                        {a.displayName}
                      </Link>
                    </li>
                  ))}
                </ul>
              </>
            ) : (
              refusal.message
            )}
          </AlertDescription>
        </Alert>
      )}
      {(gh || gt) && isApprover(org.role) && (
        <InstallationRepos orgId={orgId} connectionId={connectionId} noun={gt ? 'the bot can see' : 'the installation can access'} />
      )}
      <SetupGuideSteps steps={guide.steps} results={results} />
    </div>
  )
}
