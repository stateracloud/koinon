// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromDate } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { Health, ServerSchema, Tier, ToolSchema } from '@/gen/koinon/mcpserver/v1alpha1/mcpserver_pb'
import {
  allowStateFrom,
  allowlistProblem,
  buildAllowlist,
  buildCreateRequest,
  buildUpdateRequest,
  formErrors,
  healthLabel,
  initialForm,
  serverErrorField,
  serverNameError,
  summarizeTools,
  summaryText,
  tierLabel,
  toggleLeg,
  buildSink,
  isDefaultSink,
  parseArgs,
  sinkFormOf,
  sinkText,
  validateSink,
  legsText,
  type ServerFormState,
} from '@/lib/mcp-servers'

const base: ServerFormState = { name: 'docs', url: ' https://mcp.example.com/mcp ', transport: 'streamable-http', mode: 'none', header: '', value: '', removeCredential: false, kind: 'other', legs: ['untrusted_input', 'private_data'] }
const not = 'not-a-real-token'

describe('serverNameError', () => {
  it('accepts DNS labels of at most 24 characters', () => {
    for (const ok of ['docs', 'a', 'my-server-1', 'a'.repeat(24)]) expect(serverNameError(ok)).toBeUndefined()
  })
  it('refuses everything else', () => {
    for (const bad of ['', 'Docs', '-docs', 'docs-', 'do_cs', 'a'.repeat(25), 'two words']) expect(serverNameError(bad)).toBeDefined()
  })
})

describe('buildCreateRequest', () => {
  it('sends no credential fields without a credential', () => {
    expect(buildCreateRequest(base)).toEqual({ name: 'docs', url: 'https://mcp.example.com/mcp', transport: 'streamable-http', credentialHeader: '', credentialPrefix: '', credentialValue: '', class: { kind: 'other', dataClass: ['untrusted_input', 'private_data'] } })
  })
  it('builds a bearer token as Authorization with the Bearer prefix', () => {
    expect(buildCreateRequest({ ...base, mode: 'bearer', value: not })).toMatchObject({ credentialHeader: 'Authorization', credentialPrefix: 'Bearer ', credentialValue: not })
  })
  it('builds a custom header with an empty prefix', () => {
    expect(buildCreateRequest({ ...base, mode: 'custom', header: ' X-Api-Key ', value: not, transport: 'sse' })).toMatchObject({
      transport: 'sse',
      credentialHeader: 'X-Api-Key',
      credentialPrefix: '',
      credentialValue: not,
    })
  })
})

describe('buildUpdateRequest', () => {
  const stored = create(ServerSchema, { id: 's-1', name: 'docs', url: 'https://mcp.example.com/mcp', transport: 'streamable-http', hasCredential: true, credentialHeader: 'Authorization', credentialPrefix: 'Bearer ' })
  it('derives the mode of a stored credential and never prefills the secret', () => {
    expect(initialForm(stored)).toMatchObject({ mode: 'bearer', value: '' })
    expect(initialForm(create(ServerSchema, { hasCredential: true, credentialHeader: 'X-Api-Key' }))).toMatchObject({ mode: 'custom', header: 'X-Api-Key', value: '' })
  })
  it('sends nothing when nothing changed, which keeps the stored secret', () => {
    expect(buildUpdateRequest(stored, initialForm(stored))).toEqual({})
  })
  it('sends only the changed URL', () => {
    expect(buildUpdateRequest(stored, { ...initialForm(stored), url: 'https://other.example.com/mcp' })).toEqual({ url: 'https://other.example.com/mcp' })
  })
  it('replaces the credential when a value is typed', () => {
    expect(buildUpdateRequest(stored, { ...initialForm(stored), mode: 'custom', header: 'X-Key', value: not })).toEqual({ credentialHeader: 'X-Key', credentialPrefix: '', credentialValue: not })
  })
  it('keeps the mode and header of a lost credential and asks for its value', () => {
    const lost = create(ServerSchema, { id: 's-2', name: 'docs', url: 'https://mcp.example.com/mcp', transport: 'streamable-http', credentialHeader: 'X-Api-Key', credentialLostAt: timestampFromDate(new Date('2026-11-01T00:00:00Z')) })
    expect(initialForm(lost)).toMatchObject({ mode: 'custom', header: 'X-Api-Key', value: '' })
    expect(formErrors(initialForm(lost), lost).value).toBeDefined()
    expect(buildUpdateRequest(lost, { ...initialForm(lost), value: not })).toEqual({ credentialHeader: 'X-Api-Key', credentialPrefix: '', credentialValue: not })
    expect(buildUpdateRequest(lost, { ...initialForm(lost), removeCredential: true })).toEqual({ removeCredential: true })
  })
  it('removes the credential on request or when set to none', () => {
    expect(buildUpdateRequest(stored, { ...initialForm(stored), removeCredential: true })).toEqual({ removeCredential: true })
    expect(buildUpdateRequest(stored, { ...initialForm(stored), mode: 'none' })).toEqual({ removeCredential: true })
  })
})

