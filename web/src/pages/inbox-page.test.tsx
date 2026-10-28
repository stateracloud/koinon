// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentService, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { ApprovalService, type ApproveRequest, type DenyRequest } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { QuestionService, QuestionState, type AnswerQuestionRequest, type DismissQuestionRequest } from '@/gen/koinon/question/v1alpha1/question_pb'
import { TaskService, TaskState, type AppendMessageRequest, type CreateTaskRequest } from '@/gen/koinon/task/v1alpha1/task_pb'
import { agent, approval, question, renderApp, task, whoAmI } from '@/test-utils'

type Opts = {
  role?: OrgRole
  approves?: ApproveRequest[]
  denies?: DenyRequest[]
  creates?: CreateTaskRequest[]
  messages?: AppendMessageRequest[]
  approveError?: ConnectError
}

function transport(opts: Opts = {}) {
  const tasks = [
    task('t-1', 'Fix the login bug', TaskState.INPUT_REQUIRED),
    task('t-2', 'Write release notes', TaskState.WORKING),
    task('t-3', 'Triage issues', TaskState.SUBMITTED, 'a-3'),
    task('t-4', 'Rename the repo', TaskState.COMPLETED),
    task('t-5', 'Stale idea', TaskState.CANCELED),
    task('t-6', 'Broken job', TaskState.FAILED),
    task('t-7', 'Which branch?', TaskState.INPUT_REQUIRED, 'a-3'),
  ]
  let approvals = [approval('ap-1', 't-1')]
  return createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI({ role: opts.role }) })
    service(AgentService, {
      listAgents: () => ({ agents: [agent('a-1', 'Repo helper'), agent('a-2', 'Draft bot', AgentStatus.DRAFT), agent('a-3', 'Docs writer')] }),
    })
    service(TaskService, {
      listTasks: () => ({ tasks }),
      getTask: (req) => ({ task: tasks.find((t) => t.id === req.taskId), events: [], runs: [] }),
      appendMessage: (req) => {
        opts.messages?.push(req)
        return {}
      },
      createTask: (req) => {
        opts.creates?.push(req)
        const t = task('t-new', req.title, TaskState.SUBMITTED, req.agentId)
        tasks.push(t)
        return { task: t }
      },
    })
    service(ApprovalService, {
      // The server lists pending approvals to every role; only admins and owners may decide them.
      listApprovals: () => ({ approvals: approvals.map((a) => approval(a.id, a.ticketId, { canDecide: (opts.role ?? OrgRole.OWNER) >= OrgRole.ADMIN })) }),
      approve: (req) => {
        if (opts.approveError) throw opts.approveError
        opts.approves?.push(req)
        approvals = []
        tasks[0].state = TaskState.WORKING
        return { approval: approval('ap-1', 't-1') }
      },
      deny: (req) => {
        opts.denies?.push(req)
        approvals = []
        tasks[0].state = TaskState.WORKING
        return { approval: approval('ap-1', 't-1') }
      },
    })
  })
}

const inbox = () => screen.getByRole('list', { name: 'Waiting on you' })

