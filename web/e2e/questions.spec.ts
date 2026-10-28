// SPDX-License-Identifier: Apache-2.0

import { mkdirSync } from 'node:fs'
import { expect, test } from '@playwright/test'
import { mockApi, type Stream } from './mock-api.ts'

const org = 'org-1'
// Screenshots for the pull request; not committed.
const shots = process.env.INBOX_SCREENSHOTS

test('an agent asks a question: answer it from the inbox with a choice, an edited draft, or dismiss it', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  await page.emulateMedia({ colorScheme: 'light' })
  if (shots) mkdirSync(shots, { recursive: true })

  const now = () => new Date().toISOString()
  const answers: Record<string, unknown>[] = []
  const dismissals: Record<string, unknown>[] = []
  const open = new Set(['q-1', 'q-2', 'q-3'])
  const questions = () =>
    [
      { id: 'q-1', taskId: 't-1', agentId: 'a-1', question: 'Which region should I deploy the app to?', choices: ['eu-west-1', 'us-east-1'], defaultAnswer: 'eu-west-1' },
      { id: 'q-2', taskId: 't-2', agentId: 'a-2', question: 'May I post this reply on the support thread?', draft: 'Thanks for the report. This is fixed in 1.2; please upgrade.', choices: [], defaultAnswer: 'Do not post' },
      { id: 'q-3', taskId: 't-3', agentId: 'a-2', question: 'What should the release title be?', choices: [], defaultAnswer: 'Keep the current title' },
    ]
      .filter((q) => open.has(q.id))
      .map((q) => ({ orgId: org, runId: 'r-1', askedTo: 'u-1', state: 'QUESTION_STATE_OPEN', canAnswer: true, openedAt: now(), ...q }))
  const tasks = [
    { id: 't-1', orgId: org, title: 'Deploy the app', state: 'TASK_STATE_INPUT_REQUIRED', agentId: 'a-1', createdBy: { userId: 'u-1' }, updatedAt: now() },
    { id: 't-2', orgId: org, title: 'Answer the support thread', state: 'TASK_STATE_INPUT_REQUIRED', agentId: 'a-2', createdBy: { userId: 'u-1' }, updatedAt: now() },
    { id: 't-3', orgId: org, title: 'Prepare release 1.2', state: 'TASK_STATE_INPUT_REQUIRED', agentId: 'a-2', createdBy: { userId: 'u-1' }, updatedAt: now() },
  ]
  const settle = (id: unknown) => {
    open.delete(String(id))
    const t = tasks.find((x) => `q-${x.id.slice(2)}` === id)
    if (t) t.state = 'TASK_STATE_WORKING'
  }

  await mockApi(page, {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_OWNER' }],
      }),
      'koinon.agent.v1alpha1.AgentService/ListAgents': () => ({
        agents: [
          { id: 'a-1', orgId: org, displayName: 'Deploy agent', status: 'AGENT_STATUS_ACTIVE' },
          { id: 'a-2', orgId: org, displayName: 'Support agent', status: 'AGENT_STATUS_ACTIVE' },
        ],
      }),
      'koinon.task.v1alpha1.TaskService/ListTasks': () => ({ tasks }),
      'koinon.approval.v1alpha1.ApprovalService/ListApprovals': () => ({ approvals: [] }),
      'koinon.question.v1alpha1.QuestionService/ListQuestions': () => ({ questions: questions() }),
      'koinon.question.v1alpha1.QuestionService/AnswerQuestion': (req) => {
        answers.push(req)
        settle(req.questionId)
        return { question: { id: req.questionId, state: 'QUESTION_STATE_ANSWERED' } }
      },
      'koinon.question.v1alpha1.QuestionService/DismissQuestion': (req) => {
        dismissals.push(req)
        settle(req.questionId)
        return { question: { id: req.questionId, state: 'QUESTION_STATE_DISMISSED' } }
      },
    },
    stream: { 'koinon.run.v1alpha1.RunService/WatchRun': (): Stream => ({ messages: [] }) },
  })

  await page.goto('/')
  const waiting = page.getByRole('list', { name: 'Waiting on you' })
  await expect(waiting.getByTestId('inbox-item')).toHaveCount(3)
  await expect(page.getByRole('navigation', { name: 'Main' }).getByTestId('inbox-badge')).toHaveText('3')
  await expect(waiting.getByText('Which region should I deploy the app to?')).toBeVisible()
  await expect(waiting.getByText(/Dismissing, or no answer in time/).first()).toContainText('eu-west-1')
  if (shots) await page.screenshot({ path: `${shots}/question-inbox.png` })

  // A choice.
  await waiting.getByRole('button', { name: 'us-east-1' }).click()
  await expect(waiting.getByTestId('inbox-item')).toHaveCount(2)
  expect(answers).toEqual([expect.objectContaining({ questionId: 'q-1', choice: 'us-east-1' })])

  // The draft, edited.
  const draft = waiting.getByRole('textbox', { name: 'Draft from the agent' })
  await expect(draft).toHaveValue(/fixed in 1\.2/)
  await draft.fill('Fixed in 1.2.1, thanks for the report.')
  if (shots) await page.screenshot({ path: `${shots}/question-draft.png` })
  await waiting.getByRole('button', { name: 'Use edited draft' }).click()
  await expect(waiting.getByTestId('inbox-item')).toHaveCount(1)
  expect(answers[1]).toEqual(expect.objectContaining({ questionId: 'q-2', draft: 'Fixed in 1.2.1, thanks for the report.' }))

  // Dismissed: the agent gets its own default.
  await waiting.getByRole('button', { name: 'Dismiss' }).click()
  await expect(page.getByText('Nothing is waiting on you.')).toBeVisible()
  expect(dismissals).toEqual([expect.objectContaining({ questionId: 'q-3' })])
})

