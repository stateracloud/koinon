// SPDX-License-Identifier: Apache-2.0

import { create, type MessageInitShape } from '@bufbuild/protobuf'
import { timestampFromMs } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { OrgRole } from '@/gen/koinon/auth/v1alpha1/auth_pb'
import { TokenSchema } from '@/gen/koinon/token/v1alpha1/token_pb'
import { createdByLabel, formatLabel, providerTypeLabel, savedRecently, workspaceIdPattern, daysToSeconds, scopedModelName, tokenDaysError, tokenStatus } from '@/lib/providers'

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
  it('names the formats, and leaves an unknown one as it is', () => {
    expect(formatLabel('openai')).toBe('OpenAI chat completions')
    expect(formatLabel('anthropic')).toBe('Anthropic Messages')
    expect(formatLabel('gemini')).toBe('gemini')
  })
  it('lists Anthropic as a provider type', () => {
    expect(providerTypeLabel('anthropic')).toBe('Anthropic')
  })
  it('checks a workspace ID like the server', () => {
    expect(workspaceIdPattern.test('wrkspc_01AbC')).toBe(true)
    for (const bad of ['wrkspc_', 'ws_01', 'wrkspc_a-b', ' wrkspc_01']) expect(workspaceIdPattern.test(bad)).toBe(false)
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

describe('savedRecently', () => {
  const now = Date.parse('2026-10-24T12:00:00Z')
  it('holds for three minutes after a save', () => {
    expect(savedRecently(timestampFromMs(now - 60_000), now)).toBe(true)
    expect(savedRecently(timestampFromMs(now - 181_000), now)).toBe(false)
    expect(savedRecently(undefined, now)).toBe(false)
  })
})
