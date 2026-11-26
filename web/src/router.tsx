// SPDX-License-Identifier: Apache-2.0

import type { QueryClient } from '@tanstack/react-query'
import { createRootRouteWithContext, createRoute, createRouter, Outlet, redirect, type RouterHistory } from '@tanstack/react-router'
import { AgentPage } from '@/pages/agent-page'
import { AppLayout, OrgArea } from '@/pages/app-layout'
import { ApprovalPage } from '@/pages/approval-page'
import { InboxPage } from '@/pages/inbox-page'
import { OrgSettingsPage } from '@/pages/org-settings-page'
import { OverviewPage } from '@/pages/overview-page'
import { parseInboxTab, type InboxTab } from '@/lib/inbox'
import { parseOverviewTab, type OverviewTab } from '@/lib/overview'
import { PlatformConsolePage } from '@/pages/platform-console-page'
import { PlatformLayout } from '@/pages/platform-layout'
import { PlatformOrgsPage } from '@/pages/platform-orgs-page'
import { PlatformProvidersPage } from '@/pages/platform-providers-page'
import { PlatformTemplatesPage } from '@/pages/platform-templates-page'
import { PlatformMcpServersPage } from '@/pages/platform-mcp-servers-page'
import { ConnectionPage } from '@/pages/connection-page'
import { ConnectionsPage } from '@/pages/connections-page'
import { type GitHubLinkResult, parseGitHubLinkResult } from '@/lib/github-link'
import { BudgetsPage } from '@/pages/budgets-page'
import { PlatformBudgetsPage } from '@/pages/platform-budgets-page'
import { PricesPage } from '@/pages/prices-page'
import { ProvidersPage } from '@/pages/providers-page'
import { NewServerPage } from '@/pages/new-server-page'
import { ServerPage } from '@/pages/server-page'
import { ServersPage } from '@/pages/servers-page'
import { TokensPage } from '@/pages/tokens-page'
import { BlankAgentPage, NewAgentPage, TemplateAgentPage } from '@/pages/new-agent-page'
import { NewGiteaConnectionPage } from '@/pages/new-gitea-connection-page'
import { GitHubAppCreatedPage, GitHubAppInstalledPage } from '@/pages/github-app-setup-pages'
import { NewGitHubConnectionPage } from '@/pages/new-github-connection-page'
import { LoginPage } from '@/pages/login-page'
import { TaskPage } from '@/pages/task-page'
import { ObservabilityPage } from '@/pages/observability-page'
import { PlatformObservabilityPage } from '@/pages/platform-observability-page'
import { PlatformPricesPage } from '@/pages/platform-prices-page'
import { PlatformUsagePage } from '@/pages/platform-usage-page'

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

/** The org area: the org sidebar and the pages of one org (ADR 0126 section 4). */
const orgRoute = createRoute({ getParentRoute: () => appRoute, id: 'org', component: OrgArea })

// The org home (ADR 0126 section 4). The inbox was the home until then: its link is on the overview and in the sidebar.
export const overviewRoute = createRoute({
  getParentRoute: () => orgRoute,
  path: '/',
  validateSearch: (s: Record<string, unknown>): { tab?: OverviewTab } => {
    const tab = parseOverviewTab(s.tab)
    return tab ? { tab } : {}
  },
  component: OverviewPage,
})

export const inboxRoute = createRoute({
  getParentRoute: () => orgRoute,
  path: '/inbox',
  validateSearch: (s: Record<string, unknown>): { tab?: InboxTab } => {
    const tab = parseInboxTab(s.tab)
    return tab && tab !== 'blocked' ? { tab } : {}
  },
  component: InboxPage,
})

// The board became the Board tab of the overview (ADR 0126 section 4).
export const boardRedirectRoute = createRoute({
  getParentRoute: () => orgRoute,
  path: '/board',
  beforeLoad: () => {
    throw redirect({ to: '/', search: { tab: 'board' }, replace: true })
  },
})

export const taskRoute = createRoute({
  getParentRoute: () => orgRoute,
  path: '/tasks/$taskId',
  component: TaskPage,
})

