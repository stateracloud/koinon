// SPDX-License-Identifier: Apache-2.0

import { expect, test } from '@playwright/test'
import { mockApi, type Stream } from './mock-api.ts'

const org = 'org-1'
const now = () => new Date().toISOString()

test('sign in, create a ticket and watch the run stream, across a dropped connection', async ({ page }) => {
  let signedIn = false
  let status = 'TICKET_STATUS_TODO'
  const events: Record<string, unknown>[] = []
  const watchCalls: string[] = []
  const ticket = () => ({ id: 't-1', orgId: org, title: 'Say hello', status, agentId: 'a-1', lastEventSeq: String(events.length) })
  const run = (state: string) => ({ id: 'r-1', orgId: org, ticketId: 't-1', agentId: 'a-1', state })
  const chunk = (seq: number, text: string) => ({
    event: {
      runId: 'r-1',
      seq: String(seq),
      kind: 'session.update',
      payload: { sessionUpdate: 'agent_message_chunk', content: { type: 'text', text } },
      createdAt: now(),
    },
  })
  const addEvent = (kind: string, payload: Record<string, unknown>, actorKind = 'ACTOR_KIND_USER') =>
    events.push({ seq: String(events.length + 1), kind, actorKind, payload, createdAt: now() })

  await page.route('/auth/methods', (r) => r.fulfill({ json: { password: true, oidc: true } }))
  await page.route('/auth/login', async (r) => {
    expect(r.request().postDataJSON()).toEqual({ username: 'admin', password: 'correct-horse-battery' })
    signedIn = true
    await r.fulfill({ json: { passwordChangeRequired: false } })
  })

  await mockApi(page, {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () =>
        signedIn
          ? {
              user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
              memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_OWNER' }],
              authMethod: 'AUTH_METHOD_PASSWORD',
            }
          : { error: { code: 'unauthenticated', message: 'no session' } },
      'koinon.agent.v1alpha1.AgentService/ListAgents': () => ({
        agents: [
          { id: 'a-1', orgId: org, displayName: 'Repo helper', status: 'AGENT_STATUS_ACTIVE' },
          { id: 'a-2', orgId: org, displayName: 'Draft bot', status: 'AGENT_STATUS_DRAFT' },
        ],
      }),
      'koinon.ticket.v1alpha1.TicketService/ListTickets': () => ({ tickets: events.length ? [ticket()] : [] }),
      'koinon.ticket.v1alpha1.TicketService/CreateTicket': (req) => {
        expect(req).toMatchObject({ orgId: org, agentId: 'a-1', title: 'Say hello', message: 'Please say hello' })
        addEvent('ticket.created', { title: 'Say hello' }, 'ACTOR_KIND_SYSTEM')
        addEvent('message', { text: 'Please say hello' })
        addEvent('run.queued', {}, 'ACTOR_KIND_SYSTEM')
        status = 'TICKET_STATUS_IN_PROGRESS'
        return { ticket: ticket(), run: run('RUN_STATE_QUEUED') }
      },
      'koinon.ticket.v1alpha1.TicketService/GetTicket': () => ({ ticket: ticket(), events, runs: [run('RUN_STATE_RUNNING')] }),
      'koinon.ticket.v1alpha1.TicketService/AppendMessage': (req) => {
        addEvent('message', { text: req.text })
        return { event: events.at(-1)! }
      },
      'koinon.ticket.v1alpha1.TicketService/MoveTicket': (req) => {
        status = String(req.status)
        return { ticket: ticket() }
      },
    },
    stream: {
      'koinon.run.v1alpha1.RunService/WatchRun': (req): Stream => {
        watchCalls.push(String(req.afterSeq ?? '0'))
        if (watchCalls.length === 1) {
          return {
            messages: [
              { run: run('RUN_STATE_RUNNING') },
              { event: { runId: 'r-1', seq: '1', kind: 'run.queued', createdAt: now() } },
              chunk(2, 'Hello'),
            ],
            error: { code: 'unavailable', message: 'connection reset' },
          }
        }
        return { messages: [{ run: run('RUN_STATE_RUNNING') }, chunk(2, 'Hello'), chunk(3, ' from the fake model'), { run: run('RUN_STATE_SUCCEEDED') }] }
      },
    },
  })

  await page.goto('/')
  await expect(page.getByRole('heading', { name: 'Sign in to Koinon' })).toBeVisible()
  await expect(page.getByRole('link', { name: 'Sign in with SSO' })).toHaveAttribute('href', '/auth/oidc/start')
  await page.getByLabel('Password').fill('correct-horse-battery')
  await page.getByRole('button', { name: 'Sign in' }).click()

  for (const col of ['To do', 'In progress', 'Waiting on you', 'Done']) {
    await expect(page.getByRole('heading', { level: 2, name: new RegExp(`^${col}`) })).toBeVisible()
  }
  await page.getByLabel('Title').fill('Say hello')
  await page.getByLabel('First message (optional)').fill('Please say hello')
  await page.getByRole('button', { name: 'Create ticket' }).click()

  await expect(page).toHaveURL(/\/tickets\/t-1$/)
  await expect(page.getByRole('heading', { name: 'Say hello' })).toBeVisible()
  const timeline = page.getByRole('list', { name: 'Timeline' })
  await expect(timeline.getByText('Please say hello')).toBeVisible()
  await expect(timeline.getByText('Hello from the fake model', { exact: true })).toBeVisible()
  expect(watchCalls).toEqual(['0', '2'])

  await page.getByRole('textbox', { name: 'Message' }).fill('Thanks!')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(timeline.getByText('Thanks!')).toBeVisible()

  await page.getByRole('link', { name: '← Board' }).click()
  const inProgress = page.getByRole('region', { name: /^In progress/ })
  await expect(inProgress.getByRole('link', { name: 'Say hello' })).toBeVisible()
  await page.getByRole('button', { name: 'Move "Say hello" to Done' }).click()
  await expect(page.getByRole('region', { name: /^Done/ }).getByRole('link', { name: 'Say hello' })).toBeVisible()
  expect(status).toBe('TICKET_STATUS_DONE')
})
