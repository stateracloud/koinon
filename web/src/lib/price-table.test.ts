// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { ModelPriceSourceSchema, PlatformPriceEntrySchema } from '@/gen/koinon/budget/v1alpha1/price_pb'
import { validatePrice } from '@/lib/prices'
import { matchesPriceSearch, overrideForm, overrideMatch, overrideName, uniqueOverrideName, rateSortValue, rateText, rowFromModel, rowFromPlatformEntry, unpricedText } from '@/lib/price-table'

describe('price table rows', () => {
  it('takes the source and the replaced rates of an org model', () => {
    const r = rowFromModel(
      create(ModelPriceSourceSchema, {
        providerName: 'router', providerType: 'openrouter', model: 'a/b', source: 'org', priceRef: 'org#1',
        entry: { rates: { input: '1', output: '2' } }, replaces: { source: 'default', entry: { rates: { input: '3', output: '4' } } },
      }),
    )
    expect(r).toMatchObject({ id: 'router/a/b', source: 'org', replacedSource: 'default', rates: { input: '1' }, replaced: { input: '3' } })
  })

  it('keeps an unknown source empty and says why a model has no price', () => {
    const r = rowFromModel(create(ModelPriceSourceSchema, { providerName: 'p', model: 'm', source: 'surprise', unpricedReason: 'unusable_price' }))
    expect(r.source).toBe('')
    expect(unpricedText(r.unpricedReason)).toBe('Its price cannot be used')
    expect(unpricedText(undefined)).toBe('No price')
  })

  it('shows a platform entry by its pattern and "any provider" for *', () => {
    const r = rowFromPlatformEntry(create(PlatformPriceEntrySchema, { priceRef: 'platform@1#x', entry: { id: 'x', provider: '*', match: { regex: 'x.*' } } }))
    expect(r).toMatchObject({ id: 'platform@1#x', model: 'regex x.*', provider: 'any provider', source: 'platform' })
  })
})

describe('matchesPriceSearch', () => {
  const r = rowFromModel(create(ModelPriceSourceSchema, { providerName: 'Team Router', providerType: 'openrouter', model: 'anthropic/claude-sonnet-4', source: 'default' }))
  it('finds a row by model, provider name or provider type, without case', () => {
    expect(matchesPriceSearch(r, '')).toBe(true)
    expect(matchesPriceSearch(r, ' Sonnet ')).toBe(true)
    expect(matchesPriceSearch(r, 'team router')).toBe(true)
    expect(matchesPriceSearch(r, 'OPENROUTER')).toBe(true)
    expect(matchesPriceSearch(r, 'gpt')).toBe(false)
  })
})

describe('rates', () => {
  it('shows a dash for an unset rate and sorts it as undefined', () => {
    expect(rateText('0.3')).toBe('$0.3')
    expect(rateText('')).toBe('—')
    expect(rateSortValue('0.3')).toBe(0.3)
    expect(rateSortValue('')).toBeUndefined()
    expect(rateSortValue(undefined)).toBeUndefined()
  })
})

describe('override', () => {
  it('derives a valid price name from a model', () => {
    expect(overrideName('Anthropic/Claude Sonnet:4')).toBe('anthropic-claude-sonnet-4')
    expect(overrideName('/x')).toBe('x')
    expect(overrideName('###')).toBe('override')
    expect(overrideName('a'.repeat(300))).toHaveLength(128)
  })

  it('matches exactly one model: a glob, or a quoted regex when the name has a wildcard', () => {
    expect(overrideMatch('openai/gpt-6')).toEqual({ matchKind: 'glob', pattern: 'openai/gpt-6' })
    const m = overrideMatch('odd*model?.v1')
    expect(m.matchKind).toBe('regex')
    const re = new RegExp(`^(?:${m.pattern})$`)
    expect(re.test('odd*model?.v1')).toBe(true)
    expect(re.test('oddXmodel.v1')).toBe(false)
  })

  it('fills a form that passes the checks from the price in use', () => {
    const r = rowFromModel(
      create(ModelPriceSourceSchema, {
        providerName: 'router', providerType: 'openrouter', model: 'openai/gpt-6', source: 'platform',
        entry: { rates: { input: '4', output: '20', cacheRead: '0.4' }, maxOutput: 32000, tokenizer: 'tiktoken:o200k_base', ratio: '4.0', toolOverheadTokens: 12, hiddenThinking: true },
      }),
    )
    const f = overrideForm(r)
    expect(f).toMatchObject({
      name: 'openrouter-openai-gpt-6', provider: 'openrouter', matchKind: 'glob', pattern: 'openai/gpt-6', rates: { input: '4', output: '20', cacheRead: '0.4', cacheWrite: '' },
      maxOutput: '32000', tokenizer: 'tiktoken:o200k_base', ratio: '4.0', toolOverhead: '12', hiddenThinking: true,
    })
    expect(validatePrice(f)).toEqual({})
  })

  it('names an override by provider type and model, and never reuses a name the org has', () => {
    const row = (providerType: string) => rowFromModel(create(ModelPriceSourceSchema, { providerName: 'p', providerType, model: 'gpt-6', source: 'default', entry: { rates: { input: '1', output: '2' } } }))
    expect(overrideForm(row('openai')).name).toBe('openai-gpt-6')
    expect(overrideForm(row('openrouter')).name).toBe('openrouter-gpt-6')
    expect(overrideForm(row('openai'), ['openai-gpt-6']).name).toBe('openai-gpt-6-2')
    expect(overrideForm(row('openai'), ['openai-gpt-6', 'openai-gpt-6-2']).name).toBe('openai-gpt-6-3')
    expect(uniqueOverrideName('x', 'm'.repeat(300), []).length).toBeLessThanOrEqual(128)
    expect(uniqueOverrideName('x', 'm'.repeat(300), [uniqueOverrideName('x', 'm'.repeat(300), [])]).length).toBeLessThanOrEqual(128)
  })

  it('starts from empty rates for a model with no price', () => {
    const f = overrideForm(rowFromModel(create(ModelPriceSourceSchema, { providerName: 'home', providerType: 'ollama', model: 'local/q', unpricedReason: 'no_price' })))
    expect(f).toMatchObject({ provider: 'ollama', rates: { input: '', output: '' }, tokenizer: 'bytes', ratio: '3.0', maxOutput: '' })
  })
})
