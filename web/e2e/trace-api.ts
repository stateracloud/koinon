// SPDX-License-Identifier: Apache-2.0

import { traceDestinationHandlers, type Handlers } from './mock-api.ts'

export const org = 'org-1'

export function api(opts: { role?: string; platformAdmin?: boolean; level?: string; ceiling?: string; platform?: boolean } = {}) {
  const trace = traceDestinationHandlers(opts)
  const handlers: Handlers = {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role: opts.role ?? 'ORG_ROLE_OWNER' }],
        platformAdmin: opts.platformAdmin ?? false,
      }),
      'koinon.connection.v1alpha1.ConnectionService/ListConnections': () => ({ connections: [] }),
      ...trace.unary,
    },
    stream: {},
  }
  return { handlers, seen: trace.seen }
}
