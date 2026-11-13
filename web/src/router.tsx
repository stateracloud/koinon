// SPDX-License-Identifier: Apache-2.0

import type { QueryClient } from '@tanstack/react-query'
import { createRootRouteWithContext, createRoute, createRouter, Outlet, redirect, type RouterHistory } from '@tanstack/react-router'
import { AgentPage } from '@/pages/agent-page'
import { AgentsPage } from '@/pages/agents-page'
import { AppLayout } from '@/pages/app-layout'
import { ApprovalPage } from '@/pages/approval-page'
import { BoardPage } from '@/pages/board-page'
import { InboxPage } from '@/pages/inbox-page'
import { ConnectionPage } from '@/pages/connection-page'
import { ConnectionsPage } from '@/pages/connections-page'
import { BudgetsPage } from '@/pages/budgets-page'
import { PlatformBudgetsPage } from '@/pages/platform-budgets-page'
import { PricesPage } from '@/pages/prices-page'
import { ProvidersPage } from '@/pages/providers-page'
import { ServersPage } from '@/pages/servers-page'
import { TokensPage } from '@/pages/tokens-page'
import { NewAgentPage } from '@/pages/new-agent-page'
import { NewGiteaConnectionPage } from '@/pages/new-gitea-connection-page'
import { NewGitHubConnectionPage } from '@/pages/new-github-connection-page'
import { LoginPage } from '@/pages/login-page'
import { TaskPage } from '@/pages/task-page'
import { ObservabilityPage } from '@/pages/observability-page'
import { PlatformObservabilityPage } from '@/pages/platform-observability-page'
import { PlatformPricesPage } from '@/pages/platform-prices-page'

export type RouterContext = { queryClient: QueryClient }

const rootRoute = createRootRouteWithContext<RouterContext>()({
  component: Outlet,
  notFoundComponent: () => <p className="p-6">Page not found.</p>,
})

export const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/login',
  validateSearch: (s: Record<string, unknown>): { redirect?: string } =>
    typeof s.redirect === 'string' ? { redirect: s.redirect } : {},
  component: LoginPage,
})

// The pathless app route requires a session: AppLayout checks WhoAmI.
const appRoute = createRoute({ getParentRoute: () => rootRoute, id: 'app', component: AppLayout })

export const inboxRoute = createRoute({ getParentRoute: () => appRoute, path: '/', component: InboxPage })

export const boardRoute = createRoute({ getParentRoute: () => appRoute, path: '/board', component: BoardPage })

export const taskRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/tasks/$taskId',
  component: TaskPage,
})

// Tickets became tasks with the same ids (ADR 0065): old links keep working.
export const ticketRedirectRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/tickets/$ticketId',
  beforeLoad: ({ params }) => {
    throw redirect({ to: '/tasks/$taskId', params: { taskId: params.ticketId }, replace: true })
  },
})

export const approvalRoute = createRoute({ getParentRoute: () => appRoute, path: '/approvals/$approvalId', component: ApprovalPage })

export const agentsRoute = createRoute({ getParentRoute: () => appRoute, path: '/agents', component: AgentsPage })
export const newAgentRoute = createRoute({ getParentRoute: () => appRoute, path: '/agents/new', component: NewAgentPage })
export const agentRoute = createRoute({ getParentRoute: () => appRoute, path: '/agents/$agentId', component: AgentPage })

export const connectionsRoute = createRoute({ getParentRoute: () => appRoute, path: '/connections', component: ConnectionsPage })
export const newGitHubConnectionRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/connections/new/github',
  component: NewGitHubConnectionPage,
})
export const newGiteaConnectionRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/connections/new/gitea',
  component: NewGiteaConnectionPage,
})
export const connectionRoute = createRoute({ getParentRoute: () => appRoute, path: '/connections/$connectionId', component: ConnectionPage })

export const providersRoute = createRoute({ getParentRoute: () => appRoute, path: '/settings/providers', component: ProvidersPage })
export const serversRoute = createRoute({ getParentRoute: () => appRoute, path: '/servers', component: ServersPage })
export const pricesRoute = createRoute({ getParentRoute: () => appRoute, path: '/settings/prices', component: PricesPage })
export const budgetsRoute = createRoute({ getParentRoute: () => appRoute, path: '/settings/budgets', component: BudgetsPage })
export const platformBudgetsRoute = createRoute({ getParentRoute: () => appRoute, path: '/platform/budgets', component: PlatformBudgetsPage })
export const observabilityRoute = createRoute({ getParentRoute: () => appRoute, path: '/settings/observability', component: ObservabilityPage })
export const platformPricesRoute = createRoute({ getParentRoute: () => appRoute, path: '/platform/prices', component: PlatformPricesPage })
export const platformObservabilityRoute = createRoute({ getParentRoute: () => appRoute, path: '/platform/observability', component: PlatformObservabilityPage })
export const tokensRoute = createRoute({ getParentRoute: () => appRoute, path: '/settings/tokens', component: TokensPage })

const routeTree = rootRoute.addChildren([
  loginRoute,
  appRoute.addChildren([
    inboxRoute,
    boardRoute,
    taskRoute,
    ticketRedirectRoute,
    approvalRoute,
    agentsRoute,
    newAgentRoute,
    agentRoute,
    connectionsRoute,
    newGitHubConnectionRoute,
    newGiteaConnectionRoute,
    connectionRoute,
    providersRoute,
    serversRoute,
    pricesRoute,
    budgetsRoute,
    platformBudgetsRoute,
    observabilityRoute,
    platformPricesRoute,
    platformObservabilityRoute,
    tokensRoute,
  ]),
])

export function createAppRouter(queryClient: QueryClient, history?: RouterHistory) {
  return createRouter({ routeTree, context: { queryClient }, history, defaultPreload: false })
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>
  }
}
