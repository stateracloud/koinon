// SPDX-License-Identifier: Apache-2.0

import { fireEvent, render, screen } from '@testing-library/react'
import { createMemoryHistory, createRootRoute, createRoute, createRouter, RouterProvider } from '@tanstack/react-router'
import { act, useEffect, useState } from 'react'
import { describe, expect, it } from 'vitest'
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

const row = (id: string, at = 0): InboxItem => ({ kind: 'task-input', key: `task-input:${id}`, taskId: id, taskTitle: `Task ${id}`, at })
const noop = () => {}
const actions = { onApprove: noop, onDeny: noop, onAnswer: noop, onDismiss: noop }
const titles = () => screen.getAllByTestId('inbox-item').map((r) => r.textContent?.replace(' needs your reply', ''))

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
