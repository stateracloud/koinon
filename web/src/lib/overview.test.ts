// SPDX-License-Identifier: Apache-2.0

import { create, type MessageInitShape } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { AgentStatus, AgentSummarySchema, type AgentSummary } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { RunState } from '@/gen/koinon/run/v1alpha1/run_pb'
import { cardState, groupByTeam, nextRunText, parseOverviewTab, reportsToForest, reportsToText, runText, spendToday, waitingText, type ChartNode } from '@/lib/overview'

const now = Date.UTC(2026, 10, 26, 12, 0, 0)

function summary(id: string, init: MessageInitShape<typeof AgentSummarySchema> = {}): AgentSummary {
  return create(AgentSummarySchema, { agentId: id, displayName: id, status: AgentStatus.ACTIVE, ...init })
}
const toAgent = (agentId: string, displayName = agentId) => ({ target: { case: 'agentId' as const, value: agentId }, displayName })
const toUser = (userId: string, displayName: string) => ({ target: { case: 'userId' as const, value: userId }, displayName })

// A tree as nested [name, label, children] for short expectations.
type Shape = [string, string, Shape[]]
const shape = (nodes: ChartNode[]): Shape[] => nodes.map((n) => [n.agent.agentId, n.reportsToLabel, shape(n.children)])

describe('parseOverviewTab', () => {
  it('knows the three tabs and nothing else', () => {
    expect(['agents', 'chart', 'board', 'inbox', 1].map(parseOverviewTab)).toEqual(['agents', 'chart', 'board', undefined, undefined])
  })
})

describe('groupByTeam', () => {
  it('keeps the server order and puts the agents with no team last as "No team"', () => {
    const groups = groupByTeam([summary('a', { team: 'Ops' }), summary('b', { team: 'Ops' }), summary('c'), summary('d', { team: 'Web' })])
    expect(groups.map((g) => [g.label, g.agents.map((a) => a.agentId)])).toEqual([
      ['Ops', ['a', 'b']],
      ['Web', ['d']],
      ['No team', ['c']],
    ])
  })
})

describe('reportsToForest', () => {
  it('puts an agent under the agent of its team that it reports to, and a person as the label of a root', () => {
    const roots = reportsToForest([
      summary('lead', { reportsTo: toUser('u-1', 'Ada') }),
      summary('dev', { reportsTo: toAgent('lead') }),
      summary('qa', { reportsTo: toAgent('dev') }),
      summary('solo'),
    ])
    expect(shape(roots)).toEqual([
      ['lead', 'Reports to Ada', [['dev', '', [['qa', '', []]]]]],
      ['solo', '', []],
    ])
  })

  it('makes an agent that reports to an agent outside the team a root with a label', () => {
    expect(shape(reportsToForest([summary('dev', { reportsTo: toAgent('other-team-lead', 'Lead bot') })]))).toEqual([['dev', 'Reports to Lead bot', []]])
  })

  it('keeps every agent of a cycle, with the agent that closes it as a root', () => {
    const roots = reportsToForest([summary('c', { reportsTo: toAgent('a') }), summary('a', { reportsTo: toAgent('b') }), summary('b', { reportsTo: toAgent('a') })])
    expect(shape(roots)).toEqual([['a', 'Reports to b', [['c', '', []], ['b', '', []]]]])
  })

  it('builds a long chain in one pass, also when the chain closes a cycle at its top', () => {
    const n = 20_000
    // c-0 reports to c-1, ... c-(n-1) reports to c-0: one cycle through every agent. A walk per agent to the top would
    // be about 2·10^8 steps (seconds); one pass is a few milliseconds, so the wide limit still tells the two apart.
    const chain = Array.from({ length: n }, (_, i) => summary(`c-${i}`, { reportsTo: toAgent(`c-${(i + 1) % n}`) }))
    const start = performance.now()
    const roots = reportsToForest(chain)
    expect(performance.now() - start).toBeLessThan(2000)
    expect(roots).toHaveLength(1)
    let depth = 0
    for (let node: ChartNode | undefined = roots[0]; node; node = node.children[0]) depth++
    expect(depth).toBe(n)
  })

  it('ignores an agent that reports to itself', () => {
    expect(shape(reportsToForest([summary('a', { reportsTo: toAgent('a', 'a') })]))).toEqual([['a', 'Reports to a', []]])
  })
})

describe('card fields', () => {
  it('names the state: unhealthy wins over the status', () => {
    expect(cardState(summary('a'))).toBe('active')
    expect(cardState(summary('a', { status: AgentStatus.DRAFT }))).toBe('draft')
    expect(cardState(summary('a', { unhealthyAt: timestampFromMs(now) }))).toBe('unhealthy')
  })

  it('says whom the agent reports to', () => {
    expect(reportsToText(summary('a', { reportsTo: toUser('u-1', 'Ada') }))).toBe('Reports to Ada')
    expect(reportsToText(summary('a'))).toBe('')
  })

  it('shows the spend against the daily limit, or that there is no limit', () => {
    expect(spendToday(summary('a', { spentTodayUsd: '1.2', dailyLimitUsd: '5' }))).toEqual({ text: '$1.2 of $5.00 today', percent: 24 })
    expect(spendToday(summary('a', { spentTodayUsd: '6', dailyLimitUsd: '5' })).percent).toBe(120)
    expect(spendToday(summary('a'))).toEqual({ text: '$0.00 today, no daily limit' })
  })

  it('shows running now, else the last run, else no runs', () => {
    expect(runText(summary('a', { runningRuns: 1, lastRunState: RunState.RUNNING }), now)).toBe('Running now')
    expect(runText(summary('a', { runningRuns: 3 }), now)).toBe('Running now (3 runs)')
    expect(runText(summary('a', { lastRunState: RunState.FAILED, lastRunAt: timestampFromMs(now - 3 * 3_600_000) }), now)).toBe('Last run failed 3 hours ago')
    expect(runText(summary('a'), now)).toBe('No runs yet')
  })

  it('shows the next scheduled run, due now when its time has passed', () => {
    expect(nextRunText(summary('a', { nextScheduledAt: timestampFromMs(now + 2 * 3_600_000) }), now)).toBe('Next run in 2 hours')
    expect(nextRunText(summary('a', { nextScheduledAt: timestampFromMs(now - 10_000) }), now)).toBe('Next run due now')
    expect(nextRunText(summary('a'), now)).toBe('')
  })

  it('counts what waits on the viewer', () => {
    expect([0, 1, 4].map(waitingText)).toEqual(['', '1 waiting on you', '4 waiting on you'])
  })
})
