// SPDX-License-Identifier: Apache-2.0

/// <reference types="node" />
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { callbackError, classifyDexPage, dexPageError } from '@/lib/drill-dex'

// The pages Dex v2.46.1 renders; test/drill/oidcflow_test.go classifies the same files.
const fixture = (name: string): string =>
  readFileSync(resolve(dirname(fileURLToPath(import.meta.url)), '../../../test/drill/testdata/dex', name), 'utf8')

describe('the drill reads the pages of its own Dex', () => {
  const cases: [string, string, string | undefined][] = [
    ['login.html', 'login-form', undefined],
    ['wrong-password.html', 'wrong-password', 'dex refused the password for drill-admin@drill.invalid'],
    ['approval.html', 'approval', 'dex asks for approval: skipApprovalScreen is off'],
    ['chooser.html', 'chooser', 'dex shows a connector chooser'],
    ['mfa-totp.html', 'mfa', 'dex asks for a second factor'],
    ['error.html', 'error', 'dex error: Bad Request: Unregistered redirect_uri ("http://127.0.0.1:9/auth/oidc/callback").'],
  ]
  it.each(cases)('%s is a %s page', (file, kind, message) => {
    const page = classifyDexPage(fixture(file))
    expect(page.kind).toBe(kind)
    const err = dexPageError(page, 'drill-admin@drill.invalid')
    if (message === undefined) expect(err).toBeUndefined()
    else expect(err?.message).toContain(message)
  })

  it('does not take a page that is not Dex for a login form', () => {
    const page = classifyDexPage('<html><body><h1>Inbox</h1></body></html>')
    expect(page.kind).toBe('unknown')
    expect(dexPageError(page, 'x')).toBeInstanceOf(Error)
  })

  it('names a refusal of the callback and keeps a redirect quiet', () => {
    expect(callbackError(302, '')).toBeUndefined()
    expect(callbackError(403, 'this account is disabled\n')?.message).toBe('koinon callback 403: this account is disabled')
    expect(callbackError(503, 'x'.repeat(300))?.message).toHaveLength('koinon callback 503: '.length + 203)
  })
})

describe('the drill strips markup from a Dex page without leaving a tag', () => {
  const headingOf = (inner: string): string => classifyDexPage(`<h2 class="theme-heading">${inner}</h2>`).heading
  it('strips a tag nested in a tag', () => {
    // The inner tag goes first and the outer one is left without its opening, so no "<" survives.
    expect(headingOf('<scr<script>ipt>alert(1)</script>')).toBe('ipt>alert(1)')
  })
  it('decodes an escaped tag to literal text after the strip', () => {
    expect(headingOf('&lt;b&gt;')).toBe('<b>')
  })
  it('decodes &amp;lt; once, to &lt;', () => {
    expect(headingOf('&amp;lt;')).toBe('&lt;')
  })
})
