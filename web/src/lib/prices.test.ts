// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { FlaggedPriceSchema, PriceChangeSchema, PriceEntrySchema, PriceVersionSchema } from '@/gen/koinon/budget/v1alpha1/price_pb'
import {
  changeById,
  changeLabel,
  emptyPriceForm,
  filterFlagged,
  formFromVersion,
  groupPrices,
  ratesLine,
  reasonsOf,
  safeSource,
  toSetPrice,
  validatePrice,
  type PriceFormValues,
} from '@/lib/prices'

const valid = (patch: Partial<PriceFormValues> = {}): PriceFormValues => ({
  ...emptyPriceForm(),
  name: 'sonnet-mine',
  provider: 'openrouter',
  pattern: 'anthropic/claude-sonnet-*',
  rates: { input: '3', output: '15', cacheRead: '', cacheWrite: '', cacheWrite1h: '', reasoning: '' },
  maxOutput: '64000',
  ...patch,
})

describe('validatePrice (like the server)', () => {
  it('accepts a complete price', () => {
    expect(validatePrice(valid())).toEqual({})
  })

  it('requires input and output, and at most six fraction digits', () => {
    const e = validatePrice(valid({ rates: { input: '', output: '1.1234567', cacheRead: 'x', cacheWrite: '', cacheWrite1h: '', reasoning: '' } }))
    expect(e.input).toMatch(/required/)
    expect(e.output).toMatch(/6 digits/)
    expect(e.cacheRead).toMatch(/decimal/)
  })

  it('requires the largest output for a priced entry, not for a free one', () => {
    expect(validatePrice(valid({ maxOutput: '' })).maxOutput).toMatch(/required/)
    expect(validatePrice(valid({ maxOutput: '', rates: { input: '0', output: '0', cacheRead: '', cacheWrite: '', cacheWrite1h: '', reasoning: '' } })).maxOutput).toBeUndefined()
    expect(validatePrice(valid({ maxOutput: '20000000' })).maxOutput).toMatch(/up to/)
  })

  it('checks the name, provider and pattern', () => {
    expect(validatePrice(valid({ name: 'Bad Name' })).name).toBeDefined()
    expect(validatePrice(valid({ name: 'Bad Name' }), true).name).toBeUndefined()
    expect(validatePrice(valid({ provider: 'Open Router' })).provider).toBeDefined()
    expect(validatePrice(valid({ provider: '*' })).provider).toBeUndefined()
    expect(validatePrice(valid({ pattern: ' ' })).pattern).toMatch(/required/)
    expect(validatePrice(valid({ matchKind: 'regex', pattern: '(' })).pattern).toMatch(/regular expression/)
  })

  it('wants tier thresholds that go up and tiers that set a price', () => {
    const tier = (over: string, input = '6') => ({ contextOver: over, rates: { input, output: '', cacheRead: '', cacheWrite: '', cacheWrite1h: '', reasoning: '' } })
    expect(validatePrice(valid({ tiers: [tier('200000'), tier('100000')] })).tier1contextOver).toMatch(/go up/)
    expect(validatePrice(valid({ tiers: [tier('0')] })).tier0contextOver).toMatch(/positive/)
    expect(validatePrice(valid({ tiers: [tier('200000', '')] })).tier0contextOver).toMatch(/at least one price/)
    expect(validatePrice(valid({ tiers: [tier('200000', 'x')] })).tier0input).toMatch(/decimal/)
    expect(validatePrice(valid({ tiers: [tier('100000'), tier('200000', '7')] }))).toEqual({})
  })

  it('bounds the ratio, overhead and tokenizer', () => {
    expect(validatePrice(valid({ ratio: '0.4' })).ratio).toBeDefined()
    expect(validatePrice(valid({ ratio: '17' })).ratio).toBeDefined()
    expect(validatePrice(valid({ ratio: '2.3' })).ratio).toBeUndefined()
    expect(validatePrice(valid({ toolOverhead: '100001' })).toolOverhead).toBeDefined()
    expect(validatePrice(valid({ tokenizer: 'gpt' })).tokenizer).toBeDefined()
  })
})

describe('toSetPrice', () => {
  it('builds the request, with a UTC start date', () => {
    const req = toSetPrice('org-1', valid({ validFrom: '2026-11-01', tiers: [{ contextOver: '200000', rates: { input: '6', output: '', cacheRead: '', cacheWrite: '', cacheWrite1h: '', reasoning: '' } }] }))
    expect(req).toMatchObject({
      orgId: 'org-1',
      name: 'sonnet-mine',
      provider: 'openrouter',
      match: { glob: 'anthropic/claude-sonnet-*' },
      rates: { input: '3', output: '15' },
      tiers: [{ contextOver: 200000n, rates: { input: '6' } }],
      maxOutput: 64000,
      validFrom: { seconds: BigInt(Date.UTC(2026, 10, 1) / 1000) },
    })
    expect(toSetPrice('o', valid({ matchKind: 'regex', pattern: 'a.*' })).match).toEqual({ regex: 'a.*' })
    expect(toSetPrice('o', valid()).validFrom).toBeUndefined()
  })

  it('round-trips a stored version into the form', () => {
    const v = create(PriceVersionSchema, {
      name: 'x', provider: '*', match: { regex: 'a.*' }, rates: { input: '1', output: '2' }, tokenizer: 'bytes', ratio: '2.3', maxOutput: 100, toolOverheadTokens: 346, hiddenThinking: true,
      tiers: [{ contextOver: 5n, rates: { input: '3' } }],
    })
    const f = formFromVersion(v)
    expect(f).toMatchObject({ matchKind: 'regex', pattern: 'a.*', ratio: '2.3', maxOutput: '100', toolOverhead: '346', hiddenThinking: true })
    expect(toSetPrice('o', f)).toMatchObject({ toolOverheadTokens: 346, hiddenThinking: true })
    expect(f.tiers[0]).toMatchObject({ contextOver: '5' })
    expect(validatePrice(f, true)).toEqual({})
  })
})

