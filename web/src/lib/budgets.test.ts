// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { NotificationSchema } from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { bannerNotices, isAgentUnhealthy, isCurrent, nextMonthStart, noticeText, overridden, windowStart } from '@/lib/budgets'

const now = new Date('2026-10-24T12:00:00Z')
const n = (id: string, p: Partial<{ level: string; threshold: number; readAt: number; windowStart: string; agentId: string }>) =>
  create(NotificationSchema, {
    id,
    kind: (p.threshold ?? 80) >= 100 ? 'budget.reached' : 'budget.warning',
    level: p.level ?? 'org_monthly',
    threshold: p.threshold ?? 80,
    limitUsd: '10.00',
    spentUsd: '8.00',
    agentId: p.agentId ?? '',
    windowStart: timestampFromMs(Date.parse(p.windowStart ?? '2026-10-01T00:00:00Z')),
    readAt: p.readAt ? timestampFromMs(p.readAt) : undefined,
  })

describe('windows', () => {
  it('counts a UTC day for an agent and a UTC month otherwise', () => {
    expect(windowStart('agent_daily', now).toISOString()).toBe('2026-10-24T00:00:00.000Z')
    expect(windowStart('org_monthly', now).toISOString()).toBe('2026-10-01T00:00:00.000Z')
    expect(nextMonthStart(now).toISOString()).toBe('2026-11-01T00:00:00.000Z')
    expect(nextMonthStart(new Date('2026-12-31T23:59:59Z')).toISOString()).toBe('2027-01-01T00:00:00.000Z')
  })

  it('ignores a notice about an earlier window', () => {
    expect(isCurrent(n('a', { windowStart: '2026-10-01T00:00:00Z' }), now)).toBe(true)
    expect(isCurrent(n('b', { windowStart: '2026-09-01T00:00:00Z' }), now)).toBe(false)
    expect(isCurrent(n('c', { level: 'agent_daily', windowStart: '2026-10-23T00:00:00Z' }), now)).toBe(false)
  })
})

describe('bannerNotices', () => {
  it('shows unread, current notices, the highest threshold of each limit first', () => {
    const shown = bannerNotices(
      [
        n('warn', { threshold: 80 }),
        n('reached', { threshold: 100 }),
        n('read', { level: 'agent_daily', windowStart: '2026-10-24T00:00:00Z', readAt: 1 }),
        n('old', { windowStart: '2026-09-01T00:00:00Z', threshold: 100 }),
        n('agent', { level: 'agent_daily', windowStart: '2026-10-24T00:00:00Z', agentId: 'a-1' }),
      ],
      now,
      false,
    )
    expect(shown.map((x) => x.id)).toEqual(['reached', 'agent'])
  })

  it('keeps platform-ceiling notices for platform admins', () => {
    const list = [n('p', { level: 'platform_ceiling' })]
    expect(bannerNotices(list, now, false)).toEqual([])
    expect(bannerNotices(list, now, true)).toHaveLength(1)
  })
})

describe('noticeText', () => {
  it('says what happened in plain words', () => {
    expect(noticeText(n('a', { threshold: 80 }))).toBe('The org’s monthly cap is at 80% ($8.00 of $10.00). Runs stop when it is reached.')
    expect(noticeText(n('b', { threshold: 100, level: 'agent_daily', agentId: 'a-1' }), new Map([['a-1', 'Repo helper']]))).toBe(
      'Repo helper’s daily limit has been reached ($8.00 of $10.00). Runs that count against it were stopped.',
    )
  })
})

describe('overridden', () => {
  it('compares the limit with the template default', () => {
    expect(overridden('3.50', '10.00')).toBe(true)
    expect(overridden('10.00', '10.00')).toBe(false)
    expect(overridden('3.50', '')).toBe(false)
  })
})

describe('agent.unhealthy notifications', () => {
  const unhealthy = create(NotificationSchema, { id: 'n-u', kind: 'agent.unhealthy', agentId: 'a-1', message: 'Repo helper was marked unhealthy: its sandbox kept getting stuck.' })

  it('says the server message, and falls back to a plain sentence', () => {
    expect(isAgentUnhealthy(unhealthy)).toBe(true)
    expect(noticeText(unhealthy)).toBe('Repo helper was marked unhealthy: its sandbox kept getting stuck.')
    expect(noticeText(create(NotificationSchema, { kind: 'agent.unhealthy', agentId: 'a-1' }), new Map([['a-1', 'Repo helper']]))).toBe('Repo helper was marked unhealthy.')
  })

  it('does not call an unknown kind a budget notice', () => {
    expect(noticeText(create(NotificationSchema, { kind: 'something.new', message: 'Hello' }))).toBe('Hello')
    expect(noticeText(create(NotificationSchema, { kind: 'something.new' }))).toBe('A notification this version does not recognise.')
  })

  it('keeps an unread one in the banner list', () => {
    expect(bannerNotices([unhealthy], now, false).map((x) => x.id)).toEqual(['n-u'])
  })
})
