// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampNow } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { Health, ServerSchema } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import { OrgRefSchema } from '@/gen/koinon/platform/v1alpha1/resources_pb'
import { ProviderSchema, ServingState } from '@/gen/koinon/provider/v1alpha1/provider_pb'
import { matchesResourceSearch, platformOwnerName, providerStatusText, serverStatusText } from '@/lib/platform-resources'

describe('platformOwnerName', () => {
  it('is "Platform" for a row that no org owns', () => {
    expect(platformOwnerName(undefined)).toBe('Platform')
  })
  it('is the display name, else the slug', () => {
    expect(platformOwnerName(create(OrgRefSchema, { slug: 'acme', displayName: 'Acme' }))).toBe('Acme')
    expect(platformOwnerName(create(OrgRefSchema, { slug: 'acme' }))).toBe('acme')
  })
})

describe('providerStatusText', () => {
  it('puts removed before disabled, and a lost key before the serving state', () => {
    expect(providerStatusText(create(ProviderSchema, { removed: true, disabledReason: 'x' }))).toBe('Removed')
    expect(providerStatusText(create(ProviderSchema, { disabledReason: 'x', credentialLostAt: timestampNow() }))).toBe('Disabled')
    expect(providerStatusText(create(ProviderSchema, { credentialLostAt: timestampNow(), servingState: ServingState.SERVING }))).toBe('Key lost')
    expect(providerStatusText(create(ProviderSchema, { servingState: ServingState.SERVING }))).toBe('Serving')
    expect(providerStatusText(create(ProviderSchema, {}))).toBe('Active')
  })
})

describe('serverStatusText', () => {
  it('gives the health when nothing holds the server off', () => {
    expect(serverStatusText(create(ServerSchema, { removed: true }))).toBe('Removed')
    expect(serverStatusText(create(ServerSchema, { disabledReason: 'x' }))).toBe('Disabled')
    expect(serverStatusText(create(ServerSchema, { credentialLostAt: timestampNow() }))).toBe('Credential lost')
    expect(serverStatusText(create(ServerSchema, { health: Health.HEALTHY }))).toBe('Healthy')
  })
})

describe('matchesResourceSearch', () => {
  it('matches a part of any text in any letter case, and an empty search matches all', () => {
    expect(matchesResourceSearch('', 'a')).toBe(true)
    expect(matchesResourceSearch('  ', 'a')).toBe(true)
    expect(matchesResourceSearch('ACM', 'openrouter', 'Acme')).toBe(true)
    expect(matchesResourceSearch('beta', 'openrouter', 'Acme')).toBe(false)
  })
})