describe('groupPrices', () => {
  const now = new Date('2026-10-24T12:00:00Z')
  const v = (name: string, from: string, retired?: string) =>
    create(PriceVersionSchema, { name, validFrom: timestampFromMs(Date.parse(from)), retiredAt: retired ? timestampFromMs(Date.parse(retired)) : undefined })

  it('picks the newest version that has started and is not retired', () => {
    const g = groupPrices([v('a', '2026-10-01T00:00:00Z'), v('a', '2026-10-20T00:00:00Z'), v('a', '2026-12-01T00:00:00Z'), v('b', '2026-10-01T00:00:00Z', '2026-10-10T00:00:00Z')], now)
    expect(g.map((x) => x.name)).toEqual(['a', 'b'])
    expect(g[0].current?.validFrom?.seconds).toBe(BigInt(Date.parse('2026-10-20T00:00:00Z') / 1000))
    expect(g[0].versions).toHaveLength(3)
    expect(g[1].current).toBeUndefined()
  })
})

describe('default list markers', () => {
  it('marks added and changed entries, not removed ones', () => {
    const m = changeById([
      create(PriceChangeSchema, { kind: 'changed', id: 'a', fields: ['rates.input'] }),
      create(PriceChangeSchema, { kind: 'added', id: 'b' }),
      create(PriceChangeSchema, { kind: 'removed', id: 'c' }),
    ])
    expect([...m.keys()]).toEqual(['a', 'b'])
    expect(changeLabel(m.get('a')!)).toBe('Changed since your last upgrade: rates.input')
    expect(changeLabel(m.get('b')!)).toBe('New since your last upgrade')
  })

  it('links a source only when it is https', () => {
    expect(safeSource(create(PriceEntrySchema, { sourceUrl: 'https://example.com/pricing' }))).toBe('https://example.com/pricing')
    expect(safeSource(create(PriceEntrySchema, { sourceUrl: 'javascript:alert(1)' }))).toBeUndefined()
    expect(safeSource(create(PriceEntrySchema, { sourceUrl: 'http://example.com' }))).toBeUndefined()
    expect(safeSource(create(PriceEntrySchema, { sourceUrl: '' }))).toBeUndefined()
  })

  it('writes a price line', () => {
    expect(ratesLine({ input: '3', output: '15', cacheRead: '0.3', cacheWrite: '', cacheWrite1h: '', reasoning: '' })).toBe('$3 in, $15 out, $0.3 cache read per million tokens')
    expect(ratesLine({ input: '3', output: '15', cacheRead: '0.3', cacheWrite: '3.75', cacheWrite1h: '6', reasoning: '' })).toBe(
      '$3 in, $15 out, $0.3 cache read, $3.75 cache write, $6 1-hour cache write per million tokens',
    )
  })
})

describe('flagged entries', () => {
  const f = (model: string, p: Partial<{ gatewayDiff: bigint; usageMissing: bigint; maxOutputCapped: bigint; priceError: bigint; priceRef: string }>) =>
    create(FlaggedPriceSchema, { model, requests: 1n, ...p })
  const list = [f('openai/gpt', { gatewayDiff: 2n, priceRef: 'default@2026-10-23.1#gpt' }), f('anthropic/sonnet', { usageMissing: 1n, maxOutputCapped: 1n }), f('local/q', { priceError: 3n })]

  it('names the reasons', () => {
    expect(reasonsOf(list[1])).toEqual(['usage_missing', 'max_output_capped'])
  })

  it('filters by reason and by text', () => {
    expect(filterFlagged(list, { reason: '', model: '' })).toHaveLength(3)
    expect(filterFlagged(list, { reason: 'usage_missing', model: '' }).map((x) => x.model)).toEqual(['anthropic/sonnet'])
    expect(filterFlagged(list, { reason: '', model: 'GPT' }).map((x) => x.model)).toEqual(['openai/gpt'])
    expect(filterFlagged(list, { reason: '', model: 'default@' }).map((x) => x.model)).toEqual(['openai/gpt'])
    expect(filterFlagged(list, { reason: 'price_error', model: 'sonnet' })).toEqual([])
  })
})
