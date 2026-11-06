// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { timestampFromDate } from '@bufbuild/protobuf/wkt'
import { describe, expect, it } from 'vitest'
import { Backend, ContentLevel, DestinationSchema } from '@/gen/koinon/trace/v1alpha1/trace_pb'
import { normalEndpoint, sameOrigin, buildInput, connectionChanged, clampLevel, destinationNameError, formErrors, hasErrors, initialForm, levelDisabledReason, percentToRate, rateToPercent } from '@/lib/trace'

const not = 'koinon_test_not-a-real-token'

describe('destinationNameError', () => {
  it('accepts DNS labels', () => {
    for (const ok of ['langfuse', 'a', 'team-a-1']) expect(destinationNameError(ok)).toBeUndefined()
  })
  it('refuses everything else', () => {
    for (const bad of ['', 'Lang', '-a', 'a-', 'a_b', 'a'.repeat(65)]) expect(destinationNameError(bad)).toBeDefined()
  })
})

describe('formErrors', () => {
  it('asks for the preset fields on a new destination', () => {
    const f = { ...initialForm(), name: 'lf', endpoint: 'https://lf.example.com/x', backend: Backend.LANGFUSE }
    const e = formErrors(f, undefined, false)
    expect(Object.keys(e.fields).sort()).toEqual(['public_key', 'secret_key'])
  })
  it('asks for nothing more for Opik: key, workspace and project are optional', () => {
    const f = { ...initialForm(), name: 'o', endpoint: 'http://opik.local/x', backend: Backend.OPIK }
    expect(hasErrors(formErrors(f, undefined, false))).toBe(false)
  })
  it('asks again for every secret when the endpoint origin changes, but not for a new path', () => {
    const d = create(DestinationSchema, { id: 'd', name: 'lf', backend: Backend.LANGFUSE, endpointUrl: 'https://lf.example.com/a', secretFields: ['public_key', 'secret_key'] })
    const samePath = { ...initialForm(d), endpoint: 'https://lf.example.com/b' }
    expect(hasErrors(formErrors(samePath, d, false))).toBe(false)
    for (const endpoint of ['https://other.example.com/a', 'https://lf.example.com:8443/a', 'http://lf.example.com/a']) {
      expect(Object.keys(formErrors({ ...samePath, endpoint }, d, false).fields).sort()).toEqual(['public_key', 'secret_key'])
    }
  })
  it('asks again for stored generic header values when the origin changes', () => {
    const d = create(DestinationSchema, { id: 'd', name: 'g', backend: Backend.GENERIC, endpointUrl: 'https://c.example.com/v1', secretFields: ['X-A'] })
    const f = { ...initialForm(d), endpoint: 'https://elsewhere.example.com/v1' }
    expect(formErrors(f, d, false).headers[0]).toBeDefined()
    f.headers[0].remove = true
    f.headers.push({ name: 'X-B', value: 'v', stored: false, remove: false })
    expect(hasErrors(formErrors(f, d, false))).toBe(false)
  })
  it('does not ask again for a stored secret', () => {
    const d = create(DestinationSchema, { id: 'd', name: 'lf', backend: Backend.LANGFUSE, endpointUrl: 'https://lf.example.com/x', secretFields: ['public_key', 'secret_key'] })
    expect(hasErrors(formErrors(initialForm(d), d, false))).toBe(false)
  })
  it('asks again for the secrets when the backend differs from the stored one', () => {
    const d = create(DestinationSchema, { id: 'd', name: 'lf', backend: Backend.LANGFUSE, endpointUrl: 'https://lf.example.com/x', secretFields: ['api_key'] })
    expect(formErrors({ ...initialForm(d), backend: Backend.DATADOG }, d, false).fields.api_key).toBeDefined()
  })
  it('asks again for every secret when the stored ones were lost with a key', () => {
    const lostAt = timestampFromDate(new Date('2026-11-01T00:00:00Z'))
    const d = create(DestinationSchema, { id: 'd', name: 'lf', backend: Backend.LANGFUSE, endpointUrl: 'https://lf.example.com/x', secretFields: ['public_key', 'secret_key'], credentialLostAt: lostAt })
    expect(Object.keys(formErrors(initialForm(d), d, false).fields).sort()).toEqual(['public_key', 'secret_key'])
    const f = { ...initialForm(d), secrets: { public_key: not, secret_key: not } }
    expect(hasErrors(formErrors(f, d, false))).toBe(false)
    expect(connectionChanged(f, d)).toBe(true)
    const g = create(DestinationSchema, { id: 'g', name: 'g', backend: Backend.GENERIC, endpointUrl: 'https://c.example.com/v1', secretFields: ['X-A'], credentialLostAt: lostAt })
    expect(formErrors(initialForm(g), g, false).headers[0]).toBeDefined()
  })
  it('needs no name for the platform destination', () => {
    const f = { ...initialForm(), endpoint: 'https://dd.example.com', backend: Backend.DATADOG, secrets: { api_key: not } }
    expect(hasErrors(formErrors(f, undefined, true))).toBe(false)
  })
  it('wants one valid, unique header with a value for a generic destination', () => {
    const f = { ...initialForm(), name: 'g', endpoint: 'https://c.example.com', backend: Backend.GENERIC }
    expect(formErrors(f, undefined, false).headers[-1]).toBeDefined()
    const rows = [
      { name: 'X-Api-Key', value: not, stored: false, remove: false },
      { name: 'x-api-key', value: not, stored: false, remove: false },
      { name: 'bad name', value: not, stored: false, remove: false },
      { name: 'X-Empty', value: '', stored: false, remove: false },
    ]
    const e = formErrors({ ...f, headers: rows }, undefined, false)
    expect(Object.keys(e.headers).sort()).toEqual(['1', '2', '3'])
  })
})