describe('Inbox home', () => {
  it('lists an approval and a task waiting for a reply, newest kinds pluggable by item kind', async () => {
    renderApp(transport(), '/')
    const list = await screen.findByRole('list', { name: 'Waiting on you' })
    const rows = within(list).getAllByTestId('inbox-item')
    expect(rows.map((r) => r.dataset.kind).sort()).toEqual(['approval', 'task-input'])
    expect(within(list).getByRole('link', { name: 'Review create_pull_request on acme/widgets' })).toHaveAttribute('href', '/approvals/ap-1')
    expect(within(list).getByRole('link', { name: 'Which branch? needs your reply' })).toHaveAttribute('href', '/tasks/t-7')
    expect(screen.getByRole('heading', { level: 2, name: /Waiting on you/ })).toHaveTextContent('2')
  })

  it('approves inline with the hash the list showed and drops the item', async () => {
    const approves: ApproveRequest[] = []
    renderApp(transport({ approves }), '/')
    await userEvent.click(await screen.findByRole('button', { name: 'Approve create_pull_request on acme/widgets' }))
    await waitFor(() => expect(within(inbox()).queryByText(/Review create_pull_request/)).not.toBeInTheDocument())
    expect(approves).toEqual([expect.objectContaining({ orgId: 'org-1', approvalId: 'ap-1', argsSha256: 'f3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855' })])
  })

  it('denies inline', async () => {
    const denies: DenyRequest[] = []
    renderApp(transport({ denies }), '/')
    await userEvent.click(await screen.findByRole('button', { name: 'Deny create_pull_request on acme/widgets' }))
    await waitFor(() => expect(denies).toHaveLength(1))
    expect(denies[0]).toMatchObject({ approvalId: 'ap-1' })
  })

  it('asks for a review when the request changed since it was listed', async () => {
    renderApp(transport({ approveError: new ConnectError('arguments changed', Code.Aborted) }), '/')
    await userEvent.click(await screen.findByRole('button', { name: 'Approve create_pull_request on acme/widgets' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('This request changed. Open it to review the new version.')
  })

  it('shows no approvals to a member, only a task that waits for a reply and no approval explains', async () => {
    renderApp(transport({ role: OrgRole.MEMBER }), '/')
    const list = await screen.findByRole('list', { name: 'Waiting on you' })
    expect(within(list).queryByRole('button', { name: /Approve/ })).not.toBeInTheDocument()
    expect(within(list).getAllByTestId('inbox-item').map((r) => r.dataset.kind)).toEqual(['task-input'])
  })

  it('shows the count of waiting items as a badge in the navigation', async () => {
    renderApp(transport(), '/board')
    const badge = await screen.findByTestId('inbox-badge')
    expect(badge).toHaveAttribute('aria-label', '2 waiting on you')
    expect(badge).toHaveTextContent('2')
    await waitFor(() => expect(document.title).toMatch(/^\(2\)/))
  })

  it('chats with an agent next to the inbox', async () => {
    const messages: AppendMessageRequest[] = []
    renderApp(transport({ messages }), '/')
    const conversations = await screen.findByRole('list', { name: 'Conversations' })
    // Open tasks only, never the ended ones.
    expect(within(conversations).getAllByRole('button')).toHaveLength(4)
    expect(within(conversations).queryByText(/Rename the repo/)).not.toBeInTheDocument()
    await userEvent.click(within(conversations).getByRole('button', { name: /Write release notes/ }))
    expect(await screen.findByRole('region', { name: 'Conversation: Write release notes' })).toBeInTheDocument()
    await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'Please add a changelog')
    await userEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(messages).toHaveLength(1))
    expect(messages[0]).toMatchObject({ orgId: 'org-1', taskId: 't-2', text: 'Please add a changelog' })
  })

  it('creates a task with an active agent and opens its conversation', async () => {
    const creates: CreateTaskRequest[] = []
    renderApp(transport({ creates }), '/')
    await userEvent.click(await screen.findByRole('combobox', { name: 'Agent' }))
    expect((await screen.findAllByRole('option')).map((o) => o.textContent)).toEqual(['Repo helper', 'Docs writer'])
    await userEvent.keyboard('{Escape}')
    await userEvent.type(screen.getByLabelText('Title'), 'Say hello')
    await userEvent.click(screen.getByRole('button', { name: 'Create task' }))
    expect(await screen.findByRole('region', { name: 'Conversation: Say hello' })).toBeInTheDocument()
    expect(creates[0]).toMatchObject({ orgId: 'org-1', agentId: 'a-1', title: 'Say hello' })
  })

  it('hides editing for viewers', async () => {
    renderApp(transport({ role: OrgRole.VIEWER }), '/')
    await screen.findByRole('list', { name: 'Conversations' })
    expect(screen.queryByRole('form', { name: 'New task' })).not.toBeInTheDocument()
    expect(screen.queryByRole('textbox', { name: 'Message' })).not.toBeInTheDocument()
  })

  it('says so when nothing waits', async () => {
    const t = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI() })
      service(AgentService, { listAgents: () => ({ agents: [] }) })
      service(TaskService, { listTasks: () => ({ tasks: [task('t-2', 'Write notes', TaskState.WORKING)] }) })
      service(ApprovalService, { listApprovals: () => ({ approvals: [] }) })
    })
    renderApp(t, '/')
    expect(await screen.findByText('Nothing is waiting on you.')).toBeInTheDocument()
    expect(screen.queryByTestId('inbox-badge')).not.toBeInTheDocument()
  })
})

describe('Board', () => {
  it('puts each task in its state, as a secondary view', async () => {
    renderApp(transport(), '/board')
    await screen.findByText('Fix the login bug')
    const titles = (name: string) =>
      within(screen.getByRole('region', { name: new RegExp(`^${name}`) }))
        .queryAllByRole('link')
        .map((l) => l.textContent)
    expect(titles('Submitted')).toEqual(['Triage issues'])
    expect(titles('Working')).toEqual(['Write release notes'])
    expect(titles('Input required')).toEqual(['Fix the login bug', 'Which branch?'])
    expect(titles('Completed')).toEqual(['Rename the repo'])
    expect(titles('Canceled')).toEqual(['Stale idea'])
    expect(titles('Failed')).toEqual(['Broken job'])
    expect(screen.getByRole('link', { name: '← Inbox' })).toHaveAttribute('href', '/')
    expect(screen.queryByRole('button', { name: /Done/ })).not.toBeInTheDocument()
  })

  it('reaches the board from the navigation', async () => {
    renderApp(transport(), '/')
    await userEvent.click(await screen.findByRole('link', { name: 'Board' }))
    expect(await screen.findByRole('heading', { level: 1, name: 'Board' })).toBeInTheDocument()
  })

  it('sends old ticket links to the task', async () => {
    const { history } = renderApp(transport(), '/tickets/t-2')
    await waitFor(() => expect(history.location.pathname).toBe('/tasks/t-2'))
  })
})

