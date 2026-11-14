// SPDX-License-Identifier: Apache-2.0

import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { Timeline } from '@/components/timeline'
import { ActorKind } from '@/gen/koinon/task/v1alpha1/task_pb'
import type { TimelineItem } from '@/lib/timeline'

const msg = (key: string, actorUserId: string, actor = ActorKind.USER): TimelineItem => ({ key, at: 1000, type: 'message', actor, actorUserId, text: `text ${key}` })

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

  it('names other members from the member map, and falls back when the id is unknown', () => {
    const names = new Map([['u-2', 'Sam Example']])
    render(<Timeline meUserId="u-1" memberNames={names} items={[msg('b', 'u-2'), msg('c', 'u-3')]} />)
    const [sam, unknown] = screen.getAllByRole('article')
    expect(sam).toHaveTextContent('Sam Example')
    expect(unknown).toHaveTextContent('Another member')
  })

  it('does not say You before the viewer is known', () => {
    render(<Timeline items={[msg('a', 'u-1')]} />)
    expect(screen.getByRole('article')).not.toHaveTextContent('You')
  })
})
