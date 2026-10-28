// SPDX-License-Identifier: Apache-2.0

import { expect, test } from '@playwright/test'
import { mockApi, type Stream } from './mock-api.ts'

const org = 'org-1'
const now = () => new Date().toISOString()

test('sign in, create a task and watch the run stream, across a dropped connection', async ({ page }) => {
  let signedIn = false
  let state = 'TASK_STATE_SUBMITTED'
  const events: Record<string, unknown>[] = []
  const watchCalls: string[] = []
  const task = () => ({ id: 't-1', orgId: org, title: 'Say hello', state, agentId: 'a-1', lastEventSeq: String(events.length) })
  const run = (state: string) => ({ id: 'r-1', orgId: org, ticketId: 't-1', agentId: 'a-1', state })
  const chunk = (seq: number, text: string) => ({
    event: {
      runId: 'r-1',
      seq: String(seq),
      kind: 'acp.agent_message_chunk',
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
      'koinon.approval.v1alpha1.ApprovalService/ListApprovals': () => ({ approvals: [] }),
      'koinon.task.v1alpha1.TaskService/ListTasks': () => ({ tasks: events.length ? [task()] : [] }),
      'koinon.task.v1alpha1.TaskService/CreateTask': (req) => {
        expect(req).toMatchObject({ orgId: org, agentId: 'a-1', title: 'Say hello', message: 'Please say hello' })
        addEvent('task.created', { title: 'Say hello' }, 'ACTOR_KIND_SYSTEM')
        addEvent('message', { text: 'Please say hello' })
        addEvent('run.queued', {}, 'ACTOR_KIND_SYSTEM')
        state = 'TASK_STATE_WORKING'
        return { task: task(), run: run('RUN_STATE_QUEUED') }
      },
      'koinon.task.v1alpha1.TaskService/GetTask': () => ({ task: task(), events, runs: [run('RUN_STATE_RUNNING')] }),
      'koinon.task.v1alpha1.TaskService/AppendMessage': (req) => {
        addEvent('message', { text: req.text })
        return { event: events.at(-1)! }
      },
      'koinon.task.v1alpha1.TaskService/CancelTask': () => {
        state = 'TASK_STATE_CANCELED'
        return { task: task() }
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

  // The inbox is the home screen; chat with agents sits next to it.
  await expect(page.getByRole('heading', { level: 1, name: 'Inbox' })).toBeVisible()
  await expect(page.getByRole('heading', { level: 2, name: /^Waiting on you/ })).toBeVisible()
  await expect(page.getByRole('heading', { level: 2, name: 'Chat with agents' })).toBeVisible()
  await page.getByLabel('Title').fill('Say hello')
  await page.getByLabel('First message (optional)').fill('Please say hello')
  await page.getByRole('button', { name: 'Create task' }).click()

  // The new conversation opens next to the inbox and streams the run, across the dropped connection.
  const chat = page.getByRole('region', { name: 'Conversation: Say hello' })
  await expect(chat).toBeVisible()
  const chatTimeline = chat.getByRole('list', { name: 'Timeline' })
  await expect(chatTimeline.getByText('Please say hello')).toBeVisible()
  await expect(chatTimeline.getByText('Hello from the fake model', { exact: true })).toBeVisible()
  expect(watchCalls).toEqual(['0', '2'])

  await page.getByRole('link', { name: 'Open task' }).click()
  await expect(page).toHaveURL(/\/tasks\/t-1$/)
  await expect(page.getByRole('heading', { name: 'Say hello' })).toBeVisible()
  const timeline = page.getByRole('list', { name: 'Timeline' })
  await expect(timeline.getByText('Please say hello')).toBeVisible()

  await page.getByRole('textbox', { name: 'Message' }).fill('Thanks!')
  await page.getByRole('button', { name: 'Send' }).click()
  await expect(timeline.getByText('Thanks!')).toBeVisible()

  // The board is a secondary view by state.
  await page.getByRole('link', { name: '← Inbox' }).click()
  await page.getByRole('navigation', { name: 'Main' }).getByRole('link', { name: 'Board' }).click()
  const working = page.getByRole('region', { name: /^Working/ })
  await expect(working.getByRole('link', { name: 'Say hello' })).toBeVisible()
  await working.getByRole('link', { name: 'Say hello' }).click()
  await page.getByRole('button', { name: 'Cancel task' }).click()
  await expect(page.getByText('Canceled', { exact: true })).toBeVisible()
  expect(state).toBe('TASK_STATE_CANCELED')
})
