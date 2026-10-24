// SPDX-License-Identifier: Apache-2.0

import type { QueryClient } from '@tanstack/react-query'
import { createRootRouteWithContext, createRoute, createRouter, Outlet, type RouterHistory } from '@tanstack/react-router'
import { AgentPage } from '@/pages/agent-page'
import { AgentsPage } from '@/pages/agents-page'
import { AppLayout } from '@/pages/app-layout'
import { ApprovalPage } from '@/pages/approval-page'
import { BoardPage } from '@/pages/board-page'
import { ConnectionPage } from '@/pages/connection-page'
import { ConnectionsPage } from '@/pages/connections-page'
import { BudgetsPage } from '@/pages/budgets-page'
import { PlatformBudgetsPage } from '@/pages/platform-budgets-page'
import { PricesPage } from '@/pages/prices-page'
import { ProvidersPage } from '@/pages/providers-page'
import { ServersPage } from '@/pages/servers-page'
import { TokensPage } from '@/pages/tokens-page'
import { NewAgentPage } from '@/pages/new-agent-page'
import { NewGitHubConnectionPage } from '@/pages/new-github-connection-page'
import { LoginPage } from '@/pages/login-page'
import { TicketPage } from '@/pages/ticket-page'

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

export const boardRoute = createRoute({ getParentRoute: () => appRoute, path: '/', component: BoardPage })

export const ticketRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/tickets/$ticketId',
  component: TicketPage,
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
export const connectionRoute = createRoute({ getParentRoute: () => appRoute, path: '/connections/$connectionId', component: ConnectionPage })

export const providersRoute = createRoute({ getParentRoute: () => appRoute, path: '/settings/providers', component: ProvidersPage })
export const serversRoute = createRoute({ getParentRoute: () => appRoute, path: '/servers', component: ServersPage })
export const pricesRoute = createRoute({ getParentRoute: () => appRoute, path: '/settings/prices', component: PricesPage })
export const budgetsRoute = createRoute({ getParentRoute: () => appRoute, path: '/settings/budgets', component: BudgetsPage })
export const platformBudgetsRoute = createRoute({ getParentRoute: () => appRoute, path: '/platform/budgets', component: PlatformBudgetsPage })
export const tokensRoute = createRoute({ getParentRoute: () => appRoute, path: '/settings/tokens', component: TokensPage })

const routeTree = rootRoute.addChildren([
  loginRoute,
  appRoute.addChildren([
    boardRoute,
    ticketRoute,
    approvalRoute,
    agentsRoute,
    newAgentRoute,
    agentRoute,
    connectionsRoute,
    newGitHubConnectionRoute,
    connectionRoute,
    providersRoute,
    serversRoute,
    pricesRoute,
    budgetsRoute,
    platformBudgetsRoute,
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
