// SPDX-License-Identifier: Apache-2.0

import { timestampDate } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createClient } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery, useTransport } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { ConnectionService, type GitHubLink } from '@/gen/koinon/connection/v1alpha1/connection_pb'
import { describeError } from '@/lib/api-error'
import { type GitHubLinkResult, githubLinkResultText } from '@/lib/github-link'

function hostName(host: string) {
  try {
    return new URL(host).host
  } catch {
    return host
  }
}

/**
 * GitHubLinks shows the caller's links of a Koinon user to a GitHub login (issue #1175) and, for an
 * admin, every link of the org. Each link can be removed; the server audits both changes.
 */
export function GitHubLinks({ orgId, admin, result }: { orgId: string; admin: boolean; result?: GitHubLinkResult }) {
  const queryClient = useQueryClient()
  const mine = useQuery(ConnectionService.method.listMyGitHubLinks, { orgId })
  const all = useQuery(ConnectionService.method.listGitHubLinks, { orgId }, { enabled: admin })
  const remove = useMutation(ConnectionService.method.deleteGitHubLink, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ConnectionService, cardinality: undefined }) }),
  })
  // An install without the feature answers Unimplemented: show nothing then.
  if (mine.isError && ConnectError.from(mine.error).code === Code.Unimplemented) return null

  const removeButton = (l: GitHubLink, self: boolean) => (
    <Button
      size="sm"
      variant="outline"
      className="ml-auto"
      disabled={remove.isPending}
      aria-label={`Remove the link to ${l.githubLogin} on ${hostName(l.host)}`}
      onClick={() => remove.mutate({ orgId, host: l.host, userId: self ? '' : l.userId })}
    >
      Remove
    </Button>
  )

  return (
    <section className="grid gap-2 border-t pt-3" aria-labelledby="github-links-heading">
      <h2 id="github-links-heading" className="font-medium">
        Your GitHub login
      </h2>
      {result && (
        <Alert variant={result === 'linked' ? 'default' : 'destructive'} role={result === 'linked' ? 'status' : 'alert'}>
          <AlertDescription>{githubLinkResultText[result]}</AlertDescription>
        </Alert>
      )}
      <p className="text-muted-foreground text-sm">
        Link your GitHub login once from the page of a GitHub connection. Koinon then asks you to review the pull requests that agents open for you.
        Koinon keeps only your login and never stores a GitHub token for you.
      </p>
      {mine.isError && <p className="text-destructive text-sm">{describeError(mine.error).message}</p>}
      {mine.data && mine.data.links.length === 0 && <p className="text-sm">No GitHub login is linked.</p>}
      {mine.data && mine.data.links.length > 0 && (
        <ul className="grid gap-2">
          {mine.data.links.map((l) => (
            <li key={l.host} className="bg-card flex items-center gap-3 rounded-md border p-3 text-sm">
              <span className="font-medium">{l.githubLogin}</span>
              <span className="text-muted-foreground">on {hostName(l.host)}</span>
              {removeButton(l, true)}
            </li>
          ))}
        </ul>
      )}
      {admin && all.data && (
        <>
          <h3 className="mt-2 text-sm font-medium">Linked GitHub logins in this org</h3>
          {all.data.links.length === 0 ? (
            <p className="text-sm">No member has linked a GitHub login.</p>
          ) : (
            <ul className="grid gap-2">
              {all.data.links.map((l) => (
                <li key={`${l.userId} ${l.host}`} className="bg-card flex flex-wrap items-center gap-3 rounded-md border p-3 text-sm">
                  <span>{l.displayName || l.email || l.userId}</span>
                  <span className="font-medium">{l.githubLogin}</span>
                  <span className="text-muted-foreground">on {hostName(l.host)}</span>
                  {l.createdAt && <span className="text-muted-foreground">since {timestampDate(l.createdAt).toLocaleDateString()}</span>}
                  {removeButton(l, false)}
                </li>
              ))}
            </ul>
          )}
        </>
      )}
      {remove.error && <p className="text-destructive text-sm">Could not remove the link: {describeError(remove.error).message}</p>}
    </section>
  )
}

/**
 * LinkGitHubLoginButton starts the one-time GitHub sign-in through a connection's App. The browser
 * goes to GitHub and comes back to /connections with the result.
 */
export function LinkGitHubLoginButton({ orgId, connectionId }: { orgId: string; connectionId: string }) {
  const transport = useTransport()
  const [state, setState] = useState<{ pending: boolean; error?: string }>({ pending: false })
  async function start() {
    setState({ pending: true })
    try {
      const res = await createClient(ConnectionService, transport).startGitHubLink({ orgId, connectionId })
      window.location.assign(res.authorizeUrl)
    } catch (err) {
      setState({ pending: false, error: describeError(err).message })
    }
  }
  return (
    <div className="grid gap-1">
      <div className="flex items-center gap-3">
        <Button size="sm" variant="outline" disabled={state.pending} onClick={() => void start()}>
          {state.pending ? 'Opening GitHub…' : 'Link my GitHub login'}
        </Button>
        <span className="text-muted-foreground text-sm">Koinon then asks you to review the pull requests that agents open for you.</span>
      </div>
      {state.error && (
        <p role="alert" className="text-destructive text-sm">
          Could not start the GitHub sign-in: {state.error}
        </p>
      )}
    </div>
  )
}