// Tickets became tasks with the same ids (ADR 0065): old links keep working.
export const ticketRedirectRoute = createRoute({
  getParentRoute: () => orgRoute,
  path: '/tickets/$ticketId',
  beforeLoad: ({ params }) => {
    throw redirect({ to: '/tasks/$taskId', params: { taskId: params.ticketId }, replace: true })
  },
})

export const approvalRoute = createRoute({ getParentRoute: () => orgRoute, path: '/approvals/$approvalId', component: ApprovalPage })

// The agent list became the Agents tab of the overview (issue #1274): the cards by team replace the table.
export const agentsRedirectRoute = createRoute({
  getParentRoute: () => orgRoute,
  path: '/agents',
  beforeLoad: () => {
    throw redirect({ to: '/', replace: true })
  },
})
// New agent (ADR 0126 section 5): the gallery, then a template's wizard or the blank form.
export const newAgentRoute = createRoute({ getParentRoute: () => orgRoute, path: '/agents/new', component: NewAgentPage })
export const newBlankAgentRoute = createRoute({ getParentRoute: () => orgRoute, path: '/agents/new/blank', component: BlankAgentPage })
export const newTemplateAgentRoute = createRoute({ getParentRoute: () => orgRoute, path: '/agents/new/template/$templateId', component: TemplateAgentPage })
export const agentRoute = createRoute({ getParentRoute: () => orgRoute, path: '/agents/$agentId', component: AgentPage })

export const connectionsRoute = createRoute({
  getParentRoute: () => orgRoute,
  path: '/connections',
  // github_link is the result of the GitHub link callback (internal/githublink).
  validateSearch: (s: Record<string, unknown>): { github_link?: GitHubLinkResult } => {
    const r = parseGitHubLinkResult(s.github_link)
    return r ? { github_link: r } : {}
  },
  component: ConnectionsPage,
})
export const newGitHubConnectionRoute = createRoute({
  getParentRoute: () => orgRoute,
  path: '/connections/new/github',
  component: NewGitHubConnectionPage,
})
const searchString = (v: unknown) => (typeof v === 'string' && v.length <= 512 ? v : undefined)
// GitHub sends the browser here during the guided App setup (issue #1175).
export const githubAppCreatedRoute = createRoute({
  getParentRoute: () => orgRoute,
  path: '/connections/new/github/app-created',
  validateSearch: (s: Record<string, unknown>): { code?: string; state?: string } => ({ code: searchString(s.code), state: searchString(s.state) }),
  component: GitHubAppCreatedPage,
})
export const githubAppInstalledRoute = createRoute({
  getParentRoute: () => orgRoute,
  path: '/connections/new/github/app-installed',
  validateSearch: (s: Record<string, unknown>): { installation_id?: string; setup_action?: string; state?: string } => ({
    // A router may parse a numeric parameter as a number.
    installation_id: typeof s.installation_id === 'number' ? String(s.installation_id) : searchString(s.installation_id),
    setup_action: searchString(s.setup_action),
    state: searchString(s.state),
  }),
  component: GitHubAppInstalledPage,
})
export const newGiteaConnectionRoute = createRoute({
  getParentRoute: () => orgRoute,
  path: '/connections/new/gitea',
  component: NewGiteaConnectionPage,
})
export const connectionRoute = createRoute({ getParentRoute: () => orgRoute, path: '/connections/$connectionId', component: ConnectionPage })

