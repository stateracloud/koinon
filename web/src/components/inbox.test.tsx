// SPDX-License-Identifier: Apache-2.0

import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router'
import { act, useEffect, useState } from 'react'
import { create } from '@bufbuild/protobuf'
import { describe, expect, it, vi } from 'vitest'
import { QuestionSchema } from '@/gen/koinon/question/v1alpha1/question_pb'
import { Inbox } from '@/components/inbox'
import { stableOrder, type InboxItem } from '@/lib/inbox'


// The rows hold links, so the list renders inside a minimal router. The test re-renders it through a setter.
const feed: { set: (items: InboxItem[]) => void } = { set: () => {} }
function Host({ initial }: { initial: InboxItem[] }) {
  const [items, set] = useState(initial)
  useEffect(() => {
    feed.set = set
  }, [set])
  return <Inbox items={items} {...actions} />
}
async function mount(initial: InboxItem[]) {
  const root = createRootRoute({ component: () => <Host initial={initial} /> })
  const task = createRoute({ getParentRoute: () => root, path: '/tasks/$taskId' })
  const router = createRouter({ routeTree: root.addChildren([task]), history: createMemoryHistory({ initialEntries: ['/'] }) })
  render(<RouterProvider router={router} />)
  await screen.findByRole('list', { name: 'Waiting on you' })
}

const qrow = (id: string): InboxItem => ({
  kind: 'question',
  key: `question:${id}`,
  taskId: id,
  taskTitle: `Task ${id}`,
  agentId: '',
  at: 0,
  question: create(QuestionSchema, { id, question: `Question ${id}`, defaultAnswer: 'go on' }),
})
const row = (id: string, at = 0): InboxItem => ({ kind: 'task-input', key: `task-input:${id}`, taskId: id, taskTitle: `Task ${id}`, agentId: '', at })
const noop = () => {}
const actions: { onApprove: () => void; onDeny: () => void; onAnswer: () => void; onDismiss: (item: InboxItem) => void } = { onApprove: noop, onDeny: noop, onAnswer: noop, onDismiss: noop }
// The row's link names the task; the row head (badge, age) is not part of the title.
const titles = () => screen.getAllByTestId('inbox-item').map((r) => r.querySelector('a')?.textContent?.replace(' needs your reply', ''))

describe('stableOrder', () => {
  it('keeps shown rows in place, drops gone rows and appends new ones', () => {
    const next = [row('c'), row('a'), row('d')]
    expect(stableOrder(next, ['task-input:a', 'task-input:b', 'task-input:c']).map((i) => i.key)).toEqual(['task-input:a', 'task-input:c', 'task-input:d'])
  })
})

describe('Inbox', () => {
  it('does not move rows between pointer-down and click, then follows the data again', async () => {
    await mount([row('a'), row('b')])
    expect(titles()).toEqual(['Task a', 'Task b'])
    const list = screen.getByRole('list', { name: 'Waiting on you' })
    fireEvent.pointerDown(list)
    // A refresh puts a newer item first and drops nothing.
    act(() => feed.set([row('n'), row('b'), row('a')]))
    expect(titles()).toEqual(['Task a', 'Task b', 'Task n'])
    fireEvent.pointerCancel(list)
    expect(titles()).toEqual(['Task n', 'Task b', 'Task a'])
  })

  it('delivers a click to the row under the pointer, then follows the data after the deferred release', async () => {
    const onDismiss = vi.fn()
    actions.onDismiss = onDismiss
    try {
      await mount([qrow('a'), qrow('b')])
      const list = screen.getByRole('list', { name: 'Waiting on you' })
      const buttons = () => screen.getAllByRole('button', { name: 'Dismiss' })
      const target = buttons()[0]
      fireEvent.pointerDown(target)
      // A refresh puts a newer item first while the pointer is down.
      act(() => feed.set([qrow('n'), qrow('b'), qrow('a')]))
      expect(buttons()[0]).toBe(target)
      // The release is deferred, so the click that follows pointer-up still lands on the same row.
      fireEvent.pointerUp(target)
      fireEvent.click(target)
      expect(onDismiss).toHaveBeenCalledTimes(1)
      expect(onDismiss.mock.calls[0][0]).toMatchObject({ key: 'question:a' })
      await waitFor(() => expect(list.querySelectorAll('[data-testid="inbox-item"]')[0].textContent).toContain('Task n'))
      expect(screen.getAllByTestId('inbox-item').map((r) => r.textContent)).toEqual([
        expect.stringContaining('Task n'),
        expect.stringContaining('Task b'),
        expect.stringContaining('Task a'),
      ])
    } finally {
      actions.onDismiss = noop
    }
  })

  it('freezes the order while focus is inside the list', async () => {
    await mount([row('a'), row('b')])
    const link = screen.getAllByRole('link')[0]
    act(() => link.focus())
    act(() => feed.set([row('b'), row('a')]))
    expect(titles()).toEqual(['Task a', 'Task b'])
    act(() => link.blur())
    expect(titles()).toEqual(['Task b', 'Task a'])
  })

  it('follows the data again when the focused row is removed (no blur fires)', async () => {
    await mount([row('a'), row('b')])
    act(() => screen.getAllByRole('link')[0].focus())
    act(() => feed.set([row('b')]))
    // The focused row is gone; the browser fires no blur. A new urgent item must now come first.
    act(() => feed.set([row('n'), row('b')]))
    expect(titles()).toEqual(['Task n', 'Task b'])
  })
})
