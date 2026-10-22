// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { CostSchema } from '@/gen/koinon/run/v1alpha1/run_pb'
import { costDetail, costLabel, formatUSD } from '@/lib/cost'

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
})
