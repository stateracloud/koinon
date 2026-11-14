// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { CreateTaskForm, type NewTask } from '@/components/create-task-form'
import { AgentSchema, AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'

const agents = [create(AgentSchema, { id: 'a-1', displayName: 'Repo helper', status: AgentStatus.ACTIVE })]

describe('CreateTaskForm', () => {
  it('clears title and message on success, so a second Enter creates nothing', async () => {
    const made: NewTask[] = []
    render(<CreateTaskForm agents={agents} onCreate={async (t) => void made.push(t)} />)
    await userEvent.type(screen.getByLabelText('Title'), 'Say hello')
    await userEvent.type(screen.getByLabelText('First message (optional)'), 'Hi')
    await userEvent.click(screen.getByRole('button', { name: 'Create task' }))
    await waitFor(() => expect(screen.getByLabelText('Title')).toHaveValue(''))
    expect(screen.getByLabelText('First message (optional)')).toHaveValue('')
    await userEvent.type(screen.getByLabelText('Title'), '{Enter}')
    expect(made).toEqual([{ agentId: 'a-1', title: 'Say hello', message: 'Hi' }])
  })

  it('makes one task for two quick submits and keeps the text when the call fails', async () => {
    let calls = 0
    render(
      <CreateTaskForm
        agents={agents}
        onCreate={async () => {
          calls++
          throw new Error('refused')
        }}
      />,
    )
    await userEvent.type(screen.getByLabelText('Title'), 'Say hello')
    await userEvent.type(screen.getByLabelText('Title'), '{Enter}')
    await waitFor(() => expect(calls).toBe(1))
    expect(screen.getByLabelText('Title')).toHaveValue('Say hello')
    await userEvent.type(screen.getByLabelText('Title'), '{Enter}')
    await waitFor(() => expect(calls).toBe(2))
  })
})