describe('buildInput', () => {
  it('sends only typed secrets and keeps the resource attributes of an update', () => {
    const d = create(DestinationSchema, { id: 'd', name: 'lf', backend: Backend.LANGFUSE, endpointUrl: 'https://lf.example.com/x', secretFields: ['public_key', 'secret_key'], resourceAttributes: { env: 'prod' }, enabled: true })
    const f = { ...initialForm(d), secrets: { secret_key: ` ${not} ` } }
    expect(buildInput(f, d)).toEqual({ name: 'lf', backend: Backend.LANGFUSE, endpointUrl: 'https://lf.example.com/x', params: {}, secrets: { secret_key: not }, removeSecrets: [], resourceAttributes: { env: 'prod' }, enabled: true })
  })
  it('puts non-secret settings in params', () => {
    const f = { ...initialForm(), name: 'o', backend: Backend.OPIK, endpoint: 'https://o.example.com', params: { workspace: ' w ', project: '' }, secrets: { api_key: not } }
    expect(buildInput(f)).toMatchObject({ params: { workspace: 'w' }, secrets: { api_key: not } })
  })
  it('replaces and removes generic headers by name', () => {
    const d = create(DestinationSchema, { id: 'd', name: 'g', backend: Backend.GENERIC, endpointUrl: 'https://c.example.com', secretFields: ['X-A', 'X-B', 'X-C'] })
    const f = initialForm(d)
    f.headers[0].value = not
    f.headers[1].remove = true
    f.headers.push({ name: 'X-D', value: not, stored: false, remove: false })
    const out = buildInput(f, d)
    expect(out.secrets).toEqual({ 'X-A': not, 'X-D': not })
    expect(out.removeSecrets).toEqual(['X-B'])
  })
})

describe('levels', () => {
  it('clamps to the ceiling', () => {
    expect(clampLevel(ContentLevel.FULL, ContentLevel.REDACTED)).toBe(ContentLevel.REDACTED)
    expect(clampLevel(ContentLevel.NONE, ContentLevel.FULL)).toBe(ContentLevel.NONE)
    expect(clampLevel(ContentLevel.UNSPECIFIED, ContentLevel.UNSPECIFIED)).toBe(ContentLevel.NONE)
  })
  it('refuses levels above the ceiling and full for non-owners', () => {
    expect(levelDisabledReason(ContentLevel.FULL, ContentLevel.REDACTED, true, ContentLevel.NONE)).toMatch(/ceiling is redacted/)
    expect(levelDisabledReason(ContentLevel.FULL, ContentLevel.FULL, false, ContentLevel.NONE)).toMatch(/owner/)
    expect(levelDisabledReason(ContentLevel.FULL, ContentLevel.FULL, false, ContentLevel.FULL)).toBeUndefined()
    expect(levelDisabledReason(ContentLevel.FULL, ContentLevel.FULL, true, ContentLevel.NONE)).toBeUndefined()
  })
})