describe('Inbox questions (ask_user)', () => {
  type QOpts = { choices?: string[]; draft?: string; answers?: AnswerQuestionRequest[]; dismissals?: DismissQuestionRequest[]; answerError?: ConnectError }

  function qTransport(opts: QOpts = {}) {
    let open = [question('q-1', 't-1', { question: 'Which region should I deploy to?', choices: opts.choices, draft: opts.draft })]
    const tasks = [task('t-1', 'Deploy the app', TaskState.INPUT_REQUIRED), task('t-2', 'Write notes', TaskState.WORKING)]
    return createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI() })
      service(AgentService, { listAgents: () => ({ agents: [agent('a-1', 'Repo helper')] }) })
      service(TaskService, { listTasks: () => ({ tasks }), getTask: (req) => ({ task: tasks.find((t) => t.id === req.taskId), events: [], runs: [] }) })
      service(ApprovalService, { listApprovals: () => ({ approvals: [] }) })
      service(QuestionService, {
        listQuestions: () => ({ questions: open }),
        answerQuestion: (req) => {
          if (opts.answerError) throw opts.answerError
          opts.answers?.push(req)
          open = []
          tasks[0].state = TaskState.WORKING
          return { question: question('q-1', 't-1', { state: QuestionState.ANSWERED }) }
        },
        dismissQuestion: (req) => {
          opts.dismissals?.push(req)
          open = []
          return { question: question('q-1', 't-1', { state: QuestionState.DISMISSED }) }
        },
      })
    })
  }

  it('shows the question with its choices, its default and a badge, once, in place of the task item', async () => {
    renderApp(qTransport(), '/')
    const list = await screen.findByRole('list', { name: 'Waiting on you' })
    const rows = within(list).getAllByTestId('inbox-item')
    expect(rows.map((r) => r.dataset.kind)).toEqual(['question'])
    expect(within(list).getByText('Which region should I deploy to?')).toBeInTheDocument()
    expect(within(list).getByRole('link', { name: 'Deploy the app' })).toHaveAttribute('href', '/tasks/t-1')
    expect(within(list).getByRole('button', { name: 'eu-west-1' })).toBeInTheDocument()
    expect(within(list).getByRole('button', { name: 'us-east-1' })).toBeInTheDocument()
    expect(within(list).getByText(/Dismissing, or no answer in time, tells the agent/)).toHaveTextContent('eu-west-1')
    expect(await screen.findByTestId('inbox-badge')).toHaveTextContent('1')
  })

  it('answers with a choice and drops the item', async () => {
    const answers: AnswerQuestionRequest[] = []
    renderApp(qTransport({ answers }), '/')
    await userEvent.click(await screen.findByRole('button', { name: 'us-east-1' }))
    await waitFor(() => expect(answers).toHaveLength(1))
    expect(answers[0]).toMatchObject({ orgId: 'org-1', questionId: 'q-1', answer: { case: 'choice', value: 'us-east-1' } })
    expect(await screen.findByText('Nothing is waiting on you.')).toBeInTheDocument()
  })

  it('accepts the draft, or sends it edited', async () => {
    const answers: AnswerQuestionRequest[] = []
    renderApp(qTransport({ choices: [], draft: 'Fixed in 1.2.', answers }), '/')
    const draft = await screen.findByRole('textbox', { name: 'Draft from the agent' })
    expect(draft).toHaveValue('Fixed in 1.2.')
    await userEvent.type(draft, ' Thanks!')
    await userEvent.click(screen.getByRole('button', { name: 'Use edited draft' }))
    await waitFor(() => expect(answers).toHaveLength(1))
    expect(answers[0].answer).toEqual({ case: 'draft', value: 'Fixed in 1.2. Thanks!' })
  })

  it('answers with free text', async () => {
    const answers: AnswerQuestionRequest[] = []
    renderApp(qTransport({ answers }), '/')
    await userEvent.type(await screen.findByRole('textbox', { name: /Your answer to/ }), 'ap-south-1')
    await userEvent.click(screen.getByRole('button', { name: 'Send answer' }))
    await waitFor(() => expect(answers).toHaveLength(1))
    expect(answers[0].answer).toEqual({ case: 'text', value: 'ap-south-1' })
  })

  it('dismisses the question', async () => {
    const dismissals: DismissQuestionRequest[] = []
    renderApp(qTransport({ dismissals }), '/')
    await userEvent.click(await screen.findByRole('button', { name: 'Dismiss' }))
    await waitFor(() => expect(dismissals).toEqual([expect.objectContaining({ orgId: 'org-1', questionId: 'q-1' })]))
  })

  it('says so when the question was already resolved elsewhere', async () => {
    renderApp(qTransport({ answerError: new ConnectError('gone', Code.Aborted) }), '/')
    await userEvent.click(await screen.findByRole('button', { name: 'eu-west-1' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('already answered, dismissed or expired')
  })
})
