// SPDX-License-Identifier: Apache-2.0

import type { Agent, ListAgentsRequest } from '@/gen/koinon/agent/v1alpha1/agent_pb'

/**
 * pagedListAgents answers ListAgents with the given pages: the token of page n is `p<n>`, and the
 * last page has no next token. It records the tokens it was asked for in `asked`.
 */
export function pagedListAgents(pages: Agent[][]) {
  const asked: string[] = []
  const listAgents = (req: ListAgentsRequest) => {
    asked.push(req.pageToken)
    const n = req.pageToken === '' ? 0 : Number(req.pageToken.slice(1))
    return { agents: pages[n], nextPageToken: n + 1 < pages.length ? `p${(n + 1).toString()}` : '' }
  }
  return { listAgents, asked }
}
