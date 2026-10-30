// SPDX-License-Identifier: Apache-2.0

// Screen-only data for the overview and inbox screens (Main.dc.html, Inbox.dc.html), seeded from the mockups.

import { agents, type Task, type TaskState } from '@/preview/mock/data'

/** The board's columns, in mockup order. */
export const boardColumns: { key: TaskState; label: string }[] = [
  { key: 'submitted', label: 'Submitted' },
  { key: 'working', label: 'Working' },
  { key: 'blocked', label: 'Blocked on you' },
  { key: 'stale', label: 'Stale' },
  { key: 'completed', label: 'Completed' },
  { key: 'failed', label: 'Failed' },
  { key: 'canceled', label: 'Canceled' },
]

export type BoardView = 'upgrades' | 'failed' | 'open'

export const boardViews: { id: BoardView; label: string }[] = [
  { id: 'upgrades', label: 'Upgrades needing me' },
  { id: 'failed', label: 'Failed today' },
  { id: 'open', label: 'All open' },
]

/** The chips each saved view sets. */
export const viewChips: Record<BoardView, { state: string[]; team: string[] }> = {
  upgrades: { state: ['blocked', 'stale'], team: ['upgrades'] },
  failed: { state: ['failed'], team: [] },
  open: { state: ['submitted', 'working', 'blocked', 'stale'], team: [] },
}

export function teamOf(task: Task): string {
  return (agents.find((a) => a.name === task.agent)?.team ?? '').toLowerCase()
}

/** Which inbox card a "Waiting on you" tile opens (the tile titles differ a little from the inbox rows). */
export const waitingToInbox: Record<number, string> = { 0: 'pr', 1: 'q', 2: 'plan', 3: 'edit', 4: 'snooze' }

export const planSteps = [
  { n: 1, what: 'Read Helm releases', access: 'read', target: 'staging / cert-manager', why: 'Already allowed', badge: 'secondary' },
  { n: 2, what: 'Apply the 1.19 chart', access: 'write', target: 'staging / cert-manager', why: 'Upgrade the release', badge: 'outline' },
  { n: 3, what: 'Run the webhook smoke test', access: 'create job', target: 'staging / smoke-tests', why: 'Check the webhook answers', badge: 'outline' },
] as const

export const costCenterAnswers = ['ML Platform (CC-4410)', 'Research (CC-2071)', 'Unknown, flag them']

/** What each card's keys do, so the footer and the palette read from one place. */
export type InboxAction = 'accept' | 'edit' | 'respond' | 'snooze' | 'ignore' | 'read'

export const declared: Record<string, InboxAction[]> = {
  pr: ['accept', 'respond', 'ignore'],
  q: ['respond', 'ignore'],
  plan: ['accept', 'respond', 'ignore'],
  edit: ['accept', 'edit', 'respond', 'ignore'],
  snooze: ['snooze', 'respond'],
  mention: ['read', 'snooze'],
  reminder: ['read', 'snooze'],
}