describe('labels', () => {
  it('names tiers and health states', () => {
    expect([Tier.ASK, Tier.AUTO, Tier.NEVER].map(tierLabel)).toEqual(['Ask', 'Allow', 'Deny'])
    expect([Health.HEALTHY, Health.UNHEALTHY, Health.UNKNOWN, Health.UNSPECIFIED].map(healthLabel)).toEqual(['Healthy', 'Unhealthy', 'Unknown', 'Unknown'])
  })
  it('places a server refusal at its field', () => {
    expect(serverErrorField('this server asks each user to sign in with OAuth, which Koinon does not support yet')).toBe('url')
    expect(serverErrorField('the url\'s host is a private address')).toBe('url')
    expect(serverErrorField('a server named docs already exists; the name must be unique')).toBe('name')
    expect(serverErrorField('something else')).toBeUndefined()
  })
})

describe('allowlist', () => {
  const servers = [create(ServerSchema, { id: 's-1', name: 'docs' }), create(ServerSchema, { id: 's-2', name: 'issues' }), create(ServerSchema, { id: 's-3', name: 'wiki' })]
  it('lists allowed servers only; unlimited means the whole server', () => {
    const state = { 's-1': { allowed: true, limited: false, tools: ['x'] }, 's-2': { allowed: true, limited: true, tools: ['b', 'a'] }, 's-3': { allowed: false, limited: false, tools: [] } }
    expect(buildAllowlist(servers, state)).toEqual([
      { serverId: 's-1', tools: [] },
      { serverId: 's-2', tools: ['a', 'b'] },
    ])
  })
  it('refuses a limit with no tools picked, which would allow the whole server', () => {
    expect(allowlistProblem(servers, { 's-2': { allowed: true, limited: true, tools: [] } })).toMatch(/issues/)
    expect(allowlistProblem(servers, { 's-2': { allowed: false, limited: true, tools: [] } })).toBeUndefined()
  })
  it('reads the saved allowances back into checkbox state', () => {
    expect(allowStateFrom([{ serverId: 's-1', tools: [] }, { serverId: 's-2', tools: ['a'] }] as never)).toEqual({
      's-1': { allowed: true, limited: false, tools: [] },
      's-2': { allowed: true, limited: true, tools: ['a'] },
    })
  })
})

describe('summarizeTools', () => {
  it('counts reviews and read-only suggestions of present tools', () => {
    const tools = [
      create(ToolSchema, { name: 'a', present: true, tier: Tier.ASK, readOnlyHint: true }),
      create(ToolSchema, { name: 'b', present: true, tier: Tier.AUTO, readOnlyHint: true }),
      create(ToolSchema, { name: 'c', present: true, tier: Tier.ASK, reviewReason: 'description changed' }),
      create(ToolSchema, { name: 'd', present: false, tier: Tier.ASK, readOnlyHint: true }),
    ]
    const s = summarizeTools(tools)
    expect(s).toEqual({ total: 3, review: 1, suggested: 1, gone: 1 })
    expect(summaryText(s)).toBe('3 tools, 1 to review, 1 suggested read-only')
  })
})

