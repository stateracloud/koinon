// SPDX-License-Identifier: Apache-2.0

import { execFileSync } from 'node:child_process'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// The lint rule koinon/no-raw-message (lint/koinon-plugin.js) forbids `rawMessage` outside src/lib/api-error.ts
// (#778). The fixture reads it in six ways (member, index, optional, template, two destructurings), so the rule must fire six times. `pnpm lint` ignores the fixture.
describe('lint rule koinon/no-raw-message', () => {
  it('fires on each read of rawMessage in the fixture', () => {
    const cwd = resolve(import.meta.dirname, '../..')
    let out = ''
    try {
      out = execFileSync('node_modules/.bin/oxlint', ['-c', 'lint/fixtures/oxlintrc.json', 'lint/fixtures/raw-message.ts'], { encoding: 'utf8', cwd })
    } catch (e) {
      out = String((e as { stdout?: string }).stdout ?? '')
    }
    expect(out.match(/koinon\(no-raw-message\)/g)?.length).toBe(6)
  })
})
