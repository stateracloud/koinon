// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentService, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { ApprovalService, type ApproveRequest, type DenyRequest } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { AuthService, OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { QuestionService, QuestionState, type AnswerQuestionRequest, type DismissQuestionRequest } from '@/gen/koinon/question/v1alpha1/question_pb'
import { TaskService, TaskState, type AppendMessageRequest, type CreateTaskRequest, type Task } from '@/gen/koinon/task/v1alpha1/task_pb'
import { agent, approval, question, renderApp, task, whoAmI } from '@/test-utils'

type Opts = {
  role?: OrgRole
  approves?: ApproveRequest[]
  denies?: DenyRequest[]
  creates?: CreateTaskRequest[]
  messages?: AppendMessageRequest[]
  approveError?: ConnectError
  extraTasks?: Task[]
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
    ...(opts.extraTasks ?? []),
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
    renderApp(transport(), '/inbox')
    const list = await screen.findByRole('list', { name: 'Waiting on you' })
    const rows = within(list).getAllByTestId('inbox-item')
    expect(rows.map((r) => r.dataset.kind).sort()).toEqual(['approval', 'task-input'])
    expect(within(list).getByRole('link', { name: 'Review create_pull_request on acme/widgets' })).toHaveAttribute('href', '/approvals/ap-1')
    expect(within(list).getByRole('link', { name: 'Which branch? needs your reply' })).toHaveAttribute('href', '/tasks/t-7')
    expect(screen.getByRole('tab', { name: 'Blocked on you 2' })).toHaveAttribute('aria-selected', 'true')
    // Each blocked row has the state badge, the agent's avatar and name, and its age.
    for (const r of rows) {
      expect(within(r).getByTestId('status-badge')).toHaveTextContent('Blocked on you')
      expect(within(r).getByTestId('agent-avatar')).toHaveAttribute('aria-hidden', 'true')
      expect(within(r).getByText(/^waiting /)).toBeInTheDocument()
    }
  })

  it('approves inline with the hash the list showed and drops the item', async () => {
    const approves: ApproveRequest[] = []
    renderApp(transport({ approves }), '/inbox')
    await userEvent.click(await screen.findByRole('button', { name: 'Approve create_pull_request on acme/widgets' }))
    await waitFor(() => expect(within(inbox()).queryByText(/Review create_pull_request/)).not.toBeInTheDocument())
    expect(approves).toEqual([expect.objectContaining({ orgId: 'org-1', approvalId: 'ap-1', argsSha256: 'f3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855' })])
  })

  it('denies inline', async () => {
    const denies: DenyRequest[] = []
    renderApp(transport({ denies }), '/inbox')
    await userEvent.click(await screen.findByRole('button', { name: 'Deny create_pull_request on acme/widgets' }))
    await waitFor(() => expect(denies).toHaveLength(1))
    expect(denies[0]).toMatchObject({ approvalId: 'ap-1' })
  })

  it('asks for a review when the request changed since it was listed', async () => {
    renderApp(transport({ approveError: new ConnectError('arguments changed', Code.Aborted) }), '/inbox')
    await userEvent.click(await screen.findByRole('button', { name: 'Approve create_pull_request on acme/widgets' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('This request changed. Open it to review the new version.')
  })

  it('shows no approvals to a member, only a task that waits for a reply and no approval explains', async () => {
    renderApp(transport({ role: OrgRole.MEMBER }), '/inbox')
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
    renderApp(transport({ messages }), '/inbox')
    const conversations = await screen.findByRole('list', { name: 'Threads' })
    // Open tasks only, never the ended ones.
    expect(within(conversations).getAllByRole('button')).toHaveLength(4)
    expect(within(conversations).queryByText(/Rename the repo/)).not.toBeInTheDocument()
    await userEvent.click(within(conversations).getByRole('button', { name: /Write release notes/ }))
    expect(await screen.findByRole('region', { name: 'Thread: Write release notes' })).toBeInTheDocument()
    await userEvent.type(screen.getByRole('textbox', { name: 'Message' }), 'Please add a changelog')
    await userEvent.click(screen.getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(messages).toHaveLength(1))
    expect(messages[0]).toMatchObject({ orgId: 'org-1', taskId: 't-2', text: 'Please add a changelog' })
  })

  it('creates a task with an active agent and opens its thread', async () => {
    const creates: CreateTaskRequest[] = []
    renderApp(transport({ creates }), '/inbox')
    await userEvent.click(await screen.findByRole('combobox', { name: 'Agent' }))
    expect((await screen.findAllByRole('option')).map((o) => o.textContent)).toEqual(['Repo helper', 'Docs writer'])
    await userEvent.keyboard('{Escape}')
    await userEvent.type(screen.getByLabelText('Title'), 'Say hello')
    await userEvent.click(screen.getByRole('button', { name: 'Create task' }))
    expect(await screen.findByRole('region', { name: 'Thread: Say hello' })).toBeInTheDocument()
    expect(creates[0]).toMatchObject({ orgId: 'org-1', agentId: 'a-1', title: 'Say hello' })
  })

  it('hides editing for viewers', async () => {
    renderApp(transport({ role: OrgRole.VIEWER }), '/inbox')
    await screen.findByRole('list', { name: 'Threads' })
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
    renderApp(t, '/inbox')
    expect(await screen.findByText('Nothing is waiting on you.')).toBeInTheDocument()
    expect(screen.queryByTestId('inbox-badge')).not.toBeInTheDocument()
  })
})

describe('Inbox tabs', () => {
  const titlesIn = (name: string) => within(screen.getByRole('list', { name })).getAllByTestId('task-row').map((r) => within(r).getByRole('link').getAttribute('aria-label'))

  it('shows each tab with its count', async () => {
    renderApp(transport(), '/inbox')
    await screen.findByRole('list', { name: 'Waiting on you' })
    expect(screen.getAllByRole('tab').map((t) => t.textContent)).toEqual(['Blocked on you 2', 'Open 4', 'Done 3', 'All 7'])
  })

  it('Open lists the submitted, working and input-required tasks with badge, avatar, agent and a Review link', async () => {
    renderApp(transport(), '/inbox')
    await userEvent.click(await screen.findByRole('tab', { name: 'Open 4' }))
    const list = screen.getByRole('list', { name: 'Open tasks' })
    const rows = within(list).getAllByTestId('task-row')
    expect(rows.map((r) => r.dataset.state).sort()).toEqual(['INPUT_REQUIRED', 'INPUT_REQUIRED', 'SUBMITTED', 'WORKING'])
    expect(titlesIn('Open tasks').sort()).toEqual(['Review Fix the login bug', 'Review Triage issues', 'Review Which branch?', 'Review Write release notes'])
    const triage = rows.find((r) => r.dataset.state === 'SUBMITTED')!
    expect(within(triage).getByTestId('status-badge')).toHaveTextContent('Submitted')
    expect(within(triage).getByTestId('agent-avatar')).toHaveTextContent('D')
    expect(within(triage).getByText(/^Docs writer/)).toBeInTheDocument()
    expect(within(triage).getByRole('link', { name: 'Review Triage issues' })).toHaveAttribute('href', '/tasks/t-3')
  })

  it('Open shows Blocked on you only for the viewer\'s own items; another person\'s input-required task shows Input required', async () => {
    const other = task('t-8', 'Waits on Sam', TaskState.INPUT_REQUIRED)
    other.waitingOnUserIds = ['u-9']
    renderApp(transport({ extraTasks: [other] }), '/inbox')
    await userEvent.click(await screen.findByRole('tab', { name: 'Open 5' }))
    const rows = within(screen.getByRole('list', { name: 'Open tasks' })).getAllByTestId('task-row')
    const badge = (title: string) => within(rows.find((r) => within(r).queryByText(title))!).getByTestId('status-badge')
    expect(badge('Waits on Sam')).toHaveTextContent('Input required')
    expect(badge('Waits on Sam')).toHaveAttribute('data-tone', 'attention')
    expect(badge('Fix the login bug')).toHaveTextContent('Blocked on you')
    expect(badge('Which branch?')).toHaveTextContent('Blocked on you')
  })

  it('writes the tab to the URL and removes it for Blocked on you', async () => {
    const { history } = renderApp(transport(), '/inbox')
    await userEvent.click(await screen.findByRole('tab', { name: 'Open 4' }))
    await waitFor(() => expect(history.location.search).toContain('tab=open'))
    await userEvent.click(screen.getByRole('tab', { name: 'Blocked on you 2' }))
    await waitFor(() => expect(history.location.search).not.toContain('tab'))
  })

  it('Done lists the completed, failed and canceled tasks; a failed row shows only its state (the API gives no reason)', async () => {
    renderApp(transport(), '/inbox')
    await userEvent.click(await screen.findByRole('tab', { name: 'Done 3' }))
    const rows = within(screen.getByRole('list', { name: 'Done tasks' })).getAllByTestId('task-row')
    expect(rows.map((r) => r.dataset.state).sort()).toEqual(['CANCELED', 'COMPLETED', 'FAILED'])
    const failed = rows.find((r) => r.dataset.state === 'FAILED')!
    expect(within(failed).getByTestId('status-badge')).toHaveTextContent('Failed')
    expect(within(failed).getByText('Broken job')).toBeInTheDocument()
    expect(within(failed).getByText(/^Repo helper · /)).toBeInTheDocument()
  })

  it('All lists every task, and the URL opens a tab directly', async () => {
    renderApp(transport(), '/inbox?tab=all')
    const rows = within(await screen.findByRole('list', { name: 'All tasks' })).getAllByTestId('task-row')
    expect(rows).toHaveLength(7)
    expect(screen.getByRole('tab', { name: 'All 7' })).toHaveAttribute('aria-selected', 'true')
  })

  it('an unknown tab in the URL falls back to Blocked on you', async () => {
    renderApp(transport(), '/inbox?tab=nope')
    await screen.findByRole('list', { name: 'Waiting on you' })
    expect(screen.getByRole('tab', { name: 'Blocked on you 2' })).toHaveAttribute('aria-selected', 'true')
  })

  it('has a legend with every task state as text', async () => {
    renderApp(transport(), '/inbox')
    const legend = await screen.findByRole('list', { name: 'State legend' })
    expect(within(legend).getAllByTestId('status-badge').map((b) => b.textContent)).toEqual(['Submitted', 'Working', 'Blocked on you', 'Completed', 'Failed', 'Canceled'])
  })

  it('says so when a task tab is empty', async () => {
    const t = createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI() })
      service(AgentService, { listAgents: () => ({ agents: [] }) })
      service(TaskService, { listTasks: () => ({ tasks: [task('t-2', 'Write notes', TaskState.WORKING)] }) })
      service(ApprovalService, { listApprovals: () => ({ approvals: [] }) })
    })
    renderApp(t, '/inbox?tab=done')
    expect(await screen.findByText('No task is done yet.')).toBeInTheDocument()
  })
})

