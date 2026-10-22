// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import type { Transport } from '@connectrpc/connect'
import { QueryClient } from '@tanstack/react-query'
import { createMemoryHistory } from '@tanstack/react-router'
import { render } from '@testing-library/react'
import App from '@/App'
import { AgentSchema, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { MembershipSchema, OrgRole, UserSchema, WhoAmIResponseSchema } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TicketSchema, TicketStatus } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'

export const orgId = 'org-1'

export function whoAmI(opts: { passwordChangeRequired?: boolean; role?: OrgRole } = {}) {
  return create(WhoAmIResponseSchema, {
    user: create(UserSchema, { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' }),
    memberships: [create(MembershipSchema, { orgId, orgSlug: 'default', orgDisplayName: 'Default', role: opts.role ?? OrgRole.OWNER })],
    passwordChangeRequired: opts.passwordChangeRequired ?? false,
  })
}

export function agent(id: string, name: string, status = AgentStatus.ACTIVE) {
  return create(AgentSchema, { id, orgId, displayName: name, status })
}

export function ticket(id: string, title: string, status: TicketStatus, agentId = 'a-1') {
  return create(TicketSchema, { id, orgId, title, status, agentId, createdAt: timestampFromMs(Date.now()) })
}

/** renderApp mounts the whole SPA at path against a fake transport. */
export function renderApp(transport: Transport, path: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const history = createMemoryHistory({ initialEntries: [path] })
  return { ...render(<App transport={transport} queryClient={queryClient} history={history} />), history, queryClient }
}