export const providersRoute = createRoute({ getParentRoute: () => orgRoute, path: '/settings/providers', component: ProvidersPage })
export const orgSettingsRoute = createRoute({ getParentRoute: () => orgRoute, path: '/settings/org', component: OrgSettingsPage })
export const mcpServersRoute = createRoute({ getParentRoute: () => orgRoute, path: '/mcp-servers', component: ServersPage })
export const newMcpServerRoute = createRoute({ getParentRoute: () => orgRoute, path: '/mcp-servers/new', component: NewServerPage })
export const mcpServerRoute = createRoute({ getParentRoute: () => orgRoute, path: '/mcp-servers/$serverId', component: ServerPage })
// The org MCP servers page moved under Settings (ADR 0126 section 6).
export const serversRedirectRoute = createRoute({
  getParentRoute: () => orgRoute,
  path: '/servers',
  beforeLoad: () => {
    throw redirect({ to: '/mcp-servers', replace: true })
  },
})
export const pricesRoute = createRoute({ getParentRoute: () => orgRoute, path: '/settings/prices', component: PricesPage })
export const budgetsRoute = createRoute({ getParentRoute: () => orgRoute, path: '/settings/budgets', component: BudgetsPage })
export const observabilityRoute = createRoute({ getParentRoute: () => orgRoute, path: '/settings/observability', component: ObservabilityPage })
export const tokensRoute = createRoute({ getParentRoute: () => orgRoute, path: '/settings/tokens', component: TokensPage })

/** The platform console: its own area with its own sidebar, for platform admins only (ADR 0126 section 4). */
export const platformRoute = createRoute({ getParentRoute: () => appRoute, path: '/platform', component: PlatformLayout })
export const platformHomeRoute = createRoute({ getParentRoute: () => platformRoute, path: '/', component: PlatformConsolePage })
export const platformOrgsRoute = createRoute({ getParentRoute: () => platformRoute, path: '/orgs', component: PlatformOrgsPage })
export const platformUsageRoute = createRoute({ getParentRoute: () => platformRoute, path: '/usage', component: PlatformUsagePage })
export const platformBudgetsRoute = createRoute({ getParentRoute: () => platformRoute, path: '/usage/budgets', component: PlatformBudgetsPage })
export const platformPricesRoute = createRoute({ getParentRoute: () => platformRoute, path: '/usage/prices', component: PlatformPricesPage })
export const platformProvidersRoute = createRoute({ getParentRoute: () => platformRoute, path: '/providers', component: PlatformProvidersPage })
export const platformTemplatesRoute = createRoute({ getParentRoute: () => platformRoute, path: '/templates', component: PlatformTemplatesPage })
export const platformMcpServersRoute = createRoute({ getParentRoute: () => platformRoute, path: '/mcp-servers', component: PlatformMcpServersPage })
export const platformObservabilityRoute = createRoute({
  getParentRoute: () => platformRoute,
  path: '/settings/observability',
  component: PlatformObservabilityPage,
})
/** The platform pages had these URLs before the console had its own area; old links and bookmarks still work. */
const platformRedirects = (
  [
    ['/budgets', '/platform/usage/budgets'],
    ['/prices', '/platform/usage/prices'],
    ['/observability', '/platform/settings/observability'],
    ['/settings', '/platform/settings/observability'],
  ] as const
).map(([path, to]) =>
  createRoute({
    getParentRoute: () => platformRoute,
    path,
    beforeLoad: () => {
      throw redirect({ to, replace: true })
    },
  }),
)

const routeTree = rootRoute.addChildren([
  loginRoute,
  appRoute.addChildren([
    orgRoute.addChildren([
      overviewRoute,
      inboxRoute,
      boardRedirectRoute,
      taskRoute,
      ticketRedirectRoute,
      approvalRoute,
      agentsRedirectRoute,
      newAgentRoute,
      newBlankAgentRoute,
      newTemplateAgentRoute,
      agentRoute,
      connectionsRoute,
      newGitHubConnectionRoute,
      githubAppCreatedRoute,
      githubAppInstalledRoute,
      newGiteaConnectionRoute,
      connectionRoute,
      providersRoute,
      orgSettingsRoute,
      mcpServersRoute,
      newMcpServerRoute,
      mcpServerRoute,
      serversRedirectRoute,
      pricesRoute,
      budgetsRoute,
      observabilityRoute,
      tokensRoute,
    ]),
    platformRoute.addChildren([
      platformHomeRoute,
      platformOrgsRoute,
      platformUsageRoute,
      platformBudgetsRoute,
      platformPricesRoute,
      platformProvidersRoute,
      platformTemplatesRoute,
      platformMcpServersRoute,
      platformObservabilityRoute,
      ...platformRedirects,
    ]),
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
