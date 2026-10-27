// SPDX-License-Identifier: Apache-2.0

import { Code, ConnectError, createRouterTransport } from '@connectrpc/connect'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { AgentService } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { ApprovalService } from '@/gen/koinon/approval/v1alpha1/approval_pb'
import { AuthService } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TaskService, TaskState, type AppendMessageRequest, type SendDirectMessageRequest } from '@/gen/koinon/task/v1alpha1/task_pb'
import { agent, dmAgent, dmThread, renderApp, task, whoAmI } from '@/test-utils'

type Opts = {
  sends?: SendDirectMessageRequest[]
  appends?: AppendMessageRequest[]
  failFirstSend?: boolean
}

// a-1 takes direct messages, a-2 does not.
function transport(opts: Opts = {}) {
  const tasks = [
    dmThread('t-open', 'What changed today?', TaskState.WORKING),
    dmThread('t-done', 'Summarise the incident', TaskState.COMPLETED),
    dmThread('t-parked', 'Waiting for your pick', TaskState.INPUT_REQUIRED),
    dmThread('t-cancel', 'Dropped thread', TaskState.CANCELED),
    task('t-plain', 'Plain task of the chat agent', TaskState.WORKING, 'a-1'),
    dmThread('t-nodm', 'Thread of an agent that stopped taking messages', TaskState.WORKING, 'a-2'),
  ]
  let failed = false
  return createRouterTransport(({ service }) => {
    service(AuthService, { whoAmI: () => whoAmI() })
    service(AgentService, { listAgents: () => ({ agents: [dmAgent('a-1', 'Chat helper'), agent('a-2', 'Quiet bot')] }) })
    service(ApprovalService, { listApprovals: () => ({ approvals: [] }) })
    service(TaskService, {
      listTasks: () => ({ tasks }),
      getTask: (req) => ({ task: tasks.find((t) => t.id === req.taskId), events: [], runs: [] }),
      appendMessage: (req) => {
        opts.appends?.push(req)
        return {}
      },
      sendDirectMessage: (req) => {
        opts.sends?.push(req)
        if (opts.failFirstSend && !failed) {
          failed = true
          throw new ConnectError('the queue is busy', Code.Unavailable)
        }
        if (req.taskId) {
          const t = tasks.find((x) => x.id === req.taskId)!
          const reopened = t.state === TaskState.COMPLETED
          if (reopened) t.state = TaskState.SUBMITTED
          return { task: t, reopened }
        }
        const t = dmThread('t-new', req.text, TaskState.SUBMITTED)
        tasks.push(t)
        return { task: t, created: true }
      },
    })
  })
}

