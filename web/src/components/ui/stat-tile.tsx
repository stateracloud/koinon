// SPDX-License-Identifier: Apache-2.0

import * as React from "react"
import { cn } from "@/lib/utils"

export type StatTileProps = {
  /** What the number is: "Spend today". */
  label: string
  /** The number, already formatted (money, a count). Tabular figures come from the body style. */
  value: React.ReactNode
  /** One line of context under the value: "of $50.00 daily limit". */
  hint?: React.ReactNode
  /** A Meter or a small chart under the value. */
  children?: React.ReactNode
  className?: string
}

/**
 * StatTile shows one number with its label. It is a description list, so a screen reader reads the label with
 * the value. Put tiles in a grid: `<div className="grid gap-3 sm:grid-cols-3">`.
 */
export function StatTile({ label, value, hint, children, className }: StatTileProps) {
  return (
    <dl data-slot="stat-tile" className={cn("bg-card grid gap-1 rounded-lg border p-4", className)}>
      <dt className="text-muted-foreground text-sm">{label}</dt>
      <dd className="text-2xl font-semibold">{value}</dd>
      {hint && <dd className="text-muted-foreground text-xs">{hint}</dd>}
      {children && <dd className="mt-1">{children}</dd>}
    </dl>
  )
}
