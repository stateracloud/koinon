// SPDX-License-Identifier: Apache-2.0

import { meterTone, type MeterTone } from "@/lib/meter"
import { cn } from "@/lib/utils"

// The fills are role colours whose text pairs on `raised` pass 4.5:1 (contrast.test.ts), so the bar keeps the
// 3:1 of a non-text part on its track (WCAG 1.4.11). The colour is never the only signal: valueText says it.
const fills: Record<MeterTone, string> = {
  normal: "bg-info",
  warning: "bg-warning",
  over: "bg-danger",
}

export type MeterProps = {
  /** The accessible name: "Spend today". */
  label: string
  value: number
  max: number
  /** The value in words, read by screen readers and shown beside the bar: "$12.40 of $50.00". */
  valueText: string
  /** The share of `max` where the bar turns to the warning colour. */
  warnAt?: number
  /** Hide the label and the text (a tile shows them already); they stay for screen readers. */
  compact?: boolean
  className?: string
}

/** Meter shows a use against a limit (spend against a budget, runs against a cap). It is role=meter, not a progress bar. */
export function Meter({ label, value, max, valueText, warnAt, compact = false, className }: MeterProps) {
  const tone = meterTone(value, max, warnAt)
  const pct = max > 0 ? Math.min(100, Math.max(0, (value / max) * 100)) : value > 0 ? 100 : 0
  return (
    <div data-slot="meter" className={cn("grid gap-1", className)}>
      {!compact && (
        <div className="flex items-baseline justify-between gap-2 text-sm">
          <span>{label}</span>
          <span className="text-muted-foreground">{valueText}</span>
        </div>
      )}
      <div
        role="meter"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={max}
        aria-valuenow={Math.min(value, max)}
        aria-valuetext={valueText}
        data-tone={tone}
        className="bg-raised border-input h-2 w-full overflow-hidden rounded-full border"
      >
        <div className={cn("h-full rounded-full transition-[width]", fills[tone])} style={{ width: `${pct}%` }} />
      </div>
    </div>
  )
}
