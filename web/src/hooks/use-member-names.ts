// SPDX-License-Identifier: Apache-2.0

import { useQuery } from '@connectrpc/connect-query'
import { useMemo } from 'react'
import { QuestionService } from '@/gen/koinon/question/v1alpha1/question_pb'

/** useMemberNames maps the org's member ids to a name (display name, else email) for the Timeline. */
export function useMemberNames(orgId: string): ReadonlyMap<string, string> {
  const members = useQuery(QuestionService.method.listAskableMembers, { orgId })
  const list = members.data?.members
  return useMemo(() => new Map((list ?? []).map((m) => [m.userId, m.displayName || m.email])), [list])
}
