// SPDX-License-Identifier: Apache-2.0

import { useQuery } from '@connectrpc/connect-query'
import { useMemo } from 'react'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { ApprovalService } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { TaskService } from '@/gen/koinon/task/v1alpha1/task_pb'
import { isApprover } from '@/lib/approvals'
import { buildInbox } from '@/lib/inbox'
import { useOrg } from '@/lib/org-context'

/**
 * The API has no task stream yet (runs have WatchRun, tasks do not), so the inbox, the board and
 * the badge poll at this interval to pick up what runs and other people change. Queries with the
 * same key share one request, so the page and the badge cost one poll.
 */
export const inboxPollMs = 15_000

/** useInbox loads what waits on the signed-in person in the current org and keeps it fresh. */
export function useInbox() {
  const { org } = useOrg()
  const orgId = org.orgId
  const tasks = useQuery(TaskService.method.listTasks, { orgId }, { refetchInterval: inboxPollMs })
  const agents = useQuery(AgentService.method.listAgents, { orgId })
  const approver = isApprover(org.role)
  const approvals = useQuery(ApprovalService.method.listApprovals, { orgId }, { enabled: approver, refetchInterval: inboxPollMs })

  const agentNames = useMemo(() => new Map((agents.data?.agents ?? []).map((a) => [a.id, a.displayName])), [agents.data])
  const items = useMemo(
    () => buildInbox({ tasks: tasks.data?.tasks ?? [], approvals: approvals.data?.approvals ?? [], agentNames }),
    [tasks.data, approvals.data, agentNames],
  )
  return {
    items,
    tasks,
    agents,
    approvals,
    agentNames,
    isPending: tasks.isPending || (approver && approvals.isPending),
    error: tasks.error ?? approvals.error,
  }
}