describe('Board', () => {
  it('puts each task in its state, on the Board tab of the overview', async () => {
    renderApp(transport(), '/?tab=board')
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
    expect(screen.getByRole('tab', { name: 'Board', selected: true })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /Done/ })).not.toBeInTheDocument()
  })

  it('reaches the board from the overview tabs', async () => {
    const { history } = renderApp(transport(), '/')
    await userEvent.click(await screen.findByRole('tab', { name: 'Board' }))
    expect(await screen.findByRole('region', { name: /^Working/ })).toBeInTheDocument()
    await waitFor(() => expect(history.location.search).toBe('?tab=board'))
  })

  it('sends old ticket links to the task', async () => {
    const { history } = renderApp(transport(), '/tickets/t-2')
    await waitFor(() => expect(history.location.pathname).toBe('/tasks/t-2'))
  })
})

describe('Inbox questions (ask_user)', () => {
  type QOpts = { choices?: string[]; draft?: string; admins?: boolean; unhealthy?: boolean; answers?: AnswerQuestionRequest[]; dismissals?: DismissQuestionRequest[]; answerError?: ConnectError }

  function qTransport(opts: QOpts = {}) {
    let open = [
      question('q-1', 't-1', {
        question: 'Which region should I deploy to?',
        choices: opts.choices,
        draft: opts.draft,
        ...(opts.admins ? { targetLabel: "the org's admins", recipients: 2, waitingSinceMs: Date.now() - 3 * 3_600_000 } : {}),
      }),
    ]
    const tasks = [task('t-1', 'Deploy the app', TaskState.INPUT_REQUIRED), task('t-2', 'Write notes', TaskState.WORKING)]
    return createRouterTransport(({ service }) => {
      service(AuthService, { whoAmI: () => whoAmI() })
      service(AgentService, {
        listAgents: () => ({ agents: [opts.unhealthy ? agent('a-1', 'Repo helper', AgentStatus.ACTIVE, { atMs: Date.now() - 60_000, reason: 'its sandbox never became ready' }) : agent('a-1', 'Repo helper')] }),
        getAgent: () => ({ agent: agent('a-1', 'Repo helper') }),
      })
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
    renderApp(qTransport(), '/inbox')
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
    renderApp(qTransport({ answers }), '/inbox')
    await userEvent.click(await screen.findByRole('button', { name: 'us-east-1' }))
    await waitFor(() => expect(answers).toHaveLength(1))
    expect(answers[0]).toMatchObject({ orgId: 'org-1', questionId: 'q-1', answer: { case: 'choice', value: 'us-east-1' } })
    expect(await screen.findByText('Nothing is waiting on you.')).toBeInTheDocument()
  })

  it('accepts the draft, or sends it edited', async () => {
    const answers: AnswerQuestionRequest[] = []
    renderApp(qTransport({ choices: [], draft: 'Fixed in 1.2.', answers }), '/inbox')
    const draft = await screen.findByRole('textbox', { name: 'Draft from the agent' })
    expect(draft).toHaveValue('Fixed in 1.2.')
    await userEvent.type(draft, ' Thanks!')
    await userEvent.click(screen.getByRole('button', { name: 'Use edited draft' }))
    await waitFor(() => expect(answers).toHaveLength(1))
    expect(answers[0].answer).toEqual({ case: 'draft', value: 'Fixed in 1.2. Thanks!' })
  })

  it('answers with free text', async () => {
    const answers: AnswerQuestionRequest[] = []
    renderApp(qTransport({ answers }), '/inbox')
    await userEvent.type(await screen.findByRole('textbox', { name: /Your answer to/ }), 'ap-south-1')
    await userEvent.click(screen.getByRole('button', { name: 'Send answer' }))
    await waitFor(() => expect(answers).toHaveLength(1))
    expect(answers[0].answer).toEqual({ case: 'text', value: 'ap-south-1' })
  })

  it('dismisses the question', async () => {
    const dismissals: DismissQuestionRequest[] = []
    renderApp(qTransport({ dismissals }), '/inbox')
    await userEvent.click(await screen.findByRole('button', { name: 'Dismiss' }))
    await waitFor(() => expect(dismissals).toEqual([expect.objectContaining({ orgId: 'org-1', questionId: 'q-1' })]))
  })

  it('says so when the question was already resolved elsewhere', async () => {
    renderApp(qTransport({ answerError: new ConnectError('', Code.Aborted) }), '/inbox')
    await userEvent.click(await screen.findByRole('button', { name: 'eu-west-1' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('already answered, dismissed or expired')
  })

  it('shows who answered first when the server says so', async () => {
    renderApp(qTransport({ answerError: new ConnectError('Ana already answered this question', Code.Aborted) }), '/inbox')
    await userEvent.click(await screen.findByRole('button', { name: 'eu-west-1' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('Ana already answered this question')
  })

  it('shows who the question waits on and for how long', async () => {
    renderApp(qTransport({ admins: true }), '/inbox')
    const row = within(await screen.findByRole('list', { name: 'Waiting on you' })).getByTestId('inbox-item')
    expect(within(row).getByText("Asked to the org's admins (2 people; the first answer counts)")).toBeInTheDocument()
    // The row head counts from waitingSince (3 hours ago), not from when the question opened.
    expect(within(row).getByText('waiting 3 hours')).toBeInTheDocument()
  })

  it('gives an admin an item for an unhealthy agent', async () => {
    renderApp(qTransport({ unhealthy: true }), '/inbox')
    const link = await screen.findByRole('link', { name: 'Open the agent' })
    expect(link).toHaveAttribute('href', '/agents/a-1')
    expect(screen.getByText(/is unhealthy and takes no runs/)).toBeInTheDocument()
    expect(screen.getByText('its sandbox never became ready')).toBeInTheDocument()
  })
})
