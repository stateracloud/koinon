// SPDX-License-Identifier: Apache-2.0

import type { QueryClient } from '@tanstack/react-query'
import { createRootRouteWithContext, createRoute, createRouter, Outlet, type RouterHistory } from '@tanstack/react-router'
import { AppLayout } from '@/pages/app-layout'
import { BoardPage } from '@/pages/board-page'
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

const routeTree = rootRoute.addChildren([loginRoute, appRoute.addChildren([boardRoute, ticketRoute])])

export function createAppRouter(queryClient: QueryClient, history?: RouterHistory) {
  return createRouter({ routeTree, context: { queryClient }, history, defaultPreload: false })
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof createAppRouter>
  }
}
