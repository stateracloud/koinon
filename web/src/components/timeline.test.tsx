// SPDX-License-Identifier: Apache-2.0

import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Timeline } from '@/components/timeline'
import { ActorKind } from '@/gen/koinon/task/v1alpha1/task_pb'
import type { TimelineItem } from '@/lib/timeline'

const msg = (key: string, actorUserId: string, actor = ActorKind.USER, actorName = ''): TimelineItem => ({ key, at: 1000, type: 'message', actor, actorUserId, actorName, text: `text ${key}` })

describe('Timeline message authors', () => {
  it('says You only for the viewer and names the other member', () => {
    render(<Timeline meUserId="u-1" items={[msg('a', 'u-1'), msg('b', 'u-2'), msg('c', '', ActorKind.AGENT)]} />)
    const [mine, other, agent] = screen.getAllByRole('article')
    expect(mine).toHaveTextContent('You')
    expect(mine.className).toContain('ml-auto')
    expect(other).toHaveTextContent('Another member')
    expect(other).not.toHaveTextContent('You')
    expect(other.className).not.toContain('ml-auto')
    expect(agent).toHaveTextContent('Agent')
  })

  it('names another member from the name on the event', () => {
    render(<Timeline meUserId="u-1" items={[msg('b', 'u-2', ActorKind.USER, 'Sam Example')]} />)
    expect(screen.getByRole('article')).toHaveTextContent('Sam Example')
  })

  it('does not say You before the viewer is known', () => {
    render(<Timeline items={[msg('a', 'u-1')]} />)
    expect(screen.getByRole('article')).not.toHaveTextContent('You')
  })
})
