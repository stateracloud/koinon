// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { templateChoiceError, templateSlugOf, type TakenSlugs } from '@/hooks/use-new-agent'

describe('templateSlugOf', () => {
  it('makes the slug as the server does', () => {
    expect(templateSlugOf('Repo  Helper!')).toBe('repo-helper')
    expect(templateSlugOf('--Our__Triage--')).toBe('our-triage')
    expect(templateSlugOf('!!!')).toBe('')
  })

  it('cuts at 64 characters and trims a dash at the cut', () => {
    expect(templateSlugOf('a'.repeat(70))).toBe('a'.repeat(64))
    expect(templateSlugOf(`${'a'.repeat(63)} b`)).toBe('a'.repeat(63))
  })
})

describe('templateChoiceError', () => {
  const choice = { save: true, displayName: 'Our helper', description: '' }
  const ready: TakenSlugs = { state: 'ready', platform: ['repo-helper'], org: ['our-triage'] }

  it('accepts a free name only when the template list is loaded', () => {
    expect(templateChoiceError(choice, ready)).toBeUndefined()
    expect(templateChoiceError(choice, { ...ready, state: 'loading' })).toMatch(/still loading/)
    expect(templateChoiceError(choice, { ...ready, state: 'failed' })).toMatch(/could not load/)
    expect(templateChoiceError({ ...choice, save: false }, { ...ready, state: 'failed' })).toBeUndefined()
  })

  it('refuses a taken platform or org slug', () => {
    expect(templateChoiceError({ ...choice, displayName: 'Repo helper' }, ready)).toMatch(/platform template/)
    expect(templateChoiceError({ ...choice, displayName: 'Our Triage' }, ready)).toMatch(/This org has/)
  })
})
