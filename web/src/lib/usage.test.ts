// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { PlatformUsageRowSchema, UsageTotalsSchema } from '@/gen/koinon/usage/v1alpha1/usage_pb'
import { cacheReadShare, formatShare, groupUsage, monthLabel, monthRange, sumPico, usageMonths, usdToPico } from '@/lib/usage'

const row = (day: string, orgId: string, model: string, cost: string, tokens: Partial<{ input: bigint; read: bigint; write: bigint }> = {}) =>
  create(PlatformUsageRowSchema, {
    day,
    orgId,
    orgSlug: orgId,
    orgName: orgId.toUpperCase(),
    model,
    usage: create(UsageTotalsSchema, { requests: 1n, costUsd: cost, inputTokens: tokens.input ?? 0n, cacheReadTokens: tokens.read ?? 0n, cacheWriteTokens: tokens.write ?? 0n }),
  })

describe('usdToPico', () => {
  it('reads the server amounts exactly, and anything else as 0', () => {
    expect(usdToPico('12.50')).toBe(12_500_000_000_000n)
    expect(usdToPico('0.000000000001')).toBe(1n)
    expect(usdToPico('12345678.00')).toBe(12_345_678_000_000_000_000n)
    expect(usdToPico('')).toBe(0n)
    expect(usdToPico('-1.00')).toBe(0n)
  })
})

describe('groupUsage', () => {
  const rows = [
    row('2026-11-01', 'a', 'm1', '0.10'),
    row('2026-11-02', 'a', 'm2', '0.20'),
    row('2026-11-02', 'b', 'm1', '0.000000000001'),
    row('2026-11-03', 'c', 'm1', '1.00'),
  ]

  it('sums each key exactly and sorts by cost, highest first', () => {
    const g = groupUsage(rows, (r) => ({ key: r.orgId, label: r.orgName }))
    expect(g.map((x) => [x.label, x.costUsd, x.requests])).toEqual([
      ['C', '1.00', 1n],
      ['A', '0.30', 2n],
      ['B', '0.000000000001', 1n],
    ])
  })

  it('keeps the sum of every grouping equal to the sum of the rows', () => {
    const total = rows.reduce((s, r) => s + usdToPico(r.usage!.costUsd), 0n)
    for (const by of [(r: (typeof rows)[number]) => ({ key: r.orgId, label: r.orgId }), (r: (typeof rows)[number]) => ({ key: r.model, label: r.model }), (r: (typeof rows)[number]) => ({ key: r.day, label: r.day })]) {
      expect(sumPico(groupUsage(rows, by))).toBe(total)
    }
  })

  it('computes the cache read share as the server does', () => {
    const [g] = groupUsage([row('2026-11-01', 'a', 'm', '0', { input: 10n, read: 30n, write: 0n }), row('2026-11-02', 'a', 'm', '0', { input: 0n, read: 10n, write: 50n })], (r) => ({ key: r.orgId, label: r.orgId }))
    expect(cacheReadShare(g)).toBe(0.4)
    expect(formatShare(cacheReadShare(g))).toBe('40%')
    expect(cacheReadShare({ inputTokens: 0n, cacheReadTokens: 0n, cacheWriteTokens: 0n })).toBe(0)
  })
})

describe('months', () => {
  it('lists the current month and the months before it, across a year', () => {
    expect(usageMonths(new Date(Date.UTC(2026, 1, 15)), 3)).toEqual(['2026-02', '2026-01', '2025-12'])
  })

  it('sends a whole UTC month as the range', () => {
    const r = monthRange('2026-12')
    expect(timestampMs(r.from)).toBe(Date.UTC(2026, 11, 1))
    expect(timestampMs(r.to)).toBe(Date.UTC(2027, 0, 1))
    expect(monthLabel('2026-12')).toBe('December 2026')
  })
})
