// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { CostSchema, RunSchema } from '@/gen/koinon/run/v1alpha1/run_pb'
import { RunLimitsSchema } from '@/gen/koinon/template/v1alpha1/template_pb'
import { costDetail, costLabel, estimateNote, formatUSD, runLimitsLabel, runProgress } from '@/lib/cost'

describe('formatUSD', () => {
  it.each([
    ['0.00', '$0.00'],
    ['0.0021', '$0.0021'],
    ['12.5', '$12.5'],
    ['1234.50', '$1,234.50'],
    ['1234567', '$1,234,567.00'],
    ['-3.10', '-$3.10'],
    ['', '$0.00'],
    ['n/a', '$n/a'],
  ])('%s is %s', (usd, want) => {
    expect(formatUSD(usd)).toBe(want)
  })
})

describe('costLabel', () => {
  it('is undefined without a cost', () => {
    expect(costLabel(undefined)).toBeUndefined()
  })

  it('shows a priced cost as is and an unpriced one as a lower bound', () => {
    expect(costLabel(create(CostSchema, { usd: '0.42', priced: true }))).toBe('$0.42')
    expect(costLabel(create(CostSchema, { usd: '0.42', priced: false }))).toBe('at least $0.42')
  })
})

describe('costDetail', () => {
  it('counts requests and tokens', () => {
    expect(costDetail(create(CostSchema, { requests: 1n, inputTokens: 1200n, outputTokens: 80n, usd: '0.01', priced: true }))).toBe(
      '1 model request, 1200 input and 80 output tokens',
    )
    expect(costDetail(create(CostSchema, { requests: 3n, priced: false }))).toBe('3 model requests, 0 input and 0 output tokens; some requests had no price')
  })

  it('says which requests were estimated and why', () => {
    const c = create(CostSchema, { requests: 5n, estimatedRequests: 2n, inputTokens: 10n, outputTokens: 4n, usd: '0.42', priced: true })
    expect(estimateNote(c)).toBe('2 of 5 requests estimated')
    expect(estimateNote(create(CostSchema, { requests: 1n, estimatedRequests: 1n }))).toBe('1 of 1 request estimated')
    expect(estimateNote(create(CostSchema, { requests: 5n }))).toBeUndefined()
    expect(estimateNote(undefined)).toBeUndefined()
    expect(costDetail(c)).toContain('did not report their usage')
    expect(costDetail(create(CostSchema, { requests: 1n, priced: true }))).not.toContain('estimated')
  })
})

describe('costDetail token split', () => {
  it('says how the tokens divide by type', () => {
    const c = create(CostSchema, { requests: 2n, inputTokens: 1200n, outputTokens: 80n, cacheReadTokens: 900n, cacheWriteTokens: 100n, reasoningTokens: 30n, usd: '0.01', priced: true })
    expect(costDetail(c)).toBe('2 model requests, 1200 input and 80 output tokens (900 read from the cache, 100 written to it, 30 of the output reasoning)')
  })
})

describe('runLimitsLabel', () => {
  it('lists time, requests and spend', () => {
    expect(runLimitsLabel(create(RunLimitsSchema, { maxDurationSeconds: 1800n, maxModelRequests: 200n, maxSpendUsd: '2.00' }))).toBe('30m, 200 model requests, $2.00')
    expect(runLimitsLabel(create(RunLimitsSchema, { maxDurationSeconds: 5400n }))).toBe('1h30m')
  })
})

describe('runProgress', () => {
  const at = (iso: string) => ({ seconds: BigInt(Math.floor(Date.parse(iso) / 1000)), nanos: 0 })
  const now = new Date('2026-10-29T10:06:00Z')

  it('shows requests, spend and time against the limits', () => {
    const run = create(RunSchema, { maxWallClockSeconds: 1800n, maxModelRequests: 200n, maxSpendUsd: '2.00', startedAt: at('2026-10-29T10:00:00Z') })
    const cost = create(CostSchema, { requests: 12n, usd: '0.42', priced: true })
    expect(runProgress(run, cost, now)).toEqual(['12 of 200 requests', '$0.42 of $2.00', '6m of 30m'])
  })

  it('uses the finish time of a finished run and shows only usage for a run without request or spend limits', () => {
    const run = create(RunSchema, { maxWallClockSeconds: 1800n, startedAt: at('2026-10-29T10:00:00Z'), finishedAt: at('2026-10-29T10:02:30Z') })
    const cost = create(CostSchema, { requests: 1n, usd: '0.01', priced: true })
    expect(runProgress(run, cost, now)).toEqual(['1 request', '$0.01', '2m of 30m'])
  })

  it('shows nothing for a run that has not started and has no cost', () => {
    expect(runProgress(create(RunSchema, {}), undefined, now)).toEqual([])
  })
})
