// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { arr, obj, str } from '@/lib/json'

describe('JSON guards', () => {
  it('obj accepts only objects', () => {
    expect(obj({ a: 1 })).toEqual({ a: 1 })
    expect(obj([1])).toBeUndefined()
    expect(obj(null)).toBeUndefined()
    expect(obj('x')).toBeUndefined()
    expect(obj(undefined)).toBeUndefined()
  })

  it('arr accepts only arrays', () => {
    expect(arr([1, 'a'])).toEqual([1, 'a'])
    expect(arr({ length: 1 })).toEqual([])
    expect(arr(undefined)).toEqual([])
  })

  it('str accepts only strings', () => {
    expect(str('')).toBe('')
    expect(str(1)).toBeUndefined()
    expect(str(null)).toBeUndefined()
  })
})
