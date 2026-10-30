// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { Backend, ContentLevel, DestinationSchema } from '@/gen/koinon/trace/v1alpha1/trace_pb'
import { buildInput, clampLevel, destinationNameError, formErrors, hasErrors, initialForm, levelDisabledReason, percentToRate, rateToPercent } from '@/lib/trace'

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
    const f = { ...initialForm(), name: 'lf', endpoint: 'https://lf.example.com/x', backend: Backend.OPIK }
    const e = formErrors(f, undefined, false)
    expect(Object.keys(e.fields).sort()).toEqual(['api_key', 'workspace'])
  })
  it('does not ask again for a stored secret', () => {
    const d = create(DestinationSchema, { id: 'd', name: 'lf', backend: Backend.LANGFUSE, endpointUrl: 'https://lf.example.com/x', secretFields: ['public_key', 'secret_key'] })
    expect(hasErrors(formErrors(initialForm(d), d, false))).toBe(false)
  })
  it('asks again for the secrets when the backend differs from the stored one', () => {
    const d = create(DestinationSchema, { id: 'd', name: 'lf', backend: Backend.LANGFUSE, endpointUrl: 'https://lf.example.com/x', secretFields: ['api_key'] })
    expect(formErrors({ ...initialForm(d), backend: Backend.DATADOG }, d, false).fields.api_key).toBeDefined()
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
    for (const bad of ['', 'x', '-1', '101']) expect(percentToRate(bad)).toBeUndefined()
  })
})
