// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { AuthService, type ChangePasswordRequest } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TicketService } from '@/gen/koinon/ticket/v1alpha1/ticket_pb'
import { renderApp, whoAmI } from '@/test-utils'

function json(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

/** stubFetch fakes the plain HTTP /auth endpoints. */
function stubFetch(methods: { password: boolean; oidc: boolean }, onLogin?: (body: unknown) => Response) {
  const calls: { url: string; init?: RequestInit }[] = []
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      calls.push({ url, init })
      if (url === '/auth/methods') return json(methods)
      if (url === '/auth/login') return onLogin?.(JSON.parse(String(init?.body))) ?? json({ passwordChangeRequired: false })
      if (url === '/auth/logout') return new Response(null, { status: 204 })
      return new Response('not found', { status: 404 })
    }),
  )
  return calls
}

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('authentication', () => {
  it('sends a visitor without a session to the login page and back after signing in', async () => {
    let signedIn = false
    const calls = stubFetch({ password: true, oidc: true }, (body) => {
      expect(body).toEqual({ username: 'admin', password: 'hunter2hunter2' })
      signedIn = true
      return json({ passwordChangeRequired: false })
    })
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, {
        whoAmI: () => {
          if (!signedIn) throw new ConnectError('no session', Code.Unauthenticated)
          return whoAmI()
        },
      })
      service(AgentService, { listAgents: () => ({ agents: [] }) })
      service(TicketService, { listTickets: () => ({ tickets: [] }) })
    })

    const { history } = renderApp(transport, '/tickets/t-9')
    expect(await screen.findByRole('heading', { name: 'Sign in to Koinon' })).toBeInTheDocument()
    expect(history.location.pathname).toBe('/login')
    const sso = await screen.findByRole('link', { name: 'Sign in with SSO' })
    expect(sso).toHaveAttribute('href', '/auth/oidc/start?return_to=%2Ftickets%2Ft-9')

    await userEvent.type(screen.getByLabelText('Password'), 'hunter2hunter2')
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    await waitFor(() => expect(history.location.pathname).toBe('/tickets/t-9'))
    expect(calls.find((c) => c.url === '/auth/login')?.init?.method).toBe('POST')
  })

  it('shows a login error', async () => {
    stubFetch({ password: true, oidc: false }, () => json({ error: 'invalid username or password' }, 401))
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, {
        whoAmI: () => {
          throw new ConnectError('no session', Code.Unauthenticated)
        },
      })
    })
    renderApp(transport, '/')
    await userEvent.type(await screen.findByLabelText('Password'), 'wrong')
    await userEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('invalid username or password')
    expect(screen.queryByRole('link', { name: 'Sign in with SSO' })).not.toBeInTheDocument()
  })

  it('forces the bootstrap password change before the board', async () => {
    stubFetch({ password: true, oidc: false })
    let required = true
    const changes: ChangePasswordRequest[] = []
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, {
        whoAmI: () => whoAmI({ passwordChangeRequired: required }),
        changePassword: (req) => {
          changes.push(req)
          required = false
          return {}
        },
      })
      service(AgentService, { listAgents: () => ({ agents: [] }) })
      service(TicketService, { listTickets: () => ({ tickets: [] }) })
    })
    renderApp(transport, '/')
    expect(await screen.findByRole('heading', { name: 'Choose a new password' })).toBeInTheDocument()
    expect(screen.queryByRole('region', { name: /^To do/ })).not.toBeInTheDocument()

    await userEvent.type(screen.getByLabelText('Current password'), 'bootstrap-pw')
    await userEvent.type(screen.getByLabelText('New password'), 'short')
    await userEvent.type(screen.getByLabelText('Repeat new password'), 'short')
    await userEvent.click(screen.getByRole('button', { name: 'Change password' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('at least 12 characters')
    expect(changes).toHaveLength(0)

    await userEvent.clear(screen.getByLabelText('New password'))
    await userEvent.clear(screen.getByLabelText('Repeat new password'))
    await userEvent.type(screen.getByLabelText('New password'), 'a-much-longer-password')
    await userEvent.type(screen.getByLabelText('Repeat new password'), 'a-much-longer-password')
    await userEvent.click(screen.getByRole('button', { name: 'Change password' }))

    expect(await screen.findByRole('region', { name: /^To do/ })).toBeInTheDocument()
    expect(changes[0]).toMatchObject({ currentPassword: 'bootstrap-pw', newPassword: 'a-much-longer-password' })
  })

  it('logs out', async () => {
    const calls = stubFetch({ password: true, oidc: false })
    let signedIn = true
    const transport = createRouterTransport(({ service }) => {
      service(AuthService, {
        whoAmI: () => {
          if (!signedIn) throw new ConnectError('no session', Code.Unauthenticated)
          return whoAmI()
        },
      })
      service(AgentService, { listAgents: () => ({ agents: [] }) })
      service(TicketService, { listTickets: () => ({ tickets: [] }) })
    })
    const { history } = renderApp(transport, '/')
    await userEvent.click(await screen.findByRole('button', { name: 'Log out' }))
    signedIn = false
    await waitFor(() => expect(history.location.pathname).toBe('/login'))
    expect(calls.some((c) => c.url === '/auth/logout' && c.init?.method === 'POST')).toBe(true)
  })
})