test("a question to the org's admins shows who it waits on and who answered first", async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 900 })
  const now = Date.now()
  const iso = (offsetMs: number) => new Date(now + offsetMs).toISOString()
  const question = {
    id: 'q-1',
    orgId: org,
    taskId: 't-1',
    runId: 'r-1',
    agentId: 'a-1',
    question: 'Ship the report to finance?',
    choices: ['Yes', 'No'],
    defaultAnswer: 'No',
    state: 'QUESTION_STATE_OPEN',
    canAnswer: true,
    openedAt: iso(-2 * 3_600_000),
    waitingSince: iso(-2 * 3_600_000),
    target: { kind: 'NOTIFY_TARGET_KIND_ROLE', role: 'ORG_ROLE_ADMIN' },
    targetLabel: "the org's admins",
    recipients: [
      { userId: 'u-1', displayName: 'Admin', waitingSince: iso(-2 * 3_600_000) },
      { userId: 'u-2', displayName: 'Ana Example', waitingSince: iso(-2 * 3_600_000) },
    ],
  }
  await mockApi(page, {
    unary: {
      'koinon.auth.v1alpha1.AuthService/WhoAmI': () => ({
        user: { id: 'u-1', email: 'admin@example.test', displayName: 'Admin' },
        memberships: [{ orgId: org, orgSlug: 'default', orgDisplayName: 'Default', role: 'ORG_ROLE_ADMIN' }],
      }),
      'koinon.agent.v1alpha1.AgentService/ListAgents': () => ({ agents: [{ id: 'a-1', orgId: org, displayName: 'FinOps analyst', status: 'AGENT_STATUS_ACTIVE' }] }),
      'koinon.task.v1alpha1.TaskService/ListTasks': () => ({
        tasks: [{ id: 't-1', orgId: org, title: 'Daily cost report', state: 'TASK_STATE_INPUT_REQUIRED', agentId: 'a-1', createdBy: { kind: 'CREATOR_KIND_TRIGGER' }, updatedAt: iso(-60_000) }],
      }),
      'koinon.approval.v1alpha1.ApprovalService/ListApprovals': () => ({ approvals: [] }),
      'koinon.question.v1alpha1.QuestionService/ListQuestions': () => ({ questions: [question] }),
      'koinon.question.v1alpha1.QuestionService/AnswerQuestion': () => ({ error: { code: 'aborted', message: 'Ana Example already answered this question' } }),
    },
    stream: {},
  })
  await page.goto('/')
  const row = page.getByRole('list', { name: 'Waiting on you' }).getByTestId('inbox-item')
  await expect(row).toContainText("Asked to the org's admins (2 people")
  await expect(row).toContainText('waiting 2 hours')
  await row.getByRole('button', { name: 'Yes' }).click()
  await expect(row.getByRole('alert')).toContainText('Ana Example already answered this question')
})
