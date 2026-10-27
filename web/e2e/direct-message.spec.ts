// SPDX-License-Identifier: Apache-2.0

import { mkdirSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import { mockApi, type Stream } from './mock-api.ts'

const org = 'org-1'
// Screenshots for the pull request; not committed.
const shots = process.env.INBOX_SCREENSHOTS

test('direct messages: start a thread, reply in it, reopen a completed one, and an agent that takes none', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.emulateMedia({ colorScheme: 'light' })
  if (shots) mkdirSync(shots, { recursive: true })

  const sends: Record<string, unknown>[] = []
  const now = () => new Date().toISOString()
  const at = Math.floor(Date.now() / 1000)
  const thread = (id: string, title: string, state: string, age: number, agentId = 'a-1') => ({
    id,
    orgId: org,
    title,
    state,
    agentId,
    directMessageThread: true,
    createdBy: { triggerId: 'trg-1', kind: 'CREATOR_KIND_TRIGGER' },
    updatedAt: new Date((at - age) * 1000).toISOString(),
  })
  const tasks = [
    thread('t-1', 'What changed in the repos today?', 'TASK_STATE_WORKING', 60),
    thread('t-2', 'Summarise last night’s incident', 'TASK_STATE_COMPLETED', 600),
  ]
  const events: Record<string, unknown[]> = {
    't-1': [
      { seq: '1', kind: 'task.created', actorKind: 'ACTOR_KIND_SYSTEM', payload: { title: 'What changed in the repos today?' }, createdAt: now() },
      { seq: '2', kind: 'message', actorKind: 'ACTOR_KIND_USER', payload: { text: 'Answer briefly.\n\nWhat changed in the repos today?', display: 'What changed in the repos today?' }, createdAt: now() },
    ],
    't-2': [{ seq: '1', kind: 'message', actorKind: 'ACTOR_KIND_USER', payload: { text: 'Summarise last night’s incident' }, createdAt: now() }],
  }

  await mockApi(page, {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_OWNER' }],
      }),
      'koinon.agent.v1alpha1.AgentService/ListAgents': () => ({
        agents: [
          { id: 'a-1', orgId: org, displayName: 'Repo helper', status: 'AGENT_STATUS_ACTIVE', approvedTriggers: [{ id: 'chat', type: 'TRIGGER_TYPE_DIRECT_MESSAGE' }] },
          { id: 'a-2', orgId: org, displayName: 'Docs writer', status: 'AGENT_STATUS_ACTIVE' },
        ],
      }),
      'koinon.approval.v1alpha1.ApprovalService/ListApprovals': () => ({ approvals: [] }),
      'koinon.task.v1alpha1.TaskService/ListTasks': () => ({ tasks }),
      'koinon.task.v1alpha1.TaskService/GetTask': (req) => ({ task: tasks.find((t) => t.id === req.taskId), events: events[String(req.taskId)] ?? [], runs: [] }),
      'koinon.task.v1alpha1.TaskService/SendDirectMessage': (req) => {
        sends.push(req)
        const text = String(req.text)
        if (req.taskId) {
          const t = tasks.find((x) => x.id === req.taskId)!
          const reopened = t.state === 'TASK_STATE_COMPLETED'
          if (reopened) t.state = 'TASK_STATE_SUBMITTED'
          const list = events[String(req.taskId)]
          list.push({ seq: String(list.length + 1), kind: 'message', actorKind: 'ACTOR_KIND_USER', payload: { text }, createdAt: now() })
          return { task: t, reopened }
        }
        const t = thread('t-3', text, 'TASK_STATE_SUBMITTED', 1)
        tasks.unshift(t)
        events['t-3'] = [{ seq: '1', kind: 'message', actorKind: 'ACTOR_KIND_USER', payload: { text: `Answer briefly.\n\n${text}`, display: text }, createdAt: now() }]
        return { task: t, created: true }
      },
    },
    stream: { 'koinon.run.v1alpha1.RunService/WatchRun': (): Stream => ({ messages: [] }) },
  })

  await page.goto('/')
  const chat = page.getByRole('region', { name: 'Conversation: What changed in the repos today?' })
  await expect(chat.getByText('What changed in the repos today?', { exact: true }).first()).toBeVisible()
  await expect(chat.getByText('Answer briefly.')).toHaveCount(0)
  await expect(chat.getByText('Working')).toBeVisible()
  if (shots) await page.screenshot({ path: `${shots}/chat-thread.png` })

  // A reply in the thread goes to the same task.
  await chat.getByRole('textbox', { name: 'Message' }).fill('And what about yesterday?')
  await chat.getByRole('button', { name: 'Send' }).click()
  await expect(chat.getByText('And what about yesterday?')).toBeVisible()
  expect(sends.at(-1)).toMatchObject({ agentId: 'a-1', taskId: 't-1', text: 'And what about yesterday?' })

  // A new thread with an agent that takes direct messages.
  const form = page.getByRole('region', { name: 'New thread' })
  await form.getByRole('textbox', { name: 'Message to agent' }).fill('Is the release branch green?')
  await form.getByRole('button', { name: 'Start thread' }).click()
  await expect(page.getByRole('region', { name: 'Conversation: Is the release branch green?' })).toBeVisible()
  expect(sends.at(-1)).toMatchObject({ agentId: 'a-1', text: 'Is the release branch green?' })
  expect(sends.at(-1)?.taskId ?? '').toBe('')

  // A completed thread reopens with a reply.
  await page.getByRole('list', { name: 'Conversations' }).getByRole('button', { name: /Summarise last night/ }).click()
  const done = page.getByRole('region', { name: /Conversation: Summarise last night/ })
  await expect(done.getByText('A reply reopens it as a follow-up.')).toBeVisible()
  if (shots) await page.screenshot({ path: `${shots}/chat-completed-thread.png` })
  await done.getByRole('textbox', { name: 'Message' }).fill('Was anyone paged?')
  await done.getByRole('button', { name: 'Send' }).click()
  await expect(done.getByText('Submitted')).toBeVisible()
  expect(sends.at(-1)).toMatchObject({ taskId: 't-2', text: 'Was anyone paged?' })

  // An agent without a direct-message trigger says so.
  await form.getByRole('combobox', { name: 'Send to' }).click()
  await page.getByRole('option', { name: 'Docs writer (no direct messages)' }).click()
  await expect(form.getByText('Docs writer does not take direct messages.', { exact: false })).toBeVisible()
  await expect(form.getByRole('textbox')).toHaveCount(0)
  if (shots) await page.screenshot({ path: `${shots}/chat-no-direct-messages.png` })
})
