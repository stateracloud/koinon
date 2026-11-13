// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { bypassFromEnv, proxyFromEnv } from '@/lib/playwright-proxy'

describe('playwright proxy from env', () => {
  it('is unset without proxy env', () => {
    expect(proxyFromEnv({})).toBeUndefined()
    expect(proxyFromEnv({ HTTPS_PROXY: '  ', NO_PROXY: 'a' })).toBeUndefined()
  })

  it('splits credentials out of the server and decodes them', () => {
    const p = proxyFromEnv({ HTTPS_PROXY: 'http://koinon_test:not%2Da%40real-token@proxy.invalid:3128' })
    expect(p).toEqual({
      server: 'http://proxy.invalid:3128',
      username: 'koinon_test',
      password: 'not-a@real-token',
      bypass: '127.0.0.1,localhost',
    })
    expect(p?.server).not.toContain('koinon_test')
  })

  it('works without credentials, with a lower-case name and a bare host', () => {
    expect(proxyFromEnv({ https_proxy: 'proxy.invalid:3128' })).toEqual({
      server: 'http://proxy.invalid:3128',
      bypass: '127.0.0.1,localhost',
    })
  })

  it('prefers the upper-case name', () => {
    expect(proxyFromEnv({ HTTPS_PROXY: 'http://a.invalid:1', https_proxy: 'http://b.invalid:2' })?.server).toBe(
      'http://a.invalid:1',
    )
  })

  it('puts NO_PROXY first and does not repeat loopback', () => {
    expect(bypassFromEnv({ no_proxy: ' .svc.invalid, localhost ,,10.0.0.0/8' })).toBe(
      '.svc.invalid,localhost,10.0.0.0/8,127.0.0.1',
    )
  })

  it('never echoes the value in an error', () => {
    expect(() => proxyFromEnv({ HTTPS_PROXY: 'http://koinon_test:not-a-real-token@[bad' })).toThrow(
      /not a valid URL/,
    )
    try {
      proxyFromEnv({ HTTPS_PROXY: 'http://koinon_test:not-a-real-token@[bad' })
    } catch (e) {
      expect(String(e)).not.toContain('not-a-real-token')
    }
  })
})
