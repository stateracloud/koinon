// SPDX-License-Identifier: Apache-2.0

import { create } from '@bufbuild/protobuf'
import { describe, expect, it } from 'vitest'
import { GetAgentModelResponseSchema, ModelSource } from '@/gen/koinon/agent/v1alpha1/agent_pb'
import { modelChoices, modelSourceLabel } from './agent-model'

describe('modelSourceLabel', () => {
  it('names each layer of the precedence', () => {
    expect(modelSourceLabel(ModelSource.AGENT)).toBe('set on this agent')
    expect(modelSourceLabel(ModelSource.VERSION)).toContain('version')
    expect(modelSourceLabel(ModelSource.ORG_DEFAULT)).toContain('org default')
    expect(modelSourceLabel(ModelSource.NONE)).toBe('none')
  })
})

describe('modelChoices', () => {
  it('offers the served models, and keeps a set model that is no longer served so it can be seen and replaced', () => {
    const served = create(GetAgentModelResponseSchema, { available: [{ name: 'vendor/a', provider: 'openrouter' }] })
    expect(modelChoices(served)).toEqual([{ name: 'vendor/a', label: 'vendor/a (openrouter)' }])
    const gone = create(GetAgentModelResponseSchema, { setting: 'vendor/gone', available: served.available })
    expect(modelChoices(gone).map((c) => c.label)).toEqual(['vendor/a (openrouter)', 'vendor/gone (not served)'])
    const set = create(GetAgentModelResponseSchema, { setting: 'vendor/a', available: served.available })
    expect(modelChoices(set)).toHaveLength(1)
  })
})
