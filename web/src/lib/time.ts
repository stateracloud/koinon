// SPDX-License-Identifier: Apache-2.0

const MINUTE = 60_000
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

/**
 * age says how long ago `since` (ms) was: "less than a minute", "N minutes", "N hours" or "N days",
 * rounded down, with the singular for 1. It works on elapsed milliseconds only, never on calendar
 * fields, so a daylight-saving change does not shift it.
 */
export function age(since: number, now: number = Date.now()): string {
  const elapsed = Math.max(0, now - since)
  const unit = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`
  if (elapsed >= DAY) return unit(Math.floor(elapsed / DAY), 'day')
  if (elapsed >= HOUR) return unit(Math.floor(elapsed / HOUR), 'hour')
  if (elapsed >= MINUTE) return unit(Math.floor(elapsed / MINUTE), 'minute')
  return 'less than a minute'
}