describe('Inbox chat direct messages', () => {
  it('starts a thread with an agent that takes direct messages and opens it', async () => {
    const sends: SendDirectMessageRequest[] = []
    renderApp(transport({ sends }), '/')
    const form = await screen.findByRole('region', { name: 'New thread' })
    await userEvent.type(within(form).getByRole('textbox', { name: 'Message to agent' }), 'What is on fire?')
    await userEvent.click(within(form).getByRole('button', { name: 'Start thread' }))
    expect(await screen.findByRole('region', { name: 'Conversation: What is on fire?' })).toBeInTheDocument()
    expect(sends).toEqual([expect.objectContaining({ orgId: 'org-1', agentId: 'a-1', taskId: '', text: 'What is on fire?' })])
    expect(sends[0].messageId).toMatch(/^[0-9a-f-]{36}$/)
  })

  it('says that an agent without a direct-message trigger takes no messages, and offers no composer', async () => {
    renderApp(transport(), '/')
    const form = await screen.findByRole('region', { name: 'New thread' })
    await userEvent.click(within(form).getByRole('combobox', { name: 'Send to' }))
    await userEvent.click(await screen.findByRole('option', { name: 'Quiet bot (no direct messages)' }))
    expect(within(form).getByRole('status')).toHaveTextContent('Quiet bot does not take direct messages')
    expect(within(form).queryByRole('textbox')).not.toBeInTheDocument()
  })

  it('replies in a thread through the same task, and the thread view shows its state', async () => {
    const sends: SendDirectMessageRequest[] = []
    const appends: AppendMessageRequest[] = []
    renderApp(transport({ sends, appends }), '/')
    const chat = await screen.findByRole('region', { name: 'Conversation: What changed today?' })
    expect(within(chat).getByText('Working')).toBeInTheDocument()
    await userEvent.type(within(chat).getByRole('textbox', { name: 'Message' }), 'And yesterday?')
    await userEvent.click(within(chat).getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(sends).toHaveLength(1))
    expect(sends[0]).toMatchObject({ agentId: 'a-1', taskId: 't-open', text: 'And yesterday?' })
    expect(appends).toEqual([])
  })

  it('reopens a completed thread with a reply, and tells the person so', async () => {
    const sends: SendDirectMessageRequest[] = []
    renderApp(transport({ sends }), '/')
    const list = await screen.findByRole('list', { name: 'Conversations' })
    await userEvent.click(within(list).getByRole('button', { name: /Summarise the incident/ }))
    const chat = await screen.findByRole('region', { name: 'Conversation: Summarise the incident' })
    expect(within(chat).getByText(/A reply reopens it as a follow-up/)).toBeInTheDocument()
    await userEvent.type(within(chat).getByRole('textbox', { name: 'Message' }), 'One more question')
    await userEvent.click(within(chat).getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(sends).toEqual([expect.objectContaining({ taskId: 't-done', text: 'One more question' })]))
    await waitFor(() => expect(within(chat).getByText('Submitted')).toBeInTheDocument())
  })

  it('keeps canceled threads out of the chat, and plain tasks on the task message path', async () => {
    const sends: SendDirectMessageRequest[] = []
    const appends: AppendMessageRequest[] = []
    renderApp(transport({ sends, appends }), '/')
    const list = await screen.findByRole('list', { name: 'Conversations' })
    expect(within(list).queryByText(/Dropped thread/)).not.toBeInTheDocument()
    await userEvent.click(within(list).getByRole('button', { name: /Plain task of the chat agent/ }))
    const chat = await screen.findByRole('region', { name: 'Conversation: Plain task of the chat agent' })
    await userEvent.type(within(chat).getByRole('textbox', { name: 'Message' }), 'hello')
    await userEvent.click(within(chat).getByRole('button', { name: 'Send' }))
    await waitFor(() => expect(appends).toHaveLength(1))
    expect(sends).toEqual([])
  })

  it('shows that the agent no longer takes messages in its thread', async () => {
    renderApp(transport(), '/')
    const list = await screen.findByRole('list', { name: 'Conversations' })
    await userEvent.click(within(list).getByRole('button', { name: /Thread of an agent that stopped/ }))
    const chat = await screen.findByRole('region', { name: /Conversation: Thread of an agent/ })
    expect(within(chat).getByText(/Quiet bot does not take direct messages/)).toBeInTheDocument()
    expect(within(chat).queryByRole('textbox')).not.toBeInTheDocument()
  })

  it('asks for the pending decision instead of offering a reply while the task is parked', async () => {
    renderApp(transport(), '/')
    const list = await screen.findByRole('list', { name: 'Conversations' })
    await userEvent.click(within(list).getByRole('button', { name: /Waiting for your pick/ }))
    const chat = await screen.findByRole('region', { name: 'Conversation: Waiting for your pick' })
    expect(within(chat).getByText(/Waiting on an approval/)).toBeInTheDocument()
    expect(within(chat).queryByRole('textbox')).not.toBeInTheDocument()
  })

  it('resends a failed message under the same message ID', async () => {
    const sends: SendDirectMessageRequest[] = []
    renderApp(transport({ sends, failFirstSend: true }), '/')
    const form = await screen.findByRole('region', { name: 'New thread' })
    await userEvent.type(within(form).getByRole('textbox', { name: 'Message to agent' }), 'Hello?')
    await userEvent.click(within(form).getByRole('button', { name: 'Start thread' }))
    expect(await within(form).findByRole('alert')).toHaveTextContent('the queue is busy')
    await userEvent.click(within(form).getByRole('button', { name: 'Start thread' }))
    await waitFor(() => expect(sends).toHaveLength(2))
    expect(sends[1].messageId).toBe(sends[0].messageId)
  })
})
