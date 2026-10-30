// SPDX-License-Identifier: Apache-2.0

import type { QueryClient } from '@tanstack/react-query'
import { createRootRouteWithContext, createRoute, createRouter, lazyRouteComponent, Outlet, redirect, type RouterHistory } from '@tanstack/react-router'
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
export const tokensRoute = createRoute({ getParentRoute: () => appRoute, path: '/settings/tokens', component: TokensPage })

// The v2 UI preview (src/preview): the approved mockups as screens on mock data. It has no session check and makes
// no API calls, so it renders without a backend. Every page is lazy-loaded, so the app's bundle does not carry it.
const previewPages = {
  shell: () => import('@/preview/pages/shell-screens'),
  index: () => import('@/preview/pages/index'),
  overview: () => import('@/preview/pages/overview'),
  run: () => import('@/preview/pages/run'),
  agents: () => import('@/preview/pages/agents'),
  marketplace: () => import('@/preview/pages/marketplace'),
  providers: () => import('@/preview/pages/providers'),
  platform: () => import('@/preview/pages/platform'),
  onboarding: () => import('@/preview/pages/onboarding'),
}

export const previewRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/preview',
  component: lazyRouteComponent(() => import('@/preview/shell/preview-root'), 'PreviewRoot'),
})

function previewChild<const TPath extends string>(path: TPath, component: ReturnType<typeof lazyRouteComponent>) {
  return createRoute({ getParentRoute: () => previewRoute, path, component })
}

const previewRoutes = [
  previewChild('/', lazyRouteComponent(previewPages.index, 'PreviewIndexPage')),
  previewChild('sidebar', lazyRouteComponent(previewPages.shell, 'SidebarScreen')),
  previewChild('runs', lazyRouteComponent(previewPages.shell, 'RunsPage')),
  previewChild('commands', lazyRouteComponent(previewPages.shell, 'CommandsPage')),
  previewChild('phone', lazyRouteComponent(previewPages.shell, 'PhoneFramePage')),
  previewChild('overview', lazyRouteComponent(previewPages.overview, 'OverviewPage')),
  previewChild('inbox', lazyRouteComponent(previewPages.overview, 'InboxPage')),
  previewChild('runs/$runId', lazyRouteComponent(previewPages.run, 'RunPage')),
  previewChild('agents/new', lazyRouteComponent(previewPages.agents, 'NewAgentGalleryPage')),
  previewChild('agents/new/template', lazyRouteComponent(previewPages.agents, 'NewAgentTemplatePage')),
  previewChild('agents/new/blank', lazyRouteComponent(previewPages.agents, 'NewAgentBlankPage')),
  previewChild('agents/$agentId', lazyRouteComponent(previewPages.agents, 'AgentPage')),
  previewChild('agents/$agentId/rollback', lazyRouteComponent(previewPages.agents, 'AgentRollbackPage')),
  previewChild('marketplace', lazyRouteComponent(previewPages.marketplace, 'MarketplacePage')),
  previewChild('skills/add', lazyRouteComponent(previewPages.marketplace, 'SkillAddPage')),
  previewChild('skills/$skillId', lazyRouteComponent(previewPages.marketplace, 'SkillDetailPage')),
  previewChild('providers', lazyRouteComponent(previewPages.providers, 'ProvidersPage')),
  previewChild('providers/add', lazyRouteComponent(previewPages.providers, 'ProviderAddPage')),
  previewChild('prices', lazyRouteComponent(previewPages.providers, 'PricesPage')),
  previewChild('secrets', lazyRouteComponent(previewPages.providers, 'SecretsPage')),
  previewChild('spend', lazyRouteComponent(previewPages.providers, 'SpendPage')),
  previewChild('platform/orgs', lazyRouteComponent(previewPages.platform, 'PlatformOrgsPage')),
  previewChild('platform/usage', lazyRouteComponent(previewPages.platform, 'PlatformUsagePage')),
  previewChild('platform/shared', lazyRouteComponent(previewPages.platform, 'PlatformSharedPage')),
  previewChild('platform/settings', lazyRouteComponent(previewPages.platform, 'PlatformSettingsPage')),
  previewChild('platform/agents', lazyRouteComponent(previewPages.platform, 'PlatformAgentsPage')),
  previewChild('welcome', lazyRouteComponent(previewPages.onboarding, 'FirstLoginPage')),
  previewChild('idion', lazyRouteComponent(previewPages.onboarding, 'IdionHomePage')),
  previewChild('idion/welcome', lazyRouteComponent(previewPages.onboarding, 'IdionFirstRunPage')),
]

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
    tokensRoute,
  ]),
  previewRoute.addChildren(previewRoutes),
])

export function createAppRouter(queryClient: QueryClient, history?: RouterHistory) {
  return createRouter({ routeTree, context: { queryClient }, history, defaultPreload: false })
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>
  }
}
