// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { approveLabel, toolChanged, toolVersion } from '@/lib/approvals'

describe('MCP approval helpers', () => {
  it('shortens the content hash to 8 characters', () => {
    expect(toolVersion('0123456789abcdef')).toBe('01234567')
  })
  it('flags a change only when the current hash is known and differs', () => {
    expect(toolChanged('aaa', '')).toBe(false)
    expect(toolChanged('aaa', 'aaa')).toBe(false)
    expect(toolChanged('aaa', 'bbb')).toBe(true)
  })
  it('labels the approve button by approval round', () => {
    expect(approveLabel(1, '')).toBe('Approve')
    expect(approveLabel(0, '')).toBe('Approve')
    expect(approveLabel(2, '')).toBe('Give first approval')
    expect(approveLabel(2, 'u-2')).toBe('Give second approval')
  })
})
