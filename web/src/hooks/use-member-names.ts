// SPDX-License-Identifier: Apache-2.0

import { useQuery } from '@connectrpc/connect-query'
import { useMemo } from 'react'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { QuestionService } from '@/gen/koinon/question/v1alpha1/question_pb'
import { useOrg } from '@/lib/org-context'

/**
 * useMemberNames maps the org's member ids to a name (display name, else email) for the Timeline.
 * ListAskableMembers is an admin-only RPC (internal/services/questions.go), so the query runs only
 * for admins; members and viewers get an empty map and the Timeline shows "Another member".
 * Exit condition: a member-readable source of author names (an actor display name on the task
 * event, or a members RPC the member role may call); tracked in
 * https://github.com/stateracloud/koinon/issues/1031: remove the admin-only path when #1031 gives
 * members a readable author name.
 */
export function useMemberNames(orgId: string): ReadonlyMap<string, string> {
  const { org } = useOrg()
  const isAdmin = org.role >= OrgRole.ADMIN
  const members = useQuery(QuestionService.method.listAskableMembers, { orgId }, { enabled: isAdmin })
  const list = members.data?.members
  return useMemo(() => new Map((list ?? []).map((m) => [m.userId, m.displayName || m.email])), [list])
}
