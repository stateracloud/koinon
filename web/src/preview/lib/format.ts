// SPDX-License-Identifier: Apache-2.0

// Pure helpers behind the preview components: state labels and colours, ages, initials and hues, meter levels.

import type { CSSProperties } from 'react'
import { cn } from '@/lib/utils'
import type { AgentState, ApprovalState, ConnectionState, Hue, RunState, TaskState } from '@/preview/mock/data'

/** AgeBand is how far an age chip has escalated (README "Ageing"). */
export type AgeBand = 'new' | '1d' | '3d' | '7d'

export function ageBand(hours: number): AgeBand {
  if (hours >= 168) return '7d'
  if (hours >= 72) return '3d'
  if (hours >= 24) return '1d'
  return 'new'
}

/** ageText is "5 h" under a day, else whole days: "9 d". */
export function ageText(hours: number): string {
  return hours < 24 ? `${Math.max(1, Math.round(hours))} h` : `${Math.floor(hours / 24)} d`
}

export type StatusKind = 'task' | 'run' | 'agent' | 'approval' | 'connection'

export type StateOf = { task: TaskState; run: RunState; agent: AgentState; approval: ApprovalState; connection: ConnectionState }

/** Each status maps to one of the seven state colours (state-*) and a plain-English label. */
const statusMap: { [K in StatusKind]: Record<StateOf[K], [TaskState, string]> } = {
  task: {
    submitted: ['submitted', 'Submitted'],
    working: ['working', 'Working'],
    blocked: ['blocked', 'Blocked on you'],
    stale: ['stale', 'Stale'],
    completed: ['completed', 'Completed'],
    failed: ['failed', 'Failed'],
    canceled: ['canceled', 'Canceled'],
  },
  run: {
    queued: ['submitted', 'Queued'],
    starting: ['working', 'Starting'],
    running: ['working', 'Running'],
    parked: ['blocked', 'Parked'],
    succeeded: ['completed', 'Succeeded'],
    failed: ['failed', 'Failed'],
    canceled: ['canceled', 'Canceled'],
  },
  agent: {
    active: ['completed', 'Active'],
    draft: ['submitted', 'Draft'],
    unhealthy: ['failed', 'Unhealthy'],
    paused: ['stale', 'Paused'],
    archived: ['canceled', 'Archived'],
  },
  approval: {
    pending: ['blocked', 'Waiting for a decision'],
    approved: ['completed', 'Approved'],
    denied: ['failed', 'Denied'],
    executed: ['completed', 'Done'],
    failed: ['failed', 'Failed'],
    expired: ['canceled', 'Expired'],
    canceled: ['canceled', 'Canceled'],
  },
  connection: {
    ok: ['completed', 'Working'],
    failing: ['failed', 'Failing'],
    untested: ['canceled', 'Not tested'],
  },
}

/** statusLabel is the plain-English word for a status, such as "Blocked on you" or "Not tested". */
export function statusLabel<K extends StatusKind>(kind: K, state: StateOf[K]): string {
  return (statusMap[kind] as Record<string, [TaskState, string]>)[state]?.[1] ?? 'Unknown'
}

/** statusTone is the state colour a status uses (the data-st of ks-dot and ks-tile). */
export function statusTone<K extends StatusKind>(kind: K, state: StateOf[K]): TaskState {
  return (statusMap[kind] as Record<string, [TaskState, string]>)[state]?.[0] ?? 'canceled'
}

/** The eight categorical hues that tell agents and teams apart (cat-*). */
const categorical: Hue[] = ['orange', 'gold', 'green', 'teal', 'sky', 'blue', 'lavender', 'slate']

/** initialsOf is the first letter of the first two words: "LCM Upgrader" → "LU", "Researcher" → "Re". */
export function initialsOf(name: string): string {
  const words = name.trim().split(/\s+/).filter(Boolean)
  if (words.length === 0) return '?'
  if (words.length === 1) return words[0].slice(0, 2).replace(/^./, (c) => c.toUpperCase())
  return (words[0][0] + words[1][0]).toUpperCase()
}

/** hueOf picks a stable categorical hue for a name. */
export function hueOf(name: string): Hue {
  let h = 0
  for (const ch of name) h = (h * 31 + ch.charCodeAt(0)) >>> 0
  return categorical[h % categorical.length]
}

/** markStyle is the tinted tile of a hue: the 200 ground, 1000 text and a 600 ring (sidebar marks, avatars). */
export function markStyle(hue: Hue): CSSProperties {
  if (hue === 'gray') return { background: 'var(--gray-200)', color: 'var(--foreground)', boxShadow: 'inset 0 0 0 1px var(--border)' }
  return { background: `var(--${hue}-200)`, color: `var(--${hue}-1000)`, boxShadow: `inset 0 0 0 1px var(--${hue}-600)` }
}

/** meterLevel is neutral below 75%, amber from 75% and red at the limit. */
export function meterLevel(percent: number): 'ok' | 'warn' | 'over' {
  return percent >= 100 ? 'over' : percent >= 75 ? 'warn' : 'ok'
}

/** describedBy is the aria-describedby for a Field's control. */
export function describedBy(id: string, opts: { hint?: boolean; error?: boolean } = { hint: true }): string | undefined {
  const ids = [opts.hint && `${id}-hint`, opts.error && `${id}-error`].filter(Boolean)
  return ids.length ? ids.join(' ') : undefined
}

export type ButtonVariant = 'default' | 'destructive' | 'outline' | 'secondary' | 'ghost' | 'link'
export type ButtonSize = 'default' | 'xs' | 'sm' | 'lg' | 'icon' | 'icon-xs' | 'icon-sm' | 'icon-lg'

/** buttonClass is the design system's kn-btn classes; one default (purple) button per form. */
export function buttonClass(variant: ButtonVariant = 'default', size: ButtonSize = 'default', className?: string): string {
  return cn('kn-btn', `kn-btn-${variant}`, `kn-btn-size-${size}`, className)
}
