// SPDX-License-Identifier: Apache-2.0

import { create, type MessageInitShape } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { ProviderSchema, ServingState } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { TokenSchema } from '@/gen/koinon/token/v1alpha1/token_pb'
import { createdByLabel, formatLabelOf, failedPollMs, providerTypeLabel, servingBadge, servingPollInterval, servingPollMs, daysToSeconds, scopedModelName, tokenDaysError, tokenStatus } from '@/lib/providers'

describe('forms', () => {
  it('names a model the way the gateway serves it', () => {
    expect(scopedModelName('acme', 'deepseek/v4')).toBe('acme/deepseek/v4')
  })
  it('limits the token expiry to 1 to 90 whole days', () => {
    expect(tokenDaysError('30')).toBeUndefined()
    expect(tokenDaysError('90')).toBeUndefined()
    for (const bad of ['0', '91', '1.5', '', 'x']) expect(tokenDaysError(bad)).toBeDefined()
    expect(daysToSeconds(30)).toBe(2592000n)
  })
})

describe('model formats and Anthropic providers', () => {
  const provider = create(ProviderSchema, { formats: [{ id: 'openai', label: 'OpenAI chat completions' }] })
  it('names a format with the label the server gave, and an unlisted one as its id', () => {
    expect(formatLabelOf(provider, 'openai')).toBe('OpenAI chat completions')
    expect(formatLabelOf(provider, 'gemini')).toBe('gemini')
  })
  it('lists Anthropic as a provider type', () => {
    expect(providerTypeLabel('anthropic')).toBe('Anthropic')
  })
})

describe('tokens', () => {
  const now = new Date('2026-10-24T12:00:00Z')
  const tok = (o: MessageInitShape<typeof TokenSchema>) =>
    create(TokenSchema, { id: 't', name: 'ci', prefix: 'abc', role: OrgRole.ADMIN, expiresAt: timestampFromMs(now.getTime() + 1000), ...o })
  it('tells active, expired and revoked apart', () => {
    expect(tokenStatus(tok({}), now)).toBe('active')
    expect(tokenStatus(tok({ expiresAt: timestampFromMs(now.getTime() - 1) }), now)).toBe('expired')
    expect(tokenStatus(tok({ revokedAt: timestampFromMs(now.getTime()) }), now)).toBe('revoked')
  })
  it('names the creator and the parent token', () => {
    const parent = tok({ id: 'p', name: 'root', prefix: 'rrr', createdByName: 'Maria' })
    const child = tok({ id: 'c', parentTokenId: 'p', createdByName: 'Maria' })
    expect(createdByLabel(parent, [parent, child])).toBe('Maria, in the UI')
    expect(createdByLabel(child, [parent, child])).toBe('Maria, through token root (rrr)')
    expect(createdByLabel(child, [child])).toContain('no longer exists')
  })
})

describe('servingBadge', () => {
  it('words each serving state and the reason of a failure', () => {
    expect(servingBadge({ servingState: ServingState.SERVING, servingReason: '' })?.label).toBe('Serving')
    // The browser e2e (web/e2e-real/task-stream.spec.ts) selects on these data-serving values.
    expect(servingBadge({ servingState: ServingState.SERVING, servingReason: '' })?.state).toBe('serving')
    expect(servingBadge({ servingState: ServingState.SAVED, servingReason: '' })?.state).toBe('saved')
    expect(servingBadge({ servingState: ServingState.SAVED, servingReason: '' })?.variant).toBe('outline')
    expect(servingBadge({ servingState: ServingState.FAILED, servingReason: 'gateway rejected config' })).toEqual({ state: 'failed', label: 'Not served: gateway rejected config', variant: 'destructive' })
    expect(servingBadge({ servingState: ServingState.UNSPECIFIED, servingReason: '' })).toBeUndefined()
  })
  it('polls fast while saved, slower while failed, and stops when settled', () => {
    expect(servingPollInterval([{ servingState: ServingState.SERVING }, { servingState: ServingState.SAVED }])).toBe(servingPollMs)
    expect(servingPollInterval([{ servingState: ServingState.FAILED }, { servingState: ServingState.SAVED }])).toBe(servingPollMs)
    expect(servingPollInterval([{ servingState: ServingState.SERVING }, { servingState: ServingState.FAILED }])).toBe(failedPollMs)
    expect(servingPollInterval([{ servingState: ServingState.SERVING }, { servingState: ServingState.UNSPECIFIED }])).toBe(false)
  })
})
