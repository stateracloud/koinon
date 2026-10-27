// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import type { Transport } from '@connectrpc/connect'
import { QueryClient } from '@tanstack/react-query'
import { createMemoryHistory } from '@tanstack/react-router'
import { render } from '@testing-library/react'
import App from '@/App'
import { ApprovalSchema, Decision, Status as ApprovalStatus } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { AgentSchema, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { MembershipSchema, OrgRole, UserSchema, WhoAmIResponseSchema } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { CreatorKind, CreatorSchema, TaskSchema, TaskState } from '@/gen/koinon/task/v1alpha1/task_pb'

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

export function task(id: string, title: string, state: TaskState, agentId = 'a-1') {
  return create(TaskSchema, { id, orgId, title, state, agentId, createdBy: create(CreatorSchema, { kind: CreatorKind.USER, userId: 'u-1' }), createdAt: timestampFromMs(Date.now()), updatedAt: timestampFromMs(Date.now()) })
}

/** approval is a pending create_pull_request request of task taskId that the viewer may decide. */
export function approval(id: string, taskId: string, init: { canDecide?: boolean; agentId?: string; sha?: string } = {}) {
  return create(ApprovalSchema, {
    id,
    orgId,
    ticketId: taskId,
    runId: 'r-1',
    agentId: init.agentId ?? 'a-1',
    connector: 'github',
    tool: 'create_pull_request',
    target: 'github:acme/widgets',
    argsSha256: init.sha ?? 'f3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
    decision: Decision.ASK,
    status: ApprovalStatus.PENDING,
    canDecide: init.canDecide ?? true,
    approvalsRequired: 1,
    createdAt: timestampFromMs(Date.now()),
  })
}

/** renderApp mounts the whole SPA at path against a fake transport. */
export function renderApp(transport: Transport, path: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  const history = createMemoryHistory({ initialEntries: [path] })
  return { ...render(<App transport={transport} queryClient={queryClient} history={history} />), history, queryClient }
}
