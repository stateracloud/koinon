// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError } from '@connectrpc/connect'
import { createConnectQueryKey, useMutation, useQuery } from '@connectrpc/connect-query'
import { useQueryClient } from '@tanstack/react-query'
import { ApprovalCard } from '@/components/approval-card'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { ApprovalService } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { TicketService } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'
import { isApprover } from '@/lib/approvals'
import { useOrg } from '@/lib/org-context'

/**
 * ApprovalView loads one request with GetApproval and decides it. Approve
 * sends the args_sha256 GetApproval returned, so a change the viewer has not
 * seen cannot be approved: the API answers Aborted (HTTP 409), and the card
 * asks the viewer to reload and review the new version.
 */
export function ApprovalView({ approvalId, showPermalink }: { approvalId: string; showPermalink?: boolean }) {
  const { org } = useOrg()
  const orgId = org.orgId
  const queryClient = useQueryClient()
  const get = useQuery(ApprovalService.method.getApproval, { orgId, approvalId })
  // The diff against the base branch: Koinon reads the current versions from
  // GitHub server-side; the browser never fetches file contents itself.
  const isPullRequest = get.data?.approval?.call.case === 'pullRequest'
  const diff = useQuery(ApprovalService.method.getApprovalDiff, { orgId, approvalId }, { enabled: isPullRequest, retry: false })

  const refresh = async () => {
    // The decision writes ticket events (approved, executed) and leaves
    // Waiting on you, so the ticket, the lists and this card re-read.
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: ApprovalService, cardinality: undefined }) }),
      queryClient.invalidateQueries({ queryKey: createConnectQueryKey({ schema: TicketService, cardinality: undefined }) }),
    ])
  }
  const approve = useMutation(ApprovalService.method.approve, { onSettled: refresh })
  const deny = useMutation(ApprovalService.method.deny, { onSettled: refresh })

  if (get.isPending) return <p role="status">Loading approval…</p>
  if (get.isError) {
    return (
      <Alert variant="destructive" role="alert">
        <AlertDescription>Could not load the approval: {ConnectError.from(get.error).rawMessage}</AlertDescription>
      </Alert>
    )
  }
  const a = get.data.approval
  if (!a) return null
  const err = approve.error ? ConnectError.from(approve.error) : deny.error ? ConnectError.from(deny.error) : undefined
  const changed = err?.code === Code.Aborted
  const reload = async () => {
    approve.reset()
    deny.reset()
    await Promise.all([get.refetch(), isPullRequest ? diff.refetch() : undefined])
  }
  return (
    <ApprovalCard
      approval={a}
      canDecide={a.canDecide && isApprover(org.role)}
      pending={approve.isPending || deny.isPending}
      error={err && !changed ? err.rawMessage : undefined}
      changed={changed}
      diff={diff.data}
      diffPending={isPullRequest && diff.isPending}
      diffError={diff.error ? ConnectError.from(diff.error).rawMessage : undefined}
      onReload={() => void reload()}
      showPermalink={showPermalink}
      onApprove={(note) => approve.mutate({ orgId, approvalId: a.id, argsSha256: a.argsSha256, note })}
      onDeny={(note) => deny.mutate({ orgId, approvalId: a.id, note })}
    />
  )
}
