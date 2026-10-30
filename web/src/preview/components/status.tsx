// SPDX-License-Identifier: Apache-2.0

import type { CSSProperties } from 'react'
import { cn } from '@/lib/utils'
import { ageBand, ageText, statusLabel, statusTone, type StateOf, type StatusKind } from '@/preview/lib/format'
import type { TaskState } from '@/preview/mock/data'

/** StateDot is the state's dot: filled, or hollow for Stale and Canceled. It is decorative; pair it with a label. */
export function StateDot({ state, className }: { state: TaskState; className?: string }) {
  return <span className={cn('ks-dot', className)} data-st={state} aria-hidden="true" />
}

/**
 * AgeChip is Koinon.AgeChip: how long something has waited on you. Neutral under a day, light orange from 1 day,
 * orange from 3, dark red from 7; the chip always gives the age. Pass hours, or days.
 */
export function AgeChip({ hours, days, className }: { hours?: number; days?: number; className?: string }) {
  const h = hours ?? (days ?? 0) * 24
  const d = Math.floor(h / 24)
  const label = h < 24 ? `Waiting ${Math.max(1, Math.round(h))} hours` : `Waiting ${d} ${d === 1 ? 'day' : 'days'}`
  return (
    <span className={cn('kn-age', className)} data-age={ageBand(h)} aria-label={label} title={label}>
      {ageText(h)}
    </span>
  )
}

export type StatusBadgeProps<K extends StatusKind = StatusKind> = {
  kind: K
  state: StateOf[K]
  /** Overrides the label, such as "Unhealthy, provider failing". */
  label?: string
  /** For a blocked task: how long it has waited; adds the age chip. */
  ageHours?: number
  className?: string
}

/**
 * StatusBadge is Koinon.StatusBadge: a tinted pill with the state's dot and label. Never hand-pick colours for a
 * domain state; use this (or StateDot with a label).
 */
export function StatusBadge<K extends StatusKind>({ kind, state, label, ageHours, className }: StatusBadgeProps<K>) {
  const tone = statusTone(kind, state)
  const style: CSSProperties = { background: `var(--state-${tone}-bg)`, color: `var(--state-${tone}-fg)` }
  const badge = (
    <span className={cn('kn-state', className)} style={style}>
      <StateDot state={tone} />
      {label ?? statusLabel(kind, state)}
    </span>
  )
  if (ageHours === undefined) return badge
  return (
    <span className="ks-row" style={{ gap: 6, display: 'inline-flex' }}>
      {badge}
      <AgeChip hours={ageHours} />
    </span>
  )
}

/** RunStatus is Koinon.RunStatus: the live line per active run, announced politely. */
export function RunStatus({ runs, className }: { runs: { id: string; state: string }[]; className?: string }) {
  return (
    <span role="status" aria-live="polite" className={cn('kn-run-status', className)}>
      {runs.map((r, i) => (
        <span key={r.id}>
          {i > 0 && ' · '}
          Run <code>{r.id.slice(0, 8)}</code> is {r.state}
        </span>
      ))}
    </span>
  )
}
