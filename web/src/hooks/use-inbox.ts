// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError } from '@connectrpc/connect'
import { useQuery } from '@connectrpc/connect-query'
import { useMemo } from 'react'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ApprovalService } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { QuestionService } from '@/gen/koinon/question/v1alpha1/question_pb'
import { TaskService } from '@/gen/koinon/task/v1alpha1/task_pb'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
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
  const agents = useQuery(AgentService.method.listAgents, { orgId }, { refetchInterval: inboxPollMs })
  // Every role may list approvals (the proto says viewers and above); canDecide says who may decide.
  const approvals = useQuery(ApprovalService.method.listApprovals, { orgId }, { refetchInterval: inboxPollMs })
  // The viewer's open ask_user questions. A server without QuestionService (an older API) has none.
  const questions = useQuery(QuestionService.method.listQuestions, { orgId }, { refetchInterval: inboxPollMs })
  const questionsError = questions.error && ConnectError.from(questions.error).code !== Code.Unimplemented ? questions.error : null
  const who = useQuery(AuthService.method.whoAmI, {})
  const userId = who.data?.user?.id ?? ''

  const agentNames = useMemo(() => new Map((agents.data?.agents ?? []).map((a) => [a.id, a.displayName])), [agents.data])
  const items = useMemo(
    () =>
      buildInbox({
        tasks: tasks.data?.tasks ?? [],
        approvals: approvals.data?.approvals ?? [],
        questions: questions.data?.questions ?? [],
        agents: agents.data?.agents ?? [],
        isAdmin: org.role >= OrgRole.ADMIN,
        agentNames,
        userId,
      }),
    [tasks.data, approvals.data, questions.data, agents.data, org.role, agentNames, userId],
  )
  return {
    items,
    tasks,
    agents,
    approvals,
    questions,
    agentNames,
    isPending: tasks.isPending || approvals.isPending || (questions.isPending && !questions.error),
    error: tasks.error ?? agents.error ?? approvals.error ?? questionsError,
  }
}
