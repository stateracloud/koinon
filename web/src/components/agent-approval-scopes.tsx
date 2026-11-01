// SPDX-License-Identifier: Apache-2.0

import { timestampDate } from '@bufbuild/protobuf/wkt'
import { ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { ApprovalService, ScopeKind, type ApprovalScope } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { targetRepo } from '@/lib/approvals'
import { useOrg } from '@/lib/org-context'

function describe(s: ApprovalScope): string {
  return `${s.tool} on ${targetRepo(s.target)}`
}

/**
 * AgentApprovalScopes lists the scoped approvals that are active for an agent (ADR 0042 amendment): calls of
 * exactly this tool on exactly this target run without a new approval, for one task or until a date. Admins and
 * owners can revoke a scope at once; the agent's next matching call asks again.
 */
export function AgentApprovalScopes({ agentId, canRevoke }: { agentId: string; canRevoke: boolean }) {
  const { org } = useOrg()
  const orgId = org.orgId
  const queryClient = useQueryClient()
  const list = useQuery(ApprovalService.method.listApprovalScopes, { orgId, agentId })
  const revoke = useMutation(ApprovalService.method.revokeApprovalScope, {
    onSuccess: () => queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ApprovalService, cardinality: undefined }) }),
  })
  const scopes = list.data?.scopes ?? []
  return (
    <section aria-labelledby="agent-approval-scopes" className="bg-card grid gap-3 rounded-lg border p-4 text-sm">
      <h2 id="agent-approval-scopes" className="font-semibold">
        Scoped approvals
      </h2>
      {list.isPending ? (
        <p role="status">Loading scoped approvals…</p>
      ) : list.isError ? (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not load scoped approvals: {ConnectError.from(list.error).rawMessage}</AlertDescription>
        </Alert>
      ) : scopes.length === 0 ? (
        <p className="text-muted-foreground">None. Every call that needs approval asks first.</p>
      ) : (
        <ul className="grid gap-2" aria-label="Active scoped approvals">
          {scopes.map((s) => (
            <li key={s.id} className="flex flex-wrap items-center gap-2 border-t pt-2 first:border-t-0 first:pt-0" data-testid="approval-scope">
              <div className="grid gap-0.5">
                <span className="font-medium">
                  <code className="font-mono">{s.tool}</code> on {targetRepo(s.target)}
                </span>
                <span className="text-muted-foreground">
                  {s.kind === ScopeKind.TASK ? 'For one task' : 'For this agent'}, until {s.expiresAt ? timestampDate(s.expiresAt).toLocaleDateString() : 'unknown'}
                </span>
              </div>
              {canRevoke && (
                <Button
                  variant="outline"
                  size="sm"
                  className="ml-auto"
                  disabled={revoke.isPending}
                  aria-label={`Revoke ${describe(s)}`}
                  onClick={() => revoke.mutate({ orgId, scopeId: s.id })}
                >
                  Revoke
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
      {revoke.error && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>Could not revoke: {ConnectError.from(revoke.error).rawMessage}</AlertDescription>
        </Alert>
      )}
    </section>
  )
}
