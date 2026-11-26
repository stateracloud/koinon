// SPDX-License-Identifier: Apache-2.0

export type MeterTone = "normal" | "warning" | "over"

/** meterTone is the tone of a use against a limit: warning from `warnAt` of the limit, over at the limit. */
export function meterTone(value: number, max: number, warnAt = 0.8): MeterTone {
  if (max <= 0) return value > 0 ? "over" : "normal"
  const r = value / max
  if (r >= 1) return "over"
  return r >= warnAt ? "warning" : "normal"
}
