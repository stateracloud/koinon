// SPDX-License-Identifier: Apache-2.0

import type { Cost } from '@/gen/koinon/run/v1alpha1/run_pb'

/**
 * formatUSD shows a ledger amount, an exact decimal string such as "0.0021",
 * in US dollars: "$0.0021", "$1,234.50". The digits are kept as they are, so
 * no rounding hides a small spend.
 */
export function formatUSD(usd: string): string {
  const m = /^(-?)(\d+)(\.\d+)?$/.exec(usd.trim())
  if (!m) return usd.trim() ? `$${usd.trim()}` : '$0.00'
  const [, sign, int, frac = '.00'] = m
  return `${sign}$${int.replace(/\B(?=(\d{3})+(?!\d))/g, ',')}${frac}`
}

/**
 * costLabel is a cost for display, or undefined when the server sent none.
 * When some request had no price the amount undercounts, so it is shown as a
 * lower bound.
 */
export function costLabel(c: Cost | undefined): string | undefined {
  if (!c) return undefined
  const usd = formatUSD(c.usd)
  return c.priced ? usd : `at least ${usd}`
}

/** costDetail describes what a cost counts, for a tooltip. */
export function costDetail(c: Cost): string {
  const requests = `${c.requests} model ${c.requests === 1n ? 'request' : 'requests'}`
  const tokens = `${c.inputTokens} input and ${c.outputTokens} output tokens`
  return c.priced ? `${requests}, ${tokens}` : `${requests}, ${tokens}; some requests had no price`
}
