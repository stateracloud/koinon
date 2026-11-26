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

const ext = (key: string, platform: string, platformUserId: string): TimelineItem => ({
  key,
  at: 1000,
  type: 'message',
  actor: ActorKind.EXTERNAL,
  actorUserId: '',
  actorName: '',
  external: { platform, platformUserId },
  text: `text ${key}`,
})

describe('Timeline messages from a person without a Koinon account (ADR 0130)', () => {
  it('names the platform and the platform user ID, says there is no account, and never says You', () => {
    render(<Timeline meUserId="u-1" items={[ext('x', 'slack', 'U0TEST')]} />)
    const article = screen.getByRole('article')
    expect(article).toHaveTextContent('Slack user U0TEST')
    expect(article).toHaveTextContent('No Koinon account')
    expect(article).not.toHaveTextContent('You')
    expect(article.className).not.toContain('ml-auto')
  })

  it('says Teams for a Teams person', () => {
    render(<Timeline items={[ext('t', 'teams', '29:abc')]} />)
    expect(screen.getByRole('article')).toHaveTextContent('Teams user 29:abc')
  })
})