describe('the data class helpers', () => {
  it('keeps the canonical leg order when a leg is toggled', () => {
    expect(toggleLeg([], 'untrusted_input', true)).toEqual(['untrusted_input'])
    expect(toggleLeg(['untrusted_input', 'private_data'], 'private_data', false)).toEqual(['untrusted_input'])
    expect(toggleLeg([], 'private_data', false)).toEqual([])
  })
  it('says none for a server with no leg', () => {
    expect(legsText([])).toBe('none')
  })
  it('sends the class chosen in the form with the create request', () => {
    expect(buildCreateRequest({ ...base, kind: 'fetch', legs: ['private_data'] }).class).toEqual({ kind: 'fetch', dataClass: ['private_data'] })
  })
})

describe('the sink class of a tool (ADR 0091)', () => {
  const declared = { sinkClass: 'internal', derivation: 'fixed', addressArgs: [] as string[] }
  it('treats a tool nobody declared as external-high', () => {
    expect(isDefaultSink(undefined)).toBe(true)
    expect(isDefaultSink({ sinkClass: 'external-high', derivation: 'fixed', addressArgs: [] })).toBe(true)
    expect(isDefaultSink(declared)).toBe(false)
    expect(isDefaultSink({ sinkClass: 'external-high', derivation: 'fixed', addressArgs: [], resultPrivateData: false })).toBe(false)
    expect(sinkText(undefined)).toMatch(/not declared/)
  })
  it('says how a derived class is decided', () => {
    expect(sinkText({ sinkClass: 'external-high', derivation: 'code_host', addressArgs: ['owner', 'repo'] })).toBe('from the repository; address: owner, repo')
    expect(sinkText({ sinkClass: 'external-high', derivation: 'fetch', addressArgs: ['url'] })).toBe('from the URL; address: url')
  })
  it('round-trips the form and keeps the three answers for a result leg', () => {
    const f = sinkFormOf({ ...declared, resultUntrustedInput: false, resultPrivateData: true })
    expect([f.resultUntrustedInput, f.resultPrivateData]).toEqual(['no', 'yes'])
    expect(sinkFormOf(undefined)).toMatchObject({ sinkClass: 'external-high', derivation: 'fixed', resultUntrustedInput: 'server' })
    expect(buildSink(f)).toEqual({ sinkClass: 'internal', derivation: 'fixed', addressArgs: [], resultUntrustedInput: false, resultPrivateData: true })
  })
  it('sends external-high as the fallback class of a derived tool', () => {
    expect(buildSink({ ...sinkFormOf(declared), derivation: 'fetch', addressArgs: ' url ' })).toMatchObject({ sinkClass: 'external-high', derivation: 'fetch', addressArgs: ['url'] })
  })
  it('checks the address arguments as the API does', () => {
    const f = sinkFormOf(declared)
    expect(parseArgs(' a, ,b ,')).toEqual(['a', 'b'])
    expect(validateSink(f)).toBeUndefined()
    expect(validateSink({ ...f, derivation: 'code_host', addressArgs: 'owner' })).toMatch(/two arguments/)
    expect(validateSink({ ...f, derivation: 'fetch', addressArgs: '' })).toMatch(/one argument/)
    expect(validateSink({ ...f, addressArgs: 'bad name' })).toMatch(/not an argument name/)
    expect(validateSink({ ...f, addressArgs: 'a, a' })).toMatch(/twice/)
    expect(validateSink({ ...f, addressArgs: 'a,b,c,d,e,f,g,h,i' })).toMatch(/at most 8/)
    expect(validateSink({ ...f, derivation: 'nope' })).toMatch(/Choose/)
  })
})
