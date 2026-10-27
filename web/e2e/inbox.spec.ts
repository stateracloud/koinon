// SPDX-License-Identifier: Apache-2.0

import { mkdirSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import { mockApi, type Stream } from './mock-api.ts'

const org = 'org-1'
const now = () => new Date().toISOString()
const sha = 'f3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855'
// Screenshots for the pull request; not committed.
const shots = process.env.INBOX_SCREENSHOTS

test('the inbox is home: approve inline, chat with an agent, and see the board by state', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.emulateMedia({ colorScheme: 'light' })
  if (shots) mkdirSync(shots, { recursive: true })

  const approved: Record<string, unknown>[] = []
  const messages: Record<string, unknown>[] = []
  let pending = true
  let t1 = 'TASK_STATE_INPUT_REQUIRED'
  const at = Math.floor(Date.now() / 1000)
  const tasks = () => [
    { id: 't-1', orgId: org, title: 'Fix the README typo', state: t1, agentId: 'a-1', updatedAt: new Date((at - 60) * 1000).toISOString() },
    { id: 't-2', orgId: org, title: 'Write release notes', state: 'TASK_STATE_WORKING', agentId: 'a-2', updatedAt: new Date((at - 30) * 1000).toISOString() },
    { id: 't-3', orgId: org, title: 'Triage new issues', state: 'TASK_STATE_SUBMITTED', agentId: 'a-2', updatedAt: new Date((at - 20) * 1000).toISOString() },
    { id: 't-4', orgId: org, title: 'Rename the repo', state: 'TASK_STATE_COMPLETED', agentId: 'a-1', updatedAt: new Date((at - 900) * 1000).toISOString() },
    { id: 't-5', orgId: org, title: 'Which branch should releases come from?', state: 'TASK_STATE_INPUT_REQUIRED', agentId: 'a-2', createdBy: { userId: 'u-1' }, updatedAt: new Date((at - 120) * 1000).toISOString() },
    { id: 't-6', orgId: org, title: 'Nightly dependency check', state: 'TASK_STATE_FAILED', agentId: 'a-2', updatedAt: new Date((at - 3000) * 1000).toISOString() },
    { id: 't-7', orgId: org, title: 'Old experiment', state: 'TASK_STATE_CANCELED', agentId: 'a-1', updatedAt: new Date((at - 6000) * 1000).toISOString() },
  ]
  const events = [
    { seq: '1', kind: 'task.created', actorKind: 'ACTOR_KIND_SYSTEM', payload: { title: 'Write release notes' }, createdAt: now() },
    { seq: '2', kind: 'message', actorKind: 'ACTOR_KIND_USER', payload: { text: 'Summarise this week into release notes.' }, createdAt: now() },
  ]
  const approval = () => ({
    id: 'ap-1',
    orgId: org,
    ticketId: 't-1',
    runId: 'r-1',
    agentId: 'a-1',
    connector: 'github',
    tool: 'create_pull_request',
    target: 'github:acme/widgets',
    argsSha256: sha,
    decision: 'DECISION_ASK',
    status: 'STATUS_PENDING',
    canDecide: true,
    approvalsRequired: 1,
    createdAt: now(),
  })

  await mockApi(page, {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_OWNER' }],
      }),
      'koinon.agent.v1alpha1.AgentService/ListAgents': () => ({
        agents: [
          { id: 'a-1', orgId: org, displayName: 'Repo helper', status: 'AGENT_STATUS_ACTIVE' },
          { id: 'a-2', orgId: org, displayName: 'Docs writer', status: 'AGENT_STATUS_ACTIVE' },
        ],
      }),
      'koinon.task.v1alpha1.TaskService/ListTasks': () => ({ tasks: tasks() }),
      'koinon.task.v1alpha1.TaskService/GetTask': (req) => ({
        task: tasks().find((t) => t.id === req.taskId),
        events: req.taskId === 't-2' ? events : [],
        runs: [],
      }),
      'koinon.task.v1alpha1.TaskService/AppendMessage': (req) => {
        messages.push(req)
        events.push({ seq: String(events.length + 1), kind: 'message', actorKind: 'ACTOR_KIND_USER', payload: { text: String(req.text) }, createdAt: now() })
        return { event: events.at(-1) }
      },
      'koinon.approval.v1alpha1.ApprovalService/ListApprovals': () => ({ approvals: pending ? [approval()] : [] }),
      'koinon.approval.v1alpha1.ApprovalService/Approve': (req) => {
        approved.push(req)
        pending = false
        t1 = 'TASK_STATE_WORKING'
        return { approval: { ...approval(), status: 'STATUS_APPROVED', canDecide: false } }
      },
    },
    stream: { 'koinon.run.v1alpha1.RunService/WatchRun': (): Stream => ({ messages: [] }) },
  })

  await page.goto('/')
  await expect(page.getByRole('heading', { level: 1, name: 'Inbox' })).toBeVisible()

  // Waiting on you: an approval and a question-shaped task, with a badge in the navigation.
  const waiting = page.getByRole('list', { name: 'Waiting on you' })
  await expect(waiting.getByTestId('inbox-item')).toHaveCount(2)
  await expect(waiting.getByRole('link', { name: 'Which branch should releases come from? needs your reply' })).toBeVisible()
  const nav = page.getByRole('navigation', { name: 'Main' })
  await expect(nav.getByTestId('inbox-badge')).toHaveText('2')

  // Chat with an agent next to it.
  await page.getByRole('list', { name: 'Conversations' }).getByRole('button', { name: /Write release notes/ }).click()
  const chat = page.getByRole('region', { name: 'Conversation: Write release notes' })
  await expect(chat.getByText('Summarise this week into release notes.')).toBeVisible()
  await chat.getByRole('textbox', { name: 'Message' }).fill('Include the migration notes.')
  await chat.getByRole('button', { name: 'Send' }).click()
  await expect(chat.getByText('Include the migration notes.')).toBeVisible()
  expect(messages).toEqual([expect.objectContaining({ taskId: 't-2', text: 'Include the migration notes.' })])
  if (shots) await page.screenshot({ path: `${shots}/inbox.png` })

  // Approve inline: the item leaves the inbox and the badge drops.
  await waiting.getByRole('button', { name: 'Approve create_pull_request on acme/widgets' }).click()
  await expect(waiting.getByTestId('inbox-item')).toHaveCount(1)
  await expect(nav.getByTestId('inbox-badge')).toHaveText('1')
  expect(approved).toEqual([expect.objectContaining({ approvalId: 'ap-1', argsSha256: sha })])

  // The board is a secondary view with the task states.
  await nav.getByRole('link', { name: 'Board' }).click()
  for (const [state, title] of [
    ['Submitted', 'Triage new issues'],
    ['Working', 'Fix the README typo'],
    ['Input required', 'Which branch should releases come from?'],
    ['Completed', 'Rename the repo'],
    ['Canceled', 'Old experiment'],
    ['Failed', 'Nightly dependency check'],
  ]) {
    await expect(page.getByRole('region', { name: new RegExp(`^${state}`) }).getByRole('link', { name: title })).toBeVisible()
  }
  if (shots) await page.screenshot({ path: `${shots}/board.png` })
})
