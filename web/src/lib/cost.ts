// SPDX-License-Identifier: Apache-2.0

import type { Cost, Run } from '@/gen/koinon/run/v1alpha1/run_pb'
import type { RunLimits } from '@/gen/koinon/template/v1alpha1/template_pb'

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

/** estimateNote says how many of a cost's requests were estimated, or undefined when none were. */
export function estimateNote(c: Cost | undefined): string | undefined {
  if (!c || c.estimatedRequests <= 0n) return undefined
  return `${c.estimatedRequests} of ${c.requests} ${c.requests === 1n ? 'request' : 'requests'} estimated`
}

/** costDetail describes what a cost counts, for a tooltip. */
export function costDetail(c: Cost): string {
  const requests = `${c.requests} model ${c.requests === 1n ? 'request' : 'requests'}`
  const tokens = `${c.inputTokens} input and ${c.outputTokens} output tokens${tokenSplit(c)}`
  const base = c.priced ? `${requests}, ${tokens}` : `${requests}, ${tokens}; some requests had no price`
  if (c.estimatedRequests <= 0n) return base
  return `${base}. ${estimateNote(c)}: the provider did not report their usage, so Koinon estimated it from the size of the request, and replaces the estimate with the provider's own figure when the provider reports one. Estimates count toward budgets.`
}

/**
 * tokenSplit says how a cost's tokens divide by type (ADR 0081): of the input, what came from the
 * provider's cache and what was written to it; of the output, what was reasoning. Empty when the
 * ledger has no split.
 */
function tokenSplit(c: Cost): string {
  const parts: string[] = []
  if (c.cacheReadTokens > 0n) parts.push(`${c.cacheReadTokens} read from the cache`)
  if (c.cacheWriteTokens > 0n) parts.push(`${c.cacheWriteTokens} written to it`)
  if (c.reasoningTokens > 0n) parts.push(`${c.reasoningTokens} of the output reasoning`)
  return parts.length ? ` (${parts.join(', ')})` : ''
}

function humanDuration(totalSeconds: number): string {
  const s = Math.max(0, Math.round(totalSeconds))
  if (s < 60) return `${s}s`
  const h = Math.floor(s / 3600)
  const m = Math.floor((s % 3600) / 60)
  return `${h > 0 ? `${h}h` : ''}${m > 0 ? `${m}m` : ''}` || '1m'
}

/**
 * runLimitsLabel is the limits of a run for an agent or template page (ADR 0081): "30m, 200
 * requests, $2.00". The first limit a run reaches ends it. Tokens are not a limit.
 */
export function runLimitsLabel(l: Pick<RunLimits, 'maxDurationSeconds' | 'maxModelRequests' | 'maxSpendUsd'>): string {
  const parts: string[] = []
  if (l.maxDurationSeconds > 0n) parts.push(humanDuration(Number(l.maxDurationSeconds)))
  if (l.maxModelRequests > 0n) parts.push(`${l.maxModelRequests.toLocaleString()} model requests`)
  if (l.maxSpendUsd) parts.push(formatUSD(l.maxSpendUsd))
  return parts.join(', ')
}

/**
 * runProgress is a run's use of its limits: "12 of 200 requests", "$0.42 of $2.00", "6m of 30m". A
 * limit the run does not have (a run queued before ADR 0081 has no request or spend limit) shows
 * what was used alone. Time is the span since the run started, which counts parked time that the
 * limit does not. now is when to stop counting an unfinished run.
 */
export function runProgress(run: Pick<Run, 'maxWallClockSeconds' | 'maxModelRequests' | 'maxSpendUsd' | 'startedAt' | 'finishedAt'>, cost: Cost | undefined, now: Date): string[] {
  const parts: string[] = []
  if (cost) {
    const requests = cost.requests.toLocaleString()
    parts.push(run.maxModelRequests > 0n ? `${requests} of ${run.maxModelRequests.toLocaleString()} requests` : `${requests} ${cost.requests === 1n ? 'request' : 'requests'}`)
    const spent = formatUSD(cost.usd)
    parts.push(run.maxSpendUsd ? `${spent} of ${formatUSD(run.maxSpendUsd)}` : spent)
  }
  if (run.startedAt) {
    const end = run.finishedAt ? Number(run.finishedAt.seconds) * 1000 : now.getTime()
    const took = humanDuration((end - Number(run.startedAt.seconds) * 1000) / 1000)
    parts.push(run.maxWallClockSeconds > 0n ? `${took} of ${humanDuration(Number(run.maxWallClockSeconds))}` : took)
  }
  return parts
}
