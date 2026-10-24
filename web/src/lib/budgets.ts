// SPDX-License-Identifier: Apache-2.0

import { timestampDate, type Timestamp } from '@bufbuild/protobuf/wkt'
import type { Notification } from '@/gen/koinon/budget/v1alpha1/budget_pb'
import { percent } from '@/lib/money'

export type Level = 'agent_daily' | 'org_monthly' | 'platform_ceiling'

export function levelLabel(level: string): string {
  switch (level) {
    case 'agent_daily':
      return 'An agent’s daily limit'
    case 'org_monthly':
      return 'The org’s monthly cap'
    case 'platform_ceiling':
      return 'The platform ceiling'
    default:
      return 'A spend limit'
  }
}

/** isAgentUnhealthy: Koinon marked an agent unhealthy (a sandbox it could not recover). */
export function isAgentUnhealthy(n: Notification): boolean {
  return n.kind === 'agent.unhealthy'
}

/** isBudgetKind: the notification is a spend-limit notice (or an older one with no kind recorded). */
function isBudgetKind(n: Notification): boolean {
  return n.kind === '' || n.kind.startsWith('budget.')
}

/** noticeText says what a notification means, in plain words. */
export function noticeText(n: Notification, agentNames: ReadonlyMap<string, string> = new Map()): string {
  if (isAgentUnhealthy(n)) return n.message || `${agentNames.get(n.agentId) ?? 'An agent'} was marked unhealthy.`
  // A kind this version does not know shows the server's message rather than a wrong budget sentence.
  if (!isBudgetKind(n)) return n.message || 'A notification this version does not recognise.'
  const who = n.level === 'agent_daily' && n.agentId ? `${agentNames.get(n.agentId) ?? 'An agent'}’s daily limit` : levelLabel(n.level)
  const spent = `$${n.spentUsd} of $${n.limitUsd}`
  return n.threshold >= 100
    ? `${who} has been reached (${spent}). Runs that count against it were stopped.`
    : `${who} is at ${n.threshold}% (${spent}). Runs stop when it is reached.`
}

/** The start of the window a level counts in: the UTC day for an agent, otherwise the UTC calendar month. */
export function windowStart(level: string, now: Date): Date {
  return level === 'agent_daily'
    ? new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate()))
    : new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1))
}

/** The first moment of the next UTC calendar month, when the monthly cap counts from zero again. */
export function nextMonthStart(now: Date): Date {
  return new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() + 1, 1))
}

/** isCurrent: the notification is about the window that is running now, not an earlier month or day. */
export function isCurrent(n: Notification, now: Date): boolean {
  if (!n.windowStart) return true
  return timestampDate(n.windowStart).getTime() >= windowStart(n.level, now).getTime()
}

/**
 * bannerNotices are the notices the banner shows: unread, about the window now running, and for each limit only the
 * highest threshold (a 100% notice replaces the 80% one). Platform-ceiling notices go to platform admins only.
 */
export function bannerNotices(list: readonly Notification[], now: Date, platformAdmin: boolean): Notification[] {
  const best = new Map<string, Notification>()
  for (const n of list) {
    if (n.readAt || !isCurrent(n, now) || (n.level === 'platform_ceiling' && !platformAdmin)) continue
    const key = `${n.level}:${n.agentId}`
    const cur = best.get(key)
    if (!cur || n.threshold > cur.threshold) best.set(key, n)
  }
  return [...best.values()].sort((a, b) => b.threshold - a.threshold)
}

/** usedLabel: "62% of the cap", for the meter's text. */
export function usedLabel(spent: string, limit: string): string {
  const p = percent(spent, limit)
  return p === undefined ? 'no cap' : `${p}% used`
}

/** The agent's limit differs from what its template starts with. */
export function overridden(limit: string, templateDefault: string): boolean {
  return templateDefault !== '' && limit !== templateDefault
}

export function formatDate(ts: Timestamp | undefined): string {
  return ts ? timestampDate(ts).toLocaleString() : ''
}