describe('sampling', () => {
  it('converts between percent and a share', () => {
    expect(percentToRate('25')).toBe(0.25)
    expect(percentToRate('100')).toBe(1)
    expect(percentToRate('12.5')).toBe(0.125)
    expect(rateToPercent(0.125)).toBe('12.5')
    expect(rateToPercent(1)).toBe('100')
  })
  it('refuses anything outside 0 to 100', () => {
    for (const bad of ['', 'x', '-1', '101', '1e1', '0x10', ' 5', '5 ', '.5', '+5', '12.345', '1_0']) expect(percentToRate(bad)).toBeUndefined()
  })
})

describe('connectionChanged', () => {
  const d = create(DestinationSchema, { id: 'd', name: 'o', backend: Backend.OPIK, endpointUrl: 'https://o.example.com/x', params: { workspace: 'w' }, secretFields: ['api_key'], enabled: true })
  it('is false for enabled and name only', () => {
    expect(connectionChanged({ ...initialForm(d), enabled: false, name: 'renamed' }, d)).toBe(false)
  })
  it('is true for a new destination, endpoint, backend, params and typed secrets', () => {
    expect(connectionChanged(initialForm(), undefined)).toBe(true)
    expect(connectionChanged({ ...initialForm(d), endpoint: 'https://o.example.com/y' }, d)).toBe(true)
    expect(connectionChanged({ ...initialForm(d), backend: Backend.DATADOG }, d)).toBe(true)
    expect(connectionChanged({ ...initialForm(d), params: { workspace: 'v' } }, d)).toBe(true)
    expect(connectionChanged({ ...initialForm(d), secrets: { api_key: 'x' } }, d)).toBe(true)
  })
})

describe('sameOrigin (mirrors export.SameOrigin)', () => {
  it('compares scheme, host and port, case-insensitively, with default ports', () => {
    expect(sameOrigin('https://Lf.Example.com/a', 'HTTPS://lf.example.com:443/b')).toBe(true)
    expect(sameOrigin('http://lf.example.com/a', 'http://lf.example.com:80/a')).toBe(true)
    expect(sameOrigin('https://lf.example.com/a', 'http://lf.example.com/a')).toBe(false)
    expect(sameOrigin('https://lf.example.com/a', 'https://lf.example.com:8443/a')).toBe(false)
    expect(sameOrigin('https://lf.example.com/a', 'https://lf.example.org/a')).toBe(false)
  })
  it('does not normalise the host the way a browser does', () => {
    expect(sameOrigin('https://127.1/a', 'https://127.0.0.1/a')).toBe(false)
    expect(sameOrigin('https://[::1]:8443/a', 'https://[::1]:8443/b')).toBe(true)
    expect(sameOrigin('not a url', 'not a url')).toBe(false)
  })
})

describe('normalEndpoint (mirrors export.NormalizeEndpoint)', () => {
  it('lower-cases scheme and host only', () => {
    expect(normalEndpoint('HTTPS://Lf.Example.com:8443/Api/V1')).toBe('https://lf.example.com:8443/Api/V1')
  })
  it('makes a case-only change to scheme or host no change of the connection', () => {
    const d = create(DestinationSchema, { id: 'd', name: 'o', backend: Backend.LANGFUSE, endpointUrl: 'https://lf.example.com/x', secretFields: ['public_key', 'secret_key'] })
    expect(connectionChanged({ ...initialForm(d), endpoint: 'HTTPS://LF.example.com/x' }, d)).toBe(false)
    expect(connectionChanged({ ...initialForm(d), endpoint: 'https://lf.example.com/X' }, d)).toBe(true)
  })
})

describe('clearing an optional stored secret', () => {
  it('sends the name in removeSecrets and counts as a connection change', () => {
    const d = create(DestinationSchema, { id: 'd', name: 'o', backend: Backend.OPIK, endpointUrl: 'https://o.example.com/x', secretFields: ['api_key'], enabled: true })
    const f = { ...initialForm(d), removed: ['api_key'] }
    expect(buildInput(f, d).removeSecrets).toEqual(['api_key'])
    expect(connectionChanged(f, d)).toBe(true)
  })
})
