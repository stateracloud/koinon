// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { age } from '@/lib/time'

const minute = 60_000
const hour = 60 * minute

describe('age', () => {
  it('rounds down to the largest whole unit', () => {
    const now = Date.UTC(2026, 9, 28, 12, 0, 0)
    expect(age(now - 20_000, now)).toBe('less than a minute')
    expect(age(now - minute, now)).toBe('1 minute')
    expect(age(now - 59 * minute, now)).toBe('59 minutes')
    expect(age(now - 3 * hour - 40 * minute, now)).toBe('3 hours')
    expect(age(now - 2 * 24 * hour - hour, now)).toBe('2 days')
  })

  it('never goes negative', () => {
    expect(age(1_000_000, 0)).toBe('less than a minute')
  })

  it('counts elapsed time across a daylight-saving change, not calendar days', () => {
    // 24 hours before 2026-10-26 12:00 Europe/Zurich (after the change on the 25th) is 1 day by
    // elapsed time although the calendar dates differ by one and the wall clock moved an hour.
    const now = new Date('2026-10-26T12:00:00+01:00').getTime()
    const since = now - 24 * hour
    expect(age(since, now)).toBe('1 day')
    expect(age(since + minute, now)).toBe('23 hours')
  })
})
