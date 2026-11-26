// SPDX-License-Identifier: Apache-2.0

import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import type { UsageTotals } from '@/gen/koinon/usage/v1alpha1/usage_pb'
import { formatPico, parseLedgerPico } from '@/lib/money'

// The "Usage and cost" views (issue #1281, ADR 0126 section 4) read UsageService (issue #1268). The server sends one
// row per day and dimension; the views group the rows on the client. The sums use BigInt pico-USD, so a group total is
// the exact sum of its rows and the sum of all groups equals the `totals` of the response.

/** usdToPico reads a ledger amount ("12.5", "0.000001") as pico-USD. Anything else counts as 0 (the ledger has no negative cost). */
export function usdToPico(s: string): bigint {
  return parseLedgerPico(s) ?? 0n
}

/** UsageGroup is the sum of the rows that share one key (an org, a model, a day, an agent or a provider). */
export type UsageGroup = {
  key: string
  label: string
  /** detail is a second line under the label (an org slug), or empty. */
  detail: string
  requests: bigint
  inputTokens: bigint
  outputTokens: bigint
  cacheReadTokens: bigint
  cacheWriteTokens: bigint
  /** costUsd is the exact sum of the rows, in the server's decimal form ("12.50"). */
  costUsd: string
  costPico: bigint
}

/** tokensOf is every token of a group: input, output and the two cache buckets (they are disjoint). */
export function tokensOf(g: Pick<UsageGroup, 'inputTokens' | 'outputTokens' | 'cacheReadTokens' | 'cacheWriteTokens'>): bigint {
  return g.inputTokens + g.outputTokens + g.cacheReadTokens + g.cacheWriteTokens
}

/** cacheReadShare is cache reads over the whole input side, from 0 to 1, as the server computes it; 0 when there is none. */
export function cacheReadShare(g: Pick<UsageGroup, 'inputTokens' | 'cacheReadTokens' | 'cacheWriteTokens'>): number {
  const all = g.inputTokens + g.cacheReadTokens + g.cacheWriteTokens
  return all === 0n ? 0 : Number(g.cacheReadTokens) / Number(all)
}

/** groupUsage sums rows by key. The result is in descending cost, then by label. */
export function groupUsage<R extends { usage?: UsageTotals }>(rows: readonly R[], by: (r: R) => { key: string; label: string; detail?: string }): UsageGroup[] {
  const groups = new Map<string, UsageGroup>()
  for (const r of rows) {
    const { key, label, detail = '' } = by(r)
    let g = groups.get(key)
    if (!g) {
      g = { key, label, detail, requests: 0n, inputTokens: 0n, outputTokens: 0n, cacheReadTokens: 0n, cacheWriteTokens: 0n, costUsd: '0.00', costPico: 0n }
      groups.set(key, g)
    }
    const u = r.usage
    if (!u) continue
    g.requests += u.requests
    g.inputTokens += u.inputTokens
    g.outputTokens += u.outputTokens
    g.cacheReadTokens += u.cacheReadTokens
    g.cacheWriteTokens += u.cacheWriteTokens
    g.costPico += usdToPico(u.costUsd)
  }
  const list = [...groups.values()]
  for (const g of list) g.costUsd = formatPico(g.costPico)
  return list.sort((a, b) => (a.costPico === b.costPico ? a.label.localeCompare(b.label) : a.costPico > b.costPico ? -1 : 1))
}

/** sumPico is the exact sum of the groups' cost in pico-USD. */
export function sumPico(groups: readonly UsageGroup[]): bigint {
  return groups.reduce((s, g) => s + g.costPico, 0n)
}

/** formatShare writes a share from 0 to 1 as a percent with at most one decimal: "42.5%". */
export function formatShare(share: number): string {
  return `${Math.round(share * 1000) / 10}%`
}

const countFormat = new Intl.NumberFormat('en-US')

/** formatCount writes a count with thousands separators: "1,234,567". */
export function formatCount(n: bigint | number): string {
  return countFormat.format(n)
}

/** UsageMonth is one calendar month (UTC) the page can show: "2026-11". */
export type UsageMonth = string

/** usageMonths is the current month (UTC) and the `count - 1` months before it, newest first. */
export function usageMonths(now: Date, count = 12): UsageMonth[] {
  const out: UsageMonth[] = []
  for (let i = 0; i < count; i++) {
    const d = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() - i, 1))
    out.push(d.toISOString().slice(0, 7))
  }
  return out
}

/** monthLabel is a month for a person: "November 2026". */
export function monthLabel(m: UsageMonth): string {
  const [y, mo] = m.split('-').map(Number)
  return new Date(Date.UTC(y, mo - 1, 1)).toLocaleDateString('en-US', { month: 'long', year: 'numeric', timeZone: 'UTC' })
}

/**
 * monthRange is the UsageRange of a whole calendar month (UTC): from its first day up to, not including, the first day
 * of the next month. The page and its export send the same range, so they hold the same rows.
 */
export function monthRange(m: UsageMonth) {
  const [y, mo] = m.split('-').map(Number)
  return { from: timestampFromMs(Date.UTC(y, mo - 1, 1)), to: timestampFromMs(Date.UTC(y, mo, 1)) }
}
