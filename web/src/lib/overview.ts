// SPDX-License-Identifier: Apache-2.0

import { timestampMs, type Timestamp } from '@bufbuild/protobuf/wkt'
import { AgentStatus, type AgentSummary } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { RunState } from '@/gen/koinon/run/v1alpha1/run_pb'
import { formatUSD } from '@/lib/cost'
import { runStateLabel } from '@/lib/labels'
import { percent } from '@/lib/money'
import { age } from '@/lib/time'

/** OverviewTab is a tab of the org home; the URL carries it as ?tab= (Agents, the default, has none). */
export type OverviewTab = 'agents' | 'chart' | 'board'

export function parseOverviewTab(v: unknown): OverviewTab | undefined {
  return v === 'agents' || v === 'chart' || v === 'board' ? v : undefined
}

/** The "Waiting on you" strip of the org home shows at most this many inbox items (ADR 0126 section 4). */
export const waitingStripMax = 5

/** The heading of the agents with no team. The server orders them last. */
export const noTeamLabel = 'No team'

export type TeamGroup<T> = { team: string; label: string; agents: T[] }

/**
 * groupByTeam groups agents by their team label in the order they come. ListAgentSummaries orders by team with the
 * agents that have no team last, so each team is one run; a team that comes again later (it cannot from one server
 * order, but pages can) joins its first group.
 */
export function groupByTeam<T extends Pick<AgentSummary, 'team'>>(agents: readonly T[]): TeamGroup<T>[] {
  const groups = new Map<string, TeamGroup<T>>()
  for (const a of agents) {
    let g = groups.get(a.team)
    if (!g) {
      g = { team: a.team, label: a.team === '' ? noTeamLabel : a.team, agents: [] }
      groups.set(a.team, g)
    }
    g.agents.push(a)
  }
  const out = [...groups.values()]
  // Keep "No team" last also when a later page brings a team the first pages did not have.
  return [...out.filter((g) => g.team !== ''), ...out.filter((g) => g.team === '')]
}

/** ChartNode is one agent in a reports-to tree. */
export type ChartNode = {
  agent: AgentSummary
  children: ChartNode[]
  /**
   * The "Reports to" label of a root: a person, or an agent that is not in this tree (another team, or a page that
   * has not loaded). Empty for an agent under another agent of the tree, and for a root that reports to nobody.
   */
  reportsToLabel: string
}

/**
 * reportsToForest builds the reports-to trees of one team (ADR 0126 section 4). Only agents are nodes. An agent is
 * under the agent it reports to when that agent is in the same team; else it is a root, and a person or an agent of
 * another team shows as its "Reports to" label. A cycle (the API does not refuse one) cannot hide an agent: the agent
 * that closes it becomes a root.
 */
export function reportsToForest(agents: readonly AgentSummary[]): ChartNode[] {
  const nodes = new Map<string, ChartNode>(agents.map((a) => [a.agentId, { agent: a, children: [], reportsToLabel: '' }]))
  const parentOf = (a: AgentSummary): string | undefined => {
    const t = a.reportsTo?.target
    return t?.case === 'agentId' && t.value !== a.agentId && nodes.has(t.value) ? t.value : undefined
  }
  // A walk up the parent links that comes back to an agent found a cycle: that agent becomes a root. A walk stops at an
  // agent an earlier walk resolved (its chain up ends), so the whole check is O(n) also for one long chain.
  const cut = new Set<string>()
  const resolved = new Set<string>()
  for (const a of agents) {
    const seen = new Set<string>([a.agentId])
    for (let p = parentOf(a); p !== undefined && !resolved.has(p); p = cut.has(p) ? undefined : parentOf(nodes.get(p)!.agent)) {
      if (seen.has(p)) {
        cut.add(p)
        break
      }
      seen.add(p)
    }
    for (const id of seen) resolved.add(id)
  }
  const roots: ChartNode[] = []
  for (const a of agents) {
    const n = nodes.get(a.agentId)!
    const p = cut.has(a.agentId) ? undefined : parentOf(a)
    if (p === undefined) {
      n.reportsToLabel = a.reportsTo?.displayName ? `Reports to ${a.reportsTo.displayName}` : ''
      roots.push(n)
    } else {
      nodes.get(p)!.children.push(n)
    }
  }
  return roots
}

/** The card state of ADR 0126 section 4: Unhealthy wins over the status, then Active or Draft. */
export type CardState = 'active' | 'draft' | 'unhealthy'

export function cardState(a: Pick<AgentSummary, 'status' | 'unhealthyAt'>): CardState {
  if (a.unhealthyAt) return 'unhealthy'
  return a.status === AgentStatus.DRAFT ? 'draft' : 'active'
}

/** reportsToText is the "Reports to" line of a card; empty when the agent reports to nobody (or to a person who left). */
export function reportsToText(a: Pick<AgentSummary, 'reportsTo'>): string {
  return a.reportsTo?.displayName ? `Reports to ${a.reportsTo.displayName}` : ''
}

/** SpendToday is today's spend against the daily limit, for the card meter. */
export type SpendToday = { text: string; percent?: number }

/** spendToday says "$1.20 of $5.00 today", or "$1.20 today, no daily limit". */
export function spendToday(a: Pick<AgentSummary, 'spentTodayUsd' | 'dailyLimitUsd'>): SpendToday {
  const spent = formatUSD(a.spentTodayUsd || '0')
  if (a.dailyLimitUsd === '') return { text: `${spent} today, no daily limit` }
  return { text: `${spent} of ${formatUSD(a.dailyLimitUsd)} today`, percent: percent(a.spentTodayUsd || '0', a.dailyLimitUsd) }
}

const tsMs = (t: Timestamp | undefined) => (t ? timestampMs(t) : undefined)

/** runText says "Running now" (with the count above one), else the state and age of the last run, else "No runs yet". */
export function runText(a: Pick<AgentSummary, 'runningRuns' | 'lastRunState' | 'lastRunAt'>, now: number = Date.now()): string {
  if (a.runningRuns > 1) return `Running now (${a.runningRuns} runs)`
  if (a.runningRuns === 1) return 'Running now'
  if (a.lastRunState === RunState.UNSPECIFIED) return 'No runs yet'
  const state = runStateLabel(a.lastRunState)
  const at = tsMs(a.lastRunAt)
  return at === undefined ? `Last run ${state}` : `Last run ${state} ${age(at, now)} ago`
}

/** nextRunText says when the next scheduled run is due ("Next run in 3 hours"), or empty when there is none. */
export function nextRunText(a: Pick<AgentSummary, 'nextScheduledAt'>, now: number = Date.now()): string {
  const at = tsMs(a.nextScheduledAt)
  if (at === undefined) return ''
  // The scheduler starts a run up to 30 seconds late (ADR 0066): a due time in the past is "due now".
  return at <= now ? 'Next run due now' : `Next run in ${age(now, at)}`
}

/** waitingText is the card's count of inbox items for the caller, or empty when there are none. */
export function waitingText(n: number): string {
  if (n <= 0) return ''
  return `${n} waiting on you`
}
