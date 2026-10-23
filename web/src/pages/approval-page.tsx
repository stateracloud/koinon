// SPDX-License-Identifier: Apache-2.0

import { useQuery } from '@connectrpc/connect-query'
import { getRouteApi, Link } from '@tanstack/react-router'
import { ApprovalView } from '@/components/approval-view'
import { ApprovalService } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { useOrg } from '@/lib/org-context'

const route = getRouteApi('/app/approvals/$approvalId')

/** ApprovalPage shows one approval request outside its ticket. */
export function ApprovalPage() {
  const { approvalId } = route.useParams()
  const { org } = useOrg()
  // Shares the card's query, so this costs no extra request.
  const get = useQuery(ApprovalService.method.getApproval, { orgId: org.orgId, approvalId })
  const ticketId = get.data?.approval?.ticketId
  return (
    <div className="mx-auto grid max-w-3xl gap-4">
      <div className="flex gap-4 text-sm">
        <Link to="/" className="text-muted-foreground hover:underline">
          ← Board
        </Link>
        {ticketId && (
          <Link to="/tickets/$ticketId" params={{ ticketId }} className="text-muted-foreground hover:underline">
            Open the ticket
          </Link>
        )}
      </div>
      <h1 className="text-xl font-semibold">Approval request</h1>
      <ApprovalView approvalId={approvalId} />
    </div>
  )
}
