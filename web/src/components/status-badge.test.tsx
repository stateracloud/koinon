// SPDX-License-Identifier: Apache-2.0

import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { AgentStatus } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { TaskState } from '@/gen/koinon/task/v1alpha1/task_pb'
import { AgentStatusBadge, StatusBadge, TokenStatusBadge } from './status-badge'

// ADR 0126 section 3: the six task states, each with its name as text and a dot.
const cases: [TaskState, string, string][] = [
  [TaskState.SUBMITTED, 'Submitted', 'submitted'],
  [TaskState.WORKING, 'Working', 'working'],
  [TaskState.INPUT_REQUIRED, 'Blocked on you', 'attention'],
  [TaskState.COMPLETED, 'Completed', 'completed'],
  [TaskState.FAILED, 'Failed', 'failed'],
  [TaskState.CANCELED, 'Canceled', 'neutral'],
]

describe('StatusBadge', () => {
  it.each(cases)('state %s shows its name and a dot', (state, name, tone) => {
    const { container } = render(<StatusBadge state={state} />)
    const badge = screen.getByTestId('status-badge')
    expect(badge).toHaveTextContent(name)
    expect(badge).toHaveAttribute('data-tone', tone)
    expect(container.querySelector('[aria-hidden]')).not.toBeNull()
  })

  it('shows Canceled hollow: a border and no fill', () => {
    render(<StatusBadge state={TaskState.CANCELED} />)
    const cls = screen.getByTestId('status-badge').className
    expect(cls).toContain('border-input')
    expect(cls).not.toMatch(/\bbg-(?!current)/)
  })

  it('shows the general name and keeps the attention tone when the task waits on someone else', () => {
    render(<StatusBadge state={TaskState.INPUT_REQUIRED} onViewer={false} />)
    expect(screen.getByTestId('status-badge')).toHaveTextContent('Input required')
    expect(screen.getByTestId('status-badge')).toHaveAttribute('data-tone', 'attention')
  })

  it('ignores onViewer for the other states', () => {
    render(<StatusBadge state={TaskState.WORKING} onViewer={false} />)
    expect(screen.getByTestId('status-badge')).toHaveTextContent('Working')
  })

  it('uses the attention fill with dark text for Blocked on you', () => {
    render(<StatusBadge state={TaskState.INPUT_REQUIRED} />)
    expect(screen.getByTestId('status-badge').className).toContain('bg-attention text-attention-foreground')
  })
})

describe('AgentStatusBadge', () => {
  it.each([
    [AgentStatus.ACTIVE, false, 'Active', 'completed'],
    [AgentStatus.DRAFT, false, 'Draft', 'neutral'],
    [AgentStatus.ACTIVE, true, 'Unhealthy', 'failed'],
  ] as const)('status %s unhealthy %s shows %s', (status, unhealthy, name, tone) => {
    render(<AgentStatusBadge status={status} unhealthy={unhealthy} />)
    const badge = screen.getByTestId('status-badge')
    expect(badge).toHaveTextContent(name)
    expect(badge).toHaveAttribute('data-tone', tone)
  })
})

describe('TokenStatusBadge', () => {
  it.each([
    ['active', 'Active', 'completed'],
    ['expired', 'Expired', 'neutral'],
    ['revoked', 'Revoked', 'failed'],
  ] as const)('shows %s as %s', (status, text, tone) => {
    render(<TokenStatusBadge status={status} />)
    const badge = screen.getByTestId('status-badge')
    expect(badge).toHaveTextContent(text)
    expect(badge).toHaveAttribute('data-tone', tone)
  })
})
